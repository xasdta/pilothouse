package demo

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
	"github.com/xasdta/pilothouse/internal/transcript"
)

// feed is one session's activity: append-only items plus what they add up to.
type feed struct {
	items []api.ActivityItem
	files []api.FileChange // first touch first
	usage api.Usage
	turn  int
}

// itemRef points at one activity item.
type itemRef struct {
	session string
	index   int
}

// price is a model's list price in USD per million tokens.
type price struct{ in, out, cacheRead, cacheWrite float64 }

var prices = map[string]price{
	"claude-sonnet-4-5": {3, 15, 0.30, 3.75},
	"claude-opus-4-1":   {15, 75, 1.50, 18.75},
	"claude-haiku-4-5":  {1, 5, 0.10, 1.25},
	"gpt-5-codex":       {1.25, 10, 0.125, 0},
	"gpt-5-mini":        {0.25, 2, 0.025, 0},
	"glm-4.6":           {0.60, 2.20, 0.11, 0},
}

// costOf estimates u's cost at its model's list price.
func costOf(u api.Usage) float64 {
	p, ok := prices[u.Model]
	if !ok {
		return 0
	}
	c := float64(u.InputTokens)*p.in + float64(u.OutputTokens+u.ReasoningTokens)*p.out +
		float64(u.CacheReadTokens)*p.cacheRead + float64(u.CacheWriteTokens)*p.cacheWrite
	return float64(int64(c/1e6*10000+0.5)) / 10000 // to 1/100 cent
}

// usageFor is what one activity item costs in tokens, roughly as agents do:
// tool calls re-send the context (cache reads), text costs output.
func usageFor(model string, it api.ActivityItem) api.Usage {
	u := api.Usage{Model: model}
	n := int64(len(it.Text) + len(it.Result) + len(it.Summary))
	switch it.Kind {
	case api.ActivityTool:
		u.InputTokens, u.OutputTokens = 2400+n/3, 180+n/12
	case api.ActivityText:
		u.InputTokens, u.OutputTokens = 1200, 120+n/3
	case api.ActivityPrompt:
		u.InputTokens = 800 + n/3
		u.Turns = 1
	case api.ActivitySubagent:
		u.InputTokens, u.OutputTokens = 9000, 1400
	default:
		return u
	}
	if strings.HasPrefix(model, "claude") {
		u.CacheReadTokens = 38000 + u.InputTokens*6
		u.CacheWriteTokens = u.InputTokens / 2
	} else {
		u.CacheReadTokens = 21000 + u.InputTokens*3
	}
	if strings.HasPrefix(model, "gpt-5") && it.Kind != api.ActivityPrompt {
		u.ReasoningTokens = u.OutputTokens / 2
	}
	return u
}

// addLocked appends an item to a session's feed and touches the session
// (its usage and UpdatedAt), so clients re-fetch the feed. c.mu is held.
func (c *Core) addLocked(sid string, it api.ActivityItem) itemRef {
	f := c.feeds[sid]
	if f == nil {
		f = &feed{}
		c.feeds[sid] = f
	}
	if it.At.IsZero() {
		it.At = c.now()
	}
	if it.Kind == api.ActivityPrompt {
		f.turn++
	}
	if f.turn == 0 {
		f.turn = 1
	}
	it.Turn = f.turn
	it.ID = fmt.Sprintf("%s-%04d", sid, len(f.items)+1)
	f.items = append(f.items, it)
	s, ok := c.session(sid)
	model := ""
	if ok && s.Usage != nil {
		model = s.Usage.Model
	}
	if model == "" {
		model = modelFor(s.Agent)
	}
	u := usageFor(model, it)
	u.CostUSD = costOf(u)
	f.usage.Add(u)
	f.usage.CostUSD = costOf(f.usage)
	if ok {
		usage := f.usage
		c.UpdateSession(sid, func(s *api.Session) {
			s.Usage = &usage
			if !it.At.Before(s.UpdatedAt) {
				s.UpdatedAt = it.At
			}
		})
	}
	c.renderLocked(sid, it)
	return itemRef{session: sid, index: len(f.items) - 1}
}

// fileLocked records a file change in a session's "files changed" list.
func (c *Core) fileLocked(sid string, fc api.FileChange) {
	f := c.feeds[sid]
	if f == nil {
		f = &feed{}
		c.feeds[sid] = f
	}
	for i := range f.files {
		if f.files[i].Path == fc.Path {
			f.files[i].Added += fc.Added
			f.files[i].Removed += fc.Removed
			if fc.Op == api.FileDelete {
				f.files[i].Op = fc.Op
			}
			return
		}
	}
	f.files = append(f.files, fc)
}

// finishLocked folds a result into a pending tool item.
func (c *Core) finishLocked(ref itemRef, result string, isErr bool) {
	f := c.feeds[ref.session]
	if f == nil || ref.index < 0 || ref.index >= len(f.items) {
		return
	}
	it := &f.items[ref.index]
	it.Pending = false
	it.Result = result
	it.IsError = isErr
	c.UpdateSession(ref.session, func(s *api.Session) { s.UpdatedAt = c.now() })
	c.renderResultLocked(ref.session, *it)
}

func modelFor(a api.AgentKind) string {
	switch a {
	case api.AgentCodex:
		return "gpt-5-codex"
	case agentOpenCode:
		return "glm-4.6"
	default:
		return "claude-sonnet-4-5"
	}
}

const (
	defaultActivityLimit = 200
	maxActivityLimit     = 1000
)

// Activity returns at most Limit newest items; with a Cursor only the
// items added since that read.
func (c *Core) Activity(ctx context.Context, sessionID string, q api.ActivityQuery) (api.Activity, error) {
	s, err := c.FakeCore.Session(ctx, sessionID)
	if err != nil {
		return api.Activity{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	f := c.feeds[s.ID]
	if !s.Caps.Activity && s.State != api.StateExited || f == nil {
		return api.Activity{SessionID: s.ID, Available: false, Items: []api.ActivityItem{},
			Note: "No transcript Pilothouse can read for this session."}, nil
	}
	from := 0
	if q.Cursor != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(q.Cursor, "c"))
		if err != nil || n < 0 || !strings.HasPrefix(q.Cursor, "c") {
			return api.Activity{}, core.Invalid("unknown cursor")
		}
		from = min(n, len(f.items))
	}
	limit := q.Limit
	if limit <= 0 {
		limit = defaultActivityLimit
	}
	limit = min(limit, maxActivityLimit)
	if len(f.items)-from > limit {
		from = len(f.items) - limit
	}
	items := append([]api.ActivityItem{}, f.items[from:]...)
	usage := f.usage
	since := q.Since
	if since.IsZero() {
		since = c.seen[s.ID]
	}
	if since.IsZero() {
		since = s.StartedAt
	}
	return api.Activity{
		SessionID: s.ID,
		Available: true,
		Items:     items,
		Cursor:    "c" + strconv.Itoa(len(f.items)),
		Usage:     &usage,
		Files:     append([]api.FileChange{}, f.files...),
		CatchUp:   catchUp(s, f.items, since, c.now()),
		Receipt:   transcript.ReceiptOf(f.items, f.files),
	}, nil
}

// catchUp summarizes what happened since a moment, deterministically:
// tool calls, files, tests and their last result, questions, errors.
func catchUp(s api.Session, items []api.ActivityItem, since, now time.Time) *api.CatchUp {
	cu := &api.CatchUp{Since: since, Lines: []string{}}
	files := map[string]bool{}
	var fileOrder []string
	var lastTest *api.ActivityItem
	testFailed := false
	var lastText, pending string
	replies := 0
	for i := range items {
		it := items[i]
		if it.At.Before(since) {
			continue
		}
		switch it.Kind {
		case api.ActivityTool:
			cu.ToolCalls++
			if it.IsError {
				cu.Errors++
			}
			if it.ToolKind == api.ToolEdit {
				p := strings.TrimPrefix(strings.TrimPrefix(it.Summary, "Edit "), "Write ")
				if !files[p] {
					files[p] = true
					fileOrder = append(fileOrder, path.Base(p))
				}
			}
			if isTest(it) {
				lastTest = &items[i]
				if it.IsError {
					testFailed = true
				}
			}
			if it.Pending {
				pending = it.Summary
			}
		case api.ActivityError:
			cu.Errors++
		case api.ActivityText:
			lastText = it.Text
		case api.ActivityPrompt:
			if it.Source == "pilothouse" {
				replies++
			}
		}
	}
	cu.Files = len(fileOrder)
	if cu.ToolCalls == 0 && lastText == "" {
		cu.Lines = append(cu.Lines, "Nothing new since you last looked.")
		return cu
	}
	if s.State == api.StateBlocked && pending != "" {
		cu.Lines = append(cu.Lines, "Waiting for you: "+strings.TrimPrefix(pending, "Run: "))
	}
	if lastTest != nil {
		switch {
		case lastTest.Pending:
			cu.Lines = append(cu.Lines, "Tests are running ("+commandOf(*lastTest)+")")
		case lastTest.IsError:
			cu.Lines = append(cu.Lines, "Tests are failing ("+commandOf(*lastTest)+")")
		case testFailed:
			cu.Lines = append(cu.Lines, "Tests pass again after a failure ("+commandOf(*lastTest)+")")
		default:
			cu.Lines = append(cu.Lines, "Tests pass ("+commandOf(*lastTest)+")")
		}
	}
	if len(fileOrder) > 0 {
		names := fileOrder
		more := ""
		if len(names) > 3 {
			more = fmt.Sprintf(" and %d more", len(names)-3)
			names = names[:3]
		}
		cu.Lines = append(cu.Lines, fmt.Sprintf("Changed %d %s: %s%s.", len(fileOrder), plural(len(fileOrder), "file", "files"), strings.Join(names, ", "), more))
	}
	cu.Lines = append(cu.Lines, fmt.Sprintf("%d tool %s in %s.", cu.ToolCalls, plural(cu.ToolCalls, "call", "calls"), span(now.Sub(since))))
	if replies > 0 {
		cu.Lines = append(cu.Lines, fmt.Sprintf("You sent %d %s from Pilothouse.", replies, plural(replies, "reply", "replies")))
	}
	if lastText != "" {
		cu.Lines = append(cu.Lines, "Last said: “"+clip(lastText, 140)+"”")
	}
	return cu
}

func isTest(it api.ActivityItem) bool {
	cmd := strings.ToLower(it.Summary)
	return it.ToolKind == api.ToolShell && (strings.Contains(cmd, " test") || strings.Contains(cmd, "-bench") || strings.Contains(cmd, "pytest") || strings.Contains(cmd, "vitest"))
}

func commandOf(it api.ActivityItem) string {
	return strings.TrimPrefix(it.Summary, "Run: ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func span(d time.Duration) string {
	switch {
	case d < 2*time.Minute:
		return "the last minute"
	case d < 90*time.Minute:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%.0f h", d.Hours())
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}
