package cli

// Workstream F: the everyday commands — setup, doctor, inbox (also the
// default when pilothouse runs on a terminal with no arguments), ls,
// approve, deny, log, explain, here/away/auto, service, notify, version.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/adapters/all"
	"github.com/xasdta/pilothouse/internal/adapters/codex"
	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/client"
	"github.com/xasdta/pilothouse/internal/config"
	"github.com/xasdta/pilothouse/internal/notify"
	"github.com/xasdta/pilothouse/internal/paths"
	"github.com/xasdta/pilothouse/internal/presence"
	"github.com/xasdta/pilothouse/internal/service"
	"github.com/xasdta/pilothouse/internal/tui"
	"github.com/xasdta/pilothouse/internal/version"
	"github.com/xasdta/pilothouse/internal/wrap"
)

// serviceManager is what the CLI needs from internal/service; tests swap
// in a fake so they never touch launchd or systemd.
type serviceManager interface {
	Kind() string
	Path() string
	Install(ctx context.Context) (service.Result, error)
	Uninstall(ctx context.Context) (service.Result, error)
	Status(ctx context.Context) (service.Status, error)
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Restart(ctx context.Context) error
}

// Seams for tests. Production values touch the real system; tests replace
// every one of them (BUILD-SPEC invariant 1).
var (
	inboxAdapters   = func() []adapters.Adapter { return all.List() }
	inboxServiceMgr = func(exe string) serviceManager { return service.New(exe) }
	// inboxStartDaemon is the fallback when no service manager is used:
	// `pilothouse start` (workstream A) starts the daemon in the background.
	inboxStartDaemon = func(ctx context.Context, exe string) error {
		c := exec.CommandContext(ctx, exe, "start")
		c.Stdout, c.Stderr = os.Stderr, os.Stderr
		return c.Run()
	}
	inboxExecutable    = os.Executable
	inboxRunTUI        = tui.Run
	inboxDetector      = presence.NewDetector
	inboxHTTPClient    = &http.Client{Timeout: 5 * time.Second}
	inboxLookPath      = exec.LookPath
	inboxIsTerminal    = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) }
	inboxStdinTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
	inboxLocation      = time.Local
	inboxNow           = time.Now
	inboxHolders       = wrap.ListHolders
	inboxHealthWait    = 10 * time.Second
	inboxShellNoise    = shellNoise
)

// inboxClient is the daemon client for CLI commands; decisions made
// through it are recorded as By="cli".
func inboxClient() *client.Client { return client.New().WithClientName("cli") }

// inboxErr turns daemon errors into sentences that say what to do next.
func inboxErr(err error, ref string) error {
	var ae *client.APIError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, client.ErrDaemonDown):
		return errors.New("the pilothouse daemon isn't running — start it with `pilothouse start` (or `pilothouse setup` the first time)")
	case errors.As(err, &ae) && ae.Code == "agent_caller":
		return errors.New("refused: this shell runs inside an agent session, and agents can't approve themselves or change Pilothouse — run the command from your own terminal")
	case errors.As(err, &ae) && ae.Code == "already_decided":
		return fmt.Errorf("already answered (the first answer wins): %s — see `pilothouse log`", ae.Msg)
	case errors.As(err, &ae) && ae.Code == "not_found":
		if ref != "" {
			return fmt.Errorf("nothing matches %q — run `pilothouse ls` to see ids", ref)
		}
		return errors.New(ae.Msg)
	}
	return err
}

// resolvedExe is the absolute path of this binary that hooks and the
// login service should call. It must survive upgrades: Homebrew runs
// /opt/homebrew/bin/pilothouse, a symlink into the versioned
// …/Cellar/pilothouse/<version>/bin/pilothouse that `brew upgrade` deletes.
// So: the `pilothouse` found on PATH when it is this same file; else, for
// a Homebrew Cellar binary, <brew prefix>/bin/pilothouse when it exists;
// else the symlink-free path of this binary.
func resolvedExe() (string, error) {
	exe, err := inboxExecutable()
	if err != nil {
		return "", fmt.Errorf("can't find the pilothouse binary: %w", err)
	}
	real := exe
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		real = r
	}
	if real, err = filepath.Abs(real); err != nil {
		return "", err
	}
	if p, err := inboxLookPath("pilothouse"); err == nil {
		if p, err = filepath.Abs(p); err == nil && sameFile(p, real) {
			return p, nil
		}
	}
	if prefix, ok := brewPrefix(real); ok {
		stable := filepath.Join(prefix, "bin", "pilothouse")
		if sameFile(stable, real) || fileExists(stable) {
			return stable, nil
		}
	}
	return real, nil
}

// brewPrefix returns the Homebrew prefix of a versioned Cellar path
// (<prefix>/Cellar/pilothouse/<version>/…).
func brewPrefix(path string) (string, bool) {
	const marker = "/Cellar/pilothouse/"
	i := strings.Index(path, marker)
	if i <= 0 || !strings.Contains(path[i+len(marker):], "/") {
		return "", false
	}
	return path[:i], true
}

// versionedBrewPath reports whether path points into a versioned Homebrew
// Cellar directory, which the next `brew upgrade` removes.
func versionedBrewPath(path string) bool {
	_, ok := brewPrefix(path)
	return ok
}

func sameFile(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	return err == nil && os.SameFile(fa, fb)
}

// samePath reports whether a and b name the same binary once symlinks are
// resolved on both sides: setup records the stable, unresolved path (e.g.
// Homebrew's /opt/homebrew/bin/pilothouse, a symlink into the Cellar).
func samePath(a, b string) bool {
	resolve := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		return p
	}
	return a == b || resolve(a) == resolve(b) || sameFile(a, b)
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// unstableExe reports why exe is a bad path to wire into hooks, if it is.
func unstableExe(exe string) string {
	tmp := filepath.Clean(os.TempDir())
	if r, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = r
	}
	switch {
	case versionedBrewPath(exe):
		return "this binary lives in a versioned Homebrew directory (" + exe + ") that `brew upgrade` removes; run setup with " + filepath.Join(strings.SplitN(exe, "/Cellar/", 2)[0], "bin", "pilothouse")
	case strings.Contains(exe, string(filepath.Separator)+"go-build"):
		return "this binary was built by `go run` and will disappear; install it (`go install`, Homebrew or a release) and run setup again"
	case strings.HasPrefix(exe, tmp+string(filepath.Separator)) || strings.HasPrefix(exe, "/tmp/") || strings.HasPrefix(exe, "/private/tmp/"):
		return "this binary lives in a temporary directory; move it somewhere permanent (e.g. /usr/local/bin) and run setup again"
	}
	return ""
}

func liveAdapters() []adapters.Adapter {
	var out []adapters.Adapter
	for _, a := range inboxAdapters() {
		if a != nil { // adapters land in parallel; skip placeholders
			out = append(out, a)
		}
	}
	return out
}

// ---------------------------------------------------------------- default / inbox

func runDefault(cmd *cobra.Command, _ []string) error {
	if !inboxIsTerminal() {
		return cmd.Help()
	}
	return runInbox(cmd)
}

func newInboxCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inbox",
		Short: "Open the inbox: every agent session and everything that needs you",
		Long: `The inbox lists every agent session on this machine and every request
waiting for a person. Answer with y (allow), a (allow for session) or
n (deny), attach to wrapped sessions with enter, press ? for all keys.
Running pilothouse with no arguments in a terminal opens it too.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !inboxIsTerminal() {
				return errors.New("the inbox needs an interactive terminal — use `pilothouse ls` in scripts")
			}
			return runInbox(cmd)
		},
	}
}

func runInbox(cmd *cobra.Command) error {
	exe, _ := resolvedExe()
	return inboxRunTUI(cmd.Context(), tui.Options{
		Backend: client.New().WithClientName("tui"),
		Exe:     exe,
	})
}

// ---------------------------------------------------------------- ls

type lsOutput struct {
	Sessions []api.Session `json:"sessions"`
	Requests []api.Request `json:"requests"`
	// Attachable lists sessions whose `pilothouse run` holder answered
	// just now, i.e. `pilothouse attach <id>` will work.
	Attachable []string `json:"attachable"`
}

// liveHolders returns the session ids whose holder is up and whose agent
// is still running. Only asked when a wrapped session is listed: probing
// costs a socket round trip per holder.
func liveHolders(ctx context.Context, sessions []api.Session) map[string]bool {
	live := map[string]bool{}
	need := false
	for _, s := range sessions {
		if s.Mode == api.ModeWrapped && s.State != api.StateExited {
			need = true
		}
	}
	if !need {
		return live
	}
	hs, err := inboxHolders(ctx)
	if err != nil {
		return live
	}
	for _, h := range hs {
		if !h.Exited {
			live[h.SessionID] = true
		}
	}
	return live
}

func newLsCmd() *cobra.Command {
	var asJSON, showAll bool
	cmd := &cobra.Command{
		Use:   "ls [--json] [--all]",
		Short: "List agent sessions and requests waiting for you",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			snap, err := inboxClient().Snapshot(cmd.Context())
			if err != nil {
				return inboxErr(err, "")
			}
			out := lsOutput{Sessions: []api.Session{}, Requests: []api.Request{}, Attachable: []string{}}
			for _, s := range snap.Sessions {
				if showAll || s.State != api.StateExited {
					out.Sessions = append(out.Sessions, s)
				}
			}
			for _, r := range snap.Requests {
				if r.Status == api.RequestOpen || r.Status == api.RequestSteppedAside {
					out.Requests = append(out.Requests, r)
				}
			}
			sort.SliceStable(out.Requests, func(i, j int) bool { return out.Requests[i].CreatedAt.Before(out.Requests[j].CreatedAt) })
			sort.SliceStable(out.Sessions, func(i, j int) bool { return out.Sessions[i].StartedAt.Before(out.Sessions[j].StartedAt) })
			live := liveHolders(cmd.Context(), out.Sessions)
			for _, s := range out.Sessions {
				if live[s.ID] {
					out.Attachable = append(out.Attachable, s.ID)
				}
			}
			if asJSON {
				return inboxJSON(cmd.OutOrStdout(), out)
			}
			printLs(cmd.OutOrStdout(), out, snap.Sessions, live)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.Flags().BoolVarP(&showAll, "all", "a", false, "include sessions that exited in the last 24 hours")
	return cmd
}

func inboxJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func printLs(w io.Writer, out lsOutput, allSessions []api.Session, live map[string]bool) {
	now := inboxNow()
	byID := map[string]api.Session{}
	for _, s := range allSessions {
		byID[s.ID] = s
	}
	if len(out.Requests) > 0 {
		fmt.Fprintf(w, "NEEDS YOU (%d)\n", len(out.Requests))
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  REQUEST\tAGENT\tWHERE\tWHAT\tAGE\tANSWER")
		for _, r := range out.Requests {
			s := byID[r.SessionID]
			answer := "pilothouse approve|deny " + inboxShortRef(r.ID)
			if h, ok := askHint(r); ok {
				answer = h
			}
			if r.Status == api.RequestSteppedAside {
				answer = "in its terminal"
				if live[s.ID] {
					answer = "pilothouse attach " + inboxShortRef(s.ID)
				}
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.Agent, inboxWhere(s, r.Cwd),
				inboxClip(inboxRequestWhat(r), 60), tui.Age(now.Sub(r.CreatedAt)), answer)
		}
		tw.Flush()
		fmt.Fprintln(w)
	}
	if len(out.Sessions) == 0 {
		fmt.Fprintln(w, "No agent sessions. Start Claude Code or Codex as usual, or `pilothouse run claude`.")
		return
	}
	fmt.Fprintln(w, "SESSIONS")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  SESSION\tAGENT\tSTATE\tWHERE\tFOR\tDETAIL")
	for _, s := range out.Sessions {
		state := string(s.State)
		if s.BlockKind != "" {
			state += "/" + string(s.BlockKind)
		}
		if s.Unseen {
			state += " •"
		}
		since := s.UpdatedAt
		if s.ExitedAt != nil {
			since = *s.ExitedAt
		}
		detail := s.StateDetail
		if detail == "" {
			detail = s.LastMessage
		}
		detail = inboxClip(oneLineText(detail), 50)
		mark := " "
		if live[s.ID] {
			mark = "⏎"
		}
		fmt.Fprintf(tw, "%s %s\t%s\t%s\t%s\t%s\t%s\n", mark, s.ID, s.Agent, state, inboxWhere(s, ""), tui.Age(now.Sub(since)), detail)
	}
	tw.Flush()
	if len(out.Attachable) > 0 {
		fmt.Fprintf(w, "\n⏎ attachable: pilothouse attach %s  (detach with ctrl+\\)\n", inboxShortRef(out.Attachable[0]))
	}
}

func inboxShortRef(id string) string {
	if len(id) > 6 {
		return id[:6]
	}
	return id
}

func inboxWhere(s api.Session, cwd string) string {
	title := s.Title
	if title == "" {
		if cwd == "" {
			cwd = s.Cwd
		}
		title = filepath.Base(cwd)
	}
	if title == "" || title == "." {
		title = "?"
	}
	if s.Branch != "" {
		return title + " · " + s.Branch
	}
	return title
}

func inboxRequestWhat(r api.Request) string {
	if s, ok := askWhat(r); ok {
		return s
	}
	switch {
	case r.Command != "":
		return "$ " + oneLineText(r.Command)
	case len(r.Paths) > 0:
		s := r.Tool + " " + tui.ShortPath(r.Paths[0])
		if len(r.Paths) > 1 {
			s += fmt.Sprintf(" +%d", len(r.Paths)-1)
		}
		return s
	case r.Summary != "":
		return oneLineText(r.Summary)
	}
	return r.Tool
}

func oneLineText(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func inboxClip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// ---------------------------------------------------------------- approve / deny

func newApproveCmd() *cobra.Command {
	var forSession bool
	cmd := &cobra.Command{
		Use:   "approve <request-or-session-id> [--session]",
		Short: "Allow a request that is waiting for you",
		Long: `Allow a held request. The id may be a unique prefix of the request id, or a
session id when that session has exactly one waiting request. With
--session, later calls of the same kind (e.g. the same command prefix)
are allowed for the rest of that agent session.`,
		Example: "  pilothouse approve r_7hq\n  pilothouse approve s_k3m --session",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			scope := api.ScopeOnce
			if forSession {
				scope = api.ScopeSession
			}
			return runInboxDecide(cmd, args[0], api.DecideBody{Behavior: api.BehaviorAllow, Scope: scope})
		},
	}
	cmd.Flags().BoolVarP(&forSession, "session", "s", false, "also allow calls like this one for the rest of the session")
	return cmd
}

func newDenyCmd() *cobra.Command {
	var msg string
	cmd := &cobra.Command{
		Use:     "deny <request-or-session-id> [-m reason]",
		Short:   "Deny a request that is waiting for you",
		Example: `  pilothouse deny r_7hq -m "open a PR instead of pushing to main"`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInboxDecide(cmd, args[0], api.DecideBody{Behavior: api.BehaviorDeny, Message: msg})
		},
	}
	cmd.Flags().StringVarP(&msg, "message", "m", "", "reason shown to the agent")
	return cmd
}

// resolveRequestRef maps a request id prefix, or a session id with one
// waiting request, to a request.
func resolveRequestRef(ctx context.Context, c *client.Client, ref string) (api.Request, error) {
	if strings.HasPrefix(ref, "s_") {
		open, err := c.Requests(ctx, "")
		if err != nil {
			return api.Request{}, inboxErr(err, ref)
		}
		var match []api.Request
		for _, r := range open {
			if strings.HasPrefix(r.SessionID, ref) && (r.Status == api.RequestOpen || r.Status == api.RequestSteppedAside) {
				match = append(match, r)
			}
		}
		switch len(match) {
		case 0:
			return api.Request{}, fmt.Errorf("session %q has no request waiting for you — run `pilothouse ls`", ref)
		case 1:
			return match[0], nil
		}
		ids := make([]string, len(match))
		for i, r := range match {
			ids[i] = r.ID
		}
		return api.Request{}, fmt.Errorf("session %q has %d waiting requests (%s) — pass a request id", ref, len(match), strings.Join(ids, ", "))
	}
	r, err := c.Request(ctx, ref)
	return r, inboxErr(err, ref)
}

func runInboxDecide(cmd *cobra.Command, ref string, body api.DecideBody) error {
	c := inboxClient()
	ctx := cmd.Context()
	req, err := resolveRequestRef(ctx, c, ref)
	if err != nil {
		return err
	}
	// A stepped-aside request is in the agent's own dialog (the person was
	// at the desk); an answer here would not reach it (answer.go).
	if err := requireOpen(ctx, c, req); err != nil {
		return err
	}
	if err := askDecideCheck(req, body); err != nil {
		return err
	}
	out, err := c.Decide(ctx, req.ID, body)
	if err != nil {
		return inboxErr(err, ref)
	}
	if out.ID == "" {
		out = req
	}
	w := cmd.OutOrStdout()
	what := inboxRequestWhat(req)
	switch {
	case askDecidedLine(w, req, body):
	case body.Behavior == api.BehaviorDeny:
		fmt.Fprintf(w, "✗ Denied %s: %s\n", req.ID, what)
		if body.Message != "" {
			fmt.Fprintf(w, "  The agent was told: %s\n", body.Message)
		}
	case body.Scope == api.ScopeSession:
		fmt.Fprintf(w, "✓ Allowed %s for the rest of this session: %s\n", req.ID, what)
	default:
		fmt.Fprintf(w, "✓ Allowed %s once: %s\n", req.ID, what)
	}
	return nil
}

// ---------------------------------------------------------------- log

func newLogCmd() *cobra.Command {
	var (
		follow  bool
		asJSON  bool
		session string
		limit   int
	)
	cmd := &cobra.Command{
		Use:   "log [-f] [--json] [--session id] [-n N]",
		Short: "Show the event log: sessions, requests, decisions, presence",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := inboxClient()
			ctx := cmd.Context()
			w := cmd.OutOrStdout()
			var (
				events []api.Event
				last   int64
				sessID string
			)
			if session != "" {
				s, err := c.Session(ctx, session)
				if err != nil {
					return inboxErr(err, session)
				}
				sessID = s.ID
				evs, err := c.SessionEvents(ctx, sessID, limit)
				if err != nil {
					return inboxErr(err, session)
				}
				for i := len(evs) - 1; i >= 0; i-- { // API is newest first
					events = append(events, evs[i])
				}
			} else {
				snap, err := c.Snapshot(ctx)
				if err != nil {
					return inboxErr(err, "")
				}
				last = snap.Seq
				evs, err := c.Events(ctx, max(0, snap.Seq-int64(limit)))
				if err != nil {
					return inboxErr(err, "")
				}
				events = evs
			}
			emit := func(ev api.Event) error {
				if ev.Seq > last {
					last = ev.Seq
				}
				if asJSON {
					b, err := json.Marshal(ev)
					if err != nil {
						return err
					}
					_, err = fmt.Fprintln(w, string(b))
					return err
				}
				_, err := fmt.Fprintln(w, FormatEventLine(ev))
				return err
			}
			for _, ev := range events {
				if err := emit(ev); err != nil {
					return err
				}
			}
			if !follow {
				if len(events) == 0 && !asJSON {
					fmt.Fprintln(w, "No events yet.")
				}
				return nil
			}
			for {
				err := c.Stream(ctx, last, func(ev api.Event) {
					if sessID != "" && ev.SessionID != sessID {
						if ev.Seq > last {
							last = ev.Seq
						}
						return
					}
					_ = emit(ev)
				})
				switch {
				case err == nil || ctx.Err() != nil:
					return nil
				case errors.Is(err, client.ErrResync):
					// Fell behind retained history; continue from now.
					snap, err := c.Snapshot(ctx)
					if err != nil {
						return inboxErr(err, "")
					}
					fmt.Fprintln(cmd.ErrOrStderr(), "pilothouse: some events were skipped (the log was trimmed); continuing from now")
					last = snap.Seq
				default:
					return inboxErr(err, "")
				}
			}
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new events")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print one JSON event per line")
	cmd.Flags().StringVar(&session, "session", "", "only events of this session (id or prefix)")
	cmd.Flags().IntVarP(&limit, "lines", "n", 50, "how many past events to show")
	return cmd
}

// FormatEventLine renders one event as a human line:
// "15:04:05  s_k3m9q2  ? $ git push … (held for your devices)".
func FormatEventLine(ev api.Event) string {
	ts := ev.At.In(inboxLocation).Format("15:04:05")
	sid := ev.SessionID
	if len(sid) > 8 {
		sid = sid[:8]
	}
	if sid == "" {
		sid = "-"
	}
	return fmt.Sprintf("%s  %-8s  %s", ts, sid, describeEvent(ev))
}

func describeEvent(ev api.Event) string {
	switch ev.Type {
	case api.EventSessionStarted, api.EventSessionUpdated, api.EventSessionExited:
		var s api.Session
		if json.Unmarshal(ev.Data, &s) != nil {
			break
		}
		switch ev.Type {
		case api.EventSessionStarted:
			return fmt.Sprintf("▶ %s started in %s (%s)", s.Agent, tui.ShortPath(s.Cwd), s.Mode)
		case api.EventSessionExited:
			if s.ExitCode != nil {
				return fmt.Sprintf("■ exited with code %d", *s.ExitCode)
			}
			return "■ exited"
		}
		state := string(s.State)
		if s.BlockKind != "" {
			state += " (" + string(s.BlockKind) + ")"
		}
		if s.StateDetail != "" {
			return "· " + state + ": " + oneLineText(s.StateDetail)
		}
		return "· " + state
	case api.EventRequestOpened, api.EventRequestClosed:
		var r api.Request
		if json.Unmarshal(ev.Data, &r) != nil {
			break
		}
		what := inboxRequestWhat(r)
		if ev.Type == api.EventRequestOpened {
			switch r.Route {
			case api.RouteDevices:
				return "? " + what + " — held for your devices"
			case api.RouteTerminal:
				return "? " + what + " — shown in the agent's terminal"
			}
			return "? " + what
		}
		switch r.Status {
		case api.RequestDecided:
			if r.Decision == nil {
				return "✓ decided: " + what
			}
			d := r.Decision
			if d.Behavior == api.BehaviorDeny {
				s := "✗ denied by " + d.By + ": " + what
				if d.Message != "" {
					s += " — “" + oneLineText(d.Message) + "”"
				}
				return s
			}
			if d.Scope == api.ScopeSession {
				return "✓ allowed for the session by " + d.By + ": " + what
			}
			return "✓ allowed by " + d.By + ": " + what
		case api.RequestAnsweredLocally:
			return "✓ answered in the terminal: " + what
		case api.RequestExpired:
			return "⌛ expired, handed back to the terminal: " + what
		case api.RequestSteppedAside:
			return "→ shown in the agent's own dialog: " + what
		}
		return "closed: " + what
	case api.EventPresenceChanged:
		var p api.Presence
		if json.Unmarshal(ev.Data, &p) != nil {
			break
		}
		s := "◐ presence: " + string(p.State) + " (" + string(p.Mode)
		if p.Source != "" {
			s += ", " + p.Source
		}
		if p.IdleSecs > 0 {
			s += fmt.Sprintf(", idle %s", tui.Age(time.Duration(p.IdleSecs)*time.Second))
		}
		return s + ")"
	case api.EventPolicyDecision:
		var m map[string]any
		if json.Unmarshal(ev.Data, &m) == nil {
			parts := []string{"⚖ policy"}
			// Data is policy.DecisionEvent: action is the rule's verdict,
			// outcome what the agent was told (a grant can allow with no rule).
			for _, k := range []string{"rule", "action", "outcome", "grant", "summary", "message"} {
				if v, ok := m[k].(string); ok && v != "" {
					parts = append(parts, k+"="+oneLineText(v))
				}
			}
			return strings.Join(parts, " ")
		}
	case api.EventHook:
		var h adapters.HookEvent
		if json.Unmarshal(ev.Data, &h) == nil {
			s := "↪ " + h.VendorEvent
			if h.VendorEvent == "" {
				s = "↪ " + string(h.Kind)
			}
			if h.Summary != "" {
				s += ": " + oneLineText(h.Summary)
			} else if h.Tool != "" {
				s += " " + h.Tool
			}
			return s
		}
	}
	if len(ev.Data) > 0 && len(ev.Data) < 200 {
		return string(ev.Type) + " " + string(ev.Data)
	}
	return string(ev.Type)
}

// ---------------------------------------------------------------- explain

func newExplainCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "explain <session>",
		Short: "Show why a session is in its current state, with the evidence",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ex, err := inboxClient().Explain(cmd.Context(), args[0])
			if err != nil {
				return inboxErr(err, args[0])
			}
			if asJSON {
				return inboxJSON(cmd.OutOrStdout(), ex)
			}
			printExplain(cmd.OutOrStdout(), ex)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func printExplain(w io.Writer, ex api.Explain) {
	s := ex.Session
	now := inboxNow()
	fmt.Fprintf(w, "%s  %s · %s (%s)\n\n", s.ID, s.Agent, inboxWhere(s, ""), s.Mode)
	state := string(s.State)
	if s.BlockKind != "" {
		state += " (" + string(s.BlockKind) + ")"
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "state\t%s, for %s\n", state, tui.Age(now.Sub(s.UpdatedAt)))
	fmt.Fprintf(tw, "because\t%s\n", ex.Rule)
	if s.StateDetail != "" {
		fmt.Fprintf(tw, "detail\t%s\n", oneLineText(s.StateDetail))
	}
	if s.StateLayer != "" {
		fmt.Fprintf(tw, "source\t%s\n", s.StateLayer)
	}
	fmt.Fprintf(tw, "cwd\t%s\n", tui.ShortPath(s.Cwd))
	if s.PID != 0 {
		fmt.Fprintf(tw, "pid\t%d\n", s.PID)
	}
	tw.Flush()
	fmt.Fprintln(w)
	if len(ex.Evidence) == 0 {
		fmt.Fprintln(w, "No evidence recorded yet.")
		return
	}
	fmt.Fprintln(w, "EVIDENCE (newest first; * = still counted)")
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  \tWHEN\tLAYER\tSTATE\tDETAIL")
	for _, e := range ex.Evidence {
		mark := " "
		if e.Fresh {
			mark = "*"
		}
		st := string(e.State)
		if e.Kind != "" {
			st += "/" + string(e.Kind)
		}
		fmt.Fprintf(tw, "  %s\t%s ago\t%s\t%s\t%s\n", mark, tui.Age(now.Sub(e.At)), e.Layer, st, inboxClip(oneLineText(e.Detail), 70))
	}
	tw.Flush()
}

// ---------------------------------------------------------------- presence

func newPresenceCmds() []*cobra.Command {
	mk := func(mode api.PresenceMode, short string) *cobra.Command {
		return &cobra.Command{
			Use:   string(mode),
			Short: short,
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				p, err := inboxClient().SetPresence(cmd.Context(), mode)
				if err != nil {
					return inboxErr(err, "")
				}
				fmt.Fprintln(cmd.OutOrStdout(), presenceSentence(p))
				return nil
			},
		}
	}
	return []*cobra.Command{
		mk(api.PresenceForceHere, "I'm at the desk: show requests in the agent's own terminal"),
		mk(api.PresenceForceAway, "I'm away: hold requests and send them to my phone"),
		mk(api.PresenceAuto, "Decide here/away from keyboard and mouse idle time"),
	}
}

func presenceSentence(p api.Presence) string {
	where := "Permission requests show in the agent's own dialog."
	if p.State == api.PresenceAway {
		where = "Permission requests are held and sent to your paired devices."
	}
	switch p.Mode {
	case api.PresenceForceHere:
		return "Presence: here (until you run `pilothouse auto`). " + where
	case api.PresenceForceAway:
		return "Presence: away (until you run `pilothouse auto`). " + where
	}
	s := "Presence: auto — now " + string(p.State)
	if p.State == api.PresencePresent {
		s = "Presence: auto — now here"
	}
	if p.Source == "unknown" || p.Source == "none" {
		s += " (idle time can't be measured on this machine; use `pilothouse away` when you leave)"
	} else if p.IdleSecs > 0 {
		s += fmt.Sprintf(" (idle %s)", tui.Age(time.Duration(p.IdleSecs)*time.Second))
	}
	return s + ". " + where
}

// ---------------------------------------------------------------- service

func newServiceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Run the daemon at login (launchd on macOS, systemd --user on Linux)",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	mgr := func() (serviceManager, error) {
		exe, err := resolvedExe()
		if err != nil {
			return nil, err
		}
		m := inboxServiceMgr(exe)
		if m.Kind() == "" {
			return nil, fmt.Errorf("%w — run the daemon with `pilothouse start` instead", service.ErrUnsupported)
		}
		return m, nil
	}
	install := &cobra.Command{
		Use:   "install",
		Short: "Install and start the login service (idempotent)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			exe, err := resolvedExe()
			if err != nil {
				return err
			}
			if why := unstableExe(exe); why != "" {
				return errors.New(why)
			}
			m, err := mgr()
			if err != nil {
				return err
			}
			res, err := m.Install(cmd.Context())
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			for _, a := range res.Actions {
				fmt.Fprintln(w, "✓ "+a)
			}
			for _, n := range res.Notes {
				fmt.Fprintln(w, "  note: "+n)
			}
			h, err := waitForHealth(cmd.Context(), inboxHealthWait)
			if err != nil {
				return fmt.Errorf("the service is installed but the daemon didn't answer: %w — check %s", err, service.LogFile())
			}
			fmt.Fprintf(w, "✓ daemon running (pid %d, %s)\n", h.PID, h.Version)
			return nil
		},
	}
	uninstall := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the login service",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			m, err := mgr()
			if err != nil {
				return err
			}
			res, err := m.Uninstall(cmd.Context())
			if err != nil {
				return err
			}
			for _, a := range res.Actions {
				fmt.Fprintln(cmd.OutOrStdout(), "✓ "+a)
			}
			return nil
		},
	}
	var asJSON bool
	status := &cobra.Command{
		Use:   "status",
		Short: "Show whether the service is installed and running",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			m, err := mgr()
			if err != nil {
				return err
			}
			st, err := m.Status(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return inboxJSON(cmd.OutOrStdout(), st)
			}
			w := cmd.OutOrStdout()
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintf(tw, "manager\t%s\n", st.Kind)
			fmt.Fprintf(tw, "file\t%s\n", st.Path)
			fmt.Fprintf(tw, "installed\t%s\n", yesNo(st.Installed, st.Installed && !st.Current, "out of date — run `pilothouse service install`"))
			fmt.Fprintf(tw, "running\t%s\n", yesNo(st.Running, false, ""))
			if st.PID != 0 {
				fmt.Fprintf(tw, "pid\t%d\n", st.PID)
			}
			if st.Detail != "" {
				fmt.Fprintf(tw, "detail\t%s\n", st.Detail)
			}
			if st.Linger != nil {
				fmt.Fprintf(tw, "linger\t%s\n", yesNo(*st.Linger, !*st.Linger, "the daemon stops at logout; `loginctl enable-linger $USER` keeps it running"))
			}
			return tw.Flush()
		},
	}
	status.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	simple := func(use, short, done string, op func(serviceManager, context.Context) error) *cobra.Command {
		return &cobra.Command{
			Use:   use,
			Short: short,
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				m, err := mgr()
				if err != nil {
					return err
				}
				if err := op(m, cmd.Context()); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "✓ "+done)
				return nil
			},
		}
	}
	cmd.AddCommand(install, uninstall, status,
		simple("start", "Start the service", "started", serviceManager.Start),
		simple("stop", "Stop the service until the next start or login", "stopped", serviceManager.Stop),
		simple("restart", "Restart the service (agents keep running)", "restarted", serviceManager.Restart),
	)
	return cmd
}

func yesNo(v, warn bool, note string) string {
	s := "no"
	if v {
		s = "yes"
	}
	if warn && note != "" {
		s += " (" + note + ")"
	}
	return s
}

// waitForHealth polls the daemon until it answers or d passes.
func waitForHealth(ctx context.Context, d time.Duration) (api.Health, error) {
	c := client.New()
	deadline := time.Now().Add(d)
	for {
		hctx, cancel := context.WithTimeout(ctx, time.Second)
		h, err := c.Health(hctx)
		cancel()
		if err == nil {
			return h, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return h, inboxErr(err, "")
		}
		select {
		case <-ctx.Done():
			return h, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// ---------------------------------------------------------------- notify, version

func newNotifyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "notify",
		Short: "Notification helpers",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "test",
		Short: "Send a test notification to every configured channel",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := inboxClient().NotifyTest(cmd.Context()); err != nil {
				return inboxErr(err, "")
			}
			ch := notifyChannels(cmd.Context())
			if len(ch) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "! "+noChannelsHint)
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), "✓ Test notification sent to: "+strings.Join(ch, ", "))
			return nil
		},
	})
	return cmd
}

// notifyChannels lists the configured channels for humans.
// desktopLookPath finds the desktop notifier; tests replace it so results
// don't depend on whether the machine has notify-send.
var desktopLookPath = exec.LookPath

// notifyChannels lists the channels that can actually deliver a
// notification right now: a desktop notifier that exists on this machine,
// paired devices that have Web Push turned on, ntfy and webhooks.
func notifyChannels(ctx context.Context) []string {
	cfg, err := config.Load()
	if err != nil {
		cfg = config.Default()
	}
	var ch []string
	if cfg.Notify.Desktop {
		if name, _ := notify.DesktopCommand(runtime.GOOS, "t", "b", desktopLookPath); name != "" {
			ch = append(ch, "desktop")
		}
	}
	if devs, err := inboxClient().Devices(ctx); err == nil {
		n := 0
		for _, d := range devs {
			if d.PushOK && d.RevokedAt == nil {
				n++
			}
		}
		if n > 0 {
			ch = append(ch, fmt.Sprintf("%d paired device(s) via Web Push", n))
		}
	}
	if cfg.Notify.Ntfy.Topic != "" {
		ch = append(ch, "ntfy topic "+cfg.Notify.Ntfy.Topic)
	}
	if n := len(cfg.Notify.Webhooks); n > 0 {
		ch = append(ch, fmt.Sprintf("%d webhook(s)", n))
	}
	return ch
}

// noChannelsHint is printed when nothing can reach the person yet.
const noChannelsHint = "no notification channel can reach you yet — pair a phone with `pilothouse pair`" +
	" (or install notify-send, or set notify.ntfy.topic)"

type versionInfo struct {
	Version  string `json:"version"`
	Commit   string `json:"commit"`
	Date     string `json:"date"`
	Go       string `json:"go"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Protocol int    `json:"protocol"`
}

func newVersionCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "version [--json]",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			v := versionInfo{version.Version, version.Commit, version.Date, runtime.Version(), runtime.GOOS, runtime.GOARCH, api.ProtocolVersion}
			if asJSON {
				return inboxJSON(cmd.OutOrStdout(), v)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "pilothouse %s (commit %s, built %s)\n%s %s/%s, protocol %d\n",
				v.Version, v.Commit, v.Date, v.Go, v.OS, v.Arch, v.Protocol)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// ---------------------------------------------------------------- setup

func newSetupCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "setup [--yes]",
		Short: "Get going: install hooks, start the daemon, check notifications",
		Long: `Setup finds your coding agents, shows what it will change, installs
Pilothouse's hooks (backing up each settings file first), runs the daemon as a
login service, and sends a test notification. Run it again any time: it
only changes what is missing or out of date.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runSetup(cmd, yes) },
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "accept the defaults without asking")
	return cmd
}

type setupUI struct {
	w   io.Writer
	in  *bufio.Reader
	yes bool
}

// ask asks a yes/no question; the default is yes. With --yes it answers
// itself and says so, so logs show what was decided.
func (u *setupUI) ask(q string) bool {
	if u.yes {
		fmt.Fprintf(u.w, "? %s [Y/n] yes\n", q)
		return true
	}
	fmt.Fprintf(u.w, "? %s [Y/n] ", q)
	line, err := u.in.ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(u.w)
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true
	}
	return false
}

func runSetup(cmd *cobra.Command, yes bool) error {
	ctx := cmd.Context()
	w := cmd.OutOrStdout()
	if !yes && !inboxStdinTerminal() {
		return errors.New("setup asks before changing anything; run it in a terminal, or pass --yes to accept the defaults")
	}
	ui := &setupUI{w: w, in: bufio.NewReader(cmd.InOrStdin()), yes: yes}

	exe, err := resolvedExe()
	if err != nil {
		return err
	}
	fmt.Fprintln(w, "Pilothouse setup")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  binary  %s (%s)\n", exe, version.Version)
	if why := unstableExe(exe); why != "" {
		return errors.New(why)
	}
	if err := paths.Ensure(); err != nil {
		return fmt.Errorf("can't create %s: %w", paths.Home(), err)
	}
	fmt.Fprintf(w, "  data    %s\n\n", tui.ShortPath(paths.Home()))

	var summary []string
	note := func(s string) { summary = append(summary, s) }

	// 1. Agents and hooks.
	fmt.Fprintln(w, "Agents")
	opts := adapters.InstallOptions{ShimPath: exe}
	var toInstall []adapters.Adapter
	found := 0
	for _, a := range liveAdapters() {
		d := a.Detect()
		if !d.Installed {
			fmt.Fprintf(w, "  · %-12s not found", a.DisplayName())
			if d.Note != "" {
				fmt.Fprintf(w, " (%s)", d.Note)
			}
			fmt.Fprintln(w)
			continue
		}
		found++
		ver := ""
		if d.Version != "" {
			ver = " " + d.Version
		}
		if st, err := a.Status(opts); err == nil && st.Current {
			fmt.Fprintf(w, "  ✓ %s%s  hooks installed and current (%s)\n", a.DisplayName(), ver, tui.ShortPath(st.Path))
			note("✓ " + a.DisplayName() + " hooks are current")
			continue
		}
		dry := opts
		dry.DryRun = true
		ch, err := a.InstallHooks(dry)
		if err != nil {
			fmt.Fprintf(w, "  ✗ %s%s  can't plan the hook change: %v\n", a.DisplayName(), ver, err)
			note("✗ " + a.DisplayName() + ": " + err.Error())
			continue
		}
		fmt.Fprintf(w, "  • %s%s  %s\n", a.DisplayName(), ver, inboxOr(ch.Summary, "will add Pilothouse hooks"))
		fmt.Fprintf(w, "      file: %s (a backup is written first)\n", tui.ShortPath(ch.Path))
		toInstall = append(toInstall, a)
	}
	if found == 0 {
		fmt.Fprintln(w, "  No supported agent found yet. Install Claude Code or Codex, then run `pilothouse setup` again.")
	}
	fmt.Fprintln(w)
	for _, a := range toInstall {
		if !ui.ask("Install Pilothouse hooks for " + a.DisplayName() + "?") {
			note("· " + a.DisplayName() + " hooks skipped (install later: `pilothouse hooks install`)")
			continue
		}
		ch, err := a.InstallHooks(opts)
		if err != nil {
			fmt.Fprintf(w, "  ✗ %v\n", err)
			note("✗ " + a.DisplayName() + " hooks failed: " + err.Error())
			continue
		}
		msg := "  ✓ " + inboxOr(ch.Summary, "hooks installed")
		if ch.Backup != "" {
			msg += " (backup: " + tui.ShortPath(ch.Backup) + ")"
		}
		fmt.Fprintln(w, msg)
		note("✓ " + a.DisplayName() + " hooks installed")
	}

	// 2. Daemon.
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Daemon")
	running := false
	if h, err := client.New().Health(ctx); err == nil {
		running = true
		if h.Version != version.Version {
			fmt.Fprintf(w, "  ! a daemon is running version %s; this binary is %s\n", h.Version, version.Version)
		}
	}
	mgr := inboxServiceMgr(exe)
	usedService := false
	if mgr.Kind() != "" {
		if ui.ask(fmt.Sprintf("Run the daemon at login with %s?", mgr.Kind())) {
			res, err := mgr.Install(ctx)
			if err != nil {
				fmt.Fprintf(w, "  ✗ service install failed: %v\n", err)
				note("✗ service: " + err.Error())
				// A reload can fail after stopping the old daemon; recheck
				// so the fallback below starts one.
				_, herr := client.New().Health(ctx)
				running = herr == nil
			} else {
				usedService = true
				for _, a := range res.Actions {
					fmt.Fprintln(w, "  ✓ "+a)
				}
				for _, n := range res.Notes {
					fmt.Fprintln(w, "  note: "+n)
				}
				if running && res.Changed {
					_ = mgr.Restart(ctx) // pick up the new binary/definition
				}
				note("✓ daemon runs at login (" + mgr.Kind() + ")")
			}
		} else {
			note("· login service skipped (`pilothouse service install` later)")
		}
	}
	if !usedService && !running {
		fmt.Fprintln(w, "  starting the daemon in the background: pilothouse start")
		if err := inboxStartDaemon(ctx, exe); err != nil {
			fmt.Fprintf(w, "  ✗ pilothouse start: %v\n", err)
		}
	}
	h, err := waitForHealth(ctx, inboxHealthWait)
	daemonOK := err == nil
	if daemonOK {
		fmt.Fprintf(w, "  ✓ daemon running (pid %d, %s)\n", h.PID, h.Version)
		note("✓ daemon running")
	} else {
		fmt.Fprintf(w, "  ✗ the daemon isn't answering: %v\n", err)
		note("✗ daemon not running — see `pilothouse doctor`")
	}

	// 3. Notifications.
	if daemonOK {
		if err := inboxClient().NotifyTest(ctx); err != nil {
			fmt.Fprintf(w, "  ! test notification failed: %v\n", inboxErr(err, ""))
			note("! test notification failed")
		} else {
			if ch := notifyChannels(ctx); len(ch) > 0 {
				fmt.Fprintln(w, "  ✓ test notification sent ("+strings.Join(ch, ", ")+")")
			} else {
				fmt.Fprintln(w, "  ! "+noChannelsHint)
				note("! no notification channel yet")
			}
		}
	}

	// 4. Summary and next steps.
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Summary")
	for _, s := range summary {
		fmt.Fprintln(w, "  "+s)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Next")
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	if _, err := os.Stat(paths.Policy()); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(tw, "  pilothouse policy init\twrite a starter policy (block force-push, ask before deploys, …)")
	}
	fmt.Fprintln(tw, "  pilothouse pair\tpair your phone to approve when you're away")
	fmt.Fprintln(tw, "  pilothouse\topen the inbox")
	fmt.Fprintln(tw, "  pilothouse doctor\tcheck everything is wired up")
	tw.Flush()
	if !daemonOK {
		return ExitError{Code: 1}
	}
	return nil
}

func inboxOr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// ---------------------------------------------------------------- doctor

// shellNoise runs `$SHELL -c true` the way Codex runs hooks
// (codex.ShellNoise) and returns what it printed on stdout (stderr is
// harmless to hook replies).
func shellNoise(context.Context) (shell, noise string, err error) {
	shell = os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	out, err := codex.ShellNoise()
	return shell, strings.TrimSpace(out), err
}

func inboxTrunc(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

type checkLevel int

const (
	checkOK checkLevel = iota
	checkInfo
	checkWarn
	checkFail
)

type doctorCheck struct {
	Name   string `json:"name"`
	Level  string `json:"level"` // ok | info | warn | fail
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
	level  checkLevel
}

func newDoctorCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check that hooks, daemon, service, presence and notifications work",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			checks := runDoctor(cmd.Context())
			failed := false
			for _, c := range checks {
				if c.level == checkFail {
					failed = true
				}
			}
			if asJSON {
				if err := inboxJSON(cmd.OutOrStdout(), checks); err != nil {
					return err
				}
			} else {
				printDoctor(cmd.OutOrStdout(), checks)
			}
			if failed {
				return ExitError{Code: 1}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func printDoctor(w io.Writer, checks []doctorCheck) {
	width := 0
	for _, c := range checks {
		width = max(width, len([]rune(c.Name)))
	}
	fails, warns := 0, 0
	for _, c := range checks {
		mark := map[checkLevel]string{checkOK: "✓", checkInfo: "·", checkWarn: "!", checkFail: "✗"}[c.level]
		switch c.level {
		case checkFail:
			fails++
		case checkWarn:
			warns++
		}
		pad := strings.Repeat(" ", width-len([]rune(c.Name)))
		fmt.Fprintf(w, "%s %s%s  %s\n", mark, c.Name, pad, c.Detail)
		if c.Fix != "" {
			fmt.Fprintf(w, "  %s  → %s\n", strings.Repeat(" ", width), c.Fix)
		}
	}
	fmt.Fprintln(w)
	switch {
	case fails > 0:
		fmt.Fprintf(w, "%d problem(s), %d warning(s).\n", fails, warns)
	case warns > 0:
		fmt.Fprintf(w, "Working, with %d warning(s).\n", warns)
	default:
		fmt.Fprintln(w, "Everything looks good.")
	}
}

func runDoctor(ctx context.Context) []doctorCheck {
	var out []doctorCheck
	add := func(name string, l checkLevel, detail, fix string) {
		out = append(out, doctorCheck{Name: name, Level: [...]string{"ok", "info", "warn", "fail"}[l], Detail: detail, Fix: fix, level: l})
	}

	// Binary.
	exe, err := resolvedExe()
	switch {
	case err != nil:
		add("binary", checkFail, err.Error(), "")
	case unstableExe(exe) != "":
		add("binary", checkFail, exe, unstableExe(exe))
	default:
		onPath, perr := inboxLookPath("pilothouse")
		switch {
		case perr != nil:
			add("binary", checkWarn, exe+" ("+version.Version+")", "pilothouse isn't on your PATH; hooks still work, but add its directory to PATH to use the CLI everywhere")
		case !samePath(onPath, exe):
			add("binary", checkWarn, exe+" ("+version.Version+")", "`pilothouse` on your PATH is "+onPath+"; hooks and the service use the binary that ran setup — run `pilothouse setup` with the one you keep")
		default:
			add("binary", checkOK, exe+" ("+version.Version+")", "")
		}
	}

	// Config.
	cfg, cfgErr := config.Load()
	if cfgErr != nil {
		add("config", checkFail, cfgErr.Error(), "fix "+tui.ShortPath(paths.Config()))
		cfg = config.Default()
	}

	// Daemon.
	c := inboxClient()
	hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	h, herr := c.Health(hctx)
	cancel()
	daemonUp := herr == nil
	switch {
	case errors.Is(herr, client.ErrDaemonDown):
		add("daemon", checkFail, "not running", "`pilothouse start`, or `pilothouse service install` to run it at login")
	case herr != nil:
		add("daemon", checkFail, herr.Error(), "check "+tui.ShortPath(service.LogFile()))
	case h.Protocol != api.ProtocolVersion:
		add("daemon", checkFail, fmt.Sprintf("speaks protocol %d, this CLI %d", h.Protocol, api.ProtocolVersion), "restart it with the current binary: `pilothouse service restart` or `pilothouse stop && pilothouse start`")
	case h.Version != version.Version:
		add("daemon", checkWarn, fmt.Sprintf("running %s (pid %d), this binary is %s", h.Version, h.PID, version.Version), "`pilothouse service restart` (agents keep running)")
	default:
		add("daemon", checkOK, fmt.Sprintf("running (pid %d, %s)", h.PID, h.Version), "")
	}

	// Permissions: the socket is the only path for local decisions.
	permCheck := func(name, p string, want os.FileMode, mustExist bool) {
		fi, err := os.Stat(p)
		if err != nil {
			if mustExist {
				add(name, checkFail, tui.ShortPath(p)+" missing", "start the daemon")
			}
			return
		}
		if fi.Mode().Perm()&0o077 != 0 {
			add(name, checkFail, fmt.Sprintf("%s is %#o; only you should have access", tui.ShortPath(p), fi.Mode().Perm()), fmt.Sprintf("chmod %o %s", want, p))
			return
		}
		add(name, checkOK, fmt.Sprintf("%s (%#o)", tui.ShortPath(p), fi.Mode().Perm()), "")
	}
	permCheck("data dir", paths.Home(), 0o700, false)
	permCheck("socket", paths.Socket(), 0o600, daemonUp)

	// Hooks per agent.
	opts := adapters.InstallOptions{ShimPath: exe}
	codexInstalled := false
	for _, a := range liveAdapters() {
		name := "hooks: " + a.DisplayName()
		d := a.Detect()
		if !d.Installed {
			add(name, checkInfo, "agent not found on this machine", "")
			continue
		}
		if a.Kind() == api.AgentCodex {
			codexInstalled = true
		}
		st, err := a.Status(opts)
		var stale []string
		if err == nil {
			stale = StaleHookProblems(st.Path)
		}
		switch {
		case err != nil:
			add(name, checkFail, err.Error(), "")
		case len(stale) > 0:
			// A missing binary means the hooks fail now; a versioned
			// Homebrew path works until the next `brew upgrade`.
			level := checkWarn
			if strings.Contains(strings.Join(stale, "\n"), "doesn't exist") {
				level = checkFail
			}
			add(name, level, strings.Join(stale, "; "), "")
		case st.Current:
			add(name, checkOK, fmt.Sprintf("%d events wired in %s", len(st.Events), tui.ShortPath(st.Path)), "")
		case st.Installed:
			detail := "installed but out of date"
			if len(st.Problems) > 0 {
				detail += ": " + strings.Join(st.Problems, "; ")
			}
			add(name, checkWarn, detail, "`pilothouse hooks install`")
		default:
			add(name, checkFail, "not installed — this agent's sessions are invisible to Pilothouse", "`pilothouse setup` or `pilothouse hooks install`")
		}
	}

	// Shell noise: Codex runs every hook as `$SHELL -c "<command>"`, so
	// anything the shell's startup files print (zsh reads ~/.zshenv even
	// for -c) lands in front of the hook's JSON reply and Codex can't read
	// it: remote allows fall back to Codex's own prompt.
	if codexInstalled {
		shell, noise, err := inboxShellNoise(ctx)
		switch {
		case err != nil:
			add("shell", checkInfo, "couldn't check for shell startup output: "+err.Error(), "")
		case noise != "":
			add("shell", checkWarn, fmt.Sprintf("`%s -c true` prints %q — Codex can't read hook replies after it (remote approvals fall back to Codex's own prompt)", shell, inboxTrunc(noise, 60)),
				"move whatever prints from your shell's always-read startup file (for zsh: ~/.zshenv) into an interactive-only one (~/.zshrc), or guard it with `[[ -o interactive ]]`")
		default:
			add("shell", checkOK, "`"+shell+" -c true` prints nothing (safe for Codex hooks)", "")
		}
	}

	// Service.
	if exe != "" {
		m := inboxServiceMgr(exe)
		if m.Kind() == "" {
			add("service", checkInfo, "no launchd/systemd user manager here; start the daemon with `pilothouse start`", "")
		} else if st, err := m.Status(ctx); err != nil {
			add("service", checkWarn, err.Error(), "")
		} else {
			switch {
			case !st.Installed:
				add("service", checkWarn, "not installed — the daemon won't start at login", "`pilothouse service install`")
			case !st.Current:
				add("service", checkWarn, "installed but points at another binary or old settings", "`pilothouse service install`")
			case !st.Running:
				add("service", checkWarn, "installed but not running ("+inboxOr(st.Detail, "stopped")+")", "`pilothouse service start`")
			case st.Linger != nil && !*st.Linger:
				add("service", checkOK, fmt.Sprintf("%s, running (pid %d); stops at logout unless linger is on (`loginctl enable-linger $USER`)", st.Kind, st.PID), "")
			default:
				add("service", checkOK, fmt.Sprintf("%s, running (pid %d)", st.Kind, st.PID), "")
			}
		}
	}

	// Presence.
	det := inboxDetector()
	if idle, ok := det.Idle(); ok {
		add("presence", checkOK, fmt.Sprintf("%s works (idle %s; away after %s)", det.Source(), tui.Age(idle), cfg.Presence.IdleThreshold.Duration), "")
	} else if det.Source() == presence.SourceNone {
		add("presence", checkWarn, "idle time can't be measured here (no desktop session?)", "set it by hand: `pilothouse away` when you leave, `pilothouse here` when you're back")
	} else {
		add("presence", checkWarn, det.Source()+" didn't answer", "set it by hand with `pilothouse here|away`")
	}

	// Notifications.
	var ch []string
	if cfg.Notify.Desktop {
		helper := map[string]string{"darwin": "osascript", "linux": "notify-send"}[runtime.GOOS]
		if helper != "" {
			if _, err := inboxLookPath(helper); err != nil {
				add("notifications", checkWarn, "desktop notifications need "+helper+", which isn't installed", "")
			}
		}
		ch = append(ch, "desktop")
	}
	if cfg.Notify.Ntfy.Topic != "" {
		ch = append(ch, "ntfy ("+cfg.Notify.Ntfy.Topic+")")
	}
	if n := len(cfg.Notify.Webhooks); n > 0 {
		ch = append(ch, fmt.Sprintf("%d webhook(s)", n))
	}
	devices := -1
	if daemonUp {
		dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if ds, err := c.Devices(dctx); err == nil {
			devices = 0
			for _, d := range ds {
				if d.RevokedAt == nil {
					devices++
				}
			}
		}
		cancel()
	}
	if devices > 0 {
		ch = append(ch, fmt.Sprintf("%d paired device(s)", devices))
	}
	remote := cfg.Notify.Ntfy.Topic != "" || len(cfg.Notify.Webhooks) > 0 || devices > 0
	switch {
	case len(ch) == 0:
		add("notifications", checkWarn, "no channels configured", "`pilothouse pair` to get pushes on your phone")
	case !remote:
		add("notifications", checkWarn, strings.Join(ch, ", ")+" — nothing reaches you when you're away", "`pilothouse pair` to approve from your phone")
	default:
		add("notifications", checkOK, strings.Join(ch, ", "), "")
	}

	// Remote URL.
	if u := cfg.Web.PublicURL; u == "" {
		add("remote", checkInfo, "phone access only on this network/machine", "`pilothouse remote tailscale` to reach it from anywhere")
	} else {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		req, _ := http.NewRequestWithContext(rctx, http.MethodGet, strings.TrimRight(u, "/")+"/", nil)
		resp, err := inboxHTTPClient.Do(req)
		cancel()
		if err != nil {
			add("remote", checkFail, u+" isn't reachable: "+err.Error(), "`pilothouse remote status`")
		} else {
			resp.Body.Close()
			if resp.StatusCode >= 500 {
				add("remote", checkFail, fmt.Sprintf("%s answers %s", u, resp.Status), "`pilothouse remote status`")
			} else {
				add("remote", checkOK, u+" reachable", "")
			}
		}
	}

	// Policy.
	if daemonUp {
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		ps, err := c.PolicyStatus(pctx)
		cancel()
		switch {
		case err != nil:
			add("policy", checkWarn, "status unavailable: "+inboxErr(err, "").Error(), "")
		case len(ps.Errors) > 0:
			add("policy", checkFail, strings.Join(ps.Errors, "; "), "fix the file, then `pilothouse policy check`")
		default:
			files := 0
			for _, f := range ps.Files {
				if f.Scope != "builtin" {
					files++
				}
			}
			detail := fmt.Sprintf("%d rules from %d file(s)", ps.Rules, files)
			if files == 0 {
				add("policy", checkInfo, detail+" (built-in rules only)", "`pilothouse policy init` for a starter policy")
			} else {
				add("policy", checkOK, detail, "")
			}
		}
	}
	return out
}
