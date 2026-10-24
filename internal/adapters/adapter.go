// Package adapters defines how Pilothouse talks to one coding agent: reading
// its hook payloads, answering them, and installing hooks into its settings.
// Each agent lives in its own subpackage; internal/adapters/all lists them.
package adapters

import (
	"encoding/json"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
)

// Proc is one process in the hook shim's ancestry.
type Proc struct {
	PID  int    `json:"pid"`
	PPID int    `json:"ppid"`
	Comm string `json:"comm"` // executable base name
	Args string `json:"args,omitempty"`
}

// HookEnvelope is what the `pilothouse hook <agent>` shim sends to the daemon
// (POST /v1/hook on the Unix socket). The shim adds process context the
// agent's own payload lacks.
type HookEnvelope struct {
	Agent               api.AgentKind     `json:"agent"`
	Event               string            `json:"event,omitempty"` // from argv, if the agent passes it there
	Payload             json.RawMessage   `json:"payload"`         // stdin, verbatim
	ShimPID             int               `json:"shim_pid"`
	Ancestors           []Proc            `json:"ancestors"`                       // nearest first, up to 12
	PilothouseSessionID string            `json:"pilothouse_session_id,omitempty"` // $PILOTHOUSE_SESSION_ID (wrapped sessions)
	Cwd                 string            `json:"cwd"`
	TTY                 string            `json:"tty,omitempty"`
	Env                 map[string]string `json:"env,omitempty"` // allow-listed: TERM_PROGRAM, TMUX, PILOTHOUSE_TASK_ID, ...
	// Messaging is where a person's follow-up prompt can be delivered to
	// this session (Claude Code's CLAUDE_CODE_MESSAGING_SOCKET and _TOKEN).
	// The daemon keeps it in session_secrets only: never in events,
	// evidence, explain output, logs or the API.
	Messaging  *MessagingEndpoint `json:"messaging,omitempty"`
	ReceivedAt time.Time          `json:"received_at"`
}

// MessagingEndpoint is an agent's own channel for messages into a session.
type MessagingEndpoint struct {
	Socket string `json:"socket"`
	Token  string `json:"token"`
}

// HookKind is a vendor-neutral hook event.
type HookKind string

const (
	HookSessionStart HookKind = "session_start"
	HookSessionEnd   HookKind = "session_end"
	HookPromptSubmit HookKind = "prompt_submit"
	HookPreTool      HookKind = "pre_tool" // checkpoint ①: policy
	HookPostTool     HookKind = "post_tool"
	HookPermission   HookKind = "permission" // checkpoint ②: people
	HookNotification HookKind = "notification"
	HookStop         HookKind = "stop"
	HookSubagentStop HookKind = "subagent_stop"
	HookPreCompact   HookKind = "pre_compact"
	HookUnknown      HookKind = "unknown"
)

// HookEvent is a parsed, normalized hook payload.
type HookEvent struct {
	Kind             HookKind        `json:"kind"`
	VendorEvent      string          `json:"vendor_event"` // e.g. "PreToolUse"
	AgentSessionID   string          `json:"agent_session_id"`
	Cwd              string          `json:"cwd"`
	TranscriptPath   string          `json:"transcript_path,omitempty"`
	ToolUseID        string          `json:"tool_use_id,omitempty"`
	Tool             string          `json:"tool,omitempty"`
	ToolKind         api.ToolKind    `json:"tool_kind,omitempty"`
	Command          string          `json:"command,omitempty"`
	Paths            []string        `json:"paths,omitempty"`
	Input            json.RawMessage `json:"input,omitempty"`
	Message          string          `json:"message,omitempty"` // prompt, notification text, last assistant message
	NotificationType string          `json:"notification_type,omitempty"`
	Summary          string          `json:"summary,omitempty"` // "Run: git push origin main"
	PermissionMode   string          `json:"permission_mode,omitempty"`
	// Questions and Plan are set when the tool asks a person something
	// (Claude AskUserQuestion / ExitPlanMode, OpenCode question): the
	// permission checkpoint then raises a question or plan request.
	Questions []api.Question `json:"questions,omitempty"`
	Plan      *api.Plan      `json:"plan,omitempty"`
}

// HookOutcome is what the daemon decided for one hook call.
// Behavior "" means "no opinion": the agent carries on as if Pilothouse were absent.
type HookOutcome struct {
	Behavior api.Behavior // allow | deny | ask | ""
	Message  string       // reason shown to the agent / user
	Context  string       // optional extra context for the model (session_start, prompt_submit)
	// Rule is the policy rule that decided (policy.Result.Rule), if any.
	// Adapters whose agent cannot express a decision use it to pick the
	// fallback: Codex turns a pre_tool "ask" into a deny, worded
	// differently for the built-in self-protection rule.
	Rule string
	// Answers answers a question request (Question.Key() → answer); the
	// adapter turns them into the agent's own answer format.
	Answers map[string]string
}

// HookResponse is how the shim answers the agent: print Stdout, print
// Stderr, exit with ExitCode.
type HookResponse struct {
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	ExitCode int    `json:"exit_code"`
}

// Detection describes whether an agent is installed on this machine.
type Detection struct {
	Installed    bool   `json:"installed"`
	Version      string `json:"version,omitempty"`
	Binary       string `json:"binary,omitempty"`
	SettingsPath string `json:"settings_path"`
	Note         string `json:"note,omitempty"`
}

// InstallOptions controls hook installation.
type InstallOptions struct {
	// ShimPath is the absolute path of the pilothouse binary to call.
	ShimPath string
	// DryRun computes the change without writing.
	DryRun bool
}

// Change reports what InstallHooks/UninstallHooks did or would do.
type Change struct {
	Path    string `json:"path"`
	Changed bool   `json:"changed"`
	Before  string `json:"before,omitempty"`
	After   string `json:"after,omitempty"`
	Summary string `json:"summary"`
	Backup  string `json:"backup,omitempty"` // path of the backup written before changing
	// More lists further files the same operation changes (Codex: hook
	// trust entries in config.toml next to hooks.json).
	More []Change `json:"more,omitempty"`
}

// HookStatus reports whether Pilothouse's hooks are installed and current.
type HookStatus struct {
	Installed bool     `json:"installed"`
	Current   bool     `json:"current"` // installed and matches what InstallHooks would write
	Events    []string `json:"events"`  // vendor events wired to pilothouse
	Path      string   `json:"path"`
	Problems  []string `json:"problems,omitempty"`
}

// Adapter is one coding agent.
type Adapter interface {
	Kind() api.AgentKind
	DisplayName() string // "Claude Code"
	// ProcessNames are executable names of the agent (ancestry match, L0).
	ProcessNames() []string
	Detect() Detection

	ParseHook(env HookEnvelope) (HookEvent, error)
	// EncodeResponse turns a decision into the agent's hook protocol.
	EncodeResponse(ev HookEvent, out HookOutcome) HookResponse
	// FailOpenResponse is what the shim prints when the daemon is unreachable.
	// failClosed=true makes pre_tool calls deny with an explanation.
	FailResponse(kind HookKind, failClosed bool) HookResponse
	// MaxHold is the longest the daemon may block on this hook kind before
	// the agent's own hook timeout kills the shim. Zero = must not block.
	MaxHold(kind HookKind) time.Duration

	InstallHooks(opts InstallOptions) (Change, error) // idempotent, merges with existing hooks, backs up
	UninstallHooks(opts InstallOptions) (Change, error)
	Status(opts InstallOptions) (HookStatus, error)

	// ProtectedPaths are settings files an agent must not edit without a
	// person's approval (self-approval hardening).
	ProtectedPaths() []string
	// ResumeArgs is the command line to resume a vendor session, if supported.
	ResumeArgs(agentSessionID string) []string
}
