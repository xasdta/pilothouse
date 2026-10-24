package daemon

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/config"
	"github.com/xasdta/pilothouse/internal/notify/telegram"
)

const tgToken = "987654321:AAdaemonTESTtokenDaemonTESTtoken"

// tgMock is a minimal Bot API for daemon tests.
type tgMock struct {
	*httptest.Server
	mu      sync.Mutex
	updates []telegram.Update
	nextUpd int64
	nextMsg int64
	sent    []tgSent
	edits   []tgSent
	answers []string
	wake    chan struct{}
}

type tgSent struct {
	ID     int64
	ChatID int64           `json:"chat_id"`
	MsgID  int64           `json:"message_id"`
	Text   string          `json:"text"`
	Markup json.RawMessage `json:"reply_markup"`
}

func newTGMock(t *testing.T) *tgMock {
	m := &tgMock{nextUpd: 1, nextMsg: 500, wake: make(chan struct{}, 1)}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, method, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/bot"), "/")
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if tok != tgToken {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":401,"description":"Unauthorized"}`))
			return
		}
		ok := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": v}) }
		switch method {
		case "getUpdates":
			var p struct {
				Offset int64 `json:"offset"`
			}
			_ = json.Unmarshal(body, &p)
			deadline := time.Now().Add(300 * time.Millisecond)
			for {
				m.mu.Lock()
				var out []telegram.Update
				for _, u := range m.updates {
					if u.UpdateID >= p.Offset {
						out = append(out, u)
					}
				}
				m.mu.Unlock()
				if len(out) > 0 || time.Now().After(deadline) {
					if out == nil {
						out = []telegram.Update{}
					}
					ok(out)
					return
				}
				select {
				case <-m.wake:
				case <-time.After(20 * time.Millisecond):
				case <-r.Context().Done():
					return
				}
			}
		case "sendMessage", "editMessageText":
			var s tgSent
			_ = json.Unmarshal(body, &s)
			m.mu.Lock()
			if method == "sendMessage" {
				m.nextMsg++
				s.ID = m.nextMsg
				m.sent = append(m.sent, s)
			} else {
				m.edits = append(m.edits, s)
			}
			m.mu.Unlock()
			if method == "sendMessage" {
				ok(telegram.Message{MessageID: s.ID, Chat: telegram.Chat{ID: s.ChatID, Type: "private"}, Date: time.Now().Unix()})
			} else {
				ok(true)
			}
		case "answerCallbackQuery":
			var a struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(body, &a)
			m.mu.Lock()
			m.answers = append(m.answers, a.Text)
			m.mu.Unlock()
			ok(true)
		default:
			ok(true)
		}
	}))
	t.Cleanup(m.Close)
	return m
}

func (m *tgMock) push(u telegram.Update) {
	m.mu.Lock()
	u.UpdateID = m.nextUpd
	m.nextUpd++
	m.updates = append(m.updates, u)
	m.mu.Unlock()
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// find returns the newest message (sent or edited) whose text contains s.
func (m *tgMock) find(edits bool, s string) (tgSent, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := m.sent
	if edits {
		list = m.edits
	}
	for i := len(list) - 1; i >= 0; i-- {
		if strings.Contains(list[i].Text, s) {
			return list[i], true
		}
	}
	return tgSent{}, false
}

func tgPrivate(chat int64, text string) telegram.Update {
	return telegram.Update{Message: &telegram.Message{MessageID: 1, From: &telegram.User{ID: chat}, Date: time.Now().Unix(),
		Chat: telegram.Chat{ID: chat, Type: "private"}, Text: text}}
}

// A real daemon with the real notifier: pairing over getUpdates writes
// chat_id, a held request arrives with buttons and a press answers the
// agent, and a request answered elsewhere gets its message edited.
func TestTelegramThroughDaemon(t *testing.T) {
	h := newHarness(t)
	mock := newTGMock(t)
	h.cfg.Notify.Desktop = false // tests never show real notifications
	// Pairing saves the daemon's settings to config.yaml, which must load.
	h.cfg.Approvals.HoldLimit = config.Duration{Duration: 10 * time.Second}
	h.cfg.Notify.Telegram.Enabled = true
	h.cfg.Notify.Telegram.APIBase = mock.URL
	h.opts = func(o *Options) { o.Notifier = nil }
	k := telegram.Keys{Token: tgToken, BotUsername: "daemon_test_bot"}
	code := k.NewPairCode(time.Now())
	if err := telegram.SaveKeys(telegram.KeysPath(), k); err != nil {
		t.Fatal(err)
	}
	h.presence.setAway(true)
	h.start(t)
	waitFor(t, "away", func() bool { return h.d.Presence().State == api.PresenceAway })

	// Strangers and wrong codes are ignored; the right code pairs the chat.
	mock.push(tgPrivate(77, "/start not-the-code-not-the-code"))
	mock.push(tgPrivate(4242, "/start "+code))
	waitFor(t, "pairing saved", func() bool {
		c, err := config.Load()
		return err == nil && c.Notify.Telegram.ChatID == 4242
	})
	waitFor(t, "welcome", func() bool { _, ok := mock.find(false, "Connected to"); return ok })
	if k2, _ := telegram.LoadKeys(telegram.KeysPath()); k2.PairHash != "" || k2.Token != tgToken {
		t.Fatalf("pairing code not spent / token lost: %+v", k2.PairHash)
	}
	mock.mu.Lock()
	for _, s := range mock.sent {
		if s.ChatID != 4242 {
			t.Errorf("message to chat %d", s.ChatID)
		}
	}
	mock.mu.Unlock()

	// Wait for the restarted (paired) bot before the request goes out.
	waitFor(t, "paired bot", func() bool { b := h.d.tg.current(); return b != nil && b.Chat() == 4242 })

	// A held request: the Allow button answers the agent.
	res := make(chan fakeOutcome, 1)
	go func() { res <- h.hook(bg, t, permission("git push origin main", "")) }()
	var msg tgSent
	waitFor(t, "request message", func() bool { var ok bool; msg, ok = mock.find(false, "git push origin main"); return ok })
	var kb telegram.InlineKeyboard
	_ = json.Unmarshal(msg.Markup, &kb)
	allow := ""
	for _, row := range kb.Rows {
		for _, b := range row {
			if b.Text == "✅ Allow" {
				allow = b.CallbackData
			}
		}
	}
	if allow == "" {
		t.Fatalf("no Allow button: %s", msg.Markup)
	}
	mock.push(telegram.Update{CallbackQuery: &telegram.CallbackQuery{ID: "cb1", From: telegram.User{ID: 4242}, Data: allow,
		Message: &telegram.Message{MessageID: msg.ID, Chat: telegram.Chat{ID: 4242, Type: "private"}}}})
	select {
	case out := <-res:
		if out.Behavior != api.BehaviorAllow {
			t.Fatalf("hook = %+v", out)
		}
	case <-time.After(5 * time.Second):
		mock.mu.Lock()
		t.Fatalf("the button didn't answer the agent; callback answers: %q", mock.answers)
	}
	decided, _ := h.c.Requests(bg, api.RequestDecided)
	if len(decided) != 1 || decided[0].Decision.By != "telegram:4242" {
		t.Fatalf("decided = %+v", decided)
	}
	waitFor(t, "edit", func() bool { _, ok := mock.find(true, "✓ Allowed from Telegram"); return ok })

	// Answered from the CLI: the Telegram message is edited (withdrawal).
	go func() { res <- h.hook(bg, t, permission("npm publish", "")) }()
	req := h.openRequest(t)
	waitFor(t, "second request message", func() bool { _, ok := mock.find(false, "npm publish"); return ok })
	if _, err := h.c.Decide(bg, req.ID, api.DecideBody{Behavior: api.BehaviorDeny, Message: "not from here"}); err != nil {
		t.Fatal(err)
	}
	<-res
	waitFor(t, "withdrawal edit", func() bool { _, ok := mock.find(true, "✗ Denied from the CLI"); return ok })
}
