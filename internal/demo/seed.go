package demo

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
)

// The demo machine on a busy afternoon: four agents need you (a push to
// main, a production Terraform change with its diff, a question with
// options and a plan), two are working, one finished its turn, tasks
// wait for review. Everything is laid out on one timeline so the event
// log, the activity feeds, the terminals and the usage agree.

const src = Home + "/src/"

// step is one moment of the seeded afternoon.
type step struct {
	at time.Duration // before now
	fn func()
}

func rawJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func run(tool, cmd, result string, isErr bool) api.ActivityItem {
	return api.ActivityItem{Kind: api.ActivityTool, Tool: tool, ToolKind: api.ToolShell, Summary: "Run: " + cmd, Result: result, IsError: isErr}
}

func read(tool, p, result string) api.ActivityItem {
	return api.ActivityItem{Kind: api.ActivityTool, Tool: tool, ToolKind: api.ToolRead, Summary: "Read " + p, Result: result}
}

func search(pattern, result string) api.ActivityItem {
	return api.ActivityItem{Kind: api.ActivityTool, Tool: "Grep", ToolKind: api.ToolRead, Summary: "Search " + pattern, Result: result}
}

func edit(tool, p string, added, removed int) api.ActivityItem {
	return api.ActivityItem{Kind: api.ActivityTool, Tool: tool, ToolKind: api.ToolEdit, Summary: "Edit " + p,
		Result: "Updated " + p + " with " + itoa(added) + " " + plural(added, "addition", "additions") + " and " + itoa(removed) + " " + plural(removed, "removal", "removals")}
}

func say(text string) api.ActivityItem {
	return api.ActivityItem{Kind: api.ActivityText, Text: text}
}

func prompt(text, source string) api.ActivityItem {
	return api.ActivityItem{Kind: api.ActivityPrompt, Text: text, Source: source}
}

func itoa(n int) string { return strconv.Itoa(n) }

func (c *Core) seed() {
	now := c.now()
	at := now
	// Events carry the moment they happened, like the daemon's log.
	c.FakeCore.Now = func() time.Time { return at }
	defer func() { c.FakeCore.Now = c.opt.Now }()
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	m := time.Minute

	c.presets = seedPresets()
	c.rules = seedRules(now)

	var steps []step
	add := func(d time.Duration, fn func()) { steps = append(steps, step{d, fn}) }
	session := func(s api.Session) {
		add(now.Sub(s.StartedAt), func() {
			st := s
			st.State, st.UpdatedAt = api.StateWorking, s.StartedAt
			st.Usage = &api.Usage{Model: modelFor(s.Agent)}
			if s.Usage != nil {
				st.Usage = s.Usage
			}
			c.PutSession(st)
			c.mu.Lock()
			c.feeds[s.ID] = &feed{usage: *st.Usage}
			if s.Mode == api.ModeWrapped {
				c.openTermLocked(st)
			}
			c.mu.Unlock()
		})
	}
	item := func(sid string, d time.Duration, it api.ActivityItem, fc ...api.FileChange) {
		add(d, func() {
			it.At = ago(d)
			c.mu.Lock()
			c.addLocked(sid, it)
			for _, f := range fc {
				c.fileLocked(sid, f)
			}
			c.mu.Unlock()
		})
	}
	state := func(sid string, d time.Duration, fn func(*api.Session)) {
		add(d, func() { c.UpdateSession(sid, func(s *api.Session) { fn(s); s.UpdatedAt = ago(d) }) })
	}
	request := func(d time.Duration, r api.Request, tool api.ActivityItem, sc reqScript) {
		add(d, func() {
			r.CreatedAt = ago(d)
			c.raise(r, tool, sc)
		})
	}
	closed := func(d time.Duration, r api.Request, tool api.ActivityItem) {
		add(d, func() {
			r.CreatedAt = ago(d)
			tool.At = ago(d)
			c.PutRequest(r)
			c.mu.Lock()
			c.addLocked(r.SessionID, tool)
			c.mu.Unlock()
		})
	}
	fileEdit := func(p string, a, r int) api.FileChange {
		return api.FileChange{Path: p, Op: api.FileEdit, Added: a, Removed: r}
	}
	fileNew := func(p string, a int) api.FileChange {
		return api.FileChange{Path: p, Op: api.FileCreate, Added: a}
	}

	claudeCaps := api.SessionCaps{Reply: true, ReplyVia: api.ReplyMessage, Activity: true}
	wrappedCaps := api.SessionCaps{Reply: true, ReplyVia: api.ReplyTerminal, Interrupt: true, Watch: true, Activity: true}

	// ---- pilothouse: Claude Code under `pilothouse run`, wants to push to main ----
	ph := "s_pilothouse01"
	session(api.Session{ID: ph, Agent: api.AgentClaude, Mode: api.ModeWrapped, Machine: c.Machine, Title: "pilothouse",
		Cwd: src + "pilothouse", RepoRoot: src + "pilothouse", Branch: "main", StartedAt: ago(48 * m), Caps: wrappedCaps,
		Usage: &api.Usage{Model: "claude-opus-4-1", InputTokens: 41000, OutputTokens: 6200, CacheReadTokens: 410000, CacheWriteTokens: 22000}})
	item(ph, 47*m, prompt("Finish the web inbox, make sure every test passes, then push to main.", "human"))
	item(ph, 46*m+30*time.Second, say("I'll read the web server and the request card first, then move the cards to signed v2 answers."))
	item(ph, 46*m, read("Read", "internal/web/web.go", "Read 312 lines"))
	item(ph, 45*m+40*time.Second, read("Read", "internal/web/static/js/card.js", "Read 242 lines"))
	item(ph, 45*m, search(`PilothouseKeys\.`, "Found 4 files\ninternal/web/static/js/card.js\ninternal/web/static/js/keystore.js\ninternal/web/static/sw.js\ninternal/web/static/js/views/settings.js"))
	item(ph, 44*m, say("The card still signs v1 decisions. I'll switch it to `respond` (signed v2) and roll back when another device answered first."))
	item(ph, 41*m, edit("Edit", "internal/web/static/js/card.js", 38, 12), fileEdit("internal/web/static/js/card.js", 38, 12))
	item(ph, 38*m, edit("Edit", "internal/web/static/css/app.css", 15, 3), fileEdit("internal/web/static/css/app.css", 15, 3))
	item(ph, 33*m, run("Bash", "go test -race ./internal/web/...", "--- FAIL: TestRespondConflict (0.03s)\n    signed_test.go:142: second answer: got 200, want 409\nFAIL\nFAIL\tgithub.com/xasdta/pilothouse/internal/web\t4.112s", true))
	item(ph, 32*m, say("One test expects a 409 for the second answer, but the fake core never closed the request. Fixing the fake, not the test."))
	item(ph, 31*m, edit("Edit", "internal/web/webtest/fakecore.go", 6, 2), fileEdit("internal/web/webtest/fakecore.go", 6, 2))
	item(ph, 28*m, run("Bash", "go test -race ./...", "ok  \tgithub.com/xasdta/pilothouse/internal/api\t0.412s\nok  \tgithub.com/xasdta/pilothouse/internal/daemon\t9.870s\nok  \tgithub.com/xasdta/pilothouse/internal/policy\t2.304s\nok  \tgithub.com/xasdta/pilothouse/internal/web\t4.031s\n214 tests passed", false))
	item(ph, 26*m, api.ActivityItem{Kind: api.ActivitySubagent, Summary: "Review the inbox diff for XSS and signature handling", Text: "code-reviewer started"})
	item(ph, 25*m+30*time.Second, api.ActivityItem{Kind: api.ActivityTool, Tool: "Grep", ToolKind: api.ToolRead, Summary: "Search innerHTML", Result: "No matches", Sidechain: true})
	item(ph, 24*m, api.ActivityItem{Kind: api.ActivityTool, Tool: "Read", ToolKind: api.ToolRead, Summary: "Read internal/web/signed.go", Result: "Read 134 lines", Sidechain: true})
	item(ph, 22*m, api.ActivityItem{Kind: api.ActivitySubagent, Summary: "code-reviewer finished", Text: "No issues. One nit: cap the deny reason at 2,000 characters like the server does."})
	item(ph, 21*m, edit("Edit", "internal/web/static/js/card.js", 1, 1), fileEdit("internal/web/static/js/card.js", 1, 1))
	item(ph, 12*m, run("Bash", `git commit -am "web: signed answers with rollback"`, "[main 8d03e7b] web: signed answers with rollback\n 3 files changed, 60 insertions(+), 18 deletions(-)", false))
	item(ph, 80*time.Second, say("All 214 tests pass and the build is clean. Pushing to main so CI can cut the release."))
	request(70*time.Second, api.Request{ID: "r_gitpush03", SessionID: ph, Checkpoint: api.CheckpointPermission, Tool: "Bash", ToolKind: api.ToolShell,
		Command: "git push origin main", Summary: "Run: git push origin main", PolicyRule: "ask-before-push",
		Input:     rawJSON(map[string]string{"command": "git push origin main", "description": "Push the inbox changes to main"}),
		Risk:      []api.RiskLabel{{Code: api.RiskProtectedBranch, Level: api.RiskHigh, Text: "Pushes straight to main"}},
		Context:   "All 214 tests pass and the build is clean. Pushing to main so CI can cut the release.",
		ExpiresAt: now.Add(7*m + 50*time.Second)}, api.ActivityItem{},
		reqScript{allowResult: "To github.com:dev/pilothouse.git\n   4f2a9c1..8d03e7b  main -> main", allowSay: "Pushed to main. CI is running the release build; I'll tell you if it fails.",
			denySay: "OK, I won't push to main.", after: api.StateWaiting})

	// ---- infra: Claude Code (observed), production Terraform change ----
	inf := "s_infra0006"
	session(api.Session{ID: inf, Agent: api.AgentClaude, Mode: api.ModeObserved, Machine: c.Machine, Title: "infra",
		Cwd: src + "infra", RepoRoot: src + "infra", Branch: "chore/node-22", StartedAt: ago(36 * m), Caps: claudeCaps})
	item(inf, 36*m, prompt("Node 18 is end-of-life. Move every Lambda and the deploy workflow to Node 22.", "human"))
	item(inf, 35*m+30*time.Second, search("nodejs18.x", "Found 3 files\nterraform/staging/main.tf\nterraform/prod/main.tf\n.github/workflows/deploy.yml"))
	item(inf, 35*m, read("Read", "terraform/staging/main.tf", "Read 88 lines"))
	item(inf, 33*m, edit("Edit", "terraform/staging/main.tf", 2, 2), fileEdit("terraform/staging/main.tf", 2, 2))
	item(inf, 31*m, edit("Edit", ".github/workflows/deploy.yml", 1, 1), fileEdit(".github/workflows/deploy.yml", 1, 1))
	item(inf, 29*m, run("Bash", "terraform fmt -check -recursive", "(no changes)", false))
	item(inf, 27*m, run("Bash", "terraform -chdir=terraform/staging validate", "Success! The configuration is valid.", false))
	item(inf, 26*m, read("Read", "terraform/prod/main.tf", "Read 112 lines"))
	item(inf, 40*time.Second, say("Staging and the workflow are on Node 22. Production needs your OK — protect-prod asks before any change under terraform/prod."))
	request(25*time.Second, api.Request{ID: "r_tfedit04", SessionID: inf, Checkpoint: api.CheckpointPermission, Tool: "MultiEdit", ToolKind: api.ToolEdit,
		Paths: []string{src + "infra/terraform/prod/main.tf"}, Summary: "Edit terraform/prod/main.tf", PolicyRule: "protect-prod",
		Input:     rawJSON(map[string]any{"file_path": src + "infra/terraform/prod/main.tf", "edits": []map[string]string{{"old_string": `runtime       = "nodejs18.x"`, "new_string": `runtime       = "nodejs22.x"`}}}),
		Risk:      []api.RiskLabel{{Code: api.RiskDeploy, Level: api.RiskMedium, Text: "Changes production infrastructure"}},
		Preview:   terraformPreview(),
		Context:   "This moves both production Lambdas to Node 22 and gives the API 1 GB of memory, which the load test asked for. Nothing is applied yet.",
		ExpiresAt: now.Add(8*m + 35*time.Second)}, api.ActivityItem{},
		reqScript{allowResult: "Updated terraform/prod/main.tf with 3 additions and 3 removals", allowSay: "Production is on Node 22 in code. Run `terraform apply` in terraform/prod when you're ready — I won't apply it myself.",
			denySay: "Fine — I'll leave production on Node 18 and note it in the PR.", after: api.StateWaiting})

	// ---- webapp: Claude Code (observed) in plan mode ----
	wa := "s_webapp007"
	session(api.Session{ID: wa, Agent: api.AgentClaude, Mode: api.ModeObserved, Machine: c.Machine, Title: "webapp",
		Cwd: src + "webapp", RepoRoot: src + "webapp", Branch: "feat/passkeys", StartedAt: ago(22 * m), Caps: claudeCaps})
	item(wa, 22*m, prompt("Plan passkey sign-in for the web app. Don't write code until I approve the plan.", "human"))
	item(wa, 21*m+30*time.Second, say("I'll look at how sign-in and sessions work today."))
	item(wa, 21*m, read("Read", "src/auth/login.ts", "Read 164 lines"))
	item(wa, 20*m+30*time.Second, read("Read", "src/auth/session.ts", "Read 97 lines"))
	item(wa, 20*m, search("bcrypt|argon2", "Found 2 files\nsrc/auth/password.ts\nsrc/auth/login.ts"))
	item(wa, 19*m, read("Read", "package.json", "Read 61 lines"))
	item(wa, 18*m, api.ActivityItem{Kind: api.ActivityTool, Tool: "WebFetch", ToolKind: api.ToolWeb, Summary: "Fetch https://simplewebauthn.dev/docs/packages/server", Result: "Fetched 18.2 KB"})
	item(wa, 16*m, api.ActivityItem{Kind: api.ActivitySubagent, Summary: "Explore the session middleware and cookie settings", Text: "Explore started"})
	item(wa, 15*m, api.ActivityItem{Kind: api.ActivityTool, Tool: "Read", ToolKind: api.ToolRead, Summary: "Read src/middleware/session.ts", Result: "Read 54 lines", Sidechain: true})
	item(wa, 13*m, api.ActivityItem{Kind: api.ActivitySubagent, Summary: "Explore finished", Text: "Sessions are signed cookies (iron-session), 14-day expiry, SameSite=Lax."})
	item(wa, 2*m+40*time.Second, say("I've read the auth module and the session middleware. Here's the plan before I touch anything."))
	request(2*m+30*time.Second, api.Request{ID: "r_plan0006", SessionID: wa, Checkpoint: api.CheckpointPlan, Tool: "ExitPlanMode", ToolKind: api.ToolOther,
		Summary: "Plan: passkey sign-in", Plan: &api.Plan{Markdown: passkeyPlan, Path: Home + "/.claude/plans/passkey-sign-in.md"},
		Input:     rawJSON(map[string]string{"plan": passkeyPlan, "planFilePath": Home + "/.claude/plans/passkey-sign-in.md"}),
		Context:   "I've read the auth module and the session middleware. Here's the plan before I touch anything.",
		ExpiresAt: now.Add(9*m + 40*time.Second)}, api.ActivityItem{},
		reqScript{allowSay: "Plan approved. Starting with step 1: the passkeys table and its migration.", after: api.StateWorking, detail: "Implementing passkeys"})

	// ---- notes: OpenCode, asks two questions ----
	nt := "s_notes0004"
	session(api.Session{ID: nt, Agent: agentOpenCode, Mode: api.ModeObserved, Machine: c.Machine, Title: "notes",
		Cwd: src + "notes", RepoRoot: src + "notes", Branch: "main", StartedAt: ago(30 * m),
		Caps: api.SessionCaps{Reply: true, ReplyVia: api.ReplyPlugin, Activity: true}})
	item(nt, 30*m, prompt("Search is unusable with 40k notes. Make it fast.", "human"))
	item(nt, 29*m, read("read", "src/search.ts", "Read 58 lines"))
	item(nt, 28*m, run("bash", "npm run bench:search", "LIKE query   p50 1.81 s   p95 2.64 s   (40,112 notes)", false))
	item(nt, 26*m, say("Search scans every note body with LIKE — that's the 1.8 s. A full-text index fixes it."))
	item(nt, 25*m, read("read", "src/db/schema.sql", "Read 41 lines"))
	item(nt, 7*m+20*time.Second, say("Before I build the index I need two decisions from you."))
	request(7*m, api.Request{ID: "r_question05", SessionID: nt, Checkpoint: api.CheckpointQuestion, Tool: "question", ToolKind: api.ToolOther,
		Summary: "Asks which search index to use", Questions: notesQuestions(),
		Context:   "The LIKE query takes 1.8 s on 40k notes. Before I build the index I need two decisions.",
		ExpiresAt: now.Add(9 * m)}, api.ActivityItem{},
		reqScript{allowSay: "Going with {q1}, indexing {q2}. Building the index now.", after: api.StateWorking, detail: "Building the search index"})

	// ---- site: Codex (observed), working on its own ----
	st := "s_site000002"
	session(api.Session{ID: st, Agent: api.AgentCodex, Mode: api.ModeObserved, Machine: c.Machine, Title: "site",
		Cwd: src + "site", RepoRoot: src + "site", Branch: "feat/pricing-page", StartedAt: ago(26 * m), Caps: api.SessionCaps{Activity: true}})
	item(st, 26*m, prompt("Build the pricing page from docs/pricing.md: three plans, a monthly/yearly toggle and a link to the FAQ.", "human"))
	item(st, 25*m+30*time.Second, run("shell", "cat docs/pricing.md", "# Pricing\n\nFree · Pro $12/mo · Team $29/seat/mo\nYearly: two months free\nFAQ: /pricing#faq", false))
	item(st, 24*m, run("shell", `rg -n "PlanCard|Toggle" src`, "src/components/Toggle.tsx:3:export function Toggle({ on, onChange, label }: ToggleProps) {", false))
	item(st, 21*m, edit("apply_patch", "src/pages/pricing.tsx", 184, 0), fileNew("src/pages/pricing.tsx", 184))
	item(st, 18*m, edit("apply_patch", "src/components/PlanCard.tsx", 92, 0), fileNew("src/components/PlanCard.tsx", 92))
	item(st, 15*m, edit("apply_patch", "src/styles/pricing.css", 61, 0), fileNew("src/styles/pricing.css", 61))
	item(st, 11*m, run("shell", "npm run lint", "✔ No problems (0 errors, 0 warnings)", false))
	item(st, 8*m, say("All three plans render and the yearly toggle works. Building to check the bundle size."))
	item(st, 9*time.Second, api.ActivityItem{Kind: api.ActivityTool, Tool: "shell", ToolKind: api.ToolShell, Summary: "Run: npm run build", Pending: true})
	state(st, 8*time.Second, func(s *api.Session) { s.StateDetail = "Running: npm run build" })

	// ---- api-server: Claude Code under `pilothouse run`, working ----
	as := "s_apisrv003"
	session(api.Session{ID: as, Agent: api.AgentClaude, Mode: api.ModeWrapped, Machine: c.Machine, Title: "api-server",
		Cwd: src + "api-server", RepoRoot: src + "api-server", Branch: "perf/lru-cache", StartedAt: ago(14 * m), Caps: wrappedCaps})
	item(as, 14*m, prompt("The LRU cache allocates on every Get. Make Get allocation-free and prove it with a benchmark.", "human"))
	item(as, 13*m+30*time.Second, read("Read", "internal/cache/lru.go", "Read 203 lines"))
	item(as, 12*m+48*time.Second, run("Bash", "go test -run=^$ -bench=Get -benchmem ./internal/cache", "BenchmarkGet-12    38214562    31.2 ns/op    16 B/op    1 allocs/op\nok  \tgithub.com/dev/api-server/internal/cache\t1.540s", false))
	item(as, 11*m, say("Get boxes the key into an interface for the list element. Storing entries in a slice with index links removes that allocation."))
	item(as, 8*m, edit("Edit", "internal/cache/lru.go", 22, 17), fileEdit("internal/cache/lru.go", 22, 17))
	item(as, 6*m, run("Bash", "go test -run=^$ -bench=Get -benchmem ./internal/cache", "BenchmarkGet-12    63485220    18.9 ns/op    0 B/op    0 allocs/op\nok  \tgithub.com/dev/api-server/internal/cache\t1.402s", false))
	item(as, 4*m, say("Get is allocation-free now: 31.2 → 18.9 ns/op, 0 allocs. Running the whole suite."))
	item(as, 42*time.Second, api.ActivityItem{Kind: api.ActivityTool, Tool: "Bash", ToolKind: api.ToolShell, Summary: "Run: go test -race ./...", Pending: true})
	state(as, 40*time.Second, func(s *api.Session) { s.StateDetail = "Running: go test -race ./..." })

	// ---- proofline: finished its turn, waiting for the next prompt ----
	pl := "s_proofline"
	session(api.Session{ID: pl, Agent: api.AgentClaude, Mode: api.ModeObserved, Machine: c.Machine, Title: "proofline",
		Cwd: src + "proofline", RepoRoot: src + "proofline", Branch: "fix/webhook-retries", StartedAt: ago(95 * m), Caps: claudeCaps,
		Usage: &api.Usage{Model: "claude-sonnet-4-5", InputTokens: 120000, OutputTokens: 18000, CacheReadTokens: 1900000, CacheWriteTokens: 64000}})
	item(pl, 95*m, prompt("Webhook retries hammer the receiver when it's down. Add exponential backoff with a ceiling.", "human"))
	item(pl, 94*m, read("Read", "src/webhooks/retry.ts", "Read 77 lines"))
	item(pl, 90*m, edit("Edit", "src/webhooks/retry.ts", 28, 9), fileEdit("src/webhooks/retry.ts", 28, 9))
	closed(41*m, api.Request{ID: "r_curlsh002", SessionID: pl, Agent: api.AgentClaude, Checkpoint: api.CheckpointPermission,
		Tool: "Bash", ToolKind: api.ToolShell, Command: "curl -fsSL https://get.example.dev/install.sh | sh", Summary: "Run: curl … | sh", Cwd: src + "proofline",
		Risk:   []api.RiskLabel{{Code: api.RiskNetwork, Level: api.RiskHigh, Text: "Pipes a script from the internet into sh"}},
		Status: api.RequestDecided, Route: api.RouteTerminal, ExpiresAt: ago(32 * m), ClosedAt: ptr(ago(40 * m)),
		Decision: &api.Decision{RequestID: "r_curlsh002", Behavior: api.BehaviorDeny, Message: "No piping scripts into sh.", By: "cli", At: ago(40 * m)}},
		run("Bash", "curl -fsSL https://get.example.dev/install.sh | sh", "Denied at the computer: No piping scripts into sh.", true))
	item(pl, 39*m, say("Understood — I'll use the vendored retry helper instead of installing the CLI."))
	item(pl, 30*m, edit("Edit", "src/webhooks/retry.test.ts", 41, 0), fileNew("src/webhooks/retry.test.ts", 41))
	closed(12*m, api.Request{ID: "r_npmtest01", SessionID: pl, Agent: api.AgentClaude, Checkpoint: api.CheckpointPermission,
		Tool: "Bash", ToolKind: api.ToolShell, Command: "pnpm test --filter webhooks", Summary: "Run: pnpm test --filter webhooks", Cwd: src + "proofline",
		Status: api.RequestDecided, Route: api.RouteDevices, ExpiresAt: ago(3 * m), ClosedAt: ptr(ago(11 * m)),
		Decision: &api.Decision{RequestID: "r_npmtest01", Behavior: api.BehaviorAllow, Scope: api.ScopeSession, By: "device:d_other0001", At: ago(11 * m)}},
		run("Bash", "pnpm test --filter webhooks", " Test Files  6 passed (6)\n      Tests  47 passed | 1 failed (48)\n FAIL  retry.test.ts > stops after the ceiling", true))
	item(pl, 9*m, edit("Edit", "src/webhooks/retry.ts", 2, 1), fileEdit("src/webhooks/retry.ts", 2, 1))
	item(pl, 8*m, run("Bash", "pnpm test --filter webhooks", " Test Files  6 passed (6)\n      Tests  48 passed (48)", false))
	item(pl, 4*m, run("Bash", "pnpm lint", "No lint errors", false))
	item(pl, 3*m+10*time.Second, say(proofDone))
	item(pl, 3*m+5*time.Second, api.ActivityItem{Kind: api.ActivitySystem, Text: "Turn finished in 92 min"})
	state(pl, 3*m, func(s *api.Session) { s.State, s.LastMessage, s.Unseen = api.StateWaiting, proofDone, true })

	// ---- dotfiles: ended a while ago ----
	df := "s_dotfile05"
	session(api.Session{ID: df, Agent: api.AgentClaude, Mode: api.ModeObserved, Machine: c.Machine, Title: "dotfiles",
		Cwd: Home + "/dotfiles", StartedAt: ago(3 * time.Hour)})
	item(df, 3*time.Hour, prompt("Add a zsh alias for git log --oneline --graph --decorate.", "human"))
	item(df, 179*m, edit("Edit", ".zshrc", 2, 0), fileEdit(".zshrc", 2, 0))
	item(df, 178*m, say("Added `glg` to .zshrc. Open a new shell (or `source ~/.zshrc`) to use it."))
	state(df, 2*time.Hour, func(s *api.Session) {
		s.State, s.ExitedAt, s.ExitCode, s.LastMessage = api.StateExited, ptr(ago(2*time.Hour)), ptr(0), "Added `glg` to .zshrc."
	})

	// ---- tasks started from the phone ----
	tf := "s_taskfaq08"
	faq := api.Task{ID: "t_faq01", Title: "Add a pricing FAQ section", Prompt: "Add an FAQ section to the pricing page with the five questions in docs/faq.md.",
		Agent: api.AgentCodex, Repo: src + "site", Worktree: Home + "/.pilothouse/worktrees/site-pricing-faq", Branch: "ph/pricing-faq", BaseRef: "feat/pricing-page",
		SessionID: tf, Status: api.TaskRunning, CreatedAt: ago(52 * m), UpdatedAt: ago(52 * m)}
	add(52*m+time.Second, func() { c.PutTask(faq) })
	session(api.Session{ID: tf, Agent: api.AgentCodex, Mode: api.ModeWrapped, Machine: c.Machine, Title: "site",
		Cwd: faq.Worktree, RepoRoot: faq.Worktree, Branch: faq.Branch, TaskID: faq.ID, StartedAt: ago(52 * m), Caps: wrappedCaps})
	item(tf, 52*m, prompt(faq.Prompt, "pilothouse"))
	item(tf, 51*m, run("shell", "cat docs/faq.md", "1. Can I cancel anytime?\n2. Do you offer refunds?\n3. Is there a student discount?\n4. Can I change plans later?\n5. Where is my data stored?", false))
	item(tf, 47*m, edit("apply_patch", "src/pages/pricing.tsx", 6, 1), fileEdit("src/pages/pricing.tsx", 6, 1))
	item(tf, 44*m, edit("apply_patch", "src/components/Faq.tsx", 31, 0), fileNew("src/components/Faq.tsx", 31))
	item(tf, 40*m, run("shell", "npm run build", "✓ built in 4.21s\ndist/pricing.html  18.4 kB │ gzip: 5.1 kB", false))
	item(tf, 39*m, say(faqDone))
	add(39*m-time.Second, func() {
		faq.Status, faq.UpdatedAt = api.TaskReview, ago(39*m)
		c.PutTask(faq)
		c.UpdateSession(tf, func(s *api.Session) {
			s.State, s.LastMessage, s.Unseen, s.UpdatedAt = api.StateWaiting, faqDone, true, ago(39*m)
		})
	})
	c.tasks["t_faq01"] = &taskState{diff: faqDiff()}

	tk := "s_taskflk09"
	flaky := api.Task{ID: "t_flaky02", Title: "Fix the flaky TestLRUEviction", Prompt: "Fix the flaky TestLRUEviction — it fails about once in 50 runs on CI.",
		Agent: api.AgentClaude, Repo: src + "api-server", Worktree: Home + "/.pilothouse/worktrees/api-server-fix-flaky-lru", Branch: "ph/fix-flaky-lru", BaseRef: "main",
		SessionID: tk, Status: api.TaskRunning, CreatedAt: ago(9 * m), UpdatedAt: ago(9 * m)}
	add(9*m+time.Second, func() { c.PutTask(flaky) })
	session(api.Session{ID: tk, Agent: api.AgentClaude, Mode: api.ModeWrapped, Machine: c.Machine, Title: "api-server",
		Cwd: flaky.Worktree, RepoRoot: flaky.Worktree, Branch: flaky.Branch, TaskID: flaky.ID, StartedAt: ago(9 * m), Caps: wrappedCaps})
	item(tk, 9*m, prompt(flaky.Prompt, "pilothouse"))
	item(tk, 8*m+30*time.Second, run("Bash", "go test -run TestLRUEviction -count=200 ./internal/cache", "--- FAIL: TestLRUEviction (0.00s)\n    lru_test.go:61: evicted \"b\", want \"a\"\nFAIL: 3 of 200 runs", true))
	item(tk, 7*m, read("Read", "internal/cache/lru_test.go", "Read 118 lines"))
	item(tk, 5*m, say("The test fills the cache from a map, so insertion order changes between runs. I'll insert from a sorted slice."))
	item(tk, 2*m, edit("Edit", "internal/cache/lru_test.go", 9, 4), fileEdit("internal/cache/lru_test.go", 9, 4))
	item(tk, 25*time.Second, api.ActivityItem{Kind: api.ActivityTool, Tool: "Bash", ToolKind: api.ToolShell, Summary: "Run: go test -run TestLRUEviction -count=500 ./internal/cache", Pending: true})
	state(tk, 24*time.Second, func(s *api.Session) { s.StateDetail = "Running: go test -run TestLRUEviction -count=500" })
	c.tasks["t_flaky02"] = &taskState{diff: api.TaskDiff{TaskID: "t_flaky02", Files: []api.FilePreview{}}}

	shipped := api.Task{ID: "t_lint03", Title: "Turn on strict TypeScript in proofline", Prompt: "Turn on strict mode in tsconfig and fix what breaks.",
		Agent: api.AgentClaude, Repo: src + "proofline", Worktree: Home + "/.pilothouse/worktrees/proofline-strict-ts", Branch: "ph/strict-ts", BaseRef: "main",
		Status: api.TaskDone, CreatedAt: ago(7 * time.Hour), UpdatedAt: ago(5 * time.Hour)}
	add(5*time.Hour, func() { c.PutTask(shipped) })
	c.tasks["t_lint03"] = &taskState{pr: 131, diff: api.TaskDiff{TaskID: "t_lint03", Added: 1, Removed: 1, Files: []api.FilePreview{
		{Path: "tsconfig.json", Op: api.FileEdit, Added: 1, Removed: 1, Lang: "json", Diff: "@@ -3,7 +3,7 @@\n     \"target\": \"ES2022\",\n     \"module\": \"ESNext\",\n     \"moduleResolution\": \"Bundler\",\n-    \"strict\": false,\n+    \"strict\": true,\n     \"skipLibCheck\": true,\n     \"outDir\": \"dist\"\n   },\n"}}}}

	// Play the afternoon in order, then away from the desk.
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].at > steps[j].at })
	for _, s := range steps {
		at = ago(s.at)
		s.fn()
	}
	at = now
	_ = c.SetPresenceMode(context.Background(), api.PresenceForceAway)
	for _, s := range c.Sessions() {
		if s.ID == pl || s.ID == tf {
			continue
		}
		c.mu.Lock()
		c.seen[s.ID] = s.StartedAt.Add(now.Sub(s.StartedAt) / 3)
		c.mu.Unlock()
	}
}

const proofDone = "Done. Retries now back off exponentially (1 s, 4 s, 16 s) and stop after three attempts. Added a test for the ceiling; all 48 tests pass."

const faqDone = "Added the FAQ under the plans: five questions as an accessible disclosure list, linked from the plan cards. Build passes."

func notesQuestions() []api.Question {
	return []api.Question{
		{ID: "q1", Header: "Search index", Text: "Which search index should notes use?", AllowOther: true, Options: []api.QuestionOption{
			{Label: "SQLite FTS5", Description: "Built into the database we already ship; no new service. Good to about a million notes."},
			{Label: "Tantivy", Description: "Rust library with better ranking; adds a native build step."},
			{Label: "Meilisearch", Description: "A separate server with typo tolerance; one more thing to run."},
		}},
		{ID: "q2", Header: "Fields", Text: "Which fields should be searchable?", MultiSelect: true, Options: []api.QuestionOption{
			{Label: "Title"}, {Label: "Body"}, {Label: "Tags"}, {Label: "Attachments", Description: "Text extracted from PDFs; makes indexing about 3× slower."},
		}},
	}
}

const passkeyPlan = `# Passkey sign-in

Replace password sign-in with **passkeys** (WebAuthn), keeping passwords as a fallback for one release.

## Steps
1. Add ` + "`@simplewebauthn/server`" + ` and a ` + "`passkeys`" + ` table (user, credential id, public key, sign count).
2. New routes:
   - ` + "`POST /auth/passkey/register`" + ` — options, then verify
   - ` + "`POST /auth/passkey/login`" + ` — options, then verify
3. Settings: an *Add a passkey* button and a list of passkeys with *Remove*.
4. Sign-in page: offer the passkey first; keep the password form behind "Use a password instead".
5. Tests: unit tests for both verify handlers and a Playwright test with a virtual authenticator.

## Risks
- Safari on iOS 16 needs a user gesture before ` + "`navigator.credentials.get()`" + `.
- Existing sessions stay valid — nobody is signed out.

## Not in this change
- Removing passwords (next release, once we see how many people add a passkey).

` + "```sql" + `
CREATE TABLE passkeys (
  id            TEXT PRIMARY KEY,
  user_id       TEXT NOT NULL REFERENCES users(id),
  credential_id BLOB NOT NULL UNIQUE,
  public_key    BLOB NOT NULL,
  sign_count    INTEGER NOT NULL DEFAULT 0,
  created_at    TIMESTAMP NOT NULL
);
` + "```" + `

Background: [WebAuthn guide](https://webauthn.guide).
`

func terraformPreview() *api.Preview {
	return &api.Preview{Added: 3, Removed: 3, Files: []api.FilePreview{{
		Path: "terraform/prod/main.tf", Op: api.FileEdit, Added: 3, Removed: 3, Lang: "hcl",
		Diff: `@@ -14,10 +14,10 @@ resource "aws_lambda_function" "api" {
   function_name = "proofline-api-prod"
   role          = aws_iam_role.lambda.arn
   handler       = "dist/handler.main"
-  runtime       = "nodejs18.x"
-  memory_size   = 512
+  runtime       = "nodejs22.x"
+  memory_size   = 1024
   timeout       = 15

   environment {
     variables = {
       NODE_ENV = "production"
@@ -41,7 +41,7 @@ resource "aws_lambda_function" "webhooks" {
   function_name = "proofline-webhooks-prod"
   role          = aws_iam_role.lambda.arn
   handler       = "dist/webhooks.main"
-  runtime       = "nodejs18.x"
+  runtime       = "nodejs22.x"
   memory_size   = 256
   timeout       = 30
 }
`}}}
}

func benchPreview() *api.Preview {
	var b strings.Builder
	lines := []string{
		"package cache",
		"",
		"import (",
		"\t\"strconv\"",
		"\t\"testing\"",
		")",
		"",
		"// BenchmarkGet guards the allocation-free Get: CI fails if it allocates.",
		"func BenchmarkGet(b *testing.B) {",
		"\tc := New[string, int](1024)",
		"\tkeys := make([]string, 1024)",
		"\tfor i := range keys {",
		"\t\tkeys[i] = strconv.Itoa(i)",
		"\t\tc.Put(keys[i], i)",
		"\t}",
		"\tb.ReportAllocs()",
		"\tb.ResetTimer()",
		"\tfor i := 0; i < b.N; i++ {",
		"\t\tc.Get(keys[i&1023])",
		"\t}",
		"\tif a := testing.AllocsPerRun(100, func() { c.Get(keys[7]) }); a != 0 {",
		"\t\tb.Fatalf(\"Get allocates %v times per call\", a)",
		"\t}",
		"}",
		"",
		"func BenchmarkGetParallel(b *testing.B) {",
		"\tc := New[string, int](1024)",
		"\tc.Put(\"k\", 1)",
		"\tb.ReportAllocs()",
		"\tb.RunParallel(func(pb *testing.PB) {",
		"\t\tfor pb.Next() {",
		"\t\t\tc.Get(\"k\")",
		"\t\t}",
		"\t})",
		"}",
	}
	b.WriteString("@@ -0,0 +1," + itoa(len(lines)) + " @@\n")
	for _, l := range lines {
		b.WriteString("+" + l + "\n")
	}
	return &api.Preview{Added: len(lines), Files: []api.FilePreview{{Path: "internal/cache/lru_bench_test.go", Op: api.FileCreate, Added: len(lines), Lang: "go", Diff: b.String()}}}
}

func contrastPreview() *api.Preview {
	return &api.Preview{Added: 6, Removed: 3, Files: []api.FilePreview{
		{Path: "src/components/PlanCard.tsx", Op: api.FileEdit, Added: 2, Removed: 1, Lang: "tsx", Diff: `@@ -22,5 +22,6 @@ export function PlanCard({ plan, yearly }: PlanCardProps) {
   return (
-    <article className="plan-card">
+    <article className="plan-card" tabIndex={-1}>
+      {yearly && <span className="badge badge-yearly">2 months free</span>}
       <h3>{plan.name}</h3>
       <p className="price">{formatPrice(yearly ? plan.yearly : plan.monthly)}</p>
       <ul>
`},
		{Path: "src/styles/pricing.css", Op: api.FileEdit, Added: 4, Removed: 2, Lang: "css", Diff: `@@ -40,7 +40,9 @@
 .badge-yearly {
-  background: #fde68a;
-  color: #a16207;
+  background: #fef3c7;
+  color: #78350f; /* 5.2:1 */
 }

 .plan-card:focus-within {
+  outline: 2px solid var(--brand);
+  outline-offset: 3px;
 }
`},
	}}
}

func faqDiff() api.TaskDiff {
	return api.TaskDiff{TaskID: "t_faq01", Added: 37, Removed: 1, Files: []api.FilePreview{
		{Path: "src/pages/pricing.tsx", Op: api.FileEdit, Added: 6, Removed: 1, Lang: "tsx", Diff: `@@ -1,5 +1,6 @@
 import { useState } from "react";
 import { PlanCard } from "../components/PlanCard";
+import { Faq } from "../components/Faq";
 import { Toggle } from "../components/Toggle";
 import { plans } from "../data/plans";

@@ -48,7 +49,11 @@ export default function Pricing() {
           <PlanCard key={p.id} plan={p} yearly={yearly} />
         ))}
       </div>
-      <a href="/pricing#faq">Questions? Read the FAQ</a>
+      <section id="faq" aria-labelledby="faq-title">
+        <h2 id="faq-title">Questions</h2>
+        <Faq />
+      </section>
+      <a href="#faq">Questions? Read the FAQ</a>
     </main>
   );
 }
`},
		{Path: "src/components/Faq.tsx", Op: api.FileCreate, Added: 31, Lang: "tsx", Diff: `@@ -0,0 +1,31 @@
+const QUESTIONS = [
+  { q: "Can I cancel anytime?", a: "Yes. You keep access until the end of the billing period." },
+  { q: "Do you offer refunds?", a: "Within 14 days of any charge, no questions asked." },
+  { q: "Is there a student discount?", a: "Pro is free with a school email address." },
+  { q: "Can I change plans later?", a: "Any time; we prorate the difference." },
+  { q: "Where is my data stored?", a: "In the EU (Frankfurt), encrypted at rest." },
+];
+
+export function Faq() {
+  return (
+    <div className="faq">
+      {QUESTIONS.map(({ q, a }) => (
+        <details key={q}>
+          <summary>{q}</summary>
+          <p>{a}</p>
+        </details>
+      ))}
+    </div>
+  );
+}
+
+// Styles live in pricing.css (.faq, details, summary).
+// Each <details> is keyboard accessible without extra code:
+//   Enter / Space toggles it, and screen readers announce
+//   "expanded" / "collapsed".
+//
+// The questions come from docs/faq.md; keep both in sync
+// until the CMS migration lands.
+
+export const FAQ_COUNT = QUESTIONS.length;
+export default Faq;
`},
	}}
}

func flakyDiff(id string) api.TaskDiff {
	return api.TaskDiff{TaskID: id, Added: 9, Removed: 4, Files: []api.FilePreview{{
		Path: "internal/cache/lru_test.go", Op: api.FileEdit, Added: 9, Removed: 4, Lang: "go", Diff: `@@ -48,10 +48,15 @@ func TestLRUEviction(t *testing.T) {
 	c := New[string, int](3)
-	seed := map[string]int{"a": 1, "b": 2, "c": 3}
-	for k, v := range seed {
-		c.Put(k, v)
+	// Insert in a fixed order: map iteration order is random, which made
+	// the expected eviction ("a", the oldest) fail about once in 50 runs.
+	seed := []struct {
+		k string
+		v int
+	}{{"a", 1}, {"b", 2}, {"c", 3}}
+	for _, s := range seed {
+		c.Put(s.k, s.v)
 	}
 	c.Put("d", 4)
 	if _, ok := c.Get("a"); ok {
-		t.Fatalf("evicted %q, want %q", "b", "a")
+		t.Fatal(` + "`" + `"a" should have been evicted` + "`" + `)
 	}
 }
`}}}
}
