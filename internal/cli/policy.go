package cli

// `pilothouse policy …` (workstream C). Helpers in this file are prefixed
// with "pol" because package cli is shared by every workstream.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/xasdta/pilothouse/internal/adapters/all"
	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/client"
	"github.com/xasdta/pilothouse/internal/paths"
	"github.com/xasdta/pilothouse/internal/policy"
)

func newPolicyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "policy",
		Short: "Rules applied before every tool call",
		Long: `Pilothouse applies your rules before every tool call of every agent:
deny blocks the call, ask makes a person confirm it, allow skips the agent's
own prompt. You write rules in ~/.pilothouse/policy.yaml and, per repository,
in <repo>/.pilothouse/policy.yaml (allow rules there need ` + "`pilothouse policy trust`" + `).
Rules added from your phone or with ` + "`pilothouse policy add`" + `, and the presets you
switch on, live in ~/.pilothouse/rules.yaml, which Pilothouse writes.

Reference: docs/POLICY.md`,
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(
		newPolicyInitCmd(),
		newPolicyCheckCmd(),
		newPolicyTestCmd(),
		newPolicyReplayCmd(),
		newPolicyTrustCmd(true),
		newPolicyTrustCmd(false),
		newPolicyStatusCmd(),
		newPolicyRulesCmd(),
		newPolicyPresetCmd(),
		newPolicyAddCmd(),
		newPolicyManagedCmd("remove", "Delete a managed rule"),
		newPolicyManagedCmd("enable", "Turn a managed rule back on"),
		newPolicyManagedCmd("disable", "Turn a managed rule off without deleting it"),
	)
	return cmd
}

// polOptions are the engine options the daemon uses too: standard files
// plus every adapter's protected settings files.
func polOptions() policy.Options {
	var extra []string
	for _, a := range all.List() {
		if a != nil {
			extra = append(extra, a.ProtectedPaths()...)
		}
	}
	return policy.DefaultOptions(extra...)
}

// ---- init ---------------------------------------------------------------

func newPolicyInitCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write a starter ~/.pilothouse/policy.yaml with commented examples",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			file := paths.Policy()
			if _, err := os.Stat(file); err == nil {
				if !force {
					return fmt.Errorf("%s already exists; edit it, or run `pilothouse policy init --force` to replace it (the old file is kept as a backup)", polTilde(file))
				}
				backup := file + ".bak-" + time.Now().Format("20060102-150405")
				data, err := os.ReadFile(file)
				if err != nil {
					return err
				}
				if err := policy.WriteFileAtomic(backup, data, 0o600); err != nil {
					return fmt.Errorf("backing up %s: %w", polTilde(file), err)
				}
				fmt.Fprintf(out, "Backed up the old file to %s\n", polTilde(backup))
			}
			if err := policy.WriteFileAtomic(file, []byte(policy.StarterPolicy), 0o600); err != nil {
				return err
			}
			n, _ := policy.Check(file, []byte(policy.StarterPolicy))
			fmt.Fprintf(out, "Wrote %s (%d rules).\n", polTilde(file), n)
			fmt.Fprintln(out, "Edit it, then run `pilothouse policy check`. A running daemon picks up changes within 2 seconds.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "replace an existing policy file (keeps a backup)")
	return cmd
}

// ---- check --------------------------------------------------------------

func newPolicyCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check [file]",
		Short: "Validate policy files (default: yours and this repo's)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			st := polStyleFor(out)
			var files []string
			if len(args) == 1 {
				files = args
			} else {
				files = polDefaultFiles()
				if len(files) == 0 {
					fmt.Fprintf(out, "No policy files yet. Run `pilothouse policy init` to create %s.\n", polTilde(paths.Policy()))
					return nil
				}
			}
			bad := false
			for _, f := range files {
				n, err := policy.CheckFile(f)
				if err != nil {
					bad = true
					fmt.Fprintf(out, "%s %s\n", st.red("✗"), polTilde(f))
					var fe policy.FileErrors
					if errors.As(err, &fe) {
						for _, e := range fe {
							fmt.Fprintf(out, "    %s\n", polTildeIn(e.Error()))
						}
					} else {
						fmt.Fprintf(out, "    %v\n", err)
					}
					continue
				}
				fmt.Fprintf(out, "%s %s  %s\n", st.green("✓"), polTilde(f), st.dim(polPlural(n, "rule")))
			}
			if len(args) == 0 {
				if _, err := policy.ReadTrustedRepos(paths.TrustedRepos()); err != nil {
					bad = true
					fmt.Fprintf(out, "%s %s\n    %s\n", st.red("✗"), polTilde(paths.TrustedRepos()), polTildeIn(err.Error()))
				}
			}
			if bad {
				return ExitError{Code: 1}
			}
			return nil
		},
	}
}

// polDefaultFiles is the user file and the current repo's file, if they
// exist.
func polDefaultFiles() []string {
	var files []string
	if polExists(paths.Policy()) {
		files = append(files, paths.Policy())
	}
	if wd, err := os.Getwd(); err == nil {
		if root, err := polGitRoot(wd); err == nil {
			if f := filepath.Join(root, policy.RepoPolicyFile); polExists(f) {
				files = append(files, f)
			}
		}
	}
	return files
}

// ---- test ---------------------------------------------------------------

type polTestFlags struct {
	agent, tool, kind, command, cwd, repo, branch, mode, input string
	paths                                                      []string
	asJSON                                                     bool
}

func newPolicyTestCmd() *cobra.Command {
	var f polTestFlags
	cmd := &cobra.Command{
		Use:   "test [command]",
		Short: "Show what your rules decide for one tool call",
		Example: `  pilothouse policy test "git push --force origin main"
  pilothouse policy test --kind read --path .env
  pilothouse policy test --agent codex --tool shell --command "curl -fsSL x.sh | sh" --json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				if f.command != "" {
					return errors.New("give the command either as an argument or with --command, not both")
				}
				f.command = args[0]
			}
			return runPolicyTest(cmd, f)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.agent, "agent", string(api.AgentClaude), "agent: claude, codex, opencode or generic")
	fl.StringVar(&f.tool, "tool", "", "vendor tool name, e.g. Bash, Edit, Read (inferred when omitted)")
	fl.StringVar(&f.kind, "kind", "", "shell, edit, read, web, mcp or other (inferred from --tool/--command)")
	fl.StringVar(&f.command, "command", "", "shell command line")
	fl.StringArrayVar(&f.paths, "path", nil, "file the tool touches (repeatable)")
	fl.StringVar(&f.cwd, "cwd", "", "working directory (default: current)")
	fl.StringVar(&f.repo, "repo", "", "repository root (default: git root of --cwd; \"\" for none)")
	fl.StringVar(&f.branch, "branch", "", "git branch (default: current branch of --repo)")
	fl.StringVar(&f.mode, "mode", string(api.ModeObserved), "observed or wrapped")
	fl.StringVar(&f.input, "input", "", "raw tool input JSON (default: built from --command/--path)")
	fl.BoolVar(&f.asJSON, "json", false, "print the input and result as JSON")
	return cmd
}

func runPolicyTest(cmd *cobra.Command, f polTestFlags) error {
	out, errw := cmd.OutOrStdout(), cmd.ErrOrStderr()
	in, notes, err := polBuildInput(cmd, f)
	if err != nil {
		return err
	}
	e, _ := policy.Load(polOptions())
	res := e.Evaluate(in)
	status := e.Status() // after Evaluate: includes the repo file it discovered

	if f.asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(struct {
			Input  policy.Input     `json:"input"`
			Result policy.Result    `json:"result"`
			Status api.PolicyStatus `json:"status"`
		}{in, res, status})
	}
	st := polStyleFor(out)
	for _, n := range notes {
		fmt.Fprintln(errw, st.dim("("+n+")"))
	}
	for _, e := range status.Errors {
		fmt.Fprintf(errw, "%s %s\n", st.yellow("warning:"), polTildeIn(e))
	}
	if res.Action == "" {
		fmt.Fprintf(out, "Decision: %s — no rule matched; the agent's own permission settings apply.\n", st.dim("none"))
	} else {
		fmt.Fprintf(out, "Decision: %s by %s (%s)\n", st.action(res.Action, true), st.bold(res.Rule), polFileLabel(res.File))
		if res.Message != "" {
			fmt.Fprintf(out, "Message:  %s\n", res.Message)
		}
	}
	if len(res.Matched) > 0 {
		fmt.Fprintln(out, "Matched:")
		tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		for _, m := range res.Matched {
			line := fmt.Sprintf("  %s\t%s\t%s", st.action(m.Action, false), m.Rule, polFileLabel(m.File))
			if m.Ignored != "" {
				line += "\t" + st.dim("ignored: "+m.Ignored)
			}
			fmt.Fprintln(tw, line)
		}
		tw.Flush()
	}
	return nil
}

// polBuildInput turns flags into a policy.Input, filling in what a real
// hook would carry (tool name, kind, raw input, repo, branch).
func polBuildInput(cmd *cobra.Command, f polTestFlags) (policy.Input, []string, error) {
	var notes []string
	if f.command == "" && len(f.paths) == 0 && f.tool == "" && f.input == "" {
		return policy.Input{}, nil, errors.New("describe a call with --command and/or --path (and optionally --tool/--kind)")
	}
	agent := api.AgentKind(f.agent)
	switch agent {
	case api.AgentClaude, api.AgentCodex, api.AgentOpenCode, api.AgentGeneric:
	default:
		return policy.Input{}, nil, fmt.Errorf("unknown --agent %q (claude, codex, opencode or generic)", f.agent)
	}
	kind := api.ToolKind(f.kind)
	tool := f.tool
	if kind == "" && tool != "" {
		kind = polToolKind(tool)
	}
	if kind == "" {
		if f.command != "" {
			kind = api.ToolShell
		} else {
			kind = api.ToolEdit
			notes = append(notes, "assuming --kind edit; pass --kind read to test a read")
		}
	}
	switch kind {
	case api.ToolShell, api.ToolEdit, api.ToolRead, api.ToolWeb, api.ToolMCP, api.ToolOther:
	default:
		return policy.Input{}, nil, fmt.Errorf("unknown --kind %q (shell, edit, read, web, mcp or other)", f.kind)
	}
	if tool == "" {
		tool = polDefaultTool(agent, kind)
	}

	cwd := f.cwd
	if cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			return policy.Input{}, nil, err
		}
		cwd = wd
	}
	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}
	repo := f.repo
	if !cmd.Flags().Changed("repo") {
		if root, err := polGitRoot(cwd); err == nil {
			repo = root
		}
	}
	branch := f.branch
	if !cmd.Flags().Changed("branch") && repo != "" {
		branch = polGitBranch(repo)
	}

	var raw json.RawMessage
	switch {
	case f.input != "":
		if !json.Valid([]byte(f.input)) {
			return policy.Input{}, nil, errors.New("--input is not valid JSON")
		}
		raw = json.RawMessage(f.input)
	case f.command != "":
		raw, _ = json.Marshal(map[string]string{"command": f.command})
	case len(f.paths) == 1:
		raw, _ = json.Marshal(map[string]string{"file_path": f.paths[0]})
	}
	return policy.Input{
		Agent: agent, Tool: tool, ToolKind: kind, Command: f.command, Paths: f.paths,
		Input: raw, Cwd: cwd, Repo: repo, Branch: branch, Mode: api.Mode(f.mode),
		Checkpoint: api.CheckpointPreTool,
	}, notes, nil
}

// polToolKind classifies the common vendor tool names, mirroring the
// adapters, so `--tool Edit` alone is enough.
func polToolKind(tool string) api.ToolKind { return policy.GuessToolKind(tool) }

func polDefaultTool(agent api.AgentKind, kind api.ToolKind) string {
	if agent == api.AgentOpenCode {
		switch kind {
		case api.ToolShell:
			return "bash"
		case api.ToolEdit:
			return "edit"
		case api.ToolRead:
			return "read"
		case api.ToolWeb:
			return "webfetch"
		case api.ToolMCP:
			return "server_tool"
		}
		return "tool"
	}
	codex := agent == api.AgentCodex
	switch kind {
	case api.ToolShell:
		if codex {
			return "shell"
		}
		return "Bash"
	case api.ToolEdit:
		if codex {
			return "apply_patch"
		}
		return "Edit"
	case api.ToolRead:
		return "Read"
	case api.ToolWeb:
		if codex {
			return "web_search"
		}
		return "WebFetch"
	case api.ToolMCP:
		return "mcp__server__tool"
	}
	return "Tool"
}

// ---- replay -------------------------------------------------------------

func newPolicyReplayCmd() *cobra.Command {
	var since string
	var asJSON, showAll bool
	cmd := &cobra.Command{
		Use:   "replay",
		Short: "Re-run logged tool calls through your current rules and show what would change",
		Long: `Replay re-evaluates the pre_tool calls in the daemon's event log with the
policy files as they are now, and lists the calls whose verdict would differ
from what was decided at the time. Use it after editing rules to see their
effect on real traffic before it matters.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, err := polParseSince(since)
			if err != nil {
				return err
			}
			return runPolicyReplay(cmd, client.New(), time.Now().Add(-d), asJSON, showAll)
		},
	}
	cmd.Flags().StringVar(&since, "since", "7d", "how far back to look, e.g. 2h, 3d")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the full report as JSON")
	cmd.Flags().BoolVar(&showAll, "all", false, "list unchanged calls too")
	return cmd
}

// polParseSince accepts Go durations plus a "d" (days) suffix.
func polParseSince(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err == nil && n > 0 {
			return time.Duration(n * float64(24*time.Hour)), nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid --since %q (use e.g. 90m, 12h, 7d)", s)
	}
	return d, nil
}

func runPolicyReplay(cmd *cobra.Command, c *client.Client, since time.Time, asJSON, showAll bool) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
	defer cancel()
	events, err := polFetchEvents(ctx, c, since)
	if errors.Is(err, client.ErrDaemonDown) {
		return errors.New("replay reads the event log through the daemon, which is not running.\n" +
			"Start it with `pilothouse start` and try again, or check a single call with `pilothouse policy test`")
	}
	if err != nil {
		return fmt.Errorf("reading the event log: %w", err)
	}
	sessions := map[string]*api.Session{}
	if list, err := c.Sessions(ctx); err == nil {
		for i := range list {
			sessions[list[i].ID] = &list[i]
		}
	}
	lookup := func(id string) (api.Session, bool) {
		s, seen := sessions[id]
		if !seen {
			if got, err := c.Session(ctx, id); err == nil {
				s = &got
			}
			sessions[id] = s // cache misses too
		}
		if s == nil {
			return api.Session{}, false
		}
		return *s, true
	}
	e, _ := policy.Load(polOptions())
	rep := policy.Replay(e, events, since, lookup)

	out := cmd.OutOrStdout()
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	st := polStyleFor(out)
	fmt.Fprintf(out, "Replayed %s since %s: %s.\n", polPlural(rep.Calls, "tool call"),
		since.Format("Jan 2 15:04"), polPlural(rep.Changed, "verdict would change", "verdicts would change"))
	var rows []policy.ReplayItem
	for _, it := range rep.Items {
		if it.Changed || showAll {
			rows = append(rows, it)
		}
	}
	if len(rows) == 0 {
		return nil
	}
	fmt.Fprintln(out)
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, st.dim("WHEN\tSESSION\tTOOL\tCALL\tTHEN\t\tNOW"))
	for _, it := range rows {
		then := st.action(it.Then, false)
		if it.ThenRule != "" {
			then += " " + st.dim("("+it.ThenRule+")")
		}
		now := st.action(it.Now, false)
		if it.NowRule != "" {
			now += " " + st.dim("("+it.NowRule+")")
		}
		arrow := "→"
		if !it.Changed {
			arrow = "="
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", it.At.Local().Format("Jan 2 15:04"),
			polShort(it.SessionID, 12), it.Tool, polShort(polOneLine(it.Summary), 60), then, arrow, now)
	}
	return tw.Flush()
}

// polFetchEvents pages through the event log, keeping only what replay
// needs: pre_tool hooks and policy decisions newer than since.
func polFetchEvents(ctx context.Context, c *client.Client, since time.Time) ([]api.Event, error) {
	var out []api.Event
	var after int64
	for {
		page, err := c.Events(ctx, after)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			return out, nil
		}
		for _, ev := range page {
			if (ev.Type == api.EventHook || ev.Type == api.EventPolicyDecision) && !ev.At.Before(since) {
				out = append(out, ev)
			}
		}
		last := page[len(page)-1].Seq
		if last <= after {
			return out, nil // defensive: a daemon that ignores after= must not loop forever
		}
		after = last
	}
}

// ---- trust / untrust ----------------------------------------------------

func newPolicyTrustCmd(trust bool) *cobra.Command {
	use, short := "trust [repo]", "Let a repo's .pilothouse/policy.yaml add allow rules (default: this repo)"
	if !trust {
		use, short = "untrust [repo]", "Limit a repo's .pilothouse/policy.yaml to deny and ask rules again"
	}
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			repo, err := polRepoArg(args)
			if err != nil {
				return err
			}
			file := paths.TrustedRepos()
			if trust {
				added, err := policy.TrustRepo(file, repo)
				if err != nil {
					return err
				}
				if !added {
					fmt.Fprintf(out, "%s is already trusted.\n", polTilde(repo))
					return nil
				}
				fmt.Fprintf(out, "Trusted %s: allow rules in its %s now apply.\n", polTilde(repo), policy.RepoPolicyFile)
				if n, err := policy.CheckFile(filepath.Join(repo, policy.RepoPolicyFile)); err == nil {
					fmt.Fprintf(out, "Its policy file has %s; review them with `pilothouse policy check`.\n", polPlural(n, "rule"))
				}
				return nil
			}
			removed, err := policy.UntrustRepo(file, repo)
			if err != nil {
				return err
			}
			if !removed {
				fmt.Fprintf(out, "%s was not trusted.\n", polTilde(repo))
				return nil
			}
			fmt.Fprintf(out, "Untrusted %s: its policy file may only add deny and ask rules.\n", polTilde(repo))
			return nil
		},
	}
}

// polRepoArg resolves the repo argument, defaulting to the current git
// root.
func polRepoArg(args []string) (string, error) {
	if len(args) == 1 {
		abs, err := filepath.Abs(args[0])
		if err != nil {
			return "", err
		}
		fi, err := os.Stat(abs)
		if err != nil {
			return "", err
		}
		if !fi.IsDir() {
			return "", fmt.Errorf("%s is not a directory", args[0])
		}
		if root, err := polGitRoot(abs); err == nil {
			return root, nil
		}
		return abs, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	root, err := polGitRoot(wd)
	if err != nil {
		return "", errors.New("not inside a git repository; pass the repo path, e.g. `pilothouse policy trust ~/src/app`")
	}
	return root, nil
}

// ---- status -------------------------------------------------------------

func newPolicyStatusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show loaded policy files, rule counts and errors",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Second)
			defer cancel()
			source := "daemon"
			status, err := client.New().PolicyStatus(ctx)
			if err != nil {
				if !errors.Is(err, client.ErrDaemonDown) {
					return fmt.Errorf("asking the daemon: %w", err)
				}
				source = "local"
				e, _ := policy.Load(polOptions())
				// Evaluate once in the current repo so its file is discovered.
				if wd, err := os.Getwd(); err == nil {
					if root, err := polGitRoot(wd); err == nil {
						e.Evaluate(policy.Input{Cwd: wd, Repo: root})
					}
				}
				status = e.Status()
			}
			trusted, terr := policy.ReadTrustedRepos(paths.TrustedRepos())
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(struct {
					Source  string               `json:"source"`
					Status  api.PolicyStatus     `json:"status"`
					Trusted []policy.TrustedRepo `json:"trusted_repos"`
				}{source, status, trusted})
			}
			polPrintStatus(out, source, status, trusted, terr)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}

func polPrintStatus(out io.Writer, source string, s api.PolicyStatus, trusted []policy.TrustedRepo, terr error) {
	st := polStyleFor(out)
	if source == "daemon" {
		fmt.Fprintf(out, "Policy loaded by the daemon: %s.\n\n", polPlural(s.Rules, "rule"))
	} else {
		fmt.Fprintf(out, "Daemon not running; showing the files on disk: %s.\n\n", polPlural(s.Rules, "rule"))
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	for _, f := range s.Files {
		state := polPlural(f.Rules, "rule")
		switch {
		case f.Error != "" && f.Rules == 0:
			state = st.red("errors") + " " + st.dim("(none of its rules apply until fixed)")
		case f.Error != "":
			state = st.red("errors") + " " + st.dim("(still using "+polPlural(f.Rules, "rule")+" from its last valid version)")
		case f.Scope == policy.ScopeUser && !polExists(f.Path):
			state = st.dim("not created — run `pilothouse policy init`")
		case f.Scope == policy.ScopeRepo && !f.Trusted:
			state += " " + st.dim("(untrusted: deny/ask only)")
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", f.Scope, polFileLabel(f.Path), state)
	}
	tw.Flush()
	if len(s.Errors) > 0 {
		fmt.Fprintln(out, "\n"+st.red("Errors:"))
		for _, e := range s.Errors {
			fmt.Fprintf(out, "  %s\n", polTildeIn(e))
		}
	}
	fmt.Fprintln(out)
	switch {
	case terr != nil:
		fmt.Fprintf(out, "Trusted repos: %s %v\n", st.red("error:"), terr)
	case len(trusted) == 0:
		fmt.Fprintln(out, "Trusted repos: none "+st.dim("(`pilothouse policy trust` in a repo to allow its allow rules)"))
	default:
		fmt.Fprintln(out, "Trusted repos:")
		sort.Slice(trusted, func(i, j int) bool { return trusted[i].Path < trusted[j].Path })
		for _, r := range trusted {
			since := ""
			if !r.TrustedAt.IsZero() {
				since = st.dim(" since " + r.TrustedAt.Local().Format("2006-01-02"))
			}
			fmt.Fprintf(out, "  %s%s\n", polTilde(r.Path), since)
		}
	}
}

// ---- rules, presets and managed rules --------------------------------------

// The daemon edits ~/.pilothouse/rules.yaml (and records policy.changed)
// when it runs; otherwise these commands edit the file directly, under the
// same lock and validation, and the daemon picks it up when it starts.

const polBy = "cli"

// polRulesView asks the daemon, or reads the files when it is down.
func polRulesView(ctx context.Context) (api.RulesView, string, error) {
	v, err := client.New().Rules(ctx)
	if err == nil {
		return v, "daemon", nil
	}
	if !errors.Is(err, client.ErrDaemonDown) {
		return v, "", err
	}
	e, _ := policy.Load(polOptions())
	var repos []string
	if wd, err := os.Getwd(); err == nil {
		if root, err := polGitRoot(wd); err == nil {
			repos = append(repos, root)
		}
	}
	return api.RulesView{Rules: e.RuleInfos(repos), Presets: e.PresetsView(), Problems: e.Status().Errors}, "local", nil
}

// polErr makes a managed-rule error readable.
func polErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, policy.ErrInvalidRule) {
		return errors.New(strings.TrimPrefix(err.Error(), policy.ErrInvalidRule.Error()+": "))
	}
	return err
}

func newPolicyRulesCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "rules",
		Short: "List every rule in the order it is checked, and the presets",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()
			v, source, err := polRulesView(ctx)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(v)
			}
			polPrintRules(out, v, source)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}

func polPrintRules(out io.Writer, v api.RulesView, source string) {
	st := polStyleFor(out)
	if source == "local" {
		fmt.Fprintln(out, st.dim("(daemon not running; showing the files on disk)"))
	}
	fmt.Fprintln(out, "Rules, in the order they are checked (deny beats ask beats allow):")
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	for _, r := range v.Rules {
		src := r.Source
		if r.Source == api.RuleSourceBuiltin {
			src = "built-in"
		}
		name := r.Name
		if !r.Enabled {
			name += " " + st.dim("(off)")
		}
		what := polMatchSummary(r.RuleSpec)
		if (r.Source == api.RuleSourceBuiltin || r.Source == api.RuleSourcePreset) && r.Description != "" {
			what = r.Description // presets' globs are long; `policy preset <name>` shows them
		}
		if r.Managed && r.By != "" {
			what += st.dim("  — by " + r.By + polWhen(r.Created))
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", src, st.action(r.Action, false), name, what)
	}
	tw.Flush()
	fmt.Fprintln(out, "\nPresets:")
	for _, p := range v.Presets {
		mark := st.dim("off")
		if p.Enabled {
			mark = st.green("on ")
		}
		fmt.Fprintf(out, "  %s  %-22s %s\n", mark, p.Name, p.Title)
	}
	fmt.Fprintln(out, st.dim("\nSwitch one with `pilothouse policy preset <name> on|off`; add a rule with `pilothouse policy add`."))
	for _, p := range v.Problems {
		fmt.Fprintf(out, "%s %s\n", st.yellow("warning:"), polTildeIn(p))
	}
}

func polWhen(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return ", " + t.Local().Format("Jan 2")
}

// polMatchSummary renders a rule's match block on one line.
func polMatchSummary(s api.RuleSpec) string {
	keys := make([]string, 0, len(s.Match))
	for k := range s.Match {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, k+": "+polShort(strings.Join(s.Match[k], ", "), 60))
	}
	if s.When != "" {
		parts = append(parts, "when: "+polShort(polOneLine(s.When), 40))
	}
	return polTildeIn(strings.Join(parts, "  "))
}

func newPolicyPresetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "preset [name [on|off]]",
		Short: "List presets, show one, or switch one on or off",
		Long: `Presets are named bundles of rules. guarded-autopilot is a safe alternative to
--dangerously-skip-permissions; protect-main, no-secrets and deploys-need-approval
each cover one concern. They are stored in ~/.pilothouse/rules.yaml.`,
		Example: `  pilothouse policy preset
  pilothouse policy preset guarded-autopilot
  pilothouse policy preset guarded-autopilot on`,
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()
			out := cmd.OutOrStdout()
			if len(args) < 2 {
				v, source, err := polRulesView(ctx)
				if err != nil {
					return err
				}
				if len(args) == 0 {
					if source == "local" {
						fmt.Fprintln(out, polStyleFor(out).dim("(daemon not running; showing the files on disk)"))
					}
					for _, p := range v.Presets {
						state := "off"
						if p.Enabled {
							state = "on"
						}
						fmt.Fprintf(out, "%-22s %-3s  %s\n", p.Name, state, p.Title)
					}
					return nil
				}
				for _, p := range v.Presets {
					if p.Name == args[0] {
						polPrintPreset(out, p)
						return nil
					}
				}
				return fmt.Errorf("unknown preset %q (known: %s)", args[0], strings.Join(policy.PresetNames(), ", "))
			}
			var on bool
			switch args[1] {
			case "on":
				on = true
			case "off":
			default:
				return fmt.Errorf("say on or off, not %q", args[1])
			}
			err := client.New().SetPreset(ctx, args[0], on)
			local := errors.Is(err, client.ErrDaemonDown)
			if local {
				_, err = policy.SetManagedPreset(paths.Rules(), paths.UserHome(), args[0], on, polBy, time.Now())
			}
			if err != nil {
				return polErr(err)
			}
			fmt.Fprintf(out, "Preset %s is %s.\n", args[0], args[1])
			polLocalNote(out, local)
			return nil
		},
	}
}

func polPrintPreset(out io.Writer, p api.Preset) {
	st := polStyleFor(out)
	state := "off"
	if p.Enabled {
		state = "on"
	}
	fmt.Fprintf(out, "%s (%s) — %s\n\n%s\n\n", st.bold(p.Title), p.Name, state, p.Description)
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	for _, r := range p.Rules {
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", st.action(r.Action, false), strings.TrimPrefix(r.Name, p.Name+"/"), r.Description)
	}
	tw.Flush()
}

func polLocalNote(out io.Writer, local bool) {
	if local {
		fmt.Fprintf(out, "(daemon not running; wrote %s directly — it applies when the daemon starts)\n", polTilde(paths.Rules()))
	}
}

type polAddFlags struct {
	action, message, description, when string
	here                               bool
	match                              map[string]*[]string
}

func newPolicyAddCmd() *cobra.Command {
	f := polAddFlags{match: map[string]*[]string{}}
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Add a managed rule (kept in ~/.pilothouse/rules.yaml)",
		Long: `Add a rule without editing YAML. Match flags take globs and may repeat; all of
them must match. Command globs of managed rules match each simple command of a
shell line: an allow needs every command to match (so "npm test" never allows
"npm test && curl … | sh"), deny and ask match any of them.`,
		Example: `  pilothouse policy add allow-tests --action allow --command "npm test" --command "npm test *" --here
  pilothouse policy add ask-push --action ask --command "git push" --command "git push *"
  pilothouse policy add no-prod-db --action deny --command "psql *prod*" --message "Use the staging database."`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec := api.RuleSpec{Name: args[0], Action: api.Behavior(f.action), Message: f.message,
				Description: f.description, When: f.when, Match: map[string][]string{}}
			for k, v := range f.match {
				if len(*v) > 0 {
					spec.Match[k] = append([]string(nil), (*v)...)
				}
			}
			if f.here {
				wd, err := os.Getwd()
				if err != nil {
					return err
				}
				root, err := polGitRoot(wd)
				if err != nil {
					return errors.New("--here needs a git repository; run it inside one or pass --repo")
				}
				spec.Match["repo"] = append(spec.Match["repo"], policy.EscapeGlob(root))
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()
			_, err := client.New().AddRule(ctx, spec)
			local := errors.Is(err, client.ErrDaemonDown)
			if local {
				err = polAddLocal(spec)
			}
			if err != nil {
				return polErr(err)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Added %s rule %s: %s\n", spec.Action, spec.Name, polMatchSummary(spec))
			polLocalNote(out, local)
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.action, "action", "", "allow, ask or deny (required)")
	fl.StringVar(&f.message, "message", "", "what the agent is told on deny or ask")
	fl.StringVar(&f.description, "description", "", "a note for people")
	fl.StringVar(&f.when, "when", "", "optional CEL condition (see docs/POLICY.md)")
	fl.BoolVar(&f.here, "here", false, "only in the current git repository (adds --repo <its root>)")
	for _, k := range []string{"command", "path", "tool", "kind", "repo", "branch", "cwd", "agent"} {
		v := []string{}
		f.match[k] = &v
		fl.StringArrayVar(&v, k, nil, "match "+k+" glob (repeatable)")
	}
	_ = cmd.MarkFlagRequired("action")
	return cmd
}

// polAddLocal adds a rule to the file with the daemon's checks.
func polAddLocal(spec api.RuleSpec) error {
	home := paths.UserHome()
	if err := policy.ValidateRuleSpec(spec, policy.ValidateOptions{AllowWhen: true, Home: home}); err != nil {
		return err
	}
	e, _ := policy.Load(polOptions())
	if err := policy.NameInUse(spec.Name, e.RuleInfos(nil)); err != nil {
		return err
	}
	return policy.AddManagedRule(paths.Rules(), home, spec, polBy, time.Now())
}

// newPolicyManagedCmd is remove, enable or disable.
func newPolicyManagedCmd(op, short string) *cobra.Command {
	return &cobra.Command{
		Use:   op + " <name>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()
			c := client.New()
			var err error
			if op == "remove" {
				err = c.RemoveRule(ctx, name)
			} else {
				err = c.SetRuleEnabled(ctx, name, op == "enable")
			}
			local := errors.Is(err, client.ErrDaemonDown)
			if local {
				file, home := paths.Rules(), paths.UserHome()
				if op == "remove" {
					err = policy.RemoveManagedRule(file, home, name)
				} else {
					_, err = policy.SetManagedRuleEnabled(file, home, name, op == "enable")
				}
				if errors.Is(err, policy.ErrNoManagedRule) {
					e, _ := policy.Load(polOptions())
					err = policy.NotManagedError(name, e.RuleInfos(nil))
				}
			}
			if err != nil {
				return polErr(err)
			}
			past := map[string]string{"remove": "Removed", "enable": "Enabled", "disable": "Disabled"}[op]
			fmt.Fprintf(cmd.OutOrStdout(), "%s rule %s.\n", past, name)
			polLocalNote(cmd.OutOrStdout(), local)
			return nil
		},
	}
}

// ---- small helpers ------------------------------------------------------

func polGitRoot(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func polGitBranch(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return ""
	}
	b := strings.TrimSpace(string(out))
	if b == "HEAD" {
		return "" // detached
	}
	return b
}

func polExists(p string) bool {
	_, err := os.Stat(p)
	return !errors.Is(err, fs.ErrNotExist) && err == nil
}

func polTilde(p string) string {
	home := paths.UserHome()
	if home != "" && (p == home || strings.HasPrefix(p, home+"/")) {
		return "~" + p[len(home):]
	}
	return p
}

// polTildeIn shortens the home directory wherever it appears in a message.
func polTildeIn(s string) string {
	if home := paths.UserHome(); home != "" && home != "/" {
		return strings.ReplaceAll(s, home+"/", "~/")
	}
	return s
}

func polFileLabel(f string) string {
	if f == policy.BuiltinFile {
		return "built-in"
	}
	return polTilde(f)
}

func polPlural(n int, one string, many ...string) string {
	if n == 1 {
		return "1 " + one
	}
	if len(many) > 0 {
		return strconv.Itoa(n) + " " + many[0]
	}
	return strconv.Itoa(n) + " " + one + "s"
}

func polShort(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func polOneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// polStyle colours output only on a terminal, and never with NO_COLOR.
type polStyle struct{ on bool }

func polStyleFor(w io.Writer) polStyle {
	f, ok := w.(*os.File)
	return polStyle{on: ok && os.Getenv("NO_COLOR") == "" && term.IsTerminal(int(f.Fd()))}
}

func (s polStyle) wrap(code, text string) string {
	if !s.on || text == "" {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func (s polStyle) red(t string) string    { return s.wrap("31", t) }
func (s polStyle) green(t string) string  { return s.wrap("32", t) }
func (s polStyle) yellow(t string) string { return s.wrap("33", t) }
func (s polStyle) dim(t string) string    { return s.wrap("2", t) }
func (s polStyle) bold(t string) string   { return s.wrap("1", t) }

// action renders a verdict; upper is for the headline.
func (s polStyle) action(b api.Behavior, upper bool) string {
	label := string(b)
	if b == "" {
		label = "none"
	}
	if upper {
		label = strings.ToUpper(label)
	}
	switch b {
	case api.BehaviorDeny:
		return s.wrap("1;31", label)
	case api.BehaviorAsk:
		return s.wrap("1;33", label)
	case api.BehaviorAllow:
		return s.wrap("1;32", label)
	}
	return s.dim(label)
}
