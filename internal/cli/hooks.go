package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/adapters/all"
	"github.com/xasdta/pilothouse/internal/adapters/hookkit"
	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/client"
	"github.com/xasdta/pilothouse/internal/config"
	"github.com/xasdta/pilothouse/internal/proc"
)

// ---------------------------------------------------------------- pilothouse hooks

func newHooksCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hooks",
		Short: "Install, remove or check Pilothouse's hooks in Claude Code, Codex and OpenCode",
		Long: `Pilothouse sees agents through their hooks. ` + "`hooks install`" + ` merges a call to
` + "`pilothouse hook <agent>`" + ` into each agent's settings (~/.claude/settings.json,
~/.codex/hooks.json), keeping every other setting and hook, and backs the file
up first. OpenCode has no command hooks: install writes Pilothouse's plugin to
~/.config/opencode/plugins/pilothouse.js instead. ` + "`hooks uninstall`" + ` removes exactly
what install added.`,
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newHooksChangeCmd(true), newHooksChangeCmd(false), newHooksStatusCmd())
	return cmd
}

func newHooksChangeCmd(install bool) *cobra.Command {
	var agent string
	var dryRun bool
	use, short := "uninstall", "Remove Pilothouse's hooks (and nothing else)"
	if install {
		use, short = "install", "Wire Pilothouse's hooks into Claude Code, Codex and OpenCode"
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			targets, err := hookTargets(agent, install)
			if err != nil {
				return err
			}
			opts := adapters.InstallOptions{DryRun: dryRun}
			if install {
				if opts.ShimPath, err = shimPath(cmd.ErrOrStderr()); err != nil {
					return err
				}
			}
			return runHooksChange(cmd.OutOrStdout(), targets, opts, install)
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "", "only this agent: "+agentChoices()+" (default: every agent found on this machine)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show the changes without writing anything")
	return cmd
}

func runHooksChange(w io.Writer, targets []adapters.Adapter, opts adapters.InstallOptions, install bool) error {
	var failed []string
	changed := false
	for _, a := range targets {
		var ch adapters.Change
		var err error
		if install {
			ch, err = a.InstallHooks(opts)
		} else {
			ch, err = a.UninstallHooks(opts)
		}
		if err != nil {
			fmt.Fprintf(w, "✗ %s: %v\n", a.DisplayName(), err)
			failed = append(failed, a.DisplayName())
			continue
		}
		changed = changed || ch.Changed
		printChange(w, a.DisplayName(), ch, opts.DryRun)
	}
	switch {
	case len(failed) > 0:
		return fmt.Errorf("could not update hooks for %s", strings.Join(failed, ", "))
	case opts.DryRun && changed:
		fmt.Fprintln(w, "\nDry run: nothing was written. Run again without --dry-run to apply.")
	case install && changed:
		fmt.Fprintln(w, "\nNew agent sessions pick the hooks up; restart sessions that were already running.")
	}
	return nil
}

func printChange(w io.Writer, name string, ch adapters.Change, dryRun bool) {
	mark := "✓"
	if !ch.Changed {
		mark = "·"
	}
	fmt.Fprintf(w, "%s %s: %s\n", mark, name, ch.Summary)
	for _, c := range append([]adapters.Change{ch}, ch.More...) {
		if !c.Changed {
			continue
		}
		verb := "updated"
		if dryRun {
			verb = "would update"
		}
		fmt.Fprintf(w, "  %s %s\n", verb, c.Path)
		if c.Backup != "" {
			fmt.Fprintf(w, "  backup  %s\n", c.Backup)
		}
		if d := hookkit.Diff(c.Path, c.Before, c.After); d != "" {
			fmt.Fprintln(w, indent(d, "    "))
		}
	}
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

func newHooksStatusCmd() *cobra.Command {
	var agent string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show whether Pilothouse's hooks are installed and current",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			targets, err := hookTargets(agent, false)
			if err != nil {
				return err
			}
			// A missing binary path only weakens "current" to "all events
			// wired"; status must still work from a `go run` build.
			exe, _ := resolvedExe()
			return runHooksStatus(cmd.OutOrStdout(), targets, adapters.InstallOptions{ShimPath: exe}, asJSON)
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "", "only this agent: "+agentChoices())
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

type hookStatusJSON struct {
	Agent  api.AgentKind       `json:"agent"`
	Name   string              `json:"name"`
	Status adapters.HookStatus `json:"status"`
}

func runHooksStatus(w io.Writer, targets []adapters.Adapter, opts adapters.InstallOptions, asJSON bool) error {
	var rows []hookStatusJSON
	for _, a := range targets {
		st, err := a.Status(opts)
		if err != nil {
			st.Problems = append(st.Problems, err.Error())
		}
		if stale := StaleHookProblems(st.Path); len(stale) > 0 {
			st.Current = false
			st.Problems = append(st.Problems, stale...)
		}
		rows = append(rows, hookStatusJSON{a.Kind(), a.DisplayName(), st})
	}
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	for _, r := range rows {
		st := r.Status
		state := "not installed"
		switch {
		case st.Current:
			state = "installed, current"
		case st.Installed && hasStale(st.Problems):
			state = "installed, stale"
		case st.Installed:
			state = "installed, needs attention"
		}
		fmt.Fprintf(w, "%-12s %-27s %s\n", r.Name, state, st.Path)
		if len(st.Events) > 0 {
			fmt.Fprintf(w, "  events: %s\n", strings.Join(st.Events, ", "))
		}
		for _, p := range st.Problems {
			fmt.Fprintf(w, "  ! %s\n", p)
		}
		if !st.Installed && len(st.Problems) == 0 {
			fmt.Fprintf(w, "  run `pilothouse hooks install --agent %s`\n", r.Agent)
		}
	}
	return nil
}

// staleMark starts every StaleHookProblems message.
const staleMark = "stale — run `pilothouse setup`: "

// shimRefs finds the binaries an agent settings file calls as
// `<path> hook <agent>`: single-quoted (hookkit.ShellQuote) or bare.
var shimRefs = regexp.MustCompile(`'([^']*/pilothouse)'\s+hook\s|([^\s"'\\]+/pilothouse)\s+hook\s`)

// StaleHookProblems lists hooks in an agent settings file that call a
// pilothouse binary that is gone, or that lives in a versioned Homebrew
// Cellar directory the next `brew upgrade` deletes. `hooks status` and
// `doctor` report them as stale.
func StaleHookProblems(settingsPath string) []string {
	if settingsPath == "" {
		return nil
	}
	b, err := os.ReadFile(settingsPath)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range shimRefs.FindAllStringSubmatch(string(b), -1) {
		p := m[1] + m[2]
		if seen[p] {
			continue
		}
		seen[p] = true
		switch {
		case !fileExists(p):
			out = append(out, staleMark+"hooks call "+p+", which doesn't exist")
		case versionedBrewPath(p):
			out = append(out, staleMark+"hooks call "+p+", a versioned Homebrew path that `brew upgrade` removes")
		}
	}
	return out
}

func hasStale(problems []string) bool {
	for _, p := range problems {
		if strings.HasPrefix(p, staleMark) {
			return true
		}
	}
	return false
}

// hookTargets resolves --agent. Without it, install targets the agents
// found on this machine; uninstall and status cover every agent.
func hookTargets(agent string, install bool) ([]adapters.Adapter, error) {
	if agent != "" {
		a := all.Get(api.AgentKind(strings.ToLower(agent)))
		if a == nil {
			return nil, fmt.Errorf("unknown agent %q: use %s", agent, agentChoices())
		}
		return []adapters.Adapter{a}, nil
	}
	var out []adapters.Adapter
	for _, a := range all.List() {
		if a == nil {
			continue
		}
		if install && !a.Detect().Installed {
			continue
		}
		out = append(out, a)
	}
	if install && len(out) == 0 {
		return nil, errors.New("found no supported agent (Claude Code in ~/.claude, Codex in ~/.codex, OpenCode in ~/.config/opencode); pass --agent <name> to install anyway")
	}
	return out, nil
}

// agentChoices lists the --agent values: "claude, codex or opencode".
func agentChoices() string {
	var kinds []string
	for _, a := range all.List() {
		if a != nil {
			kinds = append(kinds, string(a.Kind()))
		}
	}
	if len(kinds) < 2 {
		return strings.Join(kinds, "")
	}
	return strings.Join(kinds[:len(kinds)-1], ", ") + " or " + kinds[len(kinds)-1]
}

// shimPath is the binary hooks will call: this one, absolute and with
// symlinks resolved (shared with `pilothouse setup`, so both write the same
// command). A path that will vanish is allowed but warned about.
func shimPath(warn io.Writer) (string, error) {
	exe, err := resolvedExe()
	if err != nil {
		return "", err
	}
	if why := unstableExe(exe); why != "" {
		fmt.Fprintf(warn, "warning: %s\n", why)
	}
	return exe, nil
}

// ---------------------------------------------------------------- pilothouse hook <agent>

const (
	maxHookPayload = 64 << 20         // stdin cap (BUILD-SPEC §6.1); the daemon accepts this much
	shimTimeout    = 2 * time.Second  // non-blocking hook kinds
	maxShimTimeout = 20 * time.Second // ceiling for large payloads
	shimEndTimeout = 1 * time.Second  // SessionEnd: Claude Code's whole budget is 1.5 s
	maxAncestors   = 12
)

// shimEnv is the environment allow-list forwarded to the daemon.
var shimEnv = []string{"TERM_PROGRAM", "TMUX", "PILOTHOUSE_SESSION_ID", "PILOTHOUSE_TASK_ID", "SSH_CONNECTION"}

// newHookShimCmd is what agents run on every hook: `pilothouse hook <agent>
// [event]`. It must never hang, never print anything but the agent's
// protocol, and fail open (BUILD-SPEC §6.1, invariant 2).
func newHookShimCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "hook <agent> [event]",
		Short:              "Hook entry point called by agents (not for people)",
		Hidden:             true,
		DisableFlagParsing: true, // never fail on argv an agent adds
		RunE: func(cmd *cobra.Command, args []string) error {
			if code := runShim(args, os.Stdin, os.Stdout, os.Stderr); code != 0 {
				return ExitError{Code: code}
			}
			return nil
		},
	}
}

// runShim forwards one hook to the daemon and relays its answer. Every
// failure path ends in the adapter's FailResponse.
func runShim(args []string, stdin io.Reader, stdout, stderr io.Writer) (code int) {
	if len(args) == 0 {
		return 0
	}
	ad := all.Get(api.AgentKind(args[0]))
	if ad == nil {
		return 0 // an unknown agent gets no opinion, never an error
	}
	kind := adapters.HookUnknown
	defer func() {
		// A bug here must not break the agent: answer like a down daemon.
		if r := recover(); r != nil {
			code = writeHookResponse(stdout, stderr, ad.FailResponse(kind, false))
		}
	}()

	payload, err := readHookStdin(stdin, shimTimeout)
	if errors.Is(err, errHookTooLarge) {
		// Never allow by omission: a payload too big to check is answered
		// like a down daemon for the event it most likely is (pre_tool
		// unless its prefix names another event), honouring fail_closed.
		kind = oversizedKind(ad, args[1:], payload)
		fmt.Fprintf(stderr, "pilothouse: hook payload is larger than %d MB; it was not checked by policy\n", maxHookPayload>>20)
		return writeHookResponse(stdout, stderr, ad.FailResponse(kind, failClosed(kind)))
	}
	if err != nil {
		return writeHookResponse(stdout, stderr, ad.FailResponse(kind, false))
	}
	env := hookEnvelope(ad.Kind(), args[1:], payload)
	ev, err := ad.ParseHook(env)
	if err != nil {
		return writeHookResponse(stdout, stderr, ad.FailResponse(kind, false))
	}
	kind = ev.Kind

	ctx, cancel := context.WithTimeout(context.Background(), hookTimeout(ad, kind, len(payload)))
	defer cancel()
	resp, err := client.New().Hook(ctx, env)
	if err != nil {
		resp = ad.FailResponse(kind, failClosed(kind))
	}
	return writeHookResponse(stdout, stderr, resp)
}

// hookTimeout: a permission hook may wait for a person up to the agent's
// own hook timeout; everything else must answer in time for the agent.
// Big payloads (a Write of a large file) get extra time to travel to the
// daemon and be parsed: one more second per MB past the first, capped well
// below the 30 s PreToolUse timeout Pilothouse installs. Timing out would
// fail open, so a too-short budget is worse than a slower answer.
func hookTimeout(ad adapters.Adapter, kind adapters.HookKind, size int) time.Duration {
	switch kind {
	case adapters.HookPermission:
		if d := ad.MaxHold(kind); d > 0 {
			return d
		}
	case adapters.HookSessionEnd:
		return shimEndTimeout
	}
	extra := time.Duration(size>>20) * time.Second
	return min(shimTimeout+extra, maxShimTimeout)
}

func hookEnvelope(agent api.AgentKind, rest []string, payload []byte) adapters.HookEnvelope {
	env := adapters.HookEnvelope{
		Agent:               agent,
		Payload:             payload,
		ShimPID:             os.Getpid(),
		PilothouseSessionID: os.Getenv("PILOTHOUSE_SESSION_ID"),
		ReceivedAt:          time.Now().UTC(),
	}
	if len(rest) > 0 {
		env.Event = rest[0]
	}
	env.Cwd, _ = os.Getwd()
	env.Ancestors, _ = proc.Ancestors(os.Getpid(), maxAncestors)
	for _, k := range shimEnv {
		if v := os.Getenv(k); v != "" {
			if env.Env == nil {
				env.Env = map[string]string{}
			}
			env.Env[k] = v
		}
	}
	if agent == api.AgentClaude {
		env.Messaging = messagingEndpoint(os.Getenv)
	}
	return env
}

// Claude Code (2.1.224+) tells hooks where its session's messaging socket
// is; a message written there reaches the model as a new prompt. The
// endpoint travels in its own envelope field, never in Env, so the daemon
// can keep it out of everything it logs or serves.
const (
	envMessagingSocket = "CLAUDE_CODE_MESSAGING_SOCKET"
	envMessagingToken  = "CLAUDE_CODE_MESSAGING_TOKEN"
	maxMessagingField  = 1024
)

// messagingEndpoint reads the messaging endpoint from the environment, or
// nil when it is missing or implausible (the daemon checks it again).
func messagingEndpoint(getenv func(string) string) *adapters.MessagingEndpoint {
	sock, tok := getenv(envMessagingSocket), getenv(envMessagingToken)
	if !strings.HasPrefix(sock, "/") || tok == "" || len(sock) > maxMessagingField || len(tok) > maxMessagingField ||
		strings.ContainsAny(sock, "\x00\n") || strings.ContainsAny(tok, "\x00\n") {
		return nil
	}
	return &adapters.MessagingEndpoint{Socket: sock, Token: tok}
}

// errHookTooLarge: stdin held more than maxHookPayload bytes.
var errHookTooLarge = errors.New("hook payload too large")

// readHookStdin reads the payload: at most maxHookPayload bytes, within
// timeout. It fails for a terminal, an empty payload or a writer that
// never closes stdin; an oversized payload returns its first
// maxHookPayload bytes with errHookTooLarge.
func readHookStdin(r io.Reader, timeout time.Duration) ([]byte, error) {
	if f, isFile := r.(*os.File); isFile && term.IsTerminal(int(f.Fd())) {
		return nil, errors.New("stdin is a terminal")
	}
	type result struct {
		b   []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			// runShim's recover cannot see this goroutine.
			if p := recover(); p != nil {
				done <- result{err: fmt.Errorf("reading stdin: %v", p)}
			}
		}()
		b, err := io.ReadAll(io.LimitReader(r, maxHookPayload+1))
		done <- result{b, err}
	}()
	select {
	case res := <-done:
		switch {
		case res.err != nil:
			return nil, res.err
		case len(res.b) == 0:
			return nil, errors.New("empty hook payload")
		case len(res.b) > maxHookPayload:
			return res.b[:maxHookPayload], errHookTooLarge
		}
		return res.b, nil
	case <-time.After(timeout):
		return nil, errors.New("timed out reading the hook payload")
	}
}

// hookEventName finds "hook_event_name" in a (truncated) payload; agents
// put it near the start.
var hookEventName = regexp.MustCompile(`"hook_event_name"\s*:\s*"([A-Za-z_]+)"`)

// oversizedKind guesses the hook kind of a payload too large to parse:
// the event named in its prefix, else pre_tool (the kind where failing
// open would skip policy).
func oversizedKind(ad adapters.Adapter, rest []string, prefix []byte) adapters.HookKind {
	head := prefix
	if len(head) > 64<<10 {
		head = head[:64<<10]
	}
	m := hookEventName.FindSubmatch(head)
	if m == nil {
		return adapters.HookPreTool
	}
	payload, _ := json.Marshal(map[string]string{"hook_event_name": string(m[1])})
	env := adapters.HookEnvelope{Agent: ad.Kind(), Payload: payload}
	if len(rest) > 0 {
		env.Event = rest[0]
	}
	ev, err := ad.ParseHook(env)
	if err != nil || ev.Kind == adapters.HookUnknown {
		return adapters.HookPreTool
	}
	return ev.Kind
}

// failClosed reads approvals.fail_closed. It only matters for pre_tool, and
// only on the failure path, so no other hook call ever parses config.
// Unreadable config means fail open.
func failClosed(kind adapters.HookKind) bool {
	if kind != adapters.HookPreTool {
		return false
	}
	cfg, err := config.Load()
	return err == nil && cfg.Approvals.FailClosed
}

func writeHookResponse(stdout, stderr io.Writer, r adapters.HookResponse) int {
	if r.Stdout != "" {
		_, _ = io.WriteString(stdout, r.Stdout)
	}
	if r.Stderr != "" {
		_, _ = io.WriteString(stderr, r.Stderr)
	}
	return r.ExitCode
}
