// Package cli wires every pilothouse subcommand. Each workstream owns its own
// file (see docs/WORKSTREAMS.md); this file only assembles them.
package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/xasdta/pilothouse/internal/version"
)

// ExitError carries a specific exit code out of a command.
type ExitError struct{ Code int }

func (e ExitError) Error() string { return fmt.Sprintf("exit %d", e.Code) }

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "pilothouse",
		Short: "One inbox for every coding agent",
		Long: `Pilothouse watches Claude Code and Codex sessions on this machine, applies your
rules to every tool call, and sends what needs a person to wherever you are:
the terminal at your desk, or your phone when you're away.`,
		Version:       version.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE:          runDefault, // inbox.go
	}
	root.SetVersionTemplate("pilothouse {{.Version}}\n")
	root.AddGroup(
		&cobra.Group{ID: "start", Title: "Getting started:"},
		&cobra.Group{ID: "work", Title: "Everyday:"},
		&cobra.Group{ID: "run", Title: "Running agents:"},
		&cobra.Group{ID: "remote", Title: "Phone and remote:"},
		&cobra.Group{ID: "admin", Title: "Daemon and setup:"},
	)
	add := func(group string, cmds ...*cobra.Command) {
		for _, c := range cmds {
			if c.GroupID == "" && !c.Hidden {
				c.GroupID = group
			}
			root.AddCommand(c)
		}
	}
	// inbox.go (workstream F)
	add("start", newSetupCmd(), newDoctorCmd())
	add("start", newDemoCmd()) // demo.go (workstream F, 0.2)
	add("work", newInboxCmd(), newLsCmd(), newApproveCmd(), newDenyCmd(), newLogCmd(), newExplainCmd())
	add("work", newPresenceCmds()...)
	// answer.go (0.2 workstream A)
	add("work", newAnswerCmd())
	add("admin", newServiceCmd(), newNotifyCmd(), newVersionCmd())
	// daemon.go (workstream A)
	add("admin", newDaemonCmd(), newStartCmd(), newStopCmd(), newStatusCmd())
	// hooks.go (workstream B)
	add("admin", newHooksCmd(), newHookShimCmd())
	// policy.go (workstream C)
	add("work", newPolicyCmd())
	add("work", newUsageCmd(), newActivityCmd()) // usage.go (0.2 workstream C)
	// run.go (workstream D)
	add("run", newRunCmd(), newAttachCmd(), newHolderCmd(), newTaskCmd())
	// remote.go (workstream E)
	add("remote", newPairCmd(), newDevicesCmd(), newRemoteCmd(), newOpenCmd())
	// reply.go (0.2 workstream B)
	add("work", newReplyCmd())
	return root
}

// Main runs the CLI and returns the process exit code.
func Main(args []string) int {
	root := newRoot()
	root.SetArgs(args)
	err := root.Execute()
	if err == nil {
		return 0
	}
	var ee ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	fmt.Fprintln(os.Stderr, "pilothouse:", err)
	return 1
}
