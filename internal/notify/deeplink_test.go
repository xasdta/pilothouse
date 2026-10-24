package notify

import (
	"context"
	"testing"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
)

// A notification's own URL (a task: /t/<id>) is where a click goes;
// anything that is not a path on the web app is ignored.
func TestNtfyDeepLink(t *testing.T) {
	ts, got := capture(t, 200)
	c := &Ntfy{Server: ts.URL, Topic: "t", Pusher: &fakePusher{public: "https://mac.tail1.ts.net"}, Client: ts.Client()}
	s := &api.Session{ID: "s_abc"}
	for _, n := range []core.Notification{
		{Kind: core.NotifyWaiting, Title: "Ready for review", Session: s, URL: "/t/t_abc"},
		{Kind: core.NotifyWaiting, Title: "x", Session: s, URL: "//evil.example/t"},
		{Kind: core.NotifyWaiting, Title: "x", Session: s, URL: "https://evil.example/"},
	} {
		if err := c.Send(context.Background(), n); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"https://mac.tail1.ts.net/t/t_abc", "https://mac.tail1.ts.net/s/s_abc", "https://mac.tail1.ts.net/s/s_abc"}
	for i, g := range got() {
		if c := g.header.Get("Click"); c != want[i] {
			t.Errorf("notification %d: Click %q, want %q", i, c, want[i])
		}
	}
}
