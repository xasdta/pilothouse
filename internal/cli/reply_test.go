package cli

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/paths"
)

// replyDaemon fakes the two routes `pilothouse reply` uses.
type replyDaemon struct {
	mu      sync.Mutex
	replies []api.Reply
	client  []string
	reject  *api.Error
}

func startReplyDaemon(t *testing.T) *replyDaemon {
	t.Helper()
	hkEnv(t)
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", paths.Socket())
	if err != nil {
		t.Fatal(err)
	}
	rd := &replyDaemon{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1/sessions/s_k3m":
			_ = json.NewEncoder(w).Encode(api.Session{ID: "s_k3m1234567", Title: "api-server"})
		case r.Method == "POST" && r.URL.Path == "/v1/sessions/s_k3m1234567/reply":
			var b api.Reply
			_ = json.NewDecoder(r.Body).Decode(&b)
			rd.mu.Lock()
			rd.replies = append(rd.replies, b)
			rd.client = append(rd.client, r.Header.Get("X-Pilothouse-Client"))
			rej := rd.reject
			rd.mu.Unlock()
			if rej != nil {
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(rej)
				return
			}
			_ = json.NewEncoder(w).Encode(api.ReplyResult{Via: api.ReplyMessage, Note: "Sent as a message from Pilothouse."})
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(api.Error{Error: "not found", Code: "not_found"})
		}
	}))
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return rd
}

func runCLI(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	root := newRoot()
	var out bytes.Buffer
	root.SetArgs(args)
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(stdin))
	err := root.Execute()
	return out.String(), err
}

func TestReplyCommand(t *testing.T) {
	rd := startReplyDaemon(t)
	out, err := runCLI(t, "", "reply", "s_k3m", "also update the changelog")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Sent to api-server (s_k3m1234567), through Claude's messaging socket.") || !strings.Contains(out, "Sent as a message") {
		t.Fatalf("output %q", out)
	}
	if _, err := runCLI(t, "line one\nline two\n", "reply", "s_k3m", "-", "--interrupt"); err != nil {
		t.Fatal(err)
	}
	rd.mu.Lock()
	got := append([]api.Reply(nil), rd.replies...)
	clients := append([]string(nil), rd.client...)
	rd.mu.Unlock()
	if len(got) != 2 || got[0].Text != "also update the changelog" || got[0].Interrupt ||
		got[1].Text != "line one\nline two\n" || !got[1].Interrupt || clients[0] != "cli" {
		t.Fatalf("daemon got %+v %v", got, clients)
	}

	if _, err := runCLI(t, "", "reply", "s_k3m"); err == nil || !strings.Contains(err.Error(), "nothing to send") {
		t.Fatalf("no text: %v", err)
	}
	if _, err := runCLI(t, "", "reply", "s_nope", "x"); err == nil || !strings.Contains(err.Error(), "no session matches") {
		t.Fatalf("unknown session: %v", err)
	}
	// The daemon's agent-caller guard reaches the person as is.
	rd.mu.Lock()
	rd.reject = &api.Error{Error: "refused: this was run from inside an agent session", Code: "agent_caller"}
	rd.mu.Unlock()
	if _, err := runCLI(t, "", "reply", "s_k3m", "x"); err == nil || !strings.Contains(err.Error(), "inside an agent session") {
		t.Fatalf("guard: %v", err)
	}
}
