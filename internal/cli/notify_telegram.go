package cli

// Workstream F: `pilothouse notify telegram` (set up the two-way Telegram
// bot) and `pilothouse notify ntfy` (subscribe the ntfy app).

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mdp/qrterminal/v3"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/xasdta/pilothouse/internal/adapters/all"
	"github.com/xasdta/pilothouse/internal/client"
	"github.com/xasdta/pilothouse/internal/config"
	"github.com/xasdta/pilothouse/internal/daemon"
	"github.com/xasdta/pilothouse/internal/notify/telegram"
	"github.com/xasdta/pilothouse/internal/version"
)

// Seams for tests.
var (
	// tgReadSecret reads the bot token without echo when stdin is a terminal.
	tgReadSecret = func() (string, error) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		return string(b), err
	}
	tgHTTP         *http.Client // nil: the telegram client's default
	tgNow          = time.Now
	tgPollInterval = 500 * time.Millisecond
	// notifySelfCheck refuses to change where notifications and decisions
	// go when run from inside an agent session: these commands write the
	// bot token, the pairing code and the ntfy topic (whose Allow buttons
	// are one-tap capability URLs) without going through the daemon's
	// agent-caller guard.
	notifySelfCheck = func() error { return daemon.CheckSelf(all.List()) }
)

// notifyGuard runs notifySelfCheck with a next step.
func notifyGuard(what string) error {
	if err := notifySelfCheck(); err != nil {
		return fmt.Errorf("%s can't be run from inside an agent session (an agent must not choose where approvals go): %v", what, err)
	}
	return nil
}

type tgOptions struct {
	noQR, noWait bool
	decisions    string // "", "on", "off"
}

func newNotifyTelegramCmd() *cobra.Command {
	var o tgOptions
	var notifyOnly bool
	cmd := &cobra.Command{
		Use:   "telegram",
		Short: "Answer requests from Telegram (buttons, replies; works without Tailscale)",
		Long: `Connects a Telegram bot you create: requests that need you while you're away
arrive with buttons (Allow / Allow for session / Deny, question options,
plan Approve / Request changes), replying to a "finished" message sends that
agent a follow-up, and messages are updated when a request is answered
anywhere else.

You need a bot token from @BotFather (send it /newbot). The token is read
from a prompt (or the first line of stdin), never from the command line,
and stored in ~/.pilothouse/keys/telegram.json (0600). Only the chat that
opens the one-time link answers requests; every other chat is ignored.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().Changed("notify-only") {
				o.decisions = map[bool]string{true: "off", false: "on"}[notifyOnly]
			}
			_, err := runNotifyTelegram(cmd.Context(), cmd.OutOrStdout(), bufio.NewReader(cmd.InOrStdin()), o)
			return err
		},
	}
	cmd.Flags().BoolVar(&o.noQR, "no-qr", false, "don't draw the QR code")
	cmd.Flags().BoolVar(&o.noWait, "no-wait", false, "exit after showing the link (the daemon finishes pairing)")
	cmd.Flags().BoolVar(&notifyOnly, "notify-only", false, "messages without buttons; nothing can be answered from Telegram")
	cmd.AddCommand(&cobra.Command{
		Use:   "off",
		Short: "Disconnect Telegram and delete the bot token",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runTelegramOff(cmd.OutOrStdout()) },
	})
	return cmd
}

// tgClient is a Bot API client for the configured server.
func tgClient(cfg *config.Config, token string) *telegram.Client {
	return &telegram.Client{Token: token, Base: cfg.Notify.Telegram.APIBase, HTTP: tgHTTP}
}

// readToken asks for the bot token: hidden input on a terminal, else the
// first line of in. An empty answer keeps the current token, if any.
func readToken(w io.Writer, in *bufio.Reader, current telegram.Keys) (string, error) {
	fmt.Fprintln(w, "1. In Telegram, open @BotFather, send /newbot and pick a name and a username.")
	fmt.Fprintln(w, "2. Paste the token it gives you (it looks like 123456789:AA…).")
	prompt := "   Bot token: "
	if current.Token != "" && current.BotUsername != "" {
		prompt = "   Bot token (Enter keeps @" + current.BotUsername + "): "
	}
	fmt.Fprint(w, prompt)
	var tok string
	if inboxStdinTerminal() {
		s, err := tgReadSecret()
		fmt.Fprintln(w)
		if err != nil {
			return "", fmt.Errorf("read the token: %w", err)
		}
		tok = s
	} else {
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			fmt.Fprintln(w)
			return "", errors.New("no bot token given (pipe it on stdin, or run this in a terminal)")
		}
		tok = line
	}
	tok = strings.TrimSpace(tok)
	if tok == "" && current.Token != "" {
		return current.Token, nil
	}
	if !telegram.ValidToken(tok) {
		return "", errors.New("that doesn't look like a bot token (digits, a colon, then letters — copy it from @BotFather)")
	}
	return tok, nil
}

// runNotifyTelegram sets up the bot and pairs a chat. It reports whether
// pairing finished.
func runNotifyTelegram(ctx context.Context, w io.Writer, in *bufio.Reader, o tgOptions) (bool, error) {
	if err := notifyGuard("`pilothouse notify telegram`"); err != nil {
		return false, err
	}
	cfg, err := config.Load()
	if err != nil {
		return false, err
	}
	path := telegram.KeysPath()
	current, err := telegram.LoadKeys(path)
	if err != nil {
		return false, err
	}
	tok, err := readToken(w, in, current)
	if err != nil {
		return false, err
	}
	c := tgClient(cfg, tok)
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	me, err := c.GetMe(cctx)
	if err == nil {
		// getUpdates fails while a webhook is set; Pilothouse polls.
		_ = c.DeleteWebhook(cctx)
	}
	cancel()
	switch {
	case telegram.IsCode(err, 401) || telegram.IsCode(err, 404):
		//lint:ignore ST1005 starts with a proper noun
		return false, errors.New("Telegram rejected that token — copy it again from @BotFather (/mybots → your bot → API Token)")
	case err != nil:
		return false, fmt.Errorf("couldn't reach Telegram to check the token: %v", err)
	case !me.IsBot || me.Username == "":
		return false, errors.New("that token doesn't belong to a bot")
	}

	// Config first, then the code: by the time the daemon sees a pending
	// code, config.yaml already says enabled with no chat, and the daemon's
	// own update (chat_id) comes after ours. Both are locked
	// read-modify-writes, so nothing anyone else saved meanwhile is lost.
	cfg, err = config.Update(nil, func(c *config.Config) error {
		c.Notify.Telegram.Enabled = true
		c.Notify.Telegram.ChatID = 0 // the chat that opens the new link takes over
		switch o.decisions {
		case "on":
			c.Notify.Telegram.Decisions = nil
		case "off":
			off := false
			c.Notify.Telegram.Decisions = &off
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	var code string
	k, err := telegram.UpdateKeys(path, func(k *telegram.Keys) error {
		*k = telegram.Keys{Token: tok, BotID: me.ID, BotUsername: me.Username}
		code = k.NewPairCode(tgNow())
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("save the token: %w", err)
	}
	fmt.Fprintf(w, "   ✓ @%s is ready (token saved to %s, mode 0600)\n\n", me.Username, path)

	link := telegram.StartLink(me.Username, code)
	fmt.Fprintln(w, "3. On your phone, open this link (or scan it) and tap Start:")
	if !o.noQR {
		tgQR(w, link)
	}
	fmt.Fprintf(w, "   %s\n", link)
	until := k.PairExpires.Local().Format("15:04")
	if !cfg.TelegramDecisions() {
		fmt.Fprintln(w, "   (notify only: Telegram won't be able to answer requests)")
	}

	hctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	h, herr := client.New().Health(hctx)
	cancel()
	daemonUp := herr == nil
	if o.noWait {
		if daemonUp {
			fmt.Fprintf(w, "\nThe link works once, until %s.\n", until)
		} else {
			fmt.Fprintf(w, "\nThe link works once, until %s, while the daemon runs (`pilothouse start`).\n", until)
		}
		return false, nil
	}
	if daemonUp && h.Version != version.Version {
		fmt.Fprintf(w, "   ! the running daemon is %s; if pairing doesn't finish, restart it with this binary (%s)\n", h.Version, version.Version)
	}
	fmt.Fprintln(w, "\nWaiting for you to tap Start… (Ctrl-C to stop waiting)")
	wctx, cancel := context.WithDeadline(ctx, k.PairExpires)
	defer cancel()
	if daemonUp {
		_, err = tgWaitForDaemon(wctx)
	} else {
		// No daemon: pair here (only one program may read the bot's updates).
		var ch telegram.Chat
		if ch, err = telegram.WaitForStart(wctx, c, k, tgNow); err == nil {
			err = tgSavePaired(ctx, c, ch, code)
		}
	}
	switch {
	case err == nil:
		fmt.Fprintln(w, "✓ Telegram connected. Send /status to the bot to try it.")
		if !daemonUp {
			fmt.Fprintln(w, "  Start the daemon (`pilothouse start`) so requests reach it.")
		}
		return true, nil
	case ctx.Err() != nil:
		fmt.Fprintf(w, "Stopped waiting. The link still works until %s while the daemon runs.\n", until)
		return false, nil
	case wctx.Err() != nil || !k.Pending(tgNow()):
		fmt.Fprintln(w, "The link expired. Run `pilothouse notify telegram` again (Enter keeps the same bot).")
		return false, nil
	}
	return false, err
}

// tgWaitForDaemon waits until the daemon records the paired chat.
func tgWaitForDaemon(ctx context.Context) (int64, error) {
	t := time.NewTicker(tgPollInterval)
	defer t.Stop()
	for {
		if c, err := config.Load(); err == nil && c.Notify.Telegram.ChatID != 0 {
			return c.Notify.Telegram.ChatID, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-t.C:
		}
	}
}

// tgSavePaired records a chat paired by the CLI (no daemon running): it
// spends the code (only if it is still the pending one), then saves the
// chat, like the daemon does.
func tgSavePaired(ctx context.Context, c *telegram.Client, ch telegram.Chat, code string) error {
	if err := telegram.SpendCode(telegram.KeysPath(), code, tgNow()); err != nil {
		return err
	}
	cfg, err := config.Update(nil, func(c *config.Config) error {
		c.Notify.Telegram.Enabled = true
		c.Notify.Telegram.ChatID = ch.ID
		return nil
	})
	if err != nil {
		return err
	}
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, _ = c.Send(sctx, telegram.SendMessage{ChatID: ch.ID, Text: "✅ <b>Connected to " + config.MachineName(cfg) +
		".</b>\nWhen you're away, requests that need you show up here. Send /status any time."})
	return nil
}

func runTelegramOff(w io.Writer) error {
	if err := notifyGuard("`pilothouse notify telegram off`"); err != nil {
		return err
	}
	if _, err := config.Update(nil, func(c *config.Config) error {
		c.Notify.Telegram.Enabled = false
		c.Notify.Telegram.ChatID = 0
		return nil
	}); err != nil {
		return err
	}
	path := telegram.KeysPath()
	if err := telegram.RemoveKeys(path); err != nil {
		return err
	}
	fmt.Fprintf(w, "✓ Telegram is off and the bot token was deleted (%s).\n", path)
	fmt.Fprintln(w, "  Delete the bot in @BotFather (/deletebot) if you don't need it any more.")
	return nil
}

func tgQR(w io.Writer, s string) {
	qrterminal.GenerateWithConfig(s, qrterminal.Config{
		Level:          qrterminal.L,
		Writer:         w,
		HalfBlocks:     true,
		BlackChar:      qrterminal.BLACK_BLACK,
		WhiteBlackChar: qrterminal.WHITE_BLACK,
		WhiteChar:      qrterminal.WHITE_WHITE,
		BlackWhiteChar: qrterminal.BLACK_WHITE,
		QuietZone:      2,
	})
}

// ---- doctor ----

// doctorKeepAwake is the hook for the keep-awake doctor line.
//
// TODO(workstream B): set this from cli/keepawake*.go (or wherever the
// keep-awake code lives) to report whether keep_awake is "working" and the
// helper exists (caffeinate on macOS, systemd-inhibit on Linux); return
// nil to print nothing. runDoctor calls it after the Telegram line.
var doctorKeepAwake func(ctx context.Context, cfg *config.Config) *doctorCheck

// doctorTelegram checks the Telegram setup; nil when it was never set up.
func doctorTelegram(ctx context.Context, cfg *config.Config) *doctorCheck {
	t := cfg.Notify.Telegram
	path := telegram.KeysPath()
	k, kerr := telegram.LoadKeys(path)
	check := func(l checkLevel, detail, fix string) *doctorCheck {
		return &doctorCheck{Name: "telegram", Detail: detail, Fix: fix, level: l}
	}
	switch {
	case !t.Enabled && k.Token == "" && kerr == nil:
		return nil
	case !t.Enabled:
		return check(checkInfo, "off (a bot token is saved)", "`pilothouse notify telegram` to turn it on, `pilothouse notify telegram off` to delete the token")
	case kerr != nil:
		return check(checkFail, kerr.Error(), "`pilothouse notify telegram` to set it up again")
	case k.Token == "":
		return check(checkFail, "on, but there is no bot token", "`pilothouse notify telegram`")
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
		return check(checkFail, fmt.Sprintf("%s is %#o; it holds the bot token", path, fi.Mode().Perm()), "chmod 600 "+path)
	}
	bot := "@" + inboxOr(k.BotUsername, "bot")
	if t.ChatID == 0 {
		if k.Pending(tgNow()) {
			return check(checkWarn, bot+": waiting for the pairing link to be opened", "")
		}
		return check(checkWarn, bot+": not paired with a chat", "`pilothouse notify telegram` (Enter keeps the same bot)")
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	_, err := tgClient(cfg, k.Token).GetMe(cctx)
	cancel()
	switch {
	case telegram.IsCode(err, 401) || telegram.IsCode(err, 404):
		return check(checkFail, bot+": Telegram rejects the token (revoked?)", "`pilothouse notify telegram` with a new token")
	case err != nil:
		return check(checkWarn, bot+": Telegram unreachable: "+err.Error(), "")
	}
	mode := "buttons and replies on"
	if !cfg.TelegramDecisions() {
		mode = "notify only"
	}
	return check(checkOK, bot+", paired, "+mode, "")
}

// ---- ntfy ----

func newNotifyNtfyCmd() *cobra.Command {
	var server string
	cmd := &cobra.Command{
		Use:   "ntfy [topic]",
		Short: "Get notifications in the ntfy app (Allow/Deny buttons with remote access)",
		Long: `Sends notifications to an ntfy topic (https://ntfy.sh). Without a topic a
random, unguessable one is made: anyone who knows a topic name on ntfy.sh
can read it, so don't pick a simple one. With remote access on
(` + "`pilothouse remote tailscale`" + `), request notifications get Allow / Deny buttons.
Notifications are cleared from the phone once a request is answered
(ntfy server 2.16 or newer).`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			topic := ""
			if len(args) == 1 {
				topic = args[0]
			}
			_, err := runNotifyNtfy(cmd.OutOrStdout(), server, topic, true)
			return err
		},
	}
	cmd.Flags().StringVar(&server, "server", "", "ntfy server (default https://ntfy.sh, or the configured one)")
	return cmd
}

// runNotifyNtfy saves the ntfy topic and shows how to subscribe. It
// returns the topic.
func runNotifyNtfy(w io.Writer, server, topic string, restartHint bool) (string, error) {
	if err := notifyGuard("`pilothouse notify ntfy`"); err != nil {
		return "", err
	}
	if topic != "" && !ntfyTopicRE(topic) {
		return "", errors.New("an ntfy topic is 1-64 letters, digits, - and _")
	}
	cfg, err := config.Update(nil, func(c *config.Config) error {
		if server != "" {
			c.Notify.Ntfy.Server = strings.TrimRight(server, "/")
		}
		switch {
		case topic != "":
			c.Notify.Ntfy.Topic = topic
		case c.Notify.Ntfy.Topic == "":
			b := make([]byte, 10)
			if _, err := rand.Read(b); err != nil {
				return err
			}
			c.Notify.Ntfy.Topic = "pilothouse-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	topic = cfg.Notify.Ntfy.Topic
	srv := cfg.Notify.Ntfy.Server
	if srv == "" {
		srv = "https://ntfy.sh"
	}
	link := strings.TrimRight(srv, "/") + "/" + topic
	fmt.Fprintf(w, "✓ ntfy topic %s on %s\n", topic, srv)
	fmt.Fprintln(w, "  In the ntfy app (iPhone or Android): + → Subscribe to topic → enter the topic above")
	if srv != "https://ntfy.sh" {
		fmt.Fprintln(w, "  (use another server: "+srv+")")
	}
	fmt.Fprintln(w, "  or open this on your phone:")
	tgQR(w, link)
	fmt.Fprintf(w, "  %s\n", link)
	if cfg.Web.PublicURL == "" {
		fmt.Fprintln(w, "  Allow / Deny buttons need remote access: `pilothouse remote tailscale`.")
	}
	if restartHint {
		fmt.Fprintln(w, "  The daemon reads this at start: `pilothouse service restart` (or `pilothouse stop && pilothouse start`), then `pilothouse notify test`.")
	}
	return topic, nil
}

func ntfyTopicRE(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}
