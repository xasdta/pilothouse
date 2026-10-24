package telegram

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "123456789:AAtestTOKENtestTOKENtestTOKEN_abc"

// sent is a sendMessage call as the mock saw it.
type sent struct {
	ID      int64
	ChatID  int64           `json:"chat_id"`
	Text    string          `json:"text"`
	Markup  json.RawMessage `json:"reply_markup"`
	ReplyTo *struct {
		MessageID int64 `json:"message_id"`
	} `json:"reply_parameters"`
}

// buttons returns the inline keyboard (text, callback data, url).
func (s sent) buttons() []Button { return parseButtons(s.Markup) }

func parseButtons(raw json.RawMessage) []Button {
	var kb InlineKeyboard
	_ = json.Unmarshal(raw, &kb)
	var out []Button
	for _, row := range kb.Rows {
		out = append(out, row...)
	}
	return out
}

type edited struct {
	ChatID    int64           `json:"chat_id"`
	MessageID int64           `json:"message_id"`
	Text      string          `json:"text"`
	Markup    json.RawMessage `json:"reply_markup"`
}

type answered struct {
	ID    string `json:"callback_query_id"`
	Text  string `json:"text"`
	Alert bool   `json:"show_alert"`
}

// mockAPI is an in-memory Bot API server.
type mockAPI struct {
	*httptest.Server
	mu       sync.Mutex
	updates  []Update
	nextUpd  int64
	nextMsg  int64
	sent     []sent
	edits    []edited
	answers  []answered
	calls    []string
	fail     map[string][]int // method → statuses to answer before succeeding
	wake     chan struct{}
	confirms []int64 // getUpdates offsets seen
}

func newMock(t *testing.T) *mockAPI {
	m := &mockAPI{fail: map[string][]int{}, wake: make(chan struct{}, 1), nextMsg: 100, nextUpd: 1}
	m.Server = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.Close)
	return m
}

func (m *mockAPI) client() *Client {
	return &Client{Token: testToken, Base: m.URL, HTTP: m.Client(), Backoff: 5 * time.Millisecond}
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func ok(w http.ResponseWriter, result any) {
	reply(w, 200, map[string]any{"ok": true, "result": result})
}

func (m *mockAPI) serve(w http.ResponseWriter, r *http.Request) {
	tok, method, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/bot"), "/")
	body, _ := io.ReadAll(r.Body)
	if tok != testToken {
		reply(w, 401, map[string]any{"ok": false, "error_code": 401, "description": "Unauthorized"})
		return
	}
	m.mu.Lock()
	m.calls = append(m.calls, method)
	if f := m.fail[method]; len(f) > 0 {
		status := f[0]
		m.fail[method] = f[1:]
		m.mu.Unlock()
		resp := map[string]any{"ok": false, "error_code": status, "description": "Too Many Requests: retry after 1"}
		if status == 429 {
			resp["parameters"] = map[string]int{"retry_after": 1}
		} else {
			resp["description"] = "Internal Server Error"
		}
		reply(w, status, resp)
		return
	}
	m.mu.Unlock()

	switch method {
	case "getMe":
		ok(w, User{ID: 123456789, IsBot: true, FirstName: "Pilot", Username: "pilot_test_bot"})
	case "getUpdates":
		var p struct {
			Offset  int64 `json:"offset"`
			Timeout int   `json:"timeout"`
		}
		_ = json.Unmarshal(body, &p)
		wait := time.Duration(p.Timeout) * time.Second
		deadline := time.Now().Add(wait)
		for {
			m.mu.Lock()
			m.confirms = append(m.confirms, p.Offset)
			var out []Update
			keep := m.updates[:0]
			for _, u := range m.updates {
				if u.UpdateID >= p.Offset {
					keep = append(keep, u)
					out = append(out, u)
				}
			}
			m.updates = keep
			m.mu.Unlock()
			if len(out) > 0 || !time.Now().Before(deadline) {
				if out == nil {
					out = []Update{}
				}
				ok(w, out)
				return
			}
			select {
			case <-m.wake:
			case <-time.After(time.Until(deadline)):
			case <-r.Context().Done():
				return
			}
		}
	case "sendMessage":
		var s sent
		_ = json.Unmarshal(body, &s)
		m.mu.Lock()
		m.nextMsg++
		s.ID = m.nextMsg
		m.sent = append(m.sent, s)
		m.mu.Unlock()
		ok(w, Message{MessageID: s.ID, Chat: Chat{ID: s.ChatID, Type: "private"}, Date: time.Now().Unix(), Text: s.Text})
	case "editMessageText":
		var e edited
		_ = json.Unmarshal(body, &e)
		m.mu.Lock()
		for _, prev := range m.edits {
			if prev.MessageID == e.MessageID && prev.Text == e.Text && string(prev.Markup) == string(e.Markup) {
				m.mu.Unlock()
				reply(w, 400, map[string]any{"ok": false, "error_code": 400, "description": "Bad Request: message is not modified"})
				return
			}
		}
		m.edits = append(m.edits, e)
		m.mu.Unlock()
		ok(w, true)
	case "answerCallbackQuery":
		var a answered
		_ = json.Unmarshal(body, &a)
		m.mu.Lock()
		m.answers = append(m.answers, a)
		m.mu.Unlock()
		ok(w, true)
	case "deleteWebhook":
		ok(w, true)
	default:
		reply(w, 404, map[string]any{"ok": false, "error_code": 404, "description": "Not Found: method " + method})
	}
}

// push queues an update for getUpdates.
func (m *mockAPI) push(u Update) {
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

func (m *mockAPI) lastSent(t *testing.T) sent {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sent) == 0 {
		t.Fatal("nothing sent")
	}
	return m.sent[len(m.sent)-1]
}

func (m *mockAPI) sentCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}

func (m *mockAPI) lastEdit(t *testing.T) edited {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.edits) == 0 {
		t.Fatal("nothing edited")
	}
	return m.edits[len(m.edits)-1]
}

func (m *mockAPI) editCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.edits)
}

func (m *mockAPI) lastAnswer(t *testing.T) answered {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.answers) == 0 {
		t.Fatal("no callback answered")
	}
	return m.answers[len(m.answers)-1]
}

func (m *mockAPI) answerCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.answers)
}

func (m *mockAPI) countCalls(method string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.calls {
		if c == method {
			n++
		}
	}
	return n
}

var cbSeq int

// press builds a callback update for a button of message msg in chat.
func press(chat, from, msg int64, data string) Update {
	cbSeq++
	return Update{CallbackQuery: &CallbackQuery{ID: "cb" + strconv.Itoa(cbSeq), From: User{ID: from},
		Message: &Message{MessageID: msg, Chat: Chat{ID: chat, Type: "private"}}, Data: data}}
}

func text(chat, from int64, s string) Update {
	return Update{Message: &Message{MessageID: 1, From: &User{ID: from}, Chat: Chat{ID: chat, Type: "private"}, Date: time.Now().Unix(), Text: s}}
}

func replyTo(chat, msg int64, s string) Update {
	u := text(chat, chat, s)
	u.Message.ReplyToMessage = &Message{MessageID: msg, Chat: Chat{ID: chat}}
	return u
}

// button returns the callback data of the button whose text contains label.
func button(t *testing.T, bs []Button, label string) string {
	t.Helper()
	for _, b := range bs {
		if strings.Contains(b.Text, label) && b.CallbackData != "" {
			return b.CallbackData
		}
	}
	t.Fatalf("no %q button in %+v", label, bs)
	return ""
}
