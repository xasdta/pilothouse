package cli

// Tests for workstream F's 0.2 commands: notify telegram / ntfy, the
// setup flow (diff, Tailscale, pairing, phone channels), Telegram doctor
// lines and the demo. Helpers are prefixed "f2".

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/config"
	"github.com/xasdta/pilothouse/internal/notify/telegram"
)

const f2Token = "555666777:AAcliTESTtokenCliTESTtokenCliTEST"

// f2Bot is a minimal Bot API.
type f2Bot struct {
	*httptest.Server
	mu      sync.Mutex
	updates []telegram.Update
	sent    []string
	calls   []string
	reject  bool // getMe answers 401
}

func newF2Bot(t *testing.T) *f2Bot {
	b := &f2Bot{}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, method, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/bot"), "/")
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		b.mu.Lock()
		b.calls = append(b.calls, method)
		reject := b.reject
		b.mu.Unlock()
		if tok != f2Token || reject {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":401,"description":"Unauthorized"}`))
			return
		}
		ok := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": v}) }
		switch method {
		case "getMe":
			ok(telegram.User{ID: 555666777, IsBot: true, FirstName: "Pilot", Username: "cli_test_bot"})
		case "getUpdates":
			var p struct {
				Offset int64 `json:"offset"`
			}
			_ = json.Unmarshal(body, &p)
			for i := 0; i < 20; i++ {
				b.mu.Lock()
				var out []telegram.Update
				for _, u := range b.updates {
					if u.UpdateID >= p.Offset {
						out = append(out, u)
					}
				}
				b.mu.Unlock()
				if len(out) > 0 {
					ok(out)
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
			ok([]telegram.Update{})
		case "sendMessage":
			var s struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(body, &s)
			b.mu.Lock()
			b.sent = append(b.sent, s.Text)
			b.mu.Unlock()
			ok(telegram.Message{MessageID: 9, Chat: telegram.Chat{ID: 4242}})
		default:
			ok(true)
		}
	}))
	t.Cleanup(b.Close)
	return b
}

func (b *f2Bot) start(code string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.updates = append(b.updates, telegram.Update{UpdateID: int64(len(b.updates) + 1), Message: &telegram.Message{MessageID: 1,
		From: &telegram.User{ID: 4242}, Chat: telegram.Chat{ID: 4242, Type: "private"}, Date: time.Now().Unix(), Text: "/start " + code}})
}

func f2Config(t *testing.T, mut func(*config.Config)) {
	t.Helper()
	cfg := config.Default()
	cfg.Notify.Desktop = false
	mut(cfg)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
}

// f2Async runs the CLI in the background with a shared output buffer.
type f2Async struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	done chan error
}

func f2Start(ctx context.Context, stdin string, args ...string) *f2Async {
	a := &f2Async{done: make(chan error, 1)}
	root := newRoot()
	w := ibxLockedWriter{mu: &a.mu, b: &a.buf}
	root.SetArgs(args)
	root.SetOut(w)
	root.SetErr(w)
	root.SetIn(strings.NewReader(stdin))
	go func() { a.done <- root.ExecuteContext(ctx) }()
	return a
}

func (a *f2Async) out() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.buf.String()
}

// waitOut waits until the output matches re and returns the first group.
func (a *f2Async) waitOut(t *testing.T, re *regexp.Regexp) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m := re.FindStringSubmatch(a.out()); m != nil {
			if len(m) > 1 {
				return m[1]
			}
			return m[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("output never matched %s:\n%s", re, a.out())
	return ""
}

func (a *f2Async) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-a.done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("command did not finish:\n%s", a.out())
		return nil
	}
}

var startRE = regexp.MustCompile(`\?start=([A-Za-z0-9_-]+)`)

// Without a daemon the CLI pairs itself: the token comes from stdin, is
// checked with getMe and stored 0600; the chat that opens the link is saved.
func TestNotifyTelegramPairsWithoutDaemon(t *testing.T) {
	ibxEnv(t)
	bot := newF2Bot(t)
	f2Config(t, func(c *config.Config) { c.Notify.Telegram.APIBase = bot.URL })

	a := f2Start(context.Background(), f2Token+"\n", "notify", "telegram", "--no-qr")
	code := a.waitOut(t, startRE)
	bot.start("wrong-code-wrong-code-1")
	bot.start(code)
	if err := a.wait(t); err != nil {
		t.Fatalf("%v\n%s", err, a.out())
	}
	out := a.out()
	ibxContains(t, out, "@cli_test_bot is ready", "https://t.me/cli_test_bot?start="+code, "✓ Telegram connected", "pilothouse start")
	if strings.Contains(out, f2Token) {
		t.Fatal("the token was echoed")
	}
	cfg, _ := config.Load()
	if !cfg.Notify.Telegram.Enabled || cfg.Notify.Telegram.ChatID != 4242 || !cfg.TelegramDecisions() {
		t.Fatalf("config = %+v", cfg.Notify.Telegram)
	}
	k, err := telegram.LoadKeys(telegram.KeysPath())
	if err != nil || k.Token != f2Token || k.PairHash != "" || k.BotUsername != "cli_test_bot" {
		t.Fatalf("keys = %+v %v", k.BotUsername, err)
	}
	if fi, _ := os.Stat(telegram.KeysPath()); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v", fi.Mode())
	}
	raw, _ := os.ReadFile(paths02Config())
	if strings.Contains(string(raw), f2Token) {
		t.Fatal("token written to config.yaml")
	}
	bot.mu.Lock()
	welcome := strings.Join(bot.sent, "\n")
	bot.mu.Unlock()
	if !strings.Contains(welcome, "Connected to") {
		t.Fatalf("no welcome: %q", welcome)
	}

	// Doctor reports it, and notification channels count it.
	out, _ = ibxRun(t, "", "doctor")
	ibxContains(t, out, "✓ telegram", "@cli_test_bot, paired, buttons and replies on")
	if ch := strings.Join(notifyChannels(context.Background()), ","); !strings.Contains(ch, "Telegram @cli_test_bot") {
		t.Fatalf("channels = %s", ch)
	}

	// Off deletes the token.
	out, err = ibxRun(t, "", "notify", "telegram", "off")
	if err != nil {
		t.Fatal(err)
	}
	ibxContains(t, out, "Telegram is off", "/deletebot")
	if _, err := os.Stat(telegram.KeysPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("token file left behind")
	}
	if cfg, _ := config.Load(); cfg.Notify.Telegram.Enabled || cfg.Notify.Telegram.ChatID != 0 {
		t.Fatal("still enabled")
	}
}

func paths02Config() string { return os.Getenv("PILOTHOUSE_HOME") + "/config.yaml" }

// With a daemon running, the daemon pairs; the CLI waits for chat_id.
func TestNotifyTelegramWaitsForDaemon(t *testing.T) {
	ibxStart(t)
	bot := newF2Bot(t)
	f2Config(t, func(c *config.Config) { c.Notify.Telegram.APIBase = bot.URL })
	saved := tgPollInterval
	tgPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { tgPollInterval = saved })

	a := f2Start(context.Background(), f2Token+"\n", "notify", "telegram", "--no-qr", "--notify-only")
	a.waitOut(t, regexp.MustCompile(`Waiting for you to tap Start`))
	k, _ := telegram.LoadKeys(telegram.KeysPath())
	if k.PairHash == "" {
		t.Fatal("no pending code for the daemon")
	}
	cfg, _ := config.Load()
	if cfg.TelegramDecisions() || !cfg.Notify.Telegram.Enabled {
		t.Fatalf("--notify-only not saved: %+v", cfg.Notify.Telegram)
	}
	f2DaemonPairs(t, 4242) // the daemon pairs (simulated)
	if err := a.wait(t); err != nil {
		t.Fatal(err)
	}
	ibxContains(t, a.out(), "notify only", "✓ Telegram connected")
	bot.mu.Lock()
	polled := strings.Contains(strings.Join(bot.calls, ","), "getUpdates")
	bot.mu.Unlock()
	if polled {
		t.Fatal("the CLI read updates while a daemon runs (only one program may)")
	}

	// --no-wait returns at once.
	out, err := ibxRun(t, "\n", "notify", "telegram", "--no-qr", "--no-wait") // Enter keeps the bot
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	ibxContains(t, out, "Enter keeps @cli_test_bot", "The link works once")
}

func TestNotifyTelegramRefusesBadTokens(t *testing.T) {
	ibxEnv(t)
	bot := newF2Bot(t)
	f2Config(t, func(c *config.Config) { c.Notify.Telegram.APIBase = bot.URL })
	if _, err := ibxRun(t, "not a token\n", "notify", "telegram"); err == nil || !strings.Contains(err.Error(), "doesn't look like a bot token") {
		t.Fatalf("shape: %v", err)
	}
	if _, err := ibxRun(t, "", "notify", "telegram"); err == nil || !strings.Contains(err.Error(), "no bot token") {
		t.Fatalf("empty stdin: %v", err)
	}
	bot.reject = true
	if _, err := ibxRun(t, f2Token+"\n", "notify", "telegram"); err == nil || !strings.Contains(err.Error(), "Telegram rejected that token") {
		t.Fatalf("401: %v", err)
	}
	// The token is never a command-line argument.
	if _, err := ibxRun(t, "", "notify", "telegram", "--token", f2Token); err == nil || !strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("--token accepted: %v", err)
	}
	if _, err := ibxRun(t, "", "notify", "telegram", f2Token); err == nil {
		t.Fatal("token accepted as an argument")
	}
	if _, err := os.Stat(telegram.KeysPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a rejected token was saved")
	}
	// On a terminal the token is read without echo.
	inboxStdinTerminal = func() bool { return true }
	tgReadSecret = func() (string, error) { return f2Token, nil }
	bot.reject = false
	out, err := ibxRun(t, "", "notify", "telegram", "--no-qr", "--no-wait")
	if err != nil || strings.Contains(out, f2Token) {
		t.Fatalf("terminal: %v\n%s", err, out)
	}
}

func TestDoctorTelegramProblems(t *testing.T) {
	ibxStart(t)
	bot := newF2Bot(t)
	f2Config(t, func(c *config.Config) {
		c.Notify.Telegram.Enabled, c.Notify.Telegram.APIBase = true, bot.URL
	})
	out, _ := ibxRun(t, "", "doctor")
	ibxContains(t, out, "✗ telegram", "no bot token")

	k := telegram.Keys{Token: f2Token, BotUsername: "cli_test_bot"}
	if err := telegram.SaveKeys(telegram.KeysPath(), k); err != nil {
		t.Fatal(err)
	}
	out, _ = ibxRun(t, "", "doctor")
	ibxContains(t, out, "! telegram", "not paired")

	f2Config(t, func(c *config.Config) {
		c.Notify.Telegram.Enabled, c.Notify.Telegram.APIBase, c.Notify.Telegram.ChatID = true, bot.URL, 4242
	})
	bot.reject = true
	out, _ = ibxRun(t, "", "doctor")
	ibxContains(t, out, "✗ telegram", "rejects the token")
	bot.reject = false
	if err := os.Chmod(telegram.KeysPath(), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ = ibxRun(t, "", "doctor")
	ibxContains(t, out, "✗ telegram", "holds the bot token", "chmod 600")

	// Never set up: no line at all.
	_ = os.Remove(telegram.KeysPath())
	f2Config(t, func(*config.Config) {})
	if out, _ = ibxRun(t, "", "doctor"); strings.Contains(out, "telegram") {
		t.Fatalf("telegram line without Telegram:\n%s", out)
	}

	// The keep-awake hook is called when set (workstream B fills it).
	doctorKeepAwake = func(context.Context, *config.Config) *doctorCheck {
		return &doctorCheck{Name: "keep-awake", Detail: "caffeinate while agents work", level: checkOK}
	}
	t.Cleanup(func() { doctorKeepAwake = nil })
	out, _ = ibxRun(t, "", "doctor")
	ibxContains(t, out, "✓ keep-awake", "caffeinate while agents work")
}

func TestNotifyNtfy(t *testing.T) {
	ibxEnv(t)
	out, err := ibxRun(t, "", "notify", "ntfy")
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load()
	topic := cfg.Notify.Ntfy.Topic
	if !regexp.MustCompile(`^pilothouse-[a-z2-7]{16}$`).MatchString(topic) {
		t.Fatalf("topic %q is not random enough", topic)
	}
	ibxContains(t, out, "✓ ntfy topic "+topic, "https://ntfy.sh/"+topic, "service restart", "pilothouse remote tailscale")
	// Running it again keeps the topic.
	_, _ = ibxRun(t, "", "notify", "ntfy")
	if c, _ := config.Load(); c.Notify.Ntfy.Topic != topic {
		t.Fatal("topic changed")
	}
	out, err = ibxRun(t, "", "notify", "ntfy", "team_alerts", "--server", "https://ntfy.example.com/")
	if err != nil {
		t.Fatal(err)
	}
	ibxContains(t, out, "https://ntfy.example.com/team_alerts")
	if _, err := ibxRun(t, "", "notify", "ntfy", "bad topic!"); err == nil {
		t.Fatal("bad topic accepted")
	}
}

// Security review 0.2: an agent must not be able to point notifications
// (and their one-tap Allow URLs) or Telegram decisions at itself.
func TestNotifyGuard(t *testing.T) {
	ibxEnv(t)
	notifySelfCheck = func() error { return errors.New("refused: run from inside claude") }
	t.Cleanup(func() { notifySelfCheck = func() error { return nil } })
	for _, args := range [][]string{{"notify", "ntfy", "attacker_topic"}, {"notify", "telegram"}, {"notify", "telegram", "off"}} {
		_, err := ibxRun(t, f2Token+"\n", args...)
		if err == nil || !strings.Contains(err.Error(), "inside an agent session") {
			t.Errorf("%v: err = %v, want the agent-session refusal", args, err)
		}
	}
	if cfg, _ := config.Load(); cfg.Notify.Ntfy.Topic != "" || cfg.Notify.Telegram.Enabled {
		t.Fatalf("config changed: %+v", cfg.Notify)
	}
	if _, err := os.Stat(telegram.KeysPath()); !os.IsNotExist(err) {
		t.Fatalf("telegram keys written: %v", err)
	}
}

// diffAdapter is an ibxAdapter whose dry run returns the file contents.
type diffAdapter struct {
	*ibxAdapter
	after string
}

func (a *diffAdapter) InstallHooks(o adapters.InstallOptions) (adapters.Change, error) {
	ch, err := a.ibxAdapter.InstallHooks(o)
	ch.Before = "{\n  \"theme\": \"dark\"\n}\n"
	ch.After = a.after
	return ch, err
}

// Setup shows the diff of each settings file before asking.
func TestSetupShowsDiffBeforeAsking(t *testing.T) {
	ibxStart(t)
	short := &diffAdapter{ibxAdapter: &ibxAdapter{kind: api.AgentClaude, name: "Claude Code", installed: true},
		after: "{\n  \"theme\": \"dark\",\n  \"hooks\": {\"Stop\": \"pilothouse hook claude\"}\n}\n"}
	inboxAdapters = func() []adapters.Adapter { return []adapters.Adapter{short} }
	out, err := ibxRun(t, "", "setup", "--yes")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	ibxContains(t, out, "--- ~/.claude/settings.json", `-  "theme": "dark"`, `+  "hooks": {"Stop": "pilothouse hook claude"}`,
		"Phone", "skipped with --yes", "Checklist", "✓ test notification sent", "pilothouse demo")
	if strings.Index(out, `+  "hooks"`) > strings.Index(out, "? Install Pilothouse hooks") {
		t.Fatalf("diff shown after the question:\n%s", out)
	}

	long := &diffAdapter{ibxAdapter: &ibxAdapter{kind: api.AgentClaude, name: "Claude Code", installed: true},
		after: strings.Repeat("\"line\",\n", 100)}
	inboxAdapters = func() []adapters.Adapter { return []adapters.Adapter{long} }
	out, _ = ibxRun(t, "", "setup", "--yes")
	ibxContains(t, out, "more line(s): `pilothouse hooks install --dry-run` shows everything")
}

// Interactive setup: Tailscale is ready, so it offers remote access and
// pairing (QR inline), then Telegram and ntfy, then tests notifications.
func TestSetupPhoneFlow(t *testing.T) {
	d := ibxStart(t)
	inboxStdinTerminal = func() bool { return true }
	f := &rmtFakeTS{installed: true, status: rmtGoodStatus}
	rmtTailscale = f.exec()
	d.pairAdds = &api.Device{ID: "d_new", Name: "iPhone"}

	// Answers: serve on tailnet (y), pair (y), Enter after scanning,
	// Telegram (no), ntfy (y).
	out, err := ibxRun(t, "y\ny\n\n\ny\n", "setup")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	ibxContains(t, out,
		"✓ Tailscale connected (mac.tail1.ts.net), HTTPS certificates on",
		"? Serve the web app on your tailnet at https://mac.tail1.ts.net",
		"✓ The web app is on your tailnet",
		"? Pair your phone now", "Scan with your phone's camera", "https://mac.tail1.ts.net/pair#K7QF-M2XA-P9TT",
		`✓ Paired "iPhone"`, "? Answer requests from Telegram too", "? Send notifications to the ntfy app? [y/N]",
		"✓ ntfy topic pilothouse-", "restart the daemon to use ntfy",
		"Checklist", "✓ remote access over Tailscale", "✓ paired iPhone", "✓ ntfy topic")
	if strings.Contains(out, "pilothouse pair\tpair") || strings.Contains(out, "  pilothouse pair   ") {
		t.Fatalf("suggests pairing after pairing:\n%s", out)
	}
	cfg, _ := config.Load()
	if cfg.Web.PublicURL != "https://mac.tail1.ts.net" || cfg.Notify.Ntfy.Topic == "" {
		t.Fatalf("config = %+v %+v", cfg.Web, cfg.Notify.Ntfy)
	}
	if strings.Join(f.calls[len(f.calls)-1][1:], " ") != "serve --bg --https=443 http://127.0.0.1:7517" {
		t.Fatalf("tailscale calls = %q", f.calls)
	}

	// Run again: everything is already there; nothing is asked twice.
	out, err = ibxRun(t, "\n", "setup")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	ibxContains(t, out, "✓ remote access: https://mac.tail1.ts.net", "✓ 1 paired device(s)", "? Pair another phone or browser? [y/N]", "✓ ntfy topic")
	if strings.Contains(out, "Serve the web app") || strings.Contains(out, "Send notifications to the ntfy app") {
		t.Fatalf("asked again:\n%s", out)
	}
}

func TestSetupWithoutTailscale(t *testing.T) {
	d := ibxStart(t)
	inboxStdinTerminal = func() bool { return true }
	out, err := ibxRun(t, "n\nn\n", "setup")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	ibxContains(t, out, "Tailscale isn't installed", "Telegram and ntfy work without it", "pilothouse pair")
	if strings.Contains(out, "Pair your phone now") || d.pairs != 0 {
		t.Fatal("offered pairing without a way for the phone to reach this computer")
	}
	// --no-phone skips the whole step.
	out, _ = ibxRun(t, "", "setup", "--no-phone")
	ibxContains(t, out, "· skipped (--no-phone)")
	for _, st := range []string{`{"BackendState":"NeedsLogin"}`, `{"BackendState":"Running","Self":{"DNSName":"mac.tail1.ts.net."},"CurrentTailnet":{"MagicDNSEnabled":true}}`} {
		rmtTailscale = (&rmtFakeTS{installed: true, status: st}).exec()
		out, _ = ibxRun(t, "n\nn\n", "setup")
		if !strings.Contains(out, "tailscale up") && !strings.Contains(out, "HTTPS certificates are off") {
			t.Fatalf("status %s:\n%s", st, out)
		}
	}
}

// Setup can set up Telegram inline; the (simulated) daemon pairs.
func TestSetupTelegramInline(t *testing.T) {
	ibxStart(t)
	bot := newF2Bot(t)
	f2Config(t, func(c *config.Config) { c.Notify.Telegram.APIBase = bot.URL })
	inboxStdinTerminal = func() bool { return true }
	tgReadSecret = func() (string, error) { return f2Token, nil }
	saved := tgPollInterval
	tgPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { tgPollInterval = saved })
	// The daemon (simulated): pairs as soon as a code is pending, however
	// the scheduler interleaves it with setup.
	stop, paired := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(paired)
		for {
			if k, _ := telegram.LoadKeys(telegram.KeysPath()); k.PairHash != "" {
				f2DaemonPairs(t, 4242)
				return
			}
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	out, err := ibxRun(t, "y\nn\n", "setup")
	close(stop)
	<-paired
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	ibxContains(t, out, "@BotFather", "@cli_test_bot is ready", "✓ Telegram connected", "test notification sent", "Telegram @cli_test_bot")
	if cfg, _ := config.Load(); !cfg.Notify.Telegram.Enabled || cfg.Notify.Telegram.ChatID != 4242 || cfg.Notify.Telegram.APIBase != bot.URL {
		t.Fatalf("telegram config after setup = %+v", cfg.Notify.Telegram)
	}
}

// f2DaemonPairs does what the daemon does when a chat opens the link: it
// spends the pending code and records chat_id, each as a locked
// read-modify-write (daemon/telegram.go paired).
func f2DaemonPairs(t *testing.T, chat int64) {
	t.Helper()
	if _, err := telegram.UpdateKeys(telegram.KeysPath(), func(k *telegram.Keys) error {
		k.PairHash, k.PairExpires = "", time.Time{}
		return nil
	}); err != nil {
		t.Error(err)
	}
	if _, err := config.Update(nil, func(c *config.Config) error {
		c.Notify.Telegram.Enabled, c.Notify.Telegram.ChatID = true, chat
		return nil
	}); err != nil {
		t.Error(err)
	}
}

// A config change saved while a command holds an older copy (here the
// daemon pairing Telegram while `tailscale serve` runs) survives that
// command's own save.
func TestRemoteTailscaleKeepsConcurrentChanges(t *testing.T) {
	ibxEnv(t)
	f2Config(t, func(c *config.Config) { c.Notify.Telegram.Enabled = true })
	f := &rmtFakeTS{installed: true, status: rmtGoodStatus}
	ts := f.exec()
	serve := ts.interactive
	ts.interactive = func(ctx context.Context, name string, args ...string) error {
		f2DaemonPairs(t, 4242) // a chat pairs while serve runs
		return serve(ctx, name, args...)
	}
	rmtTailscale = ts
	if out, err := ibxRun(t, "", "remote", "tailscale"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	cfg, _ := config.Load()
	if cfg.Web.PublicURL != "https://mac.tail1.ts.net" || cfg.Notify.Telegram.ChatID != 4242 {
		t.Fatalf("lost update: public_url=%q chat_id=%d", cfg.Web.PublicURL, cfg.Notify.Telegram.ChatID)
	}
	ts.interactive = func(context.Context, string, ...string) error {
		_, err := config.Update(nil, func(c *config.Config) error { c.Machine = "studio"; return nil })
		return err
	}
	rmtTailscale = ts
	if _, err := ibxRun(t, "", "remote", "off"); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := config.Load(); cfg.Web.PublicURL != "" || cfg.Machine != "studio" || cfg.Notify.Telegram.ChatID != 4242 {
		t.Fatalf("remote off lost an update: %+v %q %d", cfg.Web, cfg.Machine, cfg.Notify.Telegram.ChatID)
	}
}

// The demo serves the real web app from a sandbox and cleans up on Ctrl-C.
func TestDemoSmoke(t *testing.T) {
	ibxEnv(t)
	home := os.Getenv("PILOTHOUSE_HOME")
	t.Setenv("TMPDIR", t.TempDir())
	var opened string
	rmtOpenBrowser = func(u string) error { opened = u; return nil }
	ready := make(chan string, 1)
	saved := demoReady
	demoReady = func(u string) { ready <- u }
	t.Cleanup(func() { demoReady = saved })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := f2Start(ctx, "", "demo")
	var base string
	select {
	case base = <-ready:
	case err := <-a.done:
		t.Fatalf("demo exited: %v\n%s", err, a.out())
	case <-time.After(10 * time.Second):
		t.Fatalf("demo didn't start:\n%s", a.out())
	}
	login := a.waitOut(t, regexp.MustCompile(`On this computer: (\S+)`))
	sandbox := a.waitOut(t, regexp.MustCompile(`sandbox: (\S+)`))
	if opened != login || !strings.HasPrefix(login, base+"/login") {
		t.Fatalf("opened %q, login %q, base %q", opened, login, base)
	}
	for _, p := range []string{"/", "/manifest.webmanifest"} {
		resp, err := http.Get(base + p)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("GET %s: %v %v", p, err, resp)
		}
		resp.Body.Close()
	}
	// Signing in with the login link works (a real web.Server).
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(login)
	if err != nil || resp.StatusCode >= 400 {
		t.Fatalf("login link: %v %v", err, resp)
	}
	resp.Body.Close()
	if !strings.Contains(a.out(), "needs Tailscale") {
		t.Fatalf("no phone hint without Tailscale:\n%s", a.out())
	}
	if _, err := os.Stat(sandbox); err != nil {
		t.Fatalf("sandbox missing while running: %v", err)
	}

	cancel()
	if err := a.wait(t); err != nil {
		t.Fatalf("demo: %v", err)
	}
	if _, err := os.Stat(sandbox); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sandbox not deleted: %v", err)
	}
	if os.Getenv("PILOTHOUSE_HOME") != home {
		t.Fatalf("PILOTHOUSE_HOME not restored: %q", os.Getenv("PILOTHOUSE_HOME"))
	}
	if _, err := os.Stat(home + "/pilothouse.db"); err == nil {
		t.Fatal("the demo wrote to the configured home")
	}
	ibxContains(t, a.out(), "Your daemon, hooks and data are not touched", "Demo stopped; sandbox deleted.")
}

// With Tailscale ready, the demo serves itself on the tailnet (foreground
// serve, stopped on exit) and prints the phone QR; a taken port is left alone.
func TestDemoTailscale(t *testing.T) {
	ibxEnv(t)
	t.Setenv("TMPDIR", t.TempDir())
	f := &rmtFakeTS{installed: true, status: rmtGoodStatus}
	rmtTailscale = f.exec()
	var served []string
	stopped := false
	savedServe := demoServe
	demoServe = func(_ context.Context, bin string, args ...string) (func(), error) {
		served = append([]string{bin}, args...)
		return func() { stopped = true }, nil
	}
	ready := make(chan string, 1)
	savedReady := demoReady
	demoReady = func(u string) { ready <- u }
	t.Cleanup(func() { demoServe, demoReady = savedServe, savedReady })

	ctx, cancel := context.WithCancel(context.Background())
	a := f2Start(ctx, "", "demo", "--no-open")
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatalf("demo didn't start:\n%s", a.out())
	}
	port := regexp.MustCompile(`:(\d+)$`)
	if len(served) != 3 || served[1] != "--https=8443" || !port.MatchString(served[2]) {
		t.Fatalf("serve = %q", served)
	}
	ibxContains(t, a.out(), "On your phone: scan this", "https://mac.tail1.ts.net:8443/pair")
	cancel()
	_ = a.wait(t)
	if !stopped {
		t.Fatal("tailscale serve left running")
	}

	if !demoPortTaken([]byte(`{"TCP":{"8443":{"HTTPS":true}}}`), 8443) || demoPortTaken([]byte(`{"TCP":{"443":{}}}`), 8443) ||
		!demoPortTaken([]byte(`{"Web":{"mac.tail1.ts.net:8443":{}}}`), 8443) {
		t.Fatal("demoPortTaken")
	}
}
