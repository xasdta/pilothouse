package api

import "time"

// Request bodies and small responses of the daemon's local API.
// Routes are listed in docs/BUILD-SPEC.md §4 and in internal/client.

type Health struct {
	Version  string `json:"version"`
	Protocol int    `json:"protocol"`
	PID      int    `json:"pid"`
	Machine  string `json:"machine"`
	WebURL   string `json:"web_url"` // local web app URL
}

type DecideBody struct {
	Behavior Behavior          `json:"behavior"` // allow | deny
	Scope    Scope             `json:"scope,omitempty"`
	Message  string            `json:"message,omitempty"`
	Answers  map[string]string `json:"answers,omitempty"` // question requests: Question.Key() → answer
}

// WrappedSessionBody registers a session started by `pilothouse run`.
type WrappedSessionBody struct {
	SessionID  string    `json:"session_id"` // chosen by the holder (so it can export PILOTHOUSE_SESSION_ID before exec)
	Agent      AgentKind `json:"agent"`
	Title      string    `json:"title"`
	Cwd        string    `json:"cwd"`
	Cmdline    []string  `json:"cmdline"`
	PID        int       `json:"pid"` // agent process
	HolderPID  int       `json:"holder_pid"`
	HolderSock string    `json:"holder_sock"`
	TaskID     string    `json:"task_id,omitempty"`
}

// OSCBody reports an OSC 7501 status sequence seen by a holder (layer L1).
// Format: ESC ] 7501 ; state=<s> [; app=<a>] [; kind=<k>] ST
type OSCBody struct {
	State string `json:"state"` // idle | working | blocked | done | error
	App   string `json:"app,omitempty"`
	Kind  string `json:"kind,omitempty"` // permission | question | auth
	Raw   string `json:"raw,omitempty"`
}

type ExitBody struct {
	ExitCode int `json:"exit_code"`
}

type PresenceBody struct {
	Mode PresenceMode `json:"mode"`
}

// Explain is the evidence behind a session's current state (`pilothouse explain`).
type Explain struct {
	Session  Session         `json:"session"`
	Evidence []StateEvidence `json:"evidence"` // newest first
	Rule     string          `json:"rule"`     // which resolver rule picked the state
}

type StateEvidence struct {
	At     time.Time `json:"at"`
	Layer  Layer     `json:"layer"`
	State  State     `json:"state"`
	Kind   BlockKind `json:"kind,omitempty"`
	Detail string    `json:"detail"`
	Fresh  bool      `json:"fresh"` // still counted by the resolver
}

type PolicyStatus struct {
	Files  []PolicyFile `json:"files"`
	Rules  int          `json:"rules"`
	Errors []string     `json:"errors,omitempty"`
}

type PolicyFile struct {
	Path    string `json:"path"`
	Scope   string `json:"scope"` // user | repo | builtin
	Rules   int    `json:"rules"`
	Trusted bool   `json:"trusted"` // repo files: untrusted repos may only add deny/ask rules
	Error   string `json:"error,omitempty"`
}

// PairStart is returned by POST /v1/pair (Unix socket only).
type PairStart struct {
	Code      string    `json:"code"` // one-time, 10 minutes
	URL       string    `json:"url"`  // <web base>/pair#<code>
	ExpiresAt time.Time `json:"expires_at"`
	Remote    bool      `json:"remote"` // URL is reachable off this machine (Tailscale)
}

// LoginLink is returned by POST /v1/login-link: a one-time link that signs
// this machine's browser into the web app without pairing a phone.
type LoginLink struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}
