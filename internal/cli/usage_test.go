package cli

// Tests for `pilothouse usage` and `pilothouse activity` against a fake
// daemon socket. Helpers are prefixed "usg".

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
)

var usgT0 = time.Date(2026, 10, 23, 14, 30, 0, 0, time.UTC)

type usgDaemon struct {
	mu       sync.Mutex
	insights api.Insights
	days     []string // ?days= values seen
	feeds    []api.Activity
	cursors  []string
	calls    int
}

func usgStart(t *testing.T) *usgDaemon {
	t.Helper()
	run, err := os.MkdirTemp("/tmp", "usg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(run) })
	t.Setenv("PILOTHOUSE_RUN_DIR", run)
	t.Setenv("PILOTHOUSE_HOME", filepath.Join(t.TempDir(), "home"))
	t.Setenv("PILOTHOUSE_NO_UPDATE_CHECK", "1")
	loc, now, poll := usageLocation, usageNow, activityPollEvery
	usageLocation, usageNow, activityPollEvery = time.UTC, func() time.Time { return usgT0 }, 10*time.Millisecond
	t.Cleanup(func() { usageLocation, usageNow, activityPollEvery = loc, now, poll })

	d := &usgDaemon{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/insights", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.days = append(d.days, r.URL.Query().Get("days"))
		_ = json.NewEncoder(w).Encode(d.insights)
	})
	mux.HandleFunc("GET /v1/sessions/{id}/activity", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if r.PathValue("id") == "s_missing" {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(api.Error{Error: "not found", Code: "not_found"})
			return
		}
		d.cursors = append(d.cursors, r.URL.Query().Get("cursor"))
		i := min(d.calls, len(d.feeds)-1)
		d.calls++
		_ = json.NewEncoder(w).Encode(d.feeds[i])
	})
	ln, err := net.Listen("unix", filepath.Join(run, "pilothouse.sock"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(mux)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return d
}

func usgRun(t *testing.T, ctx context.Context, args ...string) (string, error) {
	t.Helper()
	root := newRoot()
	var out bytes.Buffer
	root.SetArgs(args)
	root.SetOut(&out)
	root.SetErr(&out)
	err := root.ExecuteContext(ctx)
	return out.String(), err
}

func usgContains(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
}

func usgInsights() api.Insights {
	day := func(date string, cost float64, in, out int64, sessions int) api.InsightsDay {
		return api.InsightsDay{Date: date, Usage: api.Usage{CostUSD: cost, InputTokens: in, OutputTokens: out}, Sessions: sessions}
	}
	return api.Insights{
		Days:  []api.InsightsDay{day("2026-10-21", 1.5, 12_300, 4_500, 1), day("2026-10-22", 0, 0, 0, 0), day("2026-10-23", 6, 2_000_000, 50_000, 2)},
		Total: api.Usage{CostUSD: 7.5, InputTokens: 2_012_300, OutputTokens: 54_500, CacheReadTokens: 9_000_000},
		ByAgent: []api.UsageGroup{{Key: "claude", Usage: api.Usage{CostUSD: 6, InputTokens: 1000}, Sessions: 2},
			{Key: "codex", Usage: api.Usage{CostUSD: 1.5}, Sessions: 1}},
		ByModel: []api.UsageGroup{{Key: "claude-opus-5-5", Usage: api.Usage{CostUSD: 6}, Sessions: 2}},
		ByRepo:  []api.UsageGroup{{Key: "/home/dev/src/api", Usage: api.Usage{CostUSD: 7.5}, Sessions: 3}},
		Rules: api.RuleStats{AutoAllowed: 120, Granted: 4, Asked: 14, Blocked: 3, AnsweredByDevice: 9, AnsweredAtTerminal: 4,
			Expired: 1, MedianAnswerSecs: 42, BlockedMinutes: 12.5, TopBlocked: []api.RuleCount{{Key: "Run: git push -f", Count: 2}}},
		Budget: &api.BudgetStatus{DailyUSD: 10, SpentUSD: 6, Percent: 60, BurnUSDh: 1.1},
	}
}

func TestUsageCommand(t *testing.T) {
	d := usgStart(t)
	d.insights = usgInsights()
	out, err := usgRun(t, context.Background(), "usage", "--days", "3")
	if err != nil {
		t.Fatal(err, out)
	}
	usgContains(t, out,
		"Usage, last 3 days (estimated from list prices)",
		"Wed Oct 21", "$1.50", "12.3k", "4.5k", "Fri Oct 23", "2.0M",
		"Total", "$7.50", "9.0M",
		"Cost per day  ▃▁█  (Wed Oct 21 … Fri Oct 23)",
		"By agent", "claude", "2 sessions", "codex",
		"By model", "claude-opus-5-5",
		"By repo", "api",
		"Today     $6.00 of $10.00 budget (60%) · $1.10 in the last hour",
		`120 auto-allowed (4 by "allow for session") · 14 asked you · 3 blocked`,
		"9 answered on a device, 4 at the terminal, 1 expired · median answer 42s · agents waited 12.5 min",
		"Blocked   Run: git push -f (2)",
	)
	if strings.Contains(out, "/home/dev") {
		t.Error("repos show by name")
	}
	if d.days[0] != "3" {
		t.Errorf("days sent = %q", d.days)
	}

	out, err = usgRun(t, context.Background(), "usage", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var back api.Insights
	if json.Unmarshal([]byte(out), &back) != nil || back.Total.CostUSD != 7.5 {
		t.Errorf("--json output = %s", out)
	}
	if _, err := usgRun(t, context.Background(), "usage", "--days", "0"); err == nil {
		t.Error("--days 0 must be refused")
	}

	d.insights = api.Insights{Days: []api.InsightsDay{{Date: "2026-10-23"}}}
	out, _ = usgRun(t, context.Background(), "usage", "--days", "1")
	usgContains(t, out, "No agent usage recorded yet", "Rules     0 auto-allowed")
}

func TestUsageDaemonDown(t *testing.T) {
	run, _ := os.MkdirTemp("/tmp", "usg")
	t.Cleanup(func() { os.RemoveAll(run) })
	t.Setenv("PILOTHOUSE_RUN_DIR", run)
	t.Setenv("PILOTHOUSE_HOME", t.TempDir())
	_, err := usgRun(t, context.Background(), "usage")
	if err == nil || !strings.Contains(err.Error(), "isn't running") {
		t.Errorf("err = %v", err)
	}
}

func usgFeed() api.Activity {
	at := func(s int) time.Time { return usgT0.Add(time.Duration(s) * time.Second) }
	return api.Activity{SessionID: "s_abc", Available: true, Cursor: "e1.5", Items: []api.ActivityItem{
		{ID: "p1", Kind: api.ActivityPrompt, At: at(0), Turn: 1, Text: "Fix the build", Source: "human"},
		{ID: "t1", Kind: api.ActivityTool, At: at(1), Turn: 1, Tool: "Bash", Summary: "Run: npm test", IsError: true, Result: "…\n1 failed"},
		{ID: "t2", Kind: api.ActivityTool, At: at(2), Turn: 1, Tool: "Bash", Summary: "Run: npm run build", Pending: true},
		{ID: "s1", Kind: api.ActivityTool, At: at(3), Turn: 1, Tool: "WebFetch", Summary: "Fetch: https://example.com", Sidechain: true},
		{ID: "x1", Kind: api.ActivityText, At: at(4), Turn: 1, Text: "Working on it.\nMore."},
		{ID: "p2", Kind: api.ActivityPrompt, At: at(5), Turn: 2, Text: "and docs", Source: "pilothouse"},
	},
		Usage:   &api.Usage{InputTokens: 1200, OutputTokens: 300, CacheReadTokens: 50_000, CostUSD: 0.42, Model: "claude-opus-5-5"},
		Files:   []api.FileChange{{Path: "src/a.go", Op: api.FileEdit, Added: 3, Removed: 1}},
		CatchUp: &api.CatchUp{Since: usgT0.Add(-time.Hour), Lines: []string{"Ran 3 tool calls", "Hit 1 error (last: Run: npm test)"}},
	}
}

func TestActivityCommand(t *testing.T) {
	d := usgStart(t)
	d.feeds = []api.Activity{usgFeed()}
	out, err := usgRun(t, context.Background(), "activity", "s_abc", "--catch-up")
	if err != nil {
		t.Fatal(err, out)
	}
	usgContains(t, out,
		"Since 13:30:", "  • Ran 3 tool calls",
		"14:30:00  › Fix the build",
		"14:30:01  $ Run: npm test  ✗  1 failed",
		"14:30:02  $ Run: npm run build  …",
		"14:30:03    ↳ $ Fetch: https://example.com  ✓",
		"14:30:04  ● Working on it. More.",
		"\n\n14:30:05  › and docs  (sent from Pilothouse)",
		"Usage  1.2k in · 300 out · 50.0k cached · ~$0.42 (claude-opus-5-5)",
		"Files  1 changed (+3/−1): src/a.go (+3/−1)",
	)

	d.mu.Lock()
	d.feeds = []api.Activity{{SessionID: "s_x", Available: false, Note: "No transcript yet: the agent hasn't written one."}}
	d.calls = 0
	d.mu.Unlock()
	out, err = usgRun(t, context.Background(), "activity", "s_x")
	if err != nil || strings.TrimSpace(out) != "No transcript yet: the agent hasn't written one." {
		t.Errorf("unavailable = %q %v", out, err)
	}
	if _, err := usgRun(t, context.Background(), "activity", "s_missing"); err == nil || !strings.Contains(err.Error(), "pilothouse ls") {
		t.Errorf("missing session = %v", err)
	}
	if _, err := usgRun(t, context.Background(), "activity"); err == nil {
		t.Error("a session is required")
	}
}

// -f polls with the cursor and prints a finished line for calls that
// were still running.
func TestActivityFollow(t *testing.T) {
	d := usgStart(t)
	first := usgFeed()
	done := first.Items[2]
	done.Pending = false
	second := api.Activity{SessionID: "s_abc", Available: true, Cursor: "e1.7", Items: []api.ActivityItem{
		done,
		{ID: "x2", Kind: api.ActivityText, At: usgT0.Add(10 * time.Second), Turn: 2, Text: "Built."},
	}}
	idle := api.Activity{SessionID: "s_abc", Available: true, Cursor: "e1.7", Items: []api.ActivityItem{}}
	d.feeds = []api.Activity{first, second, idle}
	ctx, cancel := context.WithCancel(context.Background())
	var out string
	errc := make(chan error, 1)
	go func() {
		var err error
		out, err = usgRun(t, ctx, "activity", "s_abc", "-f")
		errc <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		d.mu.Lock()
		n := d.calls
		d.mu.Unlock()
		if n >= 4 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	usgContains(t, out, "14:30:02  $ Run: npm run build  …", "14:30:02  $ Run: npm run build  ✓ (finished)", "14:30:10  ● Built.")
	if strings.Count(out, "Built.") != 1 || strings.Contains(out, "Usage ") {
		t.Errorf("follow output:\n%s", out)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cursors[0] != "" || d.cursors[1] != "e1.5" || d.cursors[2] != "e1.7" {
		t.Errorf("cursors = %q", d.cursors)
	}
}

func TestUsageHelpers(t *testing.T) {
	if s := usgSparkline([]float64{0, 1, 2, 4}); s != "▁▃▅█" {
		t.Errorf("sparkline = %q", s)
	}
	if s := usgSparkline([]float64{0, 0}); s != "▁▁" {
		t.Errorf("flat sparkline = %q", s)
	}
	for in, want := range map[int64]string{950: "950", 12_345: "12.3k", 4_500_000: "4.5M", 2_100_000_000: "2.1B"} {
		if got := usgTokens(in); got != want {
			t.Errorf("usgTokens(%d) = %q", in, got)
		}
	}
	for in, want := range map[float64]string{0: "$0", 0.004: "<$0.01", 12.345: "$12.35"} {
		if got := usgMoney(in); got != want {
			t.Errorf("usgMoney(%v) = %q", in, got)
		}
	}
	if s := usgSeconds(125); s != "2m 5s" {
		t.Errorf("usgSeconds = %q", s)
	}
	if s := usgMinutes(135); s != "2h 15m" {
		t.Errorf("usgMinutes = %q", s)
	}
}
