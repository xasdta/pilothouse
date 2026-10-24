// Package api holds the wire types shared by the daemon, its clients
// (CLI, TUI, web app) and the agent adapters. Everything here is JSON
// on the wire; field names are part of the public protocol (PROTOCOL.md).
package api

import (
	"encoding/json"
	"time"
)

// ProtocolVersion is bumped on any breaking change to the types below.
const ProtocolVersion = 1

// AgentKind names a supported coding agent.
type AgentKind string

const (
	AgentClaude  AgentKind = "claude"
	AgentCodex   AgentKind = "codex"
	AgentGeneric AgentKind = "generic" // anything run under `pilothouse run` without an adapter
)

// Mode is how Pilothouse is attached to a session.
type Mode string

const (
	ModeObserved Mode = "observed" // hooks only; the agent runs in any terminal
	ModeWrapped  Mode = "wrapped"  // started with `pilothouse run`; Pilothouse holds the PTY
)

// State is the resolved state of a session.
type State string

const (
	StateStarting State = "starting"
	StateWorking  State = "working"
	StateBlocked  State = "blocked" // needs a person: see BlockKind
	StateWaiting  State = "waiting" // finished its turn, waiting for the next prompt
	StateErrored  State = "errored"
	StateExited   State = "exited"
	StateUnknown  State = "unknown"
)

// BlockKind says why a session is blocked.
type BlockKind string

const (
	BlockNone       BlockKind = ""
	BlockPermission BlockKind = "permission"
	BlockQuestion   BlockKind = "question"
	BlockAuth       BlockKind = "auth"
)

// Layer is where a piece of state evidence came from. Lower number = weaker.
type Layer string

const (
	LayerProcess Layer = "L0-process"
	LayerOSC     Layer = "L1-osc"
	LayerHook    Layer = "L2-hook"
	LayerManual  Layer = "manual"
)

// Session is one running (or finished) agent conversation.
type Session struct {
	ID             string     `json:"id"`
	Agent          AgentKind  `json:"agent"`
	Mode           Mode       `json:"mode"`
	Machine        string     `json:"machine"`
	Title          string     `json:"title"` // repo/dir name or --name
	Cwd            string     `json:"cwd"`
	RepoRoot       string     `json:"repo_root,omitempty"`
	Branch         string     `json:"branch,omitempty"`
	PID            int        `json:"pid,omitempty"`              // agent process, if known
	AgentSessionID string     `json:"agent_session_id,omitempty"` // vendor's own session id
	TranscriptPath string     `json:"transcript_path,omitempty"`
	State          State      `json:"state"`
	BlockKind      BlockKind  `json:"block_kind,omitempty"`
	StateLayer     Layer      `json:"state_layer,omitempty"`
	StateDetail    string     `json:"state_detail,omitempty"` // human sentence: "Asked to run git push"
	LastMessage    string     `json:"last_message,omitempty"` // last thing the agent said, trimmed
	Unseen         bool       `json:"unseen"`                 // changed to blocked/waiting since a person last looked
	HolderSock     string     `json:"holder_sock,omitempty"`  // wrapped sessions only
	TaskID         string     `json:"task_id,omitempty"`
	StartedAt      time.Time  `json:"started_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	ExitedAt       *time.Time `json:"exited_at,omitempty"`
	ExitCode       *int       `json:"exit_code,omitempty"`
	// Caps says what a person can do with this session beyond answering
	// its requests (reply, interrupt, watch the terminal, read activity).
	Caps SessionCaps `json:"caps"`
	// Usage is the latest token/cost estimate from the agent's transcript;
	// nil until the activity reader has seen one.
	Usage *Usage `json:"usage,omitempty"`
}

// Checkpoint is where in the agent's flow a request was raised.
type Checkpoint string

const (
	CheckpointPreTool    Checkpoint = "pre_tool"   // before every tool call: policy only
	CheckpointPermission Checkpoint = "permission" // agent wants a person to approve
	CheckpointQuestion   Checkpoint = "question"   // agent asked a question (see Request.Questions)
	CheckpointPlan       Checkpoint = "plan"       // agent wants a plan approved (see Request.Plan)
)

// RequestStatus is the lifecycle of a Request.
type RequestStatus string

const (
	RequestOpen            RequestStatus = "open"             // held, waiting for an answer
	RequestDecided         RequestStatus = "decided"          // answered through Pilothouse
	RequestSteppedAside    RequestStatus = "stepped_aside"    // handed to the agent's own dialog
	RequestAnsweredLocally RequestStatus = "answered_locally" // a person answered in the terminal
	RequestExpired         RequestStatus = "expired"          // hold limit hit; handed back
)

// Route is where a permission request was sent.
type Route string

const (
	RouteTerminal Route = "terminal" // person is at the desk: native dialog
	RouteDevices  Route = "devices"  // person is away: held, pushed to devices
)

// ToolKind is a vendor-neutral classification of a tool call.
type ToolKind string

const (
	ToolShell ToolKind = "shell"
	ToolEdit  ToolKind = "edit" // write/edit/patch files
	ToolRead  ToolKind = "read" // read/glob/grep/list
	ToolWeb   ToolKind = "web"  // fetch/search
	ToolMCP   ToolKind = "mcp"
	ToolOther ToolKind = "other"
)

// Request is something an agent needs decided.
type Request struct {
	ID         string          `json:"id"`
	SessionID  string          `json:"session_id"`
	Agent      AgentKind       `json:"agent"`
	Checkpoint Checkpoint      `json:"checkpoint"`
	Tool       string          `json:"tool"` // vendor tool name, e.g. "Bash"
	ToolKind   ToolKind        `json:"tool_kind"`
	Command    string          `json:"command,omitempty"` // shell tools
	Paths      []string        `json:"paths,omitempty"`   // file tools
	Input      json.RawMessage `json:"input,omitempty"`   // raw vendor tool input
	Summary    string          `json:"summary"`           // one line for humans: "Run: git push origin main"
	Cwd        string          `json:"cwd"`
	Status     RequestStatus   `json:"status"`
	Route      Route           `json:"route,omitempty"`
	PolicyRule string          `json:"policy_rule,omitempty"` // rule that matched, if any
	Questions  []Question      `json:"questions,omitempty"`   // checkpoint question
	Plan       *Plan           `json:"plan,omitempty"`        // checkpoint plan
	Risk       []RiskLabel     `json:"risk,omitempty"`        // most severe first
	Preview    *Preview        `json:"preview,omitempty"`     // file changes the tool call would make
	Context    string          `json:"context,omitempty"`     // the agent's last words before asking, trimmed
	Decision   *Decision       `json:"decision,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	ExpiresAt  time.Time       `json:"expires_at"`
	ClosedAt   *time.Time      `json:"closed_at,omitempty"`
}

// Behavior is the outcome of a decision.
type Behavior string

const (
	BehaviorAllow Behavior = "allow"
	BehaviorDeny  Behavior = "deny"
	BehaviorAsk   Behavior = "ask" // policy only: force the agent's own permission prompt
)

// Scope is how far an allow reaches.
type Scope string

const (
	ScopeOnce    Scope = "once"
	ScopeSession Scope = "session" // stored as a grant, applied at the pre_tool checkpoint
)

// Decision answers a Request. The first decision wins (compare-and-set).
type Decision struct {
	RequestID string   `json:"request_id"`
	Behavior  Behavior `json:"behavior"`
	Scope     Scope    `json:"scope,omitempty"`
	Message   string   `json:"message,omitempty"` // shown to the agent on deny; plan feedback
	// Answers answers a question request: Question.Key() → answer. A
	// multi-select answer is the chosen labels joined with ", ". Free text
	// is allowed when the question allows "other".
	Answers map[string]string `json:"answers,omitempty"`
	By      string            `json:"by"` // "policy:<rule>", "grant:<id>", "cli", "tui", "device:<id>", "telegram:<chat>"
	At      time.Time         `json:"at"`
}

// EventType names an entry in the append-only event log.
type EventType string

const (
	EventSessionStarted  EventType = "session.started"
	EventSessionUpdated  EventType = "session.updated"
	EventSessionExited   EventType = "session.exited"
	EventRequestOpened   EventType = "request.opened"
	EventRequestClosed   EventType = "request.closed" // decided, stepped aside, expired, answered locally
	EventPolicyDecision  EventType = "policy.decision"
	EventPresenceChanged EventType = "presence.changed"
	EventDeviceChanged   EventType = "device.changed"
	EventTaskUpdated     EventType = "task.updated"
	EventSessionReply    EventType = "session.reply"  // a person sent a follow-up prompt or interrupt (Data: ReplyRecord)
	EventPolicyChanged   EventType = "policy.changed" // rules were added, removed, enabled or disabled (Data: RuleChange)
	EventHook            EventType = "hook"           // raw normalized hook, kept for `pilothouse explain`
)

// Event is one entry in the append-only log. Seq is strictly increasing.
// Data holds the full object the event is about (Session, Request, Presence…).
type Event struct {
	Seq       int64           `json:"seq"`
	At        time.Time       `json:"at"`
	Type      EventType       `json:"type"`
	SessionID string          `json:"session_id,omitempty"`
	RequestID string          `json:"request_id,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
}

// PresenceState says whether a person is at this machine.
type PresenceState string

const (
	PresencePresent PresenceState = "present"
	PresenceAway    PresenceState = "away"
)

// PresenceMode is the user's override.
type PresenceMode string

const (
	PresenceAuto      PresenceMode = "auto"
	PresenceForceHere PresenceMode = "here"
	PresenceForceAway PresenceMode = "away"
)

// Presence is the current routing input.
type Presence struct {
	State     PresenceState `json:"state"`
	Mode      PresenceMode  `json:"mode"`
	Source    string        `json:"source"`              // "os_idle", "manual", "unknown"
	IdleSecs  int           `json:"idle_secs,omitempty"` // OS idle time when known
	ChangedAt time.Time     `json:"changed_at"`
}

// Snapshot is what a client loads before streaming events with after=Seq.
type Snapshot struct {
	Seq      int64     `json:"seq"`
	Machine  string    `json:"machine"`
	Version  string    `json:"version"`
	Sessions []Session `json:"sessions"`
	Requests []Request `json:"requests"` // open requests plus the last 50 closed
	Presence Presence  `json:"presence"`
	Tasks    []Task    `json:"tasks,omitempty"`
}

// StreamControl is sent on the event stream instead of an Event when the
// client must reload a Snapshot (its `after` is older than retained history).
type StreamControl struct {
	ResyncRequired bool `json:"resync_required"`
}

// Device is a paired phone or browser.
type Device struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	PublicKey  string     `json:"public_key"` // base64url SPKI, ECDSA P-256
	PushOK     bool       `json:"push_ok"`    // has a Web Push subscription
	CreatedAt  time.Time  `json:"created_at"`
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// TaskStatus is the lifecycle of an orchestrated task.
type TaskStatus string

const (
	TaskRunning TaskStatus = "running"
	TaskReview  TaskStatus = "review" // agent finished; diff waiting for a person
	TaskDone    TaskStatus = "done"
	TaskAbandon TaskStatus = "abandoned"
	// TaskFailed: the agent never started (setup failed, or the agent
	// could not be launched). The worktree is kept for a look; discard it.
	TaskFailed TaskStatus = "failed"
)

// Task is a prompt run by an agent in its own git worktree.
type Task struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Prompt    string     `json:"prompt"`
	Agent     AgentKind  `json:"agent"`
	Repo      string     `json:"repo"`     // main repo root
	Worktree  string     `json:"worktree"` // path of the task's worktree
	Branch    string     `json:"branch"`
	BaseRef   string     `json:"base_ref"`
	SessionID string     `json:"session_id,omitempty"`
	Status    TaskStatus `json:"status"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	// By is who started the task, like Decision.By ("device:<id>", "cli").
	By string `json:"by,omitempty"`
	// PRURL is the pull request opened when the task shipped.
	PRURL string `json:"pr_url,omitempty"`
	// Setup is what .pilothouse/task.yaml did; nil when the repo has none.
	Setup *TaskSetup `json:"setup,omitempty"`
	// Note is one person-readable sentence about the last thing that
	// happened: why the task failed, that setup was skipped, where a
	// shipped branch went.
	Note string `json:"note,omitempty"`
}

// Grant is an "allow for this session" answer, applied at pre_tool.
type Grant struct {
	ID         string    `json:"id"`
	SessionID  string    `json:"session_id"`
	Tool       string    `json:"tool"`
	Prefix     string    `json:"prefix,omitempty"` // shell: first word(s) of command; empty = whole tool
	DecisionBy string    `json:"decision_by"`
	CreatedAt  time.Time `json:"created_at"`
}

// Error is the JSON body of every non-2xx API response.
type Error struct {
	Error string `json:"error"`
	Code  string `json:"code"` // stable machine code: "not_found", "already_decided", "unauthorized", ...
}
