package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/client"
	"github.com/xasdta/pilothouse/internal/paths"
	"github.com/xasdta/pilothouse/internal/task"
	"github.com/xasdta/pilothouse/internal/wrap"
)

// Workstream D: `run`, `attach`, hidden `__holder`, and `task …`.

func newRunCmd() *cobra.Command {
	var (
		name, taskID string
		detach       bool
		replay       bool
	)
	cmd := &cobra.Command{
		Use:   "run [-n name] [-d] [--] <cmd> [args…]",
		Short: "Run an agent under Pilothouse so you can detach, reattach and approve from anywhere",
		Long: `Run starts the command in a background holder that owns its terminal and
attaches this terminal to it. Detach with ctrl+\ (press it twice to send a
literal ctrl+\ to the agent); the agent keeps running when the terminal or
the daemon goes away. Reattach with ` + "`pilothouse attach <id>`" + `.

The agent kind is picked from the command name: claude, codex, or generic.`,
		Example: `  pilothouse run claude
  pilothouse run -n fix-login -d -- codex "fix the login bug"`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			exe, err := exec.LookPath(args[0])
			if err != nil {
				return fmt.Errorf("command not found: %s — check the name or your PATH", args[0])
			}
			argv := append([]string{exe}, args[1:]...)
			cols, rows := terminalSize()
			sid, err := wrap.Spawn(cmd.Context(), wrap.SpawnOptions{
				Name: name, TaskID: taskID, Argv: argv, Cols: cols, Rows: rows,
			})
			if err != nil {
				return err
			}
			if detach {
				fmt.Fprintln(cmd.OutOrStdout(), sid)
				fmt.Fprintf(cmd.ErrOrStderr(), "started %s in the background — attach: pilothouse attach %s\n", displayName(name, args[0]), sid)
				return nil
			}
			return attachTerminal(cmd.Context(), cmd.ErrOrStderr(), paths.HolderSocket(sid), replay)
		},
	}
	cmd.Flags().SetInterspersed(false) // everything after <cmd> belongs to the agent
	cmd.Flags().StringVarP(&name, "name", "n", "", "session name (default: current directory name)")
	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "start in the background and print the session id")
	cmd.Flags().StringVar(&taskID, "task", "", "link the session to a task id")
	cmd.Flags().BoolVar(&replay, "replay", true, "show output the agent printed before this terminal attached")
	return cmd
}

func displayName(name, cmd string) string {
	if name != "" {
		return name
	}
	return cmd
}

func newAttachCmd() *cobra.Command {
	var replay bool
	cmd := &cobra.Command{
		Use:   "attach [--replay] <session>",
		Short: "Attach this terminal to a session started with `pilothouse run`",
		Long: `Attach connects this terminal to a wrapped session, detaching any other
terminal attached to it. <session> is a session id or a unique prefix of
one (with or without "s_"), or a session name. Detach with ctrl+\.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sock, err := resolveHolder(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return attachTerminal(cmd.Context(), cmd.ErrOrStderr(), sock, replay)
		},
	}
	cmd.Flags().BoolVar(&replay, "replay", false, "replay recent output (up to 256 KB) before live output")
	return cmd
}

// attachTerminal attaches and turns the outcome into the process result:
// the agent's exit code, or a one-line detach message.
func attachTerminal(ctx context.Context, stderr io.Writer, sock string, replay bool) error {
	res, err := wrap.Attach(ctx, wrap.AttachOptions{Socket: sock, Replay: replay, Stderr: stderr})
	if err != nil {
		var he *wrap.HolderError
		if errors.As(err, &he) {
			return fmt.Errorf("could not attach: %s", he.Msg)
		}
		return fmt.Errorf("could not attach: %w", err)
	}
	switch {
	case res.Exited:
		if res.ExitCode != 0 {
			return ExitError{Code: res.ExitCode}
		}
		return nil
	case res.Detached:
		fmt.Fprintln(stderr, "\r\n"+wrap.DetachMessage(res))
		return nil
	default:
		return fmt.Errorf("lost the connection to %s (%s): %s — reattach: pilothouse attach %s",
			res.Name, res.SessionID, res.Reason, res.SessionID)
	}
}

// resolveHolder finds the holder socket for a session reference. The
// daemon is asked first (it knows every session, including exited ones);
// without it, live holders in the run directory are probed.
func resolveHolder(ctx context.Context, ref string) (string, error) {
	dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	s, err := client.New().Session(dctx, ref)
	cancel()
	if err == nil {
		if s.Mode != api.ModeWrapped {
			return "", fmt.Errorf("session %s was not started with `pilothouse run`, so it can't be attached — switch to the terminal it runs in", s.ID)
		}
		sock := s.HolderSock
		if sock == "" {
			sock = paths.HolderSocket(s.ID)
		}
		if _, statErr := os.Stat(sock); statErr != nil {
			if s.State == api.StateExited {
				return "", fmt.Errorf("session %s (%s) has exited", s.ID, s.Title)
			}
			return "", fmt.Errorf("session %s (%s) has no holder running (it may have crashed; see %s)", s.ID, s.Title, wrap.HolderLogPath(s.ID))
		}
		return sock, nil
	}
	holders, lerr := wrap.ListHolders(ctx)
	if lerr != nil {
		return "", lerr
	}
	var matches []wrap.HolderInfo
	for _, h := range holders {
		if h.SessionID == ref {
			return h.Socket, nil
		}
		if strings.HasPrefix(h.SessionID, ref) || strings.HasPrefix(h.SessionID, "s_"+ref) || h.Name == ref {
			matches = append(matches, h)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0].Socket, nil
	case 0:
		return "", fmt.Errorf("no running session matches %q — list sessions with `pilothouse ls`", ref)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%q matches %d sessions; use more of the id:", ref, len(matches))
	for _, m := range matches {
		fmt.Fprintf(&b, "\n  %s  %s", m.SessionID, m.Name)
	}
	return "", errors.New(b.String())
}

func terminalSize() (uint16, uint16) {
	for _, f := range []*os.File{os.Stdout, os.Stdin} {
		if c, r, err := term.GetSize(int(f.Fd())); err == nil && c > 0 && r > 0 {
			return uint16(min(c, 0xffff)), uint16(min(r, 0xffff))
		}
	}
	return 0, 0
}

func newHolderCmd() *cobra.Command {
	var (
		id, name, taskID string
		cols, rows       uint16
		linger           time.Duration
	)
	cmd := &cobra.Command{
		Use:    "__holder --id <sid> [--name n] [--task id] -- <cmd…>",
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !strings.HasPrefix(id, "s_") {
				return errors.New("__holder: --id must be a session id (s_…)")
			}
			if err := paths.Ensure(); err != nil {
				return err
			}
			logf, err := os.OpenFile(wrap.HolderLogPath(id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				return err
			}
			defer logf.Close()
			level := slog.LevelInfo
			if os.Getenv("PILOTHOUSE_DEBUG") != "" {
				level = slog.LevelDebug
			}
			log := slog.New(slog.NewTextHandler(logf, &slog.HandlerOptions{Level: level}))

			// The holder has no terminal; a stray SIGHUP (e.g. from the
			// session it was started from) must not kill the agent.
			signal.Ignore(syscall.SIGHUP)
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
			defer stop()
			code, err := wrap.RunHolder(ctx, wrap.HolderConfig{
				SessionID: id, Name: name, TaskID: taskID, Argv: args,
				Cols: cols, Rows: rows, Linger: linger, Log: log,
			})
			if err != nil {
				log.Error("holder failed", "err", err)
				return err
			}
			log.Info("holder done", "exit_code", code)
			if code != 0 {
				return ExitError{Code: code}
			}
			return nil
		},
	}
	cmd.Flags().SetInterspersed(false)
	cmd.Flags().StringVar(&id, "id", "", "session id")
	cmd.Flags().StringVar(&name, "name", "", "session name")
	cmd.Flags().StringVar(&taskID, "task", "", "task id")
	cmd.Flags().Uint16Var(&cols, "cols", 0, "initial columns")
	cmd.Flags().Uint16Var(&rows, "rows", 0, "initial rows")
	cmd.Flags().DurationVar(&linger, "linger", wrap.DefaultLinger, "how long to stay up after the agent exits")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

// ---- tasks ----

func newTaskCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "task",
		Short: "Run a prompt in its own git worktree and branch, then review and ship it",
		Long: `Tasks give each agent run its own git worktree (under ~/.pilothouse-worktrees)
and branch (pilothouse/<slug>), so several agents can work on one repo without
touching your checkout. Tasks are recorded in ~/.pilothouse/tasks.json and
mirrored to the daemon when it is running; the web app can start, review and
ship them too.

A repo's .pilothouse/task.yaml can copy untracked files (.env) into each new
worktree and run setup commands (npm ci) before the agent starts. Setup
commands and env only run in repos you trust: pilothouse policy trust.`,
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newTaskNewCmd(), newTaskLsCmd(), newTaskDiffCmd(), newTaskDoneCmd(), newTaskAbandonCmd())
	return cmd
}

func newTaskManager(cmd *cobra.Command) *task.Manager {
	return &task.Manager{Out: cmd.OutOrStdout(), Err: cmd.ErrOrStderr()}
}

func newTaskNewCmd() *cobra.Command {
	var (
		agent, base, name string
		attach            bool
	)
	cmd := &cobra.Command{
		Use:   `new [--agent claude|codex|opencode|grok] [--base ref] [--name slug] [-a] "<prompt>"`,
		Short: "Start an agent on a prompt in a new worktree",
		Example: `  pilothouse task new "add a --json flag to the export command"
  pilothouse task new --agent codex --base main -a "fix the flaky login test"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			cols, rows := terminalSize()
			m := newTaskManager(cmd)
			m.SetupOut = cmd.ErrOrStderr() // task.yaml's setup output, as it runs
			t, err := m.New(cmd.Context(), task.NewOptions{
				Prompt: args[0], Agent: agent, Base: base, Name: name, Dir: cwd, Cols: cols, Rows: rows,
				By: "cli",
			})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "task %s (%s) started\n  worktree: %s\n  branch:   %s (from %s)\n  session:  %s\n",
				t.ID, t.Title, t.Worktree, t.Branch, t.BaseRef, t.SessionID)
			if !attach {
				fmt.Fprintf(out, "attach: pilothouse attach %s · review: pilothouse task diff %s\n", t.SessionID, t.ID)
				return nil
			}
			return attachTerminal(cmd.Context(), cmd.ErrOrStderr(), paths.HolderSocket(t.SessionID), true)
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "claude", "agent to run: claude, codex, opencode, grok, or any command that takes the prompt as its argument")
	cmd.Flags().StringVar(&base, "base", "", "ref to branch from (default: the current branch)")
	cmd.Flags().StringVar(&name, "name", "", "slug for the branch and worktree (default: from the prompt)")
	cmd.Flags().BoolVarP(&attach, "attach", "a", false, "attach to the agent right away")
	return cmd
}

func newTaskLsCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List tasks",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			views, err := newTaskManager(cmd).List(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			shown := 0
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tTASK\tSTATUS\tBY\tSESSION\tSTATE\tAGE\tWORKTREE")
			for _, v := range views {
				if !all && (v.Status == api.TaskDone || v.Status == api.TaskAbandon) {
					continue
				}
				shown++
				state := string(v.SessionState)
				if state == "" {
					state = "-"
				}
				sess := v.SessionID
				if sess == "" {
					sess = "-"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", v.ID, v.Title, v.Status, startedBy(v.By), sess, state,
					age(v.CreatedAt), v.Worktree)
			}
			if shown == 0 {
				if all {
					fmt.Fprintln(out, `no tasks yet — start one with: pilothouse task new "<prompt>"`)
				} else {
					fmt.Fprintln(out, `no open tasks (pilothouse task ls --all shows finished ones) — start one with: pilothouse task new "<prompt>"`)
				}
				return nil
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "include done and abandoned tasks")
	return cmd
}

// startedBy is Task.By for `task ls`: cli, tui or device:<id>
// (`pilothouse devices` names it); "-" for tasks from before 0.2.
func startedBy(by string) string {
	if by == "" {
		return "-"
	}
	return by
}

func age(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func newTaskDiffCmd() *cobra.Command {
	var stat bool
	cmd := &cobra.Command{
		Use:   "diff <task> [--stat]",
		Short: "Show what a task changed relative to its base (committed and uncommitted)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return newTaskManager(cmd).Diff(cmd.Context(), args[0], stat, cmd.OutOrStdout())
		},
	}
	cmd.Flags().BoolVar(&stat, "stat", false, "show a diffstat instead of the patch")
	return cmd
}

func newTaskDoneCmd() *cobra.Command {
	var o task.DoneOptions
	cmd := &cobra.Command{
		Use:   "done <task> [--pr] [--keep]",
		Short: "Finish a task: optionally push and open a PR, then remove its worktree",
		Long: `Done marks a task finished. With --pr it pushes the task branch to origin and
runs ` + "`gh pr create --fill`" + `. The worktree is removed unless --keep; the
branch is always kept. Done refuses while the worktree has uncommitted
changes or the agent is still running, unless --force.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := newTaskManager(cmd).Done(cmd.Context(), args[0], o)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "task %s (%s) done; branch %s kept\n", t.ID, t.Title, t.Branch)
			return nil
		},
	}
	cmd.Flags().BoolVar(&o.PR, "pr", false, "push the branch and open a pull request with gh")
	cmd.Flags().BoolVar(&o.Keep, "keep", false, "keep the worktree")
	cmd.Flags().BoolVar(&o.Force, "force", false, "proceed even with uncommitted changes (they are discarded) or a running agent")
	return cmd
}

func newTaskAbandonCmd() *cobra.Command {
	var o task.AbandonOptions
	cmd := &cobra.Command{
		Use:   "abandon <task>",
		Short: "Throw a task away: remove its worktree and delete its branch",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := newTaskManager(cmd).Abandon(cmd.Context(), args[0], o)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "task %s (%s) abandoned\n", t.ID, t.Title)
			return nil
		},
	}
	cmd.Flags().BoolVar(&o.KeepBranch, "keep-branch", false, "keep the pilothouse/<slug> branch")
	cmd.Flags().BoolVar(&o.Force, "force", false, "abandon even while the agent is still running")
	return cmd
}
