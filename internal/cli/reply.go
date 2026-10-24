package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/client"
)

// 0.2 workstream B: `pilothouse reply`.

// maxCLIReply matches the daemon's limit on a follow-up prompt.
const maxCLIReply = 16 << 10

func newReplyCmd() *cobra.Command {
	var interrupt bool
	cmd := &cobra.Command{
		Use:   `reply <session> ["text" | -] [--interrupt]`,
		Short: "Send a follow-up prompt (or an interrupt) to a running agent",
		Long: `Reply sends text to an agent session as its next prompt. Sessions started with
` + "`pilothouse run`" + ` get it typed into their terminal (multi-line text arrives as one
paste); Claude Code sessions you started yourself get it through Claude's own
messaging socket, as a message from Pilothouse. Codex sessions need
` + "`pilothouse run codex`" + `.

--interrupt presses Esc first, so the agent stops what it is doing (sessions
started with ` + "`pilothouse run`" + ` only); without text it only interrupts. Use - to
read the text from stdin. <session> is a session id or a unique prefix.`,
		Example: `  pilothouse reply s_k3m "also update the changelog"
  pilothouse reply s_k3m --interrupt "stop, use the staging database instead"
  git diff | pilothouse reply s_k3m -`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var text string
			if len(args) == 2 {
				text = args[1]
			}
			if text == "-" {
				b, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), maxCLIReply+1))
				if err != nil {
					return err
				}
				if len(b) > maxCLIReply {
					return fmt.Errorf("the text is longer than %d KB", maxCLIReply>>10)
				}
				text = string(b)
			}
			if strings.TrimSpace(text) == "" && !interrupt {
				return errors.New("nothing to send: give the text, or --interrupt")
			}
			return runReply(cmd, inboxClient(), args[0], api.Reply{Text: text, Interrupt: interrupt})
		},
	}
	cmd.Flags().BoolVarP(&interrupt, "interrupt", "i", false, "press Esc first so the agent stops what it is doing")
	return cmd
}

func runReply(cmd *cobra.Command, c *client.Client, ref string, r api.Reply) error {
	ctx := cmd.Context()
	s, err := c.Session(ctx, ref)
	if err != nil {
		var ae *client.APIError
		if errors.As(err, &ae) && ae.Status == 404 {
			return fmt.Errorf("no session matches %q (see `pilothouse ls`)", ref)
		}
		return err
	}
	res, err := c.Reply(ctx, s.ID, r)
	if err != nil {
		return err
	}
	what := "Sent to"
	if r.Interrupt && strings.TrimSpace(r.Text) == "" {
		what = "Interrupted"
	}
	how, ok := map[api.ReplyVia]string{
		api.ReplyTerminal: "typed into its terminal",
		api.ReplyMessage:  "through Claude's messaging socket",
		api.ReplyPlugin:   "through its Pilothouse plugin",
	}[res.Via]
	if !ok {
		how = "via " + string(res.Via)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s %s (%s), %s.\n", what, s.Title, s.ID, how)
	if res.Note != "" {
		fmt.Fprintln(cmd.OutOrStdout(), res.Note)
	}
	return nil
}
