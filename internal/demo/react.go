package demo

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
)

// How the demo agents react to people: answers, plan feedback, replies,
// interrupts and requests that time out. Reactions are scheduled with
// later() and run from Run, so a person sees the agent pick up a moment
// after answering.

// reqScript says what the agent does once its request is answered.
type reqScript struct {
	allowResult string    // tool result on allow
	allowSay    string    // the agent's next words on allow ({answers} for questions)
	denySay     string    // on deny (the person's message is appended)
	after       api.State // working: it carries on; waiting: its turn ends
	detail      string    // state detail while it carries on
}

// pendingRun is a tool call that is running (not held for a person).
type pendingRun struct {
	ref    itemRef
	result string
	isErr  bool
}

// raise opens a request on a session the way an agent does: the session
// blocks, the request is held for devices, a pending tool call shows in
// the feed and the terminal.
func (c *Core) raise(r api.Request, tool api.ActivityItem, sc reqScript) api.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.raiseLocked(r, tool, sc)
}

func (c *Core) raiseLocked(r api.Request, tool api.ActivityItem, sc reqScript) api.Request {
	now := c.now()
	s, _ := c.session(r.SessionID)
	if r.ID == "" {
		c.nextID++
		r.ID = fmt.Sprintf("r_demo%04d", c.nextID)
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	if r.ExpiresAt.IsZero() {
		r.ExpiresAt = r.CreatedAt.Add(10 * time.Minute)
	}
	r.Status, r.Route = api.RequestOpen, api.RouteDevices
	if r.Agent == "" {
		r.Agent = s.Agent
	}
	if r.Cwd == "" {
		r.Cwd = s.Cwd
	}
	block := api.BlockPermission
	detail := "Asked to " + lowerFirst(strings.TrimPrefix(r.Summary, "Run: "))
	switch r.Checkpoint {
	case api.CheckpointQuestion:
		block = api.BlockQuestion
		detail = "Asked " + strings.ToLower(questionsLine(r.Questions))
	case api.CheckpointPlan:
		detail = "Wants a plan approved"
	default:
		if r.ToolKind == api.ToolShell {
			detail = "Asked to run " + r.Command
		} else if r.ToolKind == api.ToolEdit && len(r.Paths) > 0 {
			detail = "Asked to edit " + strings.TrimPrefix(r.Paths[0], strings.TrimSuffix(r.Cwd, "/")+"/")
			if len(r.Paths) > 1 {
				detail = fmt.Sprintf("Asked to edit %d files", len(r.Paths))
			}
		}
	}
	c.UpdateSession(r.SessionID, func(s *api.Session) {
		s.State, s.BlockKind, s.StateDetail, s.Unseen = api.StateBlocked, block, detail, true
		s.UpdatedAt = r.CreatedAt
	})
	c.PutRequest(r)
	if tool.Kind == "" {
		tool = api.ActivityItem{Kind: api.ActivityTool, Tool: r.Tool, ToolKind: r.ToolKind, Summary: r.Summary}
	}
	tool.Pending = true
	if tool.At.IsZero() {
		tool.At = r.CreatedAt
	}
	c.reqItem[r.ID] = c.addLocked(r.SessionID, tool)
	c.reqMeta[r.ID] = sc
	return r
}

func questionsLine(qs []api.Question) string {
	if len(qs) == 0 {
		return "a question"
	}
	if len(qs) > 1 {
		return fmt.Sprintf("%d questions", len(qs))
	}
	return strings.TrimSuffix(qs[0].Text, "?")
}

// Decide validates answers like the daemon does, decides, and lets the
// agent react.
func (c *Core) Decide(ctx context.Context, d api.Decision) (api.Request, error) {
	r, err := c.FakeCore.Request(ctx, d.RequestID)
	if err != nil {
		return api.Request{}, err
	}
	if r.Status == api.RequestOpen {
		if err := validAnswers(r, d); err != nil {
			return api.Request{}, err
		}
	}
	closed, err := c.FakeCore.Decide(ctx, d)
	if err != nil {
		return closed, err
	}
	c.react(closed)
	return closed, nil
}

// validAnswers: every question answered, with one of its options unless
// it allows free text; answers only for questions.
func validAnswers(r api.Request, d api.Decision) error {
	if r.Checkpoint != api.CheckpointQuestion {
		if len(d.Answers) > 0 {
			return core.Invalid("answers only go with questions")
		}
		return nil
	}
	if d.Behavior != api.BehaviorAllow {
		return nil
	}
	keys := map[string]api.Question{}
	for _, q := range r.Questions {
		keys[q.Key()] = q
	}
	for k := range d.Answers {
		if _, ok := keys[k]; !ok {
			return core.Invalid("there is no question %q", k)
		}
	}
	for _, q := range r.Questions {
		v := strings.TrimSpace(d.Answers[q.Key()])
		if v == "" {
			return core.Invalid("answer “%s” first", q.Text)
		}
		if q.AllowOther {
			continue
		}
		labels := []string{v}
		if q.MultiSelect {
			labels = strings.Split(v, api.MultiAnswerSep)
		}
		for _, l := range labels {
			ok := false
			for _, o := range q.Options {
				if o.Label == l {
					ok = true
				}
			}
			if !ok {
				return core.Invalid("“%s” isn't one of the options for “%s”", l, q.Text)
			}
		}
	}
	return nil
}

// react schedules what the agent does after an answer.
func (c *Core) react(r api.Request) {
	if r.Decision == nil {
		return
	}
	c.mu.Lock()
	ref, hasRef := c.reqItem[r.ID]
	sc := c.reqMeta[r.ID]
	delete(c.reqItem, r.ID)
	c.mu.Unlock()
	d := *r.Decision
	allow := d.Behavior == api.BehaviorAllow
	finish := func(result string, isErr bool) {
		if hasRef {
			c.mu.Lock()
			c.finishLocked(ref, result, isErr)
			c.mu.Unlock()
		}
	}
	sid := r.SessionID
	switch r.Checkpoint {
	case api.CheckpointQuestion:
		if !allow {
			finish("The person declined to answer from Pilothouse."+suffix(d.Message), true)
			c.later(1800*time.Millisecond, func() {
				c.say(sid, "No problem — I'll go with the simplest option and note it in the summary so you can change it.", api.StateWorking, "Building the search index")
			})
			return
		}
		var parts []string
		for _, q := range r.Questions {
			parts = append(parts, q.Header+": "+d.Answers[q.Key()])
		}
		finish("Answered: "+strings.Join(parts, " · "), false)
		say := sc.allowSay
		for _, q := range r.Questions {
			say = strings.ReplaceAll(say, "{"+q.Key()+"}", d.Answers[q.Key()])
		}
		if say == "" {
			say = "Thanks — going with " + strings.Join(parts, ", ") + "."
		}
		c.later(1500*time.Millisecond, func() { c.say(sid, say, api.StateWorking, sc.detail) })
		c.later(4*time.Second, func() {
			c.tool(sid, api.ActivityItem{Kind: api.ActivityTool, Tool: "edit", ToolKind: api.ToolEdit, Summary: "Edit src/db/schema.sql",
				Result: "+ CREATE VIRTUAL TABLE notes_fts USING fts5(title, body, tags, content='notes');"},
				&api.FileChange{Path: "src/db/schema.sql", Op: api.FileEdit, Added: 9, Removed: 0})
		})
	case api.CheckpointPlan:
		if !allow {
			finish("Changes requested"+suffix(d.Message), false)
			c.later(1500*time.Millisecond, func() {
				c.say(sid, "Revising the plan"+suffix(d.Message), api.StateWorking, "Revising the plan")
			})
			c.later(5*time.Second, func() { c.revisePlan(r, d.Message) })
			return
		}
		finish("Plan approved — leaving plan mode", false)
		say := sc.allowSay
		if say == "" {
			say = "Plan approved. Starting with step 1."
		}
		c.later(1500*time.Millisecond, func() { c.say(sid, say, api.StateWorking, sc.detail) })
		c.later(4*time.Second, func() {
			c.tool(sid, api.ActivityItem{Kind: api.ActivityTool, Tool: "Bash", ToolKind: api.ToolShell, Summary: "Run: npm install @simplewebauthn/server",
				Result: "added 4 packages in 2s"}, nil)
		})
	default:
		if !allow {
			msg := "Denied from Pilothouse" + suffix(d.Message)
			if !strings.HasPrefix(d.By, "device:") {
				msg = "Denied" + suffix(d.Message)
			}
			finish(msg, true)
			say := sc.denySay
			if say == "" {
				say = "Understood — I won't do that."
			}
			if d.Message != "" {
				say += " I'll follow your note: “" + clip(d.Message, 120) + "”"
			}
			c.later(1500*time.Millisecond, func() { c.say(sid, say, api.StateWaiting, "") })
			return
		}
		result := sc.allowResult
		if result == "" {
			result = "Done"
		}
		c.later(700*time.Millisecond, func() { finish(result, false) })
		say := sc.allowSay
		after := sc.after
		if after == "" {
			after = api.StateWaiting
		}
		if say != "" {
			c.later(2200*time.Millisecond, func() { c.say(sid, say, after, sc.detail) })
		} else if after == api.StateWorking {
			c.later(1200*time.Millisecond, func() { c.setState(sid, api.StateWorking, sc.detail, "") })
		}
	}
}

func suffix(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return ""
	}
	return ": " + clip(msg, 300)
}

// say adds the agent's words and moves the session on: waiting ends the
// turn (the words become its last message), working carries on.
func (c *Core) say(sid, text string, state api.State, detail string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.session(sid)
	if !ok || s.State == api.StateExited {
		return
	}
	c.addLocked(sid, api.ActivityItem{Kind: api.ActivityText, Text: text})
	c.setStateLocked(sid, state, detail, text)
}

func (c *Core) setState(sid string, state api.State, detail, last string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setStateLocked(sid, state, detail, last)
}

func (c *Core) setStateLocked(sid string, state api.State, detail, last string) {
	open := false
	for _, r := range c.Requests() {
		if r.SessionID == sid && r.Status == api.RequestOpen {
			open = true
		}
	}
	c.UpdateSession(sid, func(s *api.Session) {
		if s.State == api.StateExited || open && s.State == api.StateBlocked {
			return // still waiting on another request
		}
		s.State, s.BlockKind, s.StateDetail = state, api.BlockNone, detail
		if last != "" {
			s.LastMessage = clip(last, 400)
		}
		if state == api.StateWaiting {
			s.Unseen = true
			s.StateDetail = ""
		}
		s.UpdatedAt = c.now()
	})
}

// tool adds a finished tool call (and the file it changed).
func (c *Core) tool(sid string, it api.ActivityItem, fc *api.FileChange) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.session(sid); !ok || s.State == api.StateExited {
		return
	}
	c.addLocked(sid, it)
	if fc != nil {
		c.fileLocked(sid, *fc)
	}
}

// revisePlan raises the plan again with the feedback worked in.
func (c *Core) revisePlan(old api.Request, feedback string) {
	if old.Plan == nil {
		return
	}
	c.mu.Lock()
	c.revision[old.SessionID]++
	n := c.revision[old.SessionID] + 1
	c.mu.Unlock()
	md := old.Plan.Markdown
	if i := strings.Index(md, "\n## What changed"); i >= 0 {
		if j := strings.Index(md[i+1:], "\n## "); j >= 0 {
			md = md[:i] + md[i+1+j:]
		}
	}
	note := "## What changed (revision " + fmt.Sprint(n) + ")\n- " + strings.ReplaceAll(strings.TrimSpace(feedback), "\n", " ") + "\n"
	if strings.TrimSpace(feedback) == "" {
		note = "## What changed (revision " + fmt.Sprint(n) + ")\n- Tightened the scope and split the work into smaller steps.\n"
	}
	if i := strings.Index(md, "\n## "); i >= 0 {
		md = md[:i+1] + note + md[i:]
	} else {
		md += "\n" + note
	}
	r := api.Request{SessionID: old.SessionID, Agent: old.Agent, Checkpoint: api.CheckpointPlan, Tool: old.Tool, ToolKind: old.ToolKind,
		Summary: old.Summary, Plan: &api.Plan{Markdown: md, Path: old.Plan.Path}, Input: old.Input,
		Context: "Here's the revised plan with your feedback worked in."}
	c.raise(r, api.ActivityItem{}, reqScript{allowSay: "Plan approved. Starting with step 1.", after: api.StateWorking, detail: "Implementing the plan"})
}

// Reply sends a follow-up prompt or an interrupt (F2).
func (c *Core) Reply(ctx context.Context, sessionID string, r api.Reply, by string) (api.ReplyResult, error) {
	s, err := c.FakeCore.Session(ctx, sessionID)
	if err != nil {
		return api.ReplyResult{}, err
	}
	switch {
	case s.State == api.StateExited:
		return api.ReplyResult{}, fmt.Errorf("%w: this session has ended", core.ErrUnsupported)
	case !s.Caps.Reply && s.Agent == api.AgentCodex:
		return api.ReplyResult{}, fmt.Errorf("%w: Codex sessions started outside pilothouse run can't receive replies — start the next one with pilothouse run codex", core.ErrUnsupported)
	case !s.Caps.Reply:
		return api.ReplyResult{}, fmt.Errorf("%w: this session can't receive replies", core.ErrUnsupported)
	case r.Interrupt && !s.Caps.Interrupt:
		return api.ReplyResult{}, fmt.Errorf("%w: only sessions started with pilothouse run can be interrupted", core.ErrUnsupported)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.replyLocked(s, r, by), nil
}

func (c *Core) replyLocked(s api.Session, r api.Reply, by string) api.ReplyResult {
	via := s.Caps.ReplyVia
	if via == "" {
		via = api.ReplyTerminal
	}
	now := c.now()
	_, _ = c.Emit(context.Background(), api.EventSessionReply, s.ID, "", api.ReplyRecord{
		SessionID: s.ID, Text: r.Text, Interrupt: r.Interrupt, Via: via, By: by, At: now.UTC()})
	if r.Interrupt {
		if run, ok := c.runningLocked(s.ID); ok {
			c.finishLocked(run.ref, "Interrupted", true)
		}
		c.addLocked(s.ID, api.ActivityItem{Kind: api.ActivitySystem, Text: "Interrupted from Pilothouse"})
		if r.Text == "" {
			c.setStateLocked(s.ID, api.StateWaiting, "", "Interrupted — what should I do instead?")
		}
	}
	if r.Text != "" {
		c.addLocked(s.ID, api.ActivityItem{Kind: api.ActivityPrompt, Text: r.Text, Source: "pilothouse"})
		c.setStateLocked(s.ID, api.StateWorking, "Working on your reply", "")
		answer := answerFor(r.Text)
		sid := s.ID
		c.queue = append(c.queue, scheduled{at: time.Now().Add(2500 * time.Millisecond), fn: func() {
			c.say(sid, answer, api.StateWaiting, "")
		}})
	}
	res := api.ReplyResult{Via: via}
	switch via {
	case api.ReplyMessage:
		res.Note = "Claude sees this as a message sent from Pilothouse, not typed by you."
	case api.ReplyPlugin:
		res.Note = "Sent through the Pilothouse plugin."
	default:
		res.Note = "Typed into the session's terminal."
	}
	return res
}

func (c *Core) runningLocked(sid string) (pendingRun, bool) {
	f := c.feeds[sid]
	if f == nil {
		return pendingRun{}, false
	}
	for i := len(f.items) - 1; i >= 0; i-- {
		if f.items[i].Pending {
			for _, ref := range c.reqItem {
				if ref.session == sid && ref.index == i {
					return pendingRun{}, false // held for a person, not running
				}
			}
			return pendingRun{ref: itemRef{session: sid, index: i}}, true
		}
	}
	return pendingRun{}, false
}

// answerFor is the agent's reply to a follow-up prompt.
func answerFor(text string) string {
	t := strings.ToLower(text)
	switch {
	case strings.Contains(t, "stop") || strings.Contains(t, "wait") || strings.Contains(t, "pause"):
		return "Stopped. Nothing is half-written; tell me when to pick it up again."
	case strings.Contains(t, "pr") && (strings.Contains(t, "open") || strings.Contains(t, "pull")) || strings.Contains(t, "branch"):
		return "Makes sense — I'll push to a branch and open a pull request instead of pushing to main."
	case strings.Contains(t, "test"):
		return "Ran the full suite again: everything passes. I added a regression test for the case you mentioned."
	case strings.Contains(t, "summar") || strings.Contains(t, "status") || strings.Contains(t, "where"):
		return "So far: the change is in, tests pass, and the only open item is your approval on the last step."
	case strings.HasSuffix(strings.TrimSpace(t), "?"):
		return "Good question. Short answer: yes — it's covered, and I've noted the edge case in the summary."
	}
	return "Got it — " + strings.TrimSuffix(clip(text, 100), ".") + ". On it."
}

// expired: the hold limit handed the request back to the terminal.
func (c *Core) expired(r api.Request) {
	c.mu.Lock()
	ref, ok := c.reqItem[r.ID]
	sc := c.reqMeta[r.ID]
	delete(c.reqItem, r.ID)
	if ok {
		c.finishLocked(ref, "Nobody answered in time — handed back to the terminal", false)
	}
	c.UpdateSession(r.SessionID, func(s *api.Session) {
		if s.State == api.StateBlocked {
			s.StateDetail = "Waiting in the terminal: " + lowerFirst(strings.TrimPrefix(s.StateDetail, "Asked to "))
		}
	})
	c.mu.Unlock()
	if c.opt.Quiet {
		return
	}
	// The demo asks again a little later, so there's always something to answer.
	c.later(20*time.Second, func() {
		again := r
		again.ID, again.Decision, again.ClosedAt = "", nil, nil
		again.CreatedAt, again.ExpiresAt = time.Time{}, time.Time{}
		again.Context = "Asking again — " + lowerFirst(r.Context)
		c.raise(again, api.ActivityItem{}, sc)
	})
}

// ---------- The lively loop (Options.Quiet off) ----------

// workStep is one thing a working agent does on its own.
type workStep struct {
	item api.ActivityItem
	file *api.FileChange
}

var workScripts = map[string][]workStep{
	"s_site000002": {
		{item: api.ActivityItem{Kind: api.ActivityText, Text: "Bundle is 212 kB gzipped, under the 250 kB budget. Checking the page in a headless browser."}},
		{item: api.ActivityItem{Kind: api.ActivityTool, Tool: "shell", ToolKind: api.ToolShell, Summary: "Run: npx playwright test pricing", Result: "Running 4 tests using 4 workers\n  4 passed (6.2s)"}},
		{item: api.ActivityItem{Kind: api.ActivityTool, Tool: "apply_patch", ToolKind: api.ToolEdit, Summary: "Edit src/components/PlanCard.tsx", Result: "Updated src/components/PlanCard.tsx (+6 −3)"},
			file: &api.FileChange{Path: "src/components/PlanCard.tsx", Op: api.FileEdit, Added: 6, Removed: 3}},
		{item: api.ActivityItem{Kind: api.ActivityTool, Tool: "shell", ToolKind: api.ToolShell, Summary: "Run: npm run lint", Result: "✔ No problems (0 errors, 0 warnings)"}},
		{item: api.ActivityItem{Kind: api.ActivityTool, Tool: "shell", ToolKind: api.ToolShell, Summary: "Run: npm run build", Result: "✓ built in 6.4s\ndist/pricing.html  21.9 kB │ gzip: 6.0 kB"}},
	},
	"s_apisrv003": {
		{item: api.ActivityItem{Kind: api.ActivityTool, Tool: "Read", ToolKind: api.ToolRead, Summary: "Read internal/cache/shard.go", Result: "Read 141 lines"}},
		{item: api.ActivityItem{Kind: api.ActivityText, Text: "The sharded cache wraps the same LRU, so it gets the speed-up for free. Adding a parallel benchmark to be sure."}},
		{item: api.ActivityItem{Kind: api.ActivityTool, Tool: "Edit", ToolKind: api.ToolEdit, Summary: "Edit internal/cache/lru_test.go", Result: "Updated internal/cache/lru_test.go with 14 additions and 0 removals"},
			file: &api.FileChange{Path: "internal/cache/lru_test.go", Op: api.FileEdit, Added: 14}},
		{item: api.ActivityItem{Kind: api.ActivityTool, Tool: "Bash", ToolKind: api.ToolShell, Summary: "Run: go test -run=^$ -bench=. -benchmem ./internal/cache",
			Result: "BenchmarkGet-12            63485220    18.9 ns/op    0 B/op   0 allocs/op\nBenchmarkGetParallel-12   310482916     3.8 ns/op    0 B/op   0 allocs/op\nok  \tgithub.com/dev/api-server/internal/cache\t3.112s"}},
		{item: api.ActivityItem{Kind: api.ActivityTool, Tool: "Bash", ToolKind: api.ToolShell, Summary: "Run: go test -race ./...", Result: "ok  \tgithub.com/dev/api-server/internal/cache\t2.310s\nok  \tgithub.com/dev/api-server/internal/http\t4.872s\nok  \tgithub.com/dev/api-server/internal/store\t6.004s"}},
	},
}

// work advances the agents that are working on their own.
func (c *Core) work() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.Sessions() {
		if s.State != api.StateWorking {
			continue
		}
		if run, ok := c.runningLocked(s.ID); ok {
			f := c.feeds[s.ID]
			res := "Done"
			if it := f.items[run.ref.index]; strings.Contains(it.Summary, "test") {
				res = "ok  \tall packages\t5.204s"
			}
			if st := c.tasks[s.TaskID]; st != nil && s.TaskID != "" {
				c.finishLocked(run.ref, "ok  \tgithub.com/dev/api-server/internal/cache\t2.871s (500 runs, 0 failures)", false)
				c.completeTaskLocked(s.TaskID, "Fixed: the test now inserts keys from a sorted slice, so eviction order is the same on every run. 500 runs, 0 failures.")
				continue
			}
			c.finishLocked(run.ref, res, false)
			continue
		}
		steps := workScripts[s.ID]
		if len(steps) == 0 {
			continue
		}
		f := c.feeds[s.ID]
		n := 0
		if f != nil {
			n = len(f.items)
		}
		st := steps[n%len(steps)]
		it := st.item
		it.At = time.Time{}
		c.addLocked(s.ID, it)
		if st.file != nil {
			c.fileLocked(s.ID, *st.file)
		}
		if it.Kind == api.ActivityTool {
			detail := strings.Replace(it.Summary, "Run: ", "Running: ", 1)
			c.UpdateSession(s.ID, func(s *api.Session) { s.StateDetail = detail })
		}
	}
}

// liveScript is the queue of requests the lively demo raises.
func liveScript() []func(c *Core) (api.Request, api.ActivityItem, reqScript) {
	shell := func(sid, tool, cmd string, risk []api.RiskLabel, rule, ctx string, sc reqScript) func(c *Core) (api.Request, api.ActivityItem, reqScript) {
		return func(c *Core) (api.Request, api.ActivityItem, reqScript) {
			sc.after = api.StateWorking
			return api.Request{SessionID: sid, Checkpoint: api.CheckpointPermission, Tool: tool, ToolKind: api.ToolShell, Command: cmd,
				Summary: "Run: " + cmd, Risk: risk, PolicyRule: rule, Context: ctx, Input: rawJSON(map[string]any{"command": cmd})}, api.ActivityItem{}, sc
		}
	}
	return []func(c *Core) (api.Request, api.ActivityItem, reqScript){
		shell("s_site000002", "shell", "npx vercel --prod", []api.RiskLabel{{Code: api.RiskDeploy, Level: api.RiskHigh, Text: "Deploys the site to production"}},
			"autopilot-ask-risky", "The build is 212 kB gzipped, under budget, and the browser tests pass. Deploying the pricing page to production.",
			reqScript{allowResult: "✅  Production: https://site-dev.vercel.app [41s]", allowSay: "Live in production. I'll watch the error rate for a few minutes.", detail: "Watching the deploy"}),
		func(c *Core) (api.Request, api.ActivityItem, reqScript) {
			p := "/Users/dev/src/api-server/internal/cache/lru_bench_test.go"
			return api.Request{SessionID: "s_apisrv003", Checkpoint: api.CheckpointPermission, Tool: "Write", ToolKind: api.ToolEdit, Paths: []string{p},
					Summary: "Write internal/cache/lru_bench_test.go", Preview: benchPreview(), Context: "Checking the benchmark in, so CI catches it if Get ever allocates again."},
				api.ActivityItem{}, reqScript{allowResult: "Wrote 34 lines to internal/cache/lru_bench_test.go", allowSay: "Benchmark is in. Running it once more under -race.", after: api.StateWorking, detail: "Running the benchmarks"}
		},
		shell("s_site000002", "shell", "rm -rf node_modules .next && npm ci", []api.RiskLabel{{Code: api.RiskDestructive, Level: api.RiskMedium, Text: "Deletes node_modules and the build cache"}},
			"", "The build fails on a stale cache after the lockfile change. Reinstalling from scratch fixes it.",
			reqScript{allowResult: "added 1312 packages in 21s", allowSay: "Clean install done; the build passes again.", detail: "Building"}),
		shell("s_apisrv003", "Bash", "kubectl rollout restart deployment/api -n prod", []api.RiskLabel{{Code: api.RiskDeploy, Level: api.RiskHigh, Text: "Restarts the production API"}},
			"autopilot-ask-risky", "The cache fix is merged and the image is built. Restarting prod so the pods pick it up.",
			reqScript{allowResult: "deployment.apps/api restarted", allowSay: "Rolling restart started; 6 of 6 pods are healthy.", detail: "Watching the rollout"}),
		func(c *Core) (api.Request, api.ActivityItem, reqScript) {
			return api.Request{SessionID: "s_site000002", Checkpoint: api.CheckpointPermission, Tool: "apply_patch", ToolKind: api.ToolEdit,
					Paths:   []string{"/Users/dev/src/site/src/components/PlanCard.tsx", "/Users/dev/src/site/src/styles/pricing.css"},
					Summary: "Edit 2 files", Preview: contrastPreview(), Context: "The yearly badge fails contrast (3.1:1). Darkening it and giving the plan cards a visible focus ring."},
				api.ActivityItem{}, reqScript{allowResult: "Success. Updated the following files:\nM src/components/PlanCard.tsx\nM src/styles/pricing.css", allowSay: "Contrast is 5.2:1 now and keyboard focus is visible.", after: api.StateWorking, detail: "Checking contrast"}
		},
		shell("s_apisrv003", "Bash", "docker push ghcr.io/dev/api:1.42.0", []api.RiskLabel{{Code: api.RiskNetwork, Level: api.RiskMedium, Text: "Uploads an image to a registry"}},
			"autopilot-ask-risky", "Tagged 1.42.0 with the cache fix. Pushing the image so staging can pull it.",
			reqScript{allowResult: "1.42.0: digest: sha256:9c1f…e2a4 size: 1784", allowSay: "Image pushed. Staging will pick it up on the next deploy.", detail: "Waiting for staging"}),
	}
}

// raiseNext raises the next scripted request on a session that is free.
func (c *Core) raiseNext() {
	open := 0
	busy := map[string]bool{}
	for _, r := range c.Requests() {
		if r.Status == api.RequestOpen {
			open++
			busy[r.SessionID] = true
		}
	}
	if open >= 6 {
		return
	}
	script := liveScript()
	for i := 0; i < len(script); i++ {
		c.mu.Lock()
		k := c.script % len(script)
		c.script++
		c.mu.Unlock()
		r, tool, sc := script[k](c)
		s, ok := c.session(r.SessionID)
		if !ok || busy[s.ID] || s.State != api.StateWorking {
			continue
		}
		c.raise(r, tool, sc)
		return
	}
}

// sortedOpen is the open requests, oldest first (tests).
func (c *Core) sortedOpen() []api.Request {
	var out []api.Request
	for _, r := range c.Requests() {
		if r.Status == api.RequestOpen {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}
