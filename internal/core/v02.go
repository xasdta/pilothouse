package core

import (
	"context"
	"errors"
	"fmt"

	"github.com/xasdta/pilothouse/internal/api"
)

// Optional capabilities added in 0.2 (docs/PLAN-0.2.md). The daemon
// implements all of them; clients (the web server, the TUI) check with a
// type assertion so test fakes only implement what a test needs. A
// missing capability is answered with ErrUnsupported.

var (
	// ErrUnsupported: the core, the session or the agent can't do this
	// (e.g. replying to an observed Codex session).
	ErrUnsupported = errors.New("not supported")
	// ErrInvalid wraps validation failures whose message is safe to show
	// to the person (a bad rule, an unknown repo). Use Invalid().
	ErrInvalid = errors.New("invalid")
)

// Invalid returns an ErrInvalid with a person-readable message.
func Invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Replier sends follow-up prompts and interrupts to sessions (F2).
type Replier interface {
	// Reply delivers r to the session. by identifies the sender like
	// Decision.By. ErrUnsupported when the session can't receive replies.
	Reply(ctx context.Context, sessionID string, r api.Reply, by string) (api.ReplyResult, error)
}

// Terminal is a live mirror of a wrapped session's terminal (F3).
type Terminal interface {
	// WatchTerminal streams terminal output, starting with the scrollback
	// when replay is set, until ctx ends or the session exits. It never
	// detaches a terminal attached with `pilothouse attach`.
	WatchTerminal(ctx context.Context, sessionID string, replay bool, out func([]byte) error) error
	// TerminalInput types data into the session's terminal.
	TerminalInput(ctx context.Context, sessionID string, data []byte, by string) error
	// ResizeTerminal is a no-op unless no terminal is attached.
	ResizeTerminal(ctx context.Context, sessionID string, cols, rows uint16) error
}

// TerminalEvent is one item of a terminal stream: output, the mirrored
// terminal's size, or the agent's exit.
type TerminalEvent struct {
	Data     []byte // output bytes
	Cols     uint16 // with Rows: a size report (the first event, then every change)
	Rows     uint16
	Exited   bool // the agent exited with ExitCode; the stream ends
	ExitCode int
}

// TerminalStreamer is a Terminal that also reports the terminal's size
// and the agent's exit code (the daemon is one). The web stream uses it
// when the core has it and falls back to WatchTerminal otherwise.
type TerminalStreamer interface {
	StreamTerminal(ctx context.Context, sessionID string, replay bool, ev func(TerminalEvent) error) error
}

// ActivityReader reads agent transcripts (F3, F6).
type ActivityReader interface {
	Activity(ctx context.Context, sessionID string, q api.ActivityQuery) (api.Activity, error)
}

// InsightsReader aggregates usage and decisions (F6).
type InsightsReader interface {
	Insights(ctx context.Context, q api.InsightsQuery) (api.Insights, error)
}

// RuleEditor manages rules from the inbox (F5). Changes go to the
// managed rules file; built-in, hand-written and repo rules are read-only.
type RuleEditor interface {
	Rules(ctx context.Context) (api.RulesView, error)
	SuggestRules(ctx context.Context, requestID string) ([]api.RuleSuggestion, error)
	AddRule(ctx context.Context, spec api.RuleSpec, by string) (api.RuleInfo, error)
	SetRuleEnabled(ctx context.Context, name string, enabled bool, by string) error
	RemoveRule(ctx context.Context, name string, by string) error
	SetPreset(ctx context.Context, name string, enabled bool, by string) error
	TestRule(ctx context.Context, in api.RuleTestInput) (api.RuleTestResult, error)
}

// TaskRunner starts and finishes tasks from devices (F7).
type TaskRunner interface {
	Repos(ctx context.Context) ([]api.RepoInfo, error)
	Tasks(ctx context.Context) ([]api.Task, error)
	StartTask(ctx context.Context, t api.TaskStart, by string) (api.Task, error)
	TaskDiff(ctx context.Context, taskID string) (api.TaskDiff, error)
	FinishTask(ctx context.Context, taskID string, f api.TaskFinish, by string) (api.Task, error)
}
