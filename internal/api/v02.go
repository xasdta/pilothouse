package api

import (
	"strings"
	"time"
)

// Wire types added in 0.2 (docs/PLAN-0.2.md). Like the rest of this
// package, field names are part of the public protocol.

// ---------- Questions and plans (F1) ----------

// Question is one question an agent asked: Claude's AskUserQuestion,
// OpenCode's question tool.
type Question struct {
	// ID is the agent's own id for the question when it has one
	// (OpenCode); Claude identifies questions by their text.
	ID          string           `json:"id,omitempty"`
	Header      string           `json:"header,omitempty"` // short chip label, e.g. "Auth method"
	Text        string           `json:"question"`
	Options     []QuestionOption `json:"options,omitempty"`
	MultiSelect bool             `json:"multi_select,omitempty"`
	// AllowOther says a free-text answer is accepted besides the options.
	AllowOther bool `json:"allow_other"`
}

// Key is how Decision.Answers refers to this question: its ID, else its text.
func (q Question) Key() string {
	if q.ID != "" {
		return q.ID
	}
	return q.Text
}

// QuestionOption is one choice offered for a Question.
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// MultiAnswerSep joins the labels of a multi-select answer.
const MultiAnswerSep = ", "

// JoinAnswers is the Decision.Answers value for several chosen labels.
func JoinAnswers(labels []string) string { return strings.Join(labels, MultiAnswerSep) }

// Plan is a plan an agent wants approved before it starts (Claude's
// ExitPlanMode). Allow approves it; deny with a Message asks for changes.
type Plan struct {
	Markdown string `json:"markdown"`
	Path     string `json:"path,omitempty"` // plan file, when the agent wrote one
}

// ---------- Risk and previews (F4) ----------

// RiskLevel orders RiskLabels.
type RiskLevel string

const (
	RiskHigh   RiskLevel = "high"
	RiskMedium RiskLevel = "medium"
	RiskLow    RiskLevel = "low"
)

// Risk codes. Clients style unknown codes by Level.
const (
	RiskDestructive     = "destructive"      // rm -rf, reset --hard, DROP TABLE, mkfs …
	RiskForcePush       = "force_push"       // git push --force / +ref
	RiskProtectedBranch = "protected_branch" // pushes or commits to main/master/release
	RiskDeploy          = "deploy"           // kubectl apply, terraform apply, fly deploy …
	RiskSecrets         = "secrets"          // .env, ~/.ssh, credentials, tokens in arguments
	RiskNetwork         = "network"          // curl | sh, uploads, fetches
	RiskInstall         = "install"          // package installs, global installs
	RiskSudo            = "sudo"             // privilege escalation
	RiskOutsideRepo     = "outside_repo"     // writes outside the session's repo
)

// RiskLabel is one reason a tool call deserves a careful look.
type RiskLabel struct {
	Code  string    `json:"code"`
	Level RiskLevel `json:"level"`
	Text  string    `json:"text"` // one short sentence: "Force-pushes to main"
}

// FileOp is what a tool call does to a file.
type FileOp string

const (
	FileEdit      FileOp = "edit"
	FileCreate    FileOp = "create"
	FileOverwrite FileOp = "overwrite"
	FileDelete    FileOp = "delete"
	FileRename    FileOp = "rename"
)

// FilePreview is one file in a Preview or a task diff.
type FilePreview struct {
	Path    string `json:"path"`
	Op      FileOp `json:"op"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	// Diff is a unified diff (without the ---/+++ header), capped in size;
	// Truncated says it was cut.
	Diff      string `json:"diff,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Lang      string `json:"lang,omitempty"` // highlighting hint from the extension: go, ts, py, …
}

// Preview is what a file tool call would change.
type Preview struct {
	Files     []FilePreview `json:"files"`
	Added     int           `json:"added"`
	Removed   int           `json:"removed"`
	Truncated bool          `json:"truncated,omitempty"` // files were left out
}

// ---------- Sessions: capabilities, replies, usage (F2, F3, F6) ----------

// ReplyVia says how a follow-up prompt reaches a session.
type ReplyVia string

const (
	ReplyTerminal ReplyVia = "terminal" // typed into the PTY (`pilothouse run`)
	ReplyMessage  ReplyVia = "message"  // Claude Code's session messaging socket
	ReplyPlugin   ReplyVia = "plugin"   // the agent's Pilothouse plugin (OpenCode)
)

// SessionCaps lists what a person can do with a session.
type SessionCaps struct {
	Reply     bool     `json:"reply,omitempty"`
	ReplyVia  ReplyVia `json:"reply_via,omitempty"`
	Interrupt bool     `json:"interrupt,omitempty"` // Esc / Ctrl-C (wrapped sessions)
	Watch     bool     `json:"watch,omitempty"`     // live terminal mirror (wrapped sessions)
	Activity  bool     `json:"activity,omitempty"`  // a readable transcript exists
}

// Reply is a follow-up prompt or an interrupt sent to a session.
type Reply struct {
	Text string `json:"text,omitempty"`
	// Interrupt sends Esc before the text (stop what you're doing). With
	// no Text it only interrupts.
	Interrupt bool `json:"interrupt,omitempty"`
}

// ReplyResult says how a Reply was delivered.
type ReplyResult struct {
	Via  ReplyVia `json:"via"`
	Note string   `json:"note,omitempty"` // e.g. "Claude sees this as a message from Pilothouse"
}

// ReplyRecord is the Data of a session.reply event. The text is kept so
// the activity feed and audit log show what was sent.
type ReplyRecord struct {
	SessionID string    `json:"session_id"`
	Text      string    `json:"text,omitempty"`
	Interrupt bool      `json:"interrupt,omitempty"`
	Via       ReplyVia  `json:"via"`
	By        string    `json:"by"`
	At        time.Time `json:"at"`
}

// Usage is token use and an estimated cost.
type Usage struct {
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64   `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  int64   `json:"reasoning_tokens,omitempty"`
	CostUSD          float64 `json:"cost_usd"`        // estimate from list prices; 0 when the model is unknown
	Model            string  `json:"model,omitempty"` // most recent model
	Turns            int     `json:"turns,omitempty"`
}

// Add accumulates o into u (Model and Turns: o's model wins when set).
func (u *Usage) Add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheReadTokens += o.CacheReadTokens
	u.CacheWriteTokens += o.CacheWriteTokens
	u.ReasoningTokens += o.ReasoningTokens
	u.CostUSD += o.CostUSD
	u.Turns += o.Turns
	if o.Model != "" {
		u.Model = o.Model
	}
}

// ActivityKind classifies an ActivityItem.
type ActivityKind string

const (
	ActivityPrompt   ActivityKind = "prompt"   // a person (or a reply from Pilothouse) prompted the agent
	ActivityText     ActivityKind = "text"     // the agent wrote to the person
	ActivityTool     ActivityKind = "tool"     // a tool call, with its result folded in
	ActivitySubagent ActivityKind = "subagent" // a subagent started or finished
	ActivityError    ActivityKind = "error"    // an API or tool error worth showing on its own
	ActivitySystem   ActivityKind = "system"   // compaction, turn finished
)

// ActivityItem is one entry in a session's activity feed.
type ActivityItem struct {
	ID        string       `json:"id"` // stable across reads (record uuid / call id)
	Kind      ActivityKind `json:"kind"`
	At        time.Time    `json:"at"`
	Turn      int          `json:"turn"`           // 1-based: increments at each prompt
	Text      string       `json:"text,omitempty"` // capped
	Tool      string       `json:"tool,omitempty"`
	ToolKind  ToolKind     `json:"tool_kind,omitempty"`
	Summary   string       `json:"summary,omitempty"` // "Run: npm test", "Edit internal/x.go"
	Result    string       `json:"result,omitempty"`  // tail of the tool output, capped
	IsError   bool         `json:"is_error,omitempty"`
	Source    string       `json:"source,omitempty"` // prompts: "human" | "pilothouse" | "peer"
	Sidechain bool         `json:"sidechain,omitempty"`
	Pending   bool         `json:"pending,omitempty"` // tool call without a result yet
}

// FileChange is a file a session touched.
type FileChange struct {
	Path    string `json:"path"`
	Op      FileOp `json:"op"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
}

// CatchUp is a deterministic summary of a session since a moment.
type CatchUp struct {
	Since     time.Time `json:"since"`
	Lines     []string  `json:"lines"` // short sentences, most important first
	ToolCalls int       `json:"tool_calls"`
	Files     int       `json:"files"`
	Errors    int       `json:"errors"`
}

// ActivityQuery selects part of a feed.
type ActivityQuery struct {
	Cursor string    `json:"cursor,omitempty"` // from a previous Activity: only newer items
	Limit  int       `json:"limit,omitempty"`  // default 200, max 1000 (newest kept)
	Since  time.Time `json:"since,omitempty"`  // catch-up start; zero = when a person last looked
}

// Activity is a session's feed plus what can be derived from it.
type Activity struct {
	SessionID string         `json:"session_id"`
	Available bool           `json:"available"` // false: no transcript Pilothouse can read
	Note      string         `json:"note,omitempty"`
	Items     []ActivityItem `json:"items"`
	Cursor    string         `json:"cursor"`
	Usage     *Usage         `json:"usage,omitempty"`
	Files     []FileChange   `json:"files,omitempty"`
	CatchUp   *CatchUp       `json:"catch_up,omitempty"`
}

// ---------- Insights (F6) ----------

// InsightsQuery selects a time range (days back from today, local time).
type InsightsQuery struct {
	Days int `json:"days,omitempty"` // default 7, max 90
}

// UsageGroup is usage grouped by agent, model or repo.
type UsageGroup struct {
	Key      string `json:"key"`
	Usage    Usage  `json:"usage"`
	Sessions int    `json:"sessions"`
}

// InsightsDay is one calendar day.
type InsightsDay struct {
	Date        string `json:"date"` // YYYY-MM-DD, local time
	Usage       Usage  `json:"usage"`
	Sessions    int    `json:"sessions"`
	AutoAllowed int    `json:"auto_allowed"` // pre_tool allows by rules and grants
	Asked       int    `json:"asked"`        // requests that reached a person
	Blocked     int    `json:"blocked"`      // denied by rules
}

// RuleCount counts decisions per rule or command.
type RuleCount struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}

// RuleStats is what the rules and people decided in a range.
type RuleStats struct {
	AutoAllowed        int         `json:"auto_allowed"`
	Asked              int         `json:"asked"`
	Blocked            int         `json:"blocked"`
	Granted            int         `json:"granted"` // allowed by "allow for session"
	AnsweredByDevice   int         `json:"answered_by_device"`
	AnsweredAtTerminal int         `json:"answered_at_terminal"`
	Expired            int         `json:"expired"`
	MedianAnswerSecs   float64     `json:"median_answer_secs"`
	BlockedMinutes     float64     `json:"blocked_minutes"` // agent time spent waiting on a person
	TopRules           []RuleCount `json:"top_rules,omitempty"`
	TopBlocked         []RuleCount `json:"top_blocked,omitempty"`
}

// BudgetStatus compares today's estimated spend with usage.daily_budget_usd.
type BudgetStatus struct {
	DailyUSD float64 `json:"daily_usd"`
	SpentUSD float64 `json:"spent_usd"`
	Percent  float64 `json:"percent"`
	BurnUSDh float64 `json:"burn_usd_per_hour"` // last hour
}

// Insights is the dashboard data for a range.
type Insights struct {
	From    time.Time     `json:"from"`
	To      time.Time     `json:"to"`
	Total   Usage         `json:"total"`
	Days    []InsightsDay `json:"days"`
	ByAgent []UsageGroup  `json:"by_agent"`
	ByModel []UsageGroup  `json:"by_model"`
	ByRepo  []UsageGroup  `json:"by_repo"`
	Rules   RuleStats     `json:"rules"`
	Budget  *BudgetStatus `json:"budget,omitempty"`
}

// ---------- Rules (F5) ----------

// RuleSpec is a rule as written to a policy file: the `match:` keys are
// agent, tool, kind, command, path, cwd, repo, branch (glob lists).
type RuleSpec struct {
	Name        string              `json:"name"`
	Description string              `json:"description,omitempty"`
	Action      Behavior            `json:"action"`
	Match       map[string][]string `json:"match"`
	When        string              `json:"when,omitempty"` // CEL; not accepted from devices
	Message     string              `json:"message,omitempty"`
}

// Rule sources.
const (
	RuleSourceBuiltin = "builtin"
	RuleSourceUser    = "user"    // ~/.pilothouse/policy.yaml (hand-written)
	RuleSourceManaged = "managed" // ~/.pilothouse/rules.yaml (from the inbox)
	RuleSourcePreset  = "preset"
	RuleSourceRepo    = "repo"
)

// RuleInfo is one loaded rule.
type RuleInfo struct {
	RuleSpec
	Source  string    `json:"source"`           // builtin | user | managed | preset | repo
	Preset  string    `json:"preset,omitempty"` // preset name for source=preset
	File    string    `json:"file,omitempty"`
	Enabled bool      `json:"enabled"`
	Managed bool      `json:"managed"` // devices may disable or remove it
	Created time.Time `json:"created,omitempty"`
	By      string    `json:"by,omitempty"` // who added a managed rule
}

// Preset is a named bundle of rules that can be switched on and off.
type Preset struct {
	Name        string     `json:"name"` // guarded-autopilot, protect-main, no-secrets, deploys-need-approval
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Enabled     bool       `json:"enabled"`
	Rules       []RuleSpec `json:"rules"`
}

// RulesView is everything the rules page shows.
type RulesView struct {
	Rules    []RuleInfo `json:"rules"` // evaluation order
	Presets  []Preset   `json:"presets"`
	Problems []string   `json:"problems,omitempty"` // policy files that failed to load
}

// RuleSuggestion is a rule a person can create with one tap.
type RuleSuggestion struct {
	ID     string   `json:"id"`    // stable for this request + spec
	Title  string   `json:"title"` // "Always allow `npm test` in api-server"
	Scope  string   `json:"scope"` // "repo" | "everywhere"
	Reason string   `json:"reason,omitempty"`
	Rule   RuleSpec `json:"rule"`
}

// RuleChange is the Data of a policy.changed event.
type RuleChange struct {
	Op   string    `json:"op"` // add | remove | enable | disable | preset_on | preset_off
	Name string    `json:"name"`
	By   string    `json:"by"`
	At   time.Time `json:"at"`
}

// RuleTestInput is a hypothetical tool call for "would this be allowed?".
type RuleTestInput struct {
	Agent   AgentKind `json:"agent,omitempty"` // default claude
	Tool    string    `json:"tool,omitempty"`  // default Bash for commands, Edit for paths
	Command string    `json:"command,omitempty"`
	Path    string    `json:"path,omitempty"`
	Cwd     string    `json:"cwd,omitempty"`
}

// RuleTestResult is what the policy would decide.
type RuleTestResult struct {
	Behavior Behavior `json:"behavior"` // "" = no rule matched (the agent's own settings decide)
	Rule     string   `json:"rule,omitempty"`
	Matches  []string `json:"matches,omitempty"` // every matching rule, strongest first
	Message  string   `json:"message,omitempty"`
}

// ---------- Tasks from anywhere (F7) ----------

// RepoInfo is a repository Pilothouse has seen sessions in.
type RepoInfo struct {
	Root     string    `json:"root"`
	Name     string    `json:"name"`
	Branch   string    `json:"branch,omitempty"`
	LastUsed time.Time `json:"last_used"`
}

// TaskStart starts a task from a device or the CLI.
type TaskStart struct {
	Repo   string    `json:"repo"` // must be a known repo root
	Agent  AgentKind `json:"agent"`
	Prompt string    `json:"prompt"`
	Base   string    `json:"base,omitempty"` // default: the repo's current branch
	Name   string    `json:"name,omitempty"` // branch slug; default from the prompt
}

// TaskDiff is what a task changed relative to its base.
type TaskDiff struct {
	TaskID    string        `json:"task_id"`
	Files     []FilePreview `json:"files"`
	Added     int           `json:"added"`
	Removed   int           `json:"removed"`
	Truncated bool          `json:"truncated,omitempty"`
}

// TaskFinish is how a person ends a task.
type TaskFinish struct {
	Action string `json:"action"` // "ship" (push + open a PR) | "discard"
}
