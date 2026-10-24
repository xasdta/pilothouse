package demo

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
)

// Tasks from anywhere (F7): a prompt runs in its own worktree session,
// ends with a diff for review, and is shipped (push + PR) or discarded.

type taskState struct {
	diff api.TaskDiff
	pr   int
}

// Repos lists the repositories sessions ran in, most recently used first.
func (c *Core) Repos(ctx context.Context) ([]api.RepoInfo, error) {
	seen := map[string]*api.RepoInfo{}
	for _, s := range c.Sessions() {
		if s.RepoRoot == "" || s.TaskID != "" {
			continue
		}
		r := seen[s.RepoRoot]
		if r == nil {
			r = &api.RepoInfo{Root: s.RepoRoot, Name: path.Base(s.RepoRoot), Branch: s.Branch, LastUsed: s.UpdatedAt}
			seen[s.RepoRoot] = r
		}
		if s.UpdatedAt.After(r.LastUsed) {
			r.LastUsed, r.Branch = s.UpdatedAt, s.Branch
		}
	}
	out := make([]api.RepoInfo, 0, len(seen))
	for _, r := range seen {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastUsed.After(out[j].LastUsed) })
	return out, nil
}

// Tasks lists every task, most recently updated first.
func (c *Core) Tasks(ctx context.Context) ([]api.Task, error) { return c.FakeCore.Tasks(), nil }

// StartTask creates a worktree session for the prompt; the agent works
// through it in a few seconds and leaves a diff for review.
func (c *Core) StartTask(ctx context.Context, t api.TaskStart, by string) (api.Task, error) {
	repos, _ := c.Repos(ctx)
	var repo *api.RepoInfo
	for i := range repos {
		if repos[i].Root == t.Repo {
			repo = &repos[i]
		}
	}
	if repo == nil {
		return api.Task{}, core.Invalid("Pilothouse hasn't seen a session in %s — start one there first", t.Repo)
	}
	switch t.Agent {
	case "":
		t.Agent = api.AgentClaude
	case api.AgentClaude, api.AgentCodex, agentOpenCode:
	default:
		return api.Task{}, core.Invalid("agent must be claude, codex or opencode")
	}
	prompt := strings.TrimSpace(t.Prompt)
	if prompt == "" {
		return api.Task{}, core.Invalid("tell the agent what to do")
	}
	slug := slugify(t.Name)
	if t.Name == "" {
		words := strings.Fields(prompt)
		slug = slugify(strings.Join(words[:min(5, len(words))], " "))
	}
	if t.Base == "" {
		t.Base = "main"
	}
	now := c.now()
	c.mu.Lock()
	c.nextID++
	n := c.nextID
	c.mu.Unlock()
	for _, x := range c.FakeCore.Tasks() {
		if x.Branch == "ph/"+slug {
			slug = fmt.Sprintf("%s-%d", slug, n)
		}
	}
	task := api.Task{
		ID: fmt.Sprintf("t_demo%04d", n), Title: clip(firstLine(prompt), 80), Prompt: prompt, Agent: t.Agent,
		Repo: repo.Root, Worktree: Home + "/.pilothouse/worktrees/" + repo.Name + "-" + slug, Branch: "ph/" + slug, BaseRef: t.Base,
		SessionID: fmt.Sprintf("s_task%04d", n), Status: api.TaskRunning, CreatedAt: now, UpdatedAt: now,
	}
	s := api.Session{ID: task.SessionID, Agent: t.Agent, Mode: api.ModeWrapped, Machine: c.Machine, Title: repo.Name,
		Cwd: task.Worktree, RepoRoot: task.Worktree, Branch: task.Branch, State: api.StateWorking, StateDetail: "Setting up the worktree",
		TaskID: task.ID, StartedAt: now, UpdatedAt: now,
		Caps: api.SessionCaps{Reply: true, ReplyVia: api.ReplyTerminal, Interrupt: true, Watch: true, Activity: true}}
	c.PutSession(s)
	c.mu.Lock()
	c.tasks[task.ID] = &taskState{diff: api.TaskDiff{TaskID: task.ID, Files: []api.FilePreview{}}}
	c.openTermLocked(s)
	c.addLocked(s.ID, api.ActivityItem{Kind: api.ActivityPrompt, Text: prompt, Source: "pilothouse"})
	c.addLocked(s.ID, api.ActivityItem{Kind: api.ActivitySystem, Text: "Worktree " + tilde(task.Worktree) + " on " + task.Branch + " from " + t.Base})
	c.mu.Unlock()
	c.PutTask(task)

	files := taskFiles(repo.Name, prompt)
	sid, tid := s.ID, task.ID
	c.later(2*time.Second, func() {
		c.tool(sid, api.ActivityItem{Kind: api.ActivityTool, Tool: toolName(t.Agent, "read"), ToolKind: api.ToolRead, Summary: "Read README.md", Result: "Read 86 lines"}, nil)
		c.setState(sid, api.StateWorking, "Reading the code", "")
	})
	c.later(4500*time.Millisecond, func() {
		c.say(sid, "I'll make the change in "+path.Base(files[0].Path)+" and add a test.", api.StateWorking, "Editing "+files[0].Path)
	})
	for i, f := range files {
		f := f
		c.later(time.Duration(6500+1500*i)*time.Millisecond, func() {
			c.tool(sid, api.ActivityItem{Kind: api.ActivityTool, Tool: toolName(t.Agent, "edit"), ToolKind: api.ToolEdit, Summary: "Edit " + f.Path,
				Result: fmt.Sprintf("Updated %s with %d additions and %d removals", f.Path, f.Added, f.Removed)},
				&api.FileChange{Path: f.Path, Op: f.Op, Added: f.Added, Removed: f.Removed})
			c.mu.Lock()
			if st := c.tasks[tid]; st != nil {
				st.diff.Files = append(st.diff.Files, f)
				st.diff.Added += f.Added
				st.diff.Removed += f.Removed
			}
			c.mu.Unlock()
		})
	}
	c.later(10*time.Second, func() {
		c.tool(sid, api.ActivityItem{Kind: api.ActivityTool, Tool: toolName(t.Agent, "shell"), ToolKind: api.ToolShell, Summary: "Run: " + testCommand(repo.Name), Result: testResult(repo.Name)}, nil)
	})
	c.later(12*time.Second, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.completeTaskLocked(tid, fmt.Sprintf("Done: changed %d files and the tests pass. The diff is ready for review.", len(files)))
	})
	return task, nil
}

// completeTaskLocked ends a task's turn: the diff waits for a person.
func (c *Core) completeTaskLocked(id, summary string) {
	t, ok := c.FakeCore.Task(id)
	if !ok || t.Status != api.TaskRunning {
		return
	}
	if st := c.tasks[id]; st != nil && len(st.diff.Files) == 0 {
		st.diff = flakyDiff(id)
	}
	c.addLocked(t.SessionID, api.ActivityItem{Kind: api.ActivityText, Text: summary})
	c.setStateLocked(t.SessionID, api.StateWaiting, "", summary)
	t.Status, t.UpdatedAt = api.TaskReview, c.now()
	c.PutTask(t)
}

// TaskDiff is what the task changed against its base.
func (c *Core) TaskDiff(ctx context.Context, taskID string) (api.TaskDiff, error) {
	if _, ok := c.FakeCore.Task(taskID); !ok {
		return api.TaskDiff{}, core.ErrNotFound
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.tasks[taskID]
	if st == nil {
		return api.TaskDiff{TaskID: taskID, Files: []api.FilePreview{}}, nil
	}
	d := st.diff
	d.Files = append([]api.FilePreview{}, st.diff.Files...)
	return d, nil
}

// FinishTask ships (push + pull request) or discards a task.
func (c *Core) FinishTask(ctx context.Context, taskID string, f api.TaskFinish, by string) (api.Task, error) {
	t, ok := c.FakeCore.Task(taskID)
	if !ok {
		return api.Task{}, core.ErrNotFound
	}
	switch {
	case t.Status == api.TaskDone || t.Status == api.TaskAbandon:
		return api.Task{}, core.Invalid("this task is already %s", map[api.TaskStatus]string{api.TaskDone: "shipped", api.TaskAbandon: "discarded"}[t.Status])
	case f.Action == "ship" && t.Status != api.TaskReview:
		return api.Task{}, core.Invalid("the agent is still working — wait for the diff, or discard the task")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	text := "Discarded: removed the worktree and the branch " + t.Branch
	if f.Action == "ship" {
		c.nextID++
		pr := 140 + c.nextID%60
		if st := c.tasks[taskID]; st != nil {
			st.pr = pr
		}
		text = fmt.Sprintf("Shipped: pushed %s and opened pull request #%d (github.com/dev/%s/pull/%d)", t.Branch, pr, path.Base(t.Repo), pr)
		t.Status = api.TaskDone
	} else {
		t.Status = api.TaskAbandon
	}
	t.UpdatedAt = now
	if t.SessionID != "" {
		if _, ok := c.session(t.SessionID); ok {
			c.addLocked(t.SessionID, api.ActivityItem{Kind: api.ActivitySystem, Text: text})
			code := 0
			c.UpdateSession(t.SessionID, func(s *api.Session) {
				s.State, s.StateDetail, s.Caps = api.StateExited, "", api.SessionCaps{}
				s.ExitedAt, s.ExitCode, s.UpdatedAt = &now, &code, now
				s.LastMessage = text
			})
		}
	}
	c.PutTask(t)
	return t, nil
}

func toolName(a api.AgentKind, kind string) string {
	switch a {
	case api.AgentCodex:
		return map[string]string{"read": "shell", "edit": "apply_patch", "shell": "shell"}[kind]
	case agentOpenCode:
		return kind
	}
	return map[string]string{"read": "Read", "edit": "Edit", "shell": "Bash"}[kind]
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func testCommand(repo string) string {
	switch repo {
	case "api-server", "pilothouse":
		return "go test ./..."
	case "notes":
		return "npm test"
	}
	return "pnpm test"
}

func testResult(repo string) string {
	switch repo {
	case "api-server", "pilothouse":
		return "ok  \t./...\t6.120s"
	}
	return " Test Files  9 passed (9)\n      Tests  73 passed (73)"
}

// taskFiles is the diff a new task produces: a change and a test that
// mention what was asked.
func taskFiles(repo, prompt string) []api.FilePreview {
	what := clip(firstLine(prompt), 70)
	switch repo {
	case "api-server", "pilothouse":
		return []api.FilePreview{
			{Path: "internal/handler/health.go", Op: api.FileEdit, Added: 7, Removed: 2, Lang: "go", Diff: "@@ -18,7 +18,12 @@ func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {\n \tctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)\n \tdefer cancel()\n-\tif err := h.db.PingContext(ctx); err != nil {\n-\t\thttp.Error(w, \"db down\", http.StatusServiceUnavailable)\n+\t// " + what + "\n+\tstatus := map[string]string{\"db\": \"ok\", \"cache\": \"ok\"}\n+\tif err := h.db.PingContext(ctx); err != nil {\n+\t\tstatus[\"db\"] = err.Error()\n+\t}\n+\tif !h.cache.Healthy() {\n+\t\tstatus[\"cache\"] = \"degraded\"\n \t}\n \twriteJSON(w, status)\n }\n"},
			{Path: "internal/handler/health_test.go", Op: api.FileEdit, Added: 12, Removed: 0, Lang: "go", Diff: "@@ -40,3 +40,15 @@ func TestHealthOK(t *testing.T) {\n \t\tt.Fatalf(\"status = %d\", w.Code)\n \t}\n }\n+\n+func TestHealthReportsCache(t *testing.T) {\n+\th := newHandler(t, withCache(degradedCache{}))\n+\tw := httptest.NewRecorder()\n+\th.Health(w, httptest.NewRequest(\"GET\", \"/health\", nil))\n+\tvar got map[string]string\n+\tjson.NewDecoder(w.Body).Decode(&got)\n+\tif got[\"cache\"] != \"degraded\" {\n+\t\tt.Fatalf(\"cache = %q\", got[\"cache\"])\n+\t}\n+}\n+\n"},
		}
	}
	return []api.FilePreview{
		{Path: "src/lib/format.ts", Op: api.FileEdit, Added: 8, Removed: 3, Lang: "ts", Diff: "@@ -1,7 +1,12 @@\n-export function formatPrice(cents: number): string {\n-  return \"$\" + (cents / 100).toFixed(2);\n+// " + what + "\n+export function formatPrice(cents: number, currency = \"USD\", locale = \"en-US\"): string {\n+  return new Intl.NumberFormat(locale, {\n+    style: \"currency\",\n+    currency,\n+    minimumFractionDigits: cents % 100 === 0 ? 0 : 2,\n+  }).format(cents / 100);\n }\n \n export function formatDate(d: Date): string {\n-  return d.toISOString().slice(0, 10);\n+  return d.toLocaleDateString(\"en-US\", { month: \"short\", day: \"numeric\", year: \"numeric\" });\n }\n"},
		{Path: "src/lib/format.test.ts", Op: api.FileCreate, Added: 14, Removed: 0, Lang: "ts", Diff: "@@ -0,0 +1,14 @@\n+import { describe, expect, it } from \"vitest\";\n+import { formatPrice } from \"./format\";\n+\n+describe(\"formatPrice\", () => {\n+  it(\"drops cents for whole amounts\", () => {\n+    expect(formatPrice(1200)).toBe(\"$12\");\n+  });\n+  it(\"keeps cents otherwise\", () => {\n+    expect(formatPrice(1299)).toBe(\"$12.99\");\n+  });\n+  it(\"formats euros\", () => {\n+    expect(formatPrice(1200, \"EUR\", \"de-DE\")).toBe(\"12 €\");\n+  });\n+});\n"},
	}
}
