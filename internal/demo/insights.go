package demo

import (
	"context"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"path"
	"sort"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
)

// Insights (F6) from a deterministic history: every past day is built
// from synthetic sessions seeded by its date, so a day looks the same on
// every read, and today adds the live sessions. Totals, days and the
// by-agent/model/repo groups are all sums of the same records.

const day = 24 * time.Hour

// DailyBudgetUSD is the demo machine's usage.daily_budget_usd.
const DailyBudgetUSD = 40.0

type usageRec struct {
	agent, model, repo string
	usage              api.Usage
}

type dayRec struct {
	date                        time.Time
	recs                        []usageRec
	autoAllowed, asked, blocked int
	granted, byDevice, atDesk   int
	expired                     int
	answerSecs                  []float64
	ruleHits                    map[string]int
	blockHits                   map[string]int
}

var demoRepos = []string{"pilothouse", "api-server", "site", "proofline", "notes", "infra", "webapp"}

func seedFor(d time.Time) uint64 {
	h := fnv.New64a()
	h.Write([]byte(d.Format("2006-01-02")))
	return h.Sum64()
}

// history builds one past (or, partially, the current) day.
func history(d time.Time, fraction float64) dayRec {
	rng := rand.New(rand.NewPCG(seedFor(d), 0x9e3779b97f4a7c15))
	rec := dayRec{date: d, ruleHits: map[string]int{}, blockHits: map[string]int{}}
	n := 6 + rng.IntN(9)
	switch d.Weekday() {
	case time.Saturday, time.Sunday:
		n = 1 + rng.IntN(4)
	}
	n = int(math.Round(float64(n) * fraction))
	for i := 0; i < n; i++ {
		var agent, model string
		switch x := rng.Float64(); {
		case x < 0.56:
			agent, model = "claude", "claude-sonnet-4-5"
			if rng.Float64() < 0.18 {
				model = "claude-opus-4-1"
			}
		case x < 0.86:
			agent, model = "codex", "gpt-5-codex"
		default:
			agent, model = string(agentOpenCode), "glm-4.6"
		}
		in := int64(40000 + rng.IntN(380000))
		u := api.Usage{Model: model, InputTokens: in, OutputTokens: in/9 + int64(rng.IntN(9000)), Turns: 1 + rng.IntN(7)}
		if agent == "claude" {
			u.CacheReadTokens = in * int64(6+rng.IntN(10))
			u.CacheWriteTokens = in / 3
		} else {
			u.CacheReadTokens = in * int64(2+rng.IntN(5))
			if agent == "codex" {
				u.ReasoningTokens = u.OutputTokens / 2
			}
		}
		u.CostUSD = costOf(u)
		rec.recs = append(rec.recs, usageRec{agent: agent, model: model, repo: demoRepos[rng.IntN(len(demoRepos))], usage: u})
		calls := 18 + rng.IntN(70)
		asked := rng.IntN(4)
		blocked := 0
		if rng.Float64() < 0.22 {
			blocked = 1 + rng.IntN(2)
		}
		rec.autoAllowed += calls
		rec.asked += asked
		rec.blocked += blocked
		for j := 0; j < calls; j++ {
			switch x := rng.Float64(); {
			case x < 0.58:
				rec.ruleHits["autopilot-allow-routine"]++
			case x < 0.84:
				rec.ruleHits["allow-read-only"]++
			case x < 0.93:
				rec.ruleHits["allow-go-test"]++
			case x < 0.97:
				rec.ruleHits["allow-pnpm-test-proofline"]++
			default:
				rec.ruleHits["grant: allow for session"]++
			}
		}
		for j := 0; j < blocked; j++ {
			switch x := rng.Float64(); {
			case x < 0.5:
				rec.blockHits["autopilot-deny-destructive"]++
			case x < 0.75:
				rec.blockHits["no-secrets-files"]++
			case x < 0.9:
				rec.blockHits["no-curl-pipe-sh"]++
			default:
				rec.blockHits["self-protection"]++
			}
		}
		for j := 0; j < asked; j++ {
			switch x := rng.Float64(); {
			case x < 0.62:
				rec.byDevice++
				rec.answerSecs = append(rec.answerSecs, 8+rng.Float64()*70)
			case x < 0.92:
				rec.atDesk++
				rec.answerSecs = append(rec.answerSecs, 3+rng.Float64()*25)
			default:
				rec.expired++
			}
			if rng.Float64() < 0.2 {
				rec.granted++
			}
		}
	}
	return rec
}

// today is the morning's history plus the sessions on screen.
func (c *Core) today(midnight, now time.Time) dayRec {
	frac := now.Sub(midnight).Hours() / 24
	rec := history(midnight, max(0.25, frac*0.8))
	for _, s := range c.Sessions() {
		if s.Usage == nil || s.StartedAt.Before(midnight) {
			continue
		}
		repo := s.Title
		if s.RepoRoot != "" {
			repo = path.Base(s.RepoRoot)
		}
		rec.recs = append(rec.recs, usageRec{agent: string(s.Agent), model: s.Usage.Model, repo: repo, usage: *s.Usage})
	}
	for _, r := range c.Requests() {
		if r.CreatedAt.Before(midnight) {
			continue
		}
		rec.asked++
		switch {
		case r.Status == api.RequestExpired:
			rec.expired++
		case r.Decision != nil && r.Decision.By == "cli", r.Decision != nil && r.Decision.By == "tui", r.Status == api.RequestAnsweredLocally:
			rec.atDesk++
		case r.Decision != nil:
			rec.byDevice++
			if r.ClosedAt != nil {
				rec.answerSecs = append(rec.answerSecs, r.ClosedAt.Sub(r.CreatedAt).Seconds())
			}
			if r.Decision.Scope == api.ScopeSession {
				rec.granted++
			}
		}
	}
	return rec
}

// Insights aggregates the last q.Days days (default 7, max 90).
func (c *Core) Insights(ctx context.Context, q api.InsightsQuery) (api.Insights, error) {
	days := q.Days
	if days <= 0 {
		days = 7
	}
	days = min(days, 90)
	now := c.now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	var recs []dayRec
	for i := days - 1; i >= 1; i-- {
		recs = append(recs, history(midnight.AddDate(0, 0, -i), 1))
	}
	recs = append(recs, c.today(midnight, now))

	in := api.Insights{From: midnight.AddDate(0, 0, -(days - 1)), To: now, Days: []api.InsightsDay{}}
	groups := map[string]map[string]*api.UsageGroup{"agent": {}, "model": {}, "repo": {}}
	group := func(kind, key string, u api.Usage) {
		g := groups[kind][key]
		if g == nil {
			g = &api.UsageGroup{Key: key}
			groups[kind][key] = g
		}
		model := g.Usage.Model
		g.Usage.Add(u)
		if kind != "model" {
			g.Usage.Model = model // a group of many models names none
		}
		g.Sessions++
	}
	var secs []float64
	hits := map[string]int{}
	blocks := map[string]int{}
	st := &in.Rules
	for _, d := range recs {
		day := api.InsightsDay{Date: d.date.Format("2006-01-02"), AutoAllowed: d.autoAllowed, Asked: d.asked, Blocked: d.blocked}
		for _, r := range d.recs {
			day.Usage.Add(r.usage)
			day.Sessions++
			group("agent", r.agent, r.usage)
			group("model", r.model, r.usage)
			group("repo", r.repo, r.usage)
		}
		day.Usage.Model = ""
		day.Usage.CostUSD = round2(day.Usage.CostUSD)
		in.Total.Add(day.Usage)
		in.Days = append(in.Days, day)
		st.AutoAllowed += d.autoAllowed
		st.Asked += d.asked
		st.Blocked += d.blocked
		st.Granted += d.granted
		st.AnsweredByDevice += d.byDevice
		st.AnsweredAtTerminal += d.atDesk
		st.Expired += d.expired
		secs = append(secs, d.answerSecs...)
		for k, v := range d.ruleHits {
			hits[k] += v
		}
		for k, v := range d.blockHits {
			blocks[k] += v
		}
	}
	in.Total.CostUSD = round2(in.Total.CostUSD)
	in.ByAgent = sortedGroups(groups["agent"])
	in.ByModel = sortedGroups(groups["model"])
	in.ByRepo = sortedGroups(groups["repo"])
	sort.Float64s(secs)
	if len(secs) > 0 {
		st.MedianAnswerSecs = math.Round(secs[len(secs)/2])
		total := 0.0
		for _, s := range secs {
			total += s
		}
		st.BlockedMinutes = math.Round(total/60*10) / 10
	}
	st.TopRules = topCounts(hits, 6)
	st.TopBlocked = topCounts(blocks, 5)

	spent := in.Days[len(in.Days)-1].Usage.CostUSD
	hours := max(1, now.Sub(midnight).Hours())
	in.Budget = &api.BudgetStatus{
		DailyUSD: DailyBudgetUSD,
		SpentUSD: spent,
		Percent:  math.Round(spent/DailyBudgetUSD*1000) / 10,
		BurnUSDh: round2(spent / hours * 1.6), // the afternoon is busier than the night
	}
	return in, nil
}

func sortedGroups(m map[string]*api.UsageGroup) []api.UsageGroup {
	out := make([]api.UsageGroup, 0, len(m))
	for _, g := range m {
		g.Usage.CostUSD = round2(g.Usage.CostUSD)
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Usage.CostUSD != out[j].Usage.CostUSD {
			return out[i].Usage.CostUSD > out[j].Usage.CostUSD
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func topCounts(m map[string]int, n int) []api.RuleCount {
	out := make([]api.RuleCount, 0, len(m))
	for k, v := range m {
		out = append(out, api.RuleCount{Key: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Key < out[j].Key
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
