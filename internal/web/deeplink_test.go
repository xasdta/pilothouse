package web

import (
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
)

// A notification's own URL (a task: /t/<id>) is where tapping the push
// goes; anything that is not a path on the web app is ignored.
func TestPushPayloadDeepLink(t *testing.T) {
	s := &api.Session{ID: "s_abc"}
	for url, want := range map[string]string{
		"/t/t_abc":             "/t/t_abc",
		"":                     "/s/s_abc",
		"//evil.example/x":     "/s/s_abc",
		"https://evil.example": "/s/s_abc",
	} {
		p, _, _ := payloadFor(core.Notification{Kind: core.NotifyWaiting, Session: s, URL: url}, time.Now())
		if p.URL != want || p.Tag != "s_abc" {
			t.Errorf("URL %q: payload URL %q tag %q, want %q", url, p.URL, p.Tag, want)
		}
	}
}
