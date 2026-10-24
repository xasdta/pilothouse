package insights

import (
	"reflect"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/store"
)

// A fixed clock and zone: 2026-10-23 14:30 in a UTC−7 zone, which is
// 21:30 UTC — late enough that UTC and local days differ for some events.
var (
	zone = time.FixedZone("PDT", -7*3600)
	now  = time.Date(2026, 10, 23, 14, 30, 0, 0, zone)
)

func TestNewRange(t *testing.T) {
	r := NewRange(now, zone, 3)
	if r.FromDay != "2026-10-21" || r.ToDay != "2026-10-23" || !reflect.DeepEqual(r.Days, []string{"2026-10-21", "2026-10-22", "2026-10-23"}) {
		t.Errorf("range = %+v", r)
	}
	if !r.From.Equal(time.Date(2026, 10, 21, 0, 0, 0, 0, zone)) || !r.To.Equal(now) {
		t.Errorf("from/to = %v %v", r.From, r.To)
	}
	if n := len(NewRange(now, zone, 0).Days); n != DefaultDays {
		t.Errorf("default days = %d", n)
	}
	if n := len(NewRange(now, zone, 1000).Days); n != MaxDays {
		t.Errorf("max days = %d", n)
	}
}

func TestBuild(t *testing.T) {
	rg := NewRange(now, zone, 3)
	at := func(day, hour int) time.Time { return time.Date(2026, 10, day, hour, 0, 0, 0, zone) }
	usage := []store.UsageRow{
		{SessionID: "s1", Day: "2026-10-21", Agent: api.AgentClaude, Model: "claude-opus-5-5", Repo: "/src/api", Input: 100, Output: 10, CostUSD: 1.5, Turns: 2},
		{SessionID: "s1", Day: "2026-10-23", Agent: api.AgentClaude, Model: "claude-opus-5-5", Repo: "/src/api", Input: 50, Output: 5, CacheRead: 1000, CostUSD: 2, Turns: 1},
		{SessionID: "s1", Day: "2026-10-23", Agent: api.AgentClaude, Model: "claude-haiku-5-5", Repo: "/src/api", Input: 10, Output: 1, CostUSD: 0.01},
		{SessionID: "s2", Day: "2026-10-23", Agent: api.AgentCodex, Model: "gpt-6-sol", Repo: "", Input: 400, Output: 40, CostUSD: 3, Turns: 4},
		{SessionID: "s3", Day: "2026-10-23", Agent: api.AgentCodex, Model: "", Input: 7},
		{SessionID: "s9", Day: "2026-10-01", Agent: api.AgentClaude, Model: "x", Input: 1e6, CostUSD: 99}, // outside the range
	}
	created := func(t time.Time) time.Time { return t }
	events := []store.DecisionRow{
		{Type: api.EventPolicyDecision, At: at(21, 9), Outcome: api.BehaviorAllow, Rule: "allow-tests"},
		{Type: api.EventPolicyDecision, At: at(21, 10), Outcome: api.BehaviorAllow, Rule: "allow-tests"},
		{Type: api.EventPolicyDecision, At: at(22, 23), Outcome: api.BehaviorAllow, Grant: "g_1"}, // 23:00 local = next day UTC
		{Type: api.EventPolicyDecision, At: at(23, 9), Outcome: api.BehaviorDeny, Rule: "no-force-push", Summary: "Run: git push -f"},
		{Type: api.EventPolicyDecision, At: at(23, 9), Outcome: api.BehaviorDeny, Rule: "no-force-push", Summary: "Run: git push -f"},
		{Type: api.EventPolicyDecision, At: at(23, 9), Outcome: api.BehaviorDeny, Rule: "no-rm-rf", Summary: "Run: rm -rf /"},
		{Type: api.EventPolicyDecision, At: at(23, 9), Outcome: api.BehaviorAsk, Rule: "ask-push"},
		{Type: api.EventRequestOpened, At: at(23, 10)},
		{Type: api.EventRequestOpened, At: at(23, 11)},
		{Type: api.EventRequestOpened, At: at(23, 12)},
		{Type: api.EventRequestOpened, At: at(22, 12)},
		{Type: api.EventRequestClosed, At: at(23, 10), Status: api.RequestDecided, By: "device:d_phone",
			CreatedAt: created(at(23, 10)), ClosedAt: at(23, 10).Add(30 * time.Second)},
		{Type: api.EventRequestClosed, At: at(23, 11), Status: api.RequestAnsweredLocally,
			CreatedAt: at(23, 11), ClosedAt: at(23, 11).Add(90 * time.Second)},
		{Type: api.EventRequestClosed, At: at(23, 12), Status: api.RequestDecided, By: "cli",
			CreatedAt: at(23, 12), ClosedAt: at(23, 12).Add(10 * time.Second)},
		{Type: api.EventRequestClosed, At: at(22, 12), Status: api.RequestExpired,
			CreatedAt: at(22, 12), ClosedAt: at(22, 12).Add(9 * time.Minute)},
		// Granted at the permission checkpoint: nobody was asked.
		{Type: api.EventRequestClosed, At: at(23, 13), Status: api.RequestDecided, By: "grant:g_1", CreatedAt: at(23, 13), ClosedAt: at(23, 13)},
		// A request left open for days says nothing about answer speed.
		{Type: api.EventRequestClosed, At: at(23, 13), Status: api.RequestDecided, By: "telegram:42", CreatedAt: at(20, 13), ClosedAt: at(23, 13)},
	}
	in := Build(Input{Range: rg, Loc: zone, Usage: usage, Events: events, BudgetUSD: 10, BurnUSDh: 1.234})

	if in.Total.CostUSD != 1.5+2+0.01+3 || in.Total.InputTokens != 100+50+10+400+7 || in.Total.Turns != 7 {
		t.Errorf("total = %+v", in.Total)
	}
	if len(in.Days) != 3 || in.Days[0].Usage.CostUSD != 1.5 || in.Days[1].Usage.CostUSD != 0 || in.Days[2].Sessions != 3 {
		t.Errorf("days = %+v", in.Days)
	}
	if in.Days[0].AutoAllowed != 2 || in.Days[1].AutoAllowed != 1 || in.Days[2].AutoAllowed != 1 || in.Days[2].Blocked != 3 ||
		in.Days[2].Asked != 3 || in.Days[1].Asked != 1 {
		t.Errorf("day decisions = %+v", in.Days)
	}
	keys := func(gs []api.UsageGroup) []string {
		var out []string
		for _, g := range gs {
			out = append(out, g.Key)
		}
		return out
	}
	if got := keys(in.ByAgent); !reflect.DeepEqual(got, []string{"claude", "codex"}) || in.ByAgent[0].Sessions != 1 || in.ByAgent[1].Sessions != 2 {
		t.Errorf("by agent = %+v", in.ByAgent)
	}
	if got := keys(in.ByModel); !reflect.DeepEqual(got, []string{"claude-opus-5-5", "gpt-6-sol", "claude-haiku-5-5", "unknown"}) {
		t.Errorf("by model = %q", got)
	}
	if in.ByModel[0].Usage.Model != "claude-opus-5-5" {
		t.Errorf("model group usage carries the model: %+v", in.ByModel[0])
	}
	if got := keys(in.ByRepo); !reflect.DeepEqual(got, []string{"/src/api", "(no repo)"}) {
		t.Errorf("by repo = %q", got)
	}

	wantRules := api.RuleStats{
		AutoAllowed: 4, Asked: 4, Blocked: 3, Granted: 2,
		AnsweredByDevice: 2, AnsweredAtTerminal: 2, Expired: 1,
		MedianAnswerSecs: 30,                          // 10 s, 30 s, 90 s (the 3-day one is ignored)
		BlockedMinutes:   (30 + 90 + 10 + 540) / 60.0, // waits of answered and expired requests
		TopRules: []api.RuleCount{{Key: "allow for session", Count: 2}, {Key: "allow-tests", Count: 2},
			{Key: "no-force-push", Count: 2}, {Key: "ask-push", Count: 1}, {Key: "no-rm-rf", Count: 1}},
		TopBlocked: []api.RuleCount{{Key: "Run: git push -f", Count: 2}, {Key: "Run: rm -rf /", Count: 1}},
	}
	wantRules.BlockedMinutes = round1(wantRules.BlockedMinutes)
	if !reflect.DeepEqual(in.Rules, wantRules) {
		t.Errorf("rules = %+v\nwant  %+v", in.Rules, wantRules)
	}
	if in.Budget == nil || in.Budget.DailyUSD != 10 || in.Budget.SpentUSD != 2+0.01+3 || in.Budget.Percent != 50.1 || in.Budget.BurnUSDh != 1.23 {
		t.Errorf("budget = %+v", in.Budget)
	}
	if !in.From.Equal(rg.From) || !in.To.Equal(now) {
		t.Errorf("from/to = %v %v", in.From, in.To)
	}
}

func TestBuildEmpty(t *testing.T) {
	in := Build(Input{Range: NewRange(now, zone, 7), Loc: zone})
	if len(in.Days) != 7 || in.ByAgent == nil || in.ByModel == nil || in.ByRepo == nil || in.Budget == nil ||
		in.Budget.DailyUSD != 0 || in.Budget.Percent != 0 || in.Rules.MedianAnswerSecs != 0 {
		t.Errorf("empty insights = %+v", in)
	}
}

func TestMedian(t *testing.T) {
	cases := []struct {
		in   []float64
		want float64
	}{{nil, 0}, {[]float64{3}, 3}, {[]float64{4, 1}, 2.5}, {[]float64{5, 1, 3}, 3}}
	for _, c := range cases {
		if got := median(c.in); got != c.want {
			t.Errorf("median(%v) = %v", c.in, got)
		}
	}
}
