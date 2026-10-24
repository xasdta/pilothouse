package insights

import (
	"math"
	"sort"
	"strings"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/store"
)

// Range limits (api.InsightsQuery).
const (
	DefaultDays = 7
	MaxDays     = 90
	topN        = 5
	// maxWait ignores request durations longer than this: a request left
	// open over a weekend says nothing about how fast people answer.
	maxWait = 24 * time.Hour
)

// ClampDays applies the default and the maximum.
func ClampDays(days int) int {
	switch {
	case days <= 0:
		return DefaultDays
	case days > MaxDays:
		return MaxDays
	}
	return days
}

// Range is the window of an insights query: the last days calendar days
// in loc, today included.
type Range struct {
	From, To       time.Time // From is local midnight `days-1` days ago; To is now
	FromDay, ToDay string    // YYYY-MM-DD
	Days           []string  // every day in the range, oldest first
}

// NewRange computes the range ending now.
func NewRange(now time.Time, loc *time.Location, days int) Range {
	days = ClampDays(days)
	local := now.In(loc)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	r := Range{From: today.AddDate(0, 0, -(days - 1)), To: now}
	for i := days - 1; i >= 0; i-- {
		r.Days = append(r.Days, today.AddDate(0, 0, -i).Format("2006-01-02"))
	}
	r.FromDay, r.ToDay = r.Days[0], r.Days[len(r.Days)-1]
	return r
}

// Input is what Build aggregates.
type Input struct {
	Range     Range
	Loc       *time.Location
	Usage     []store.UsageRow    // usage_daily rows in the range
	Events    []store.DecisionRow // decision events in the range
	BudgetUSD float64             // usage.daily_budget_usd (0: none)
	BurnUSDh  float64             // estimated spend in the last hour
}

// Build aggregates usage and decisions. Budget is always set: DailyUSD 0
// means no budget is configured, and the spend and burn rate still show.
func Build(in Input) api.Insights {
	loc := in.Loc
	if loc == nil {
		loc = time.Local
	}
	out := api.Insights{From: in.Range.From, To: in.Range.To, Days: make([]api.InsightsDay, len(in.Range.Days)),
		ByAgent: []api.UsageGroup{}, ByModel: []api.UsageGroup{}, ByRepo: []api.UsageGroup{}}
	dayIdx := map[string]int{}
	for i, d := range in.Range.Days {
		out.Days[i].Date = d
		dayIdx[d] = i
	}

	agents, models, repos := groups{}, groups{}, groups{}
	daySessions := make([]map[string]bool, len(out.Days))
	for _, u := range in.Usage {
		i, ok := dayIdx[u.Day]
		if !ok {
			continue
		}
		us := api.Usage{InputTokens: u.Input, OutputTokens: u.Output, CacheReadTokens: u.CacheRead,
			CacheWriteTokens: u.CacheWrite, ReasoningTokens: u.Reasoning, CostUSD: u.CostUSD, Turns: u.Turns}
		out.Days[i].Usage.Add(us)
		out.Total.Add(us)
		if daySessions[i] == nil {
			daySessions[i] = map[string]bool{}
		}
		daySessions[i][u.SessionID] = true
		agents.add(orUnknown(string(u.Agent)), u.SessionID, us)
		mu := us
		mu.Model = u.Model
		models.add(orUnknown(u.Model), u.SessionID, mu)
		repos.add(orNone(u.Repo), u.SessionID, us)
	}
	for i, s := range daySessions {
		out.Days[i].Sessions = len(s)
	}
	out.ByAgent, out.ByModel, out.ByRepo = agents.list(), models.list(), repos.list()

	out.Rules = ruleStats(in.Events, loc, dayIdx, out.Days)

	b := &api.BudgetStatus{DailyUSD: in.BudgetUSD, BurnUSDh: round2(in.BurnUSDh)}
	if n := len(out.Days); n > 0 {
		b.SpentUSD = out.Days[n-1].Usage.CostUSD
	}
	if b.DailyUSD > 0 {
		b.Percent = math.Round(b.SpentUSD/b.DailyUSD*1000) / 10
	}
	out.Budget = b
	return out
}

func ruleStats(events []store.DecisionRow, loc *time.Location, dayIdx map[string]int, days []api.InsightsDay) api.RuleStats {
	var st api.RuleStats
	topRules, topBlocked := counter{}, counter{}
	var answers []float64
	var blocked time.Duration
	day := func(t time.Time) *api.InsightsDay {
		if i, ok := dayIdx[t.In(loc).Format("2006-01-02")]; ok {
			return &days[i]
		}
		return &api.InsightsDay{} // outside the range: counted in totals only
	}
	for _, e := range events {
		switch e.Type {
		case api.EventPolicyDecision:
			switch e.Outcome {
			case api.BehaviorAllow:
				st.AutoAllowed++
				day(e.At).AutoAllowed++
				if e.Grant != "" {
					st.Granted++
					topRules.add("allow for session")
				} else {
					topRules.add(e.Rule)
				}
			case api.BehaviorDeny:
				st.Blocked++
				day(e.At).Blocked++
				topRules.add(e.Rule)
				topBlocked.add(firstNonEmpty(e.Summary, e.Rule))
			case api.BehaviorAsk:
				topRules.add(e.Rule)
			}
		case api.EventRequestOpened:
			st.Asked++
			day(e.At).Asked++
		case api.EventRequestClosed:
			wait := e.ClosedAt.Sub(e.CreatedAt)
			valid := !e.CreatedAt.IsZero() && !e.ClosedAt.IsZero() && wait >= 0 && wait <= maxWait
			switch {
			case strings.HasPrefix(e.By, "grant:"):
				// Answered by "allow for session" at the permission
				// checkpoint: nobody was asked.
				st.AutoAllowed++
				st.Granted++
				day(e.At).AutoAllowed++
				topRules.add("allow for session")
				continue
			case e.Status == api.RequestDecided && (strings.HasPrefix(e.By, "device:") || strings.HasPrefix(e.By, "telegram:")):
				st.AnsweredByDevice++
			case e.Status == api.RequestDecided && (e.By == "cli" || e.By == "tui"):
				st.AnsweredAtTerminal++
			case e.Status == api.RequestAnsweredLocally:
				st.AnsweredAtTerminal++
			case e.Status == api.RequestExpired:
				st.Expired++
			}
			if !valid {
				continue
			}
			blocked += wait
			if e.Status == api.RequestDecided || e.Status == api.RequestAnsweredLocally {
				answers = append(answers, wait.Seconds())
			}
		}
	}
	st.MedianAnswerSecs = round1(median(answers))
	st.BlockedMinutes = round1(blocked.Minutes())
	st.TopRules, st.TopBlocked = topRules.top(topN), topBlocked.top(topN)
	return st
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sort.Float64s(xs)
	n := len(xs)
	if n%2 == 1 {
		return xs[n/2]
	}
	return (xs[n/2-1] + xs[n/2]) / 2
}

func round1(x float64) float64 { return math.Round(x*10) / 10 }
func round2(x float64) float64 { return math.Round(x*100) / 100 }

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func orNone(s string) string {
	if s == "" {
		return "(no repo)"
	}
	return s
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

type groups map[string]*groupAgg

type groupAgg struct {
	usage    api.Usage
	sessions map[string]bool
}

func (g groups) add(key, session string, u api.Usage) {
	a := g[key]
	if a == nil {
		a = &groupAgg{sessions: map[string]bool{}}
		g[key] = a
	}
	a.usage.Add(u)
	a.sessions[session] = true
}

// list sorts groups by cost, then tokens, then name.
func (g groups) list() []api.UsageGroup {
	out := make([]api.UsageGroup, 0, len(g))
	for k, a := range g {
		out = append(out, api.UsageGroup{Key: k, Usage: a.usage, Sessions: len(a.sessions)})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Usage.CostUSD != b.Usage.CostUSD {
			return a.Usage.CostUSD > b.Usage.CostUSD
		}
		ta, tb := tokens(a.Usage), tokens(b.Usage)
		if ta != tb {
			return ta > tb
		}
		return a.Key < b.Key
	})
	return out
}

func tokens(u api.Usage) int64 {
	return u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

type counter map[string]int

func (c counter) add(k string) {
	if k = strings.TrimSpace(k); k != "" {
		c[k]++
	}
}

func (c counter) top(n int) []api.RuleCount {
	out := make([]api.RuleCount, 0, len(c))
	for k, v := range c {
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
