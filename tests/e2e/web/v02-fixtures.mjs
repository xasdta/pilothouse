// Fixtures for the 0.2 view checks: one believable afternoon, shaped like
// internal/api/v02.go (Activity, CatchUp, Usage, Insights, Task, TaskDiff…).

const MIN = 60e3;
const iso = (t) => new Date(t).toISOString();

export function world(now = Date.now()) {
  const ago = (m) => iso(now - m * MIN);
  const usageA = { input_tokens: 48210, output_tokens: 18334, cache_read_tokens: 1120400, cache_write_tokens: 61200, cost_usd: 2.37, model: 'claude-sonnet-4-5', turns: 4 };
  const usageB = { input_tokens: 9100, output_tokens: 4022, cache_read_tokens: 210000, cache_write_tokens: 12000, cost_usd: 0.41, model: 'claude-haiku-4-5', turns: 2 };
  const sessions = [
    { id: 's_web01', agent: 'claude', mode: 'wrapped', machine: 'studio', title: 'pilothouse', cwd: '/Users/dev/src/pilothouse', repo_root: '/Users/dev/src/pilothouse', branch: 'feat/session-page', state: 'working', state_detail: 'Running go test -race ./internal/web/...', started_at: ago(52), updated_at: ago(0.2), caps: { reply: true, reply_via: 'terminal', interrupt: true, watch: true, activity: true }, usage: usageA, unseen: false },
    { id: 's_api02', agent: 'claude', mode: 'observed', machine: 'studio', title: 'api-server', cwd: '/Users/dev/src/api-server', repo_root: '/Users/dev/src/api-server', branch: 'perf/lru-cache', state: 'waiting', last_message: 'Done — the LRU cache now evicts by size, and the benchmark is 38% faster. Want me to open a PR?', started_at: ago(140), updated_at: ago(6), caps: { reply: true, reply_via: 'message', activity: true }, usage: usageB, unseen: true },
    { id: 's_site03', agent: 'codex', mode: 'observed', machine: 'studio', title: 'site', cwd: '/Users/dev/src/site', repo_root: '/Users/dev/src/site', branch: 'feat/pricing', state: 'working', state_detail: 'Running npm run build', started_at: ago(30), updated_at: ago(1), caps: { activity: true } },
    { id: 's_notes4', agent: 'codex', mode: 'observed', machine: 'studio', title: 'notes', cwd: '/Users/dev/src/notes', repo_root: '/Users/dev/src/notes', branch: 'main', state: 'waiting', started_at: ago(70), updated_at: ago(20), caps: {} },
    { id: 's_task05', agent: 'claude', mode: 'wrapped', machine: 'studio', title: 'proofline', cwd: '/Users/dev/.pilothouse/worktrees/proofline-webhook-retries', repo_root: '/Users/dev/src/proofline', branch: 'ph/webhook-retries', state: 'waiting', task_id: 't_retry01', started_at: ago(95), updated_at: ago(9), caps: { reply: true, reply_via: 'terminal', interrupt: true, watch: true, activity: true }, usage: { input_tokens: 30100, output_tokens: 9800, cache_read_tokens: 640000, cache_write_tokens: 41000, cost_usd: 1.12, model: 'claude-sonnet-4-5', turns: 3 } },
    { id: 's_old006', agent: 'claude', mode: 'observed', machine: 'studio', title: 'dotfiles', cwd: '/Users/dev/dotfiles', state: 'exited', started_at: ago(300), updated_at: ago(200), exited_at: ago(200), exit_code: 0, caps: {} },
  ];
  const requests = [
    { id: 'r_push01', session_id: 's_web01', agent: 'claude', checkpoint: 'permission', tool: 'Bash', tool_kind: 'shell', command: 'git push origin feat/session-page', summary: 'Run: git push origin feat/session-page', cwd: '/Users/dev/src/pilothouse', status: 'open', route: 'devices', policy_rule: 'ask-before-push', created_at: ago(1), expires_at: iso(now + 8 * MIN) },
  ];
  const tasks = [
    { id: 't_retry01', title: 'Retry failed webhooks with backoff', prompt: 'Webhook deliveries fail silently when the endpoint is down. Retry them with exponential backoff (1s, 4s, 16s), stop after three attempts, and add tests for the ceiling.', agent: 'claude', repo: '/Users/dev/src/proofline', worktree: '/Users/dev/.pilothouse/worktrees/proofline-webhook-retries', branch: 'ph/webhook-retries', base_ref: 'main', session_id: 's_task05', status: 'review', by: 'device:SELF', created_at: ago(95), updated_at: ago(9) },
    { id: 't_dark002', title: 'Dark mode for the pricing page', prompt: 'Add a dark theme to the pricing page that follows prefers-color-scheme.', agent: 'codex', repo: '/Users/dev/src/site', worktree: '/Users/dev/.pilothouse/worktrees/site-dark-pricing', branch: 'ph/dark-pricing', base_ref: 'main', session_id: 's_site03', status: 'running', by: 'cli', created_at: ago(30), updated_at: ago(1) },
    { id: 't_docs003', title: 'Document the cache flags', prompt: 'Document every cache flag in docs/CACHE.md with examples.', agent: 'claude', repo: '/Users/dev/src/api-server', worktree: '/Users/dev/.pilothouse/worktrees/api-cache-docs', branch: 'ph/cache-docs', base_ref: 'perf/lru-cache', status: 'done', by: 'device:d_other01', created_at: ago(60 * 26), updated_at: ago(60 * 20) },
    { id: 't_spike04', title: 'Try SQLite FTS for search', prompt: 'Spike: replace the search index with SQLite FTS5 and measure.', agent: 'codex', repo: '/Users/dev/src/notes', worktree: '/Users/dev/.pilothouse/worktrees/notes-fts', branch: 'ph/fts-spike', base_ref: 'main', status: 'abandoned', created_at: ago(60 * 50), updated_at: ago(60 * 48) },
  ];
  const repos = [
    { root: '/Users/dev/src/pilothouse', name: 'pilothouse', branch: 'main', last_used: ago(2) },
    { root: '/Users/dev/src/proofline', name: 'proofline', branch: 'main', last_used: ago(9) },
    { root: '/Users/dev/src/api-server', name: 'api-server', branch: 'perf/lru-cache', last_used: ago(6) },
    { root: '/Users/dev/src/site', name: 'site', branch: 'main', last_used: ago(30) },
  ];
  const snapshot = {
    seq: 1e9, machine: 'studio', version: '0.2.0-dev', sessions, requests, tasks,
    presence: { state: 'away', mode: 'away', source: 'manual', changed_at: ago(90) },
  };
  return { now, ago, sessions, requests, tasks, repos, snapshot, usageA };
}

// ---------- Activity ----------

/** The first page for s_web01: four turns, subagents, an error, a pending call. */
export function activityWeb(w) {
  const { ago } = w;
  let n = 0;
  const it = (o) => ({ id: o.id || `a${++n}`, at: ago(o.m), turn: o.turn, kind: o.kind, ...o, m: undefined });
  const items = [
    it({ turn: 1, m: 50, kind: 'prompt', source: 'human', text: 'The session page is empty. Build the activity feed: group by turn, show tool calls with their results, subagents, errors, and the files changed.' }),
    it({ turn: 1, m: 49.5, kind: 'text', text: 'I’ll start by reading the current session view and the 0.2 wire types.' }),
    it({ turn: 1, m: 49, kind: 'tool', tool: 'Read', tool_kind: 'read', summary: 'Read internal/api/v02.go', result: 'package api\n\n// Wire types added in 0.2 …\n(434 lines)' }),
    it({ turn: 1, m: 48.6, kind: 'tool', tool: 'Grep', tool_kind: 'read', summary: 'Search “ActivityItem” in internal/', result: 'internal/api/v02.go:181\ninternal/daemon/activity.go:18' }),
    it({ turn: 1, m: 47, kind: 'tool', tool: 'Edit', tool_kind: 'edit', summary: 'Edit internal/web/static/js/views/session.js', result: 'Applied 3 edits' }),
    it({ turn: 1, m: 44, kind: 'tool', tool: 'Bash', tool_kind: 'shell', summary: 'Run: go vet ./...', result: '' }),
    it({ turn: 1, m: 43, kind: 'text', text: 'The feed renders. Next I’ll add the files-changed list.' }),
    it({ turn: 2, m: 40, kind: 'prompt', source: 'pilothouse', text: 'Also show tokens and the estimated cost in the header.' }),
    it({ turn: 2, m: 39, kind: 'tool', tool: 'Edit', tool_kind: 'edit', summary: 'Edit internal/web/static/css/views.css', result: 'Applied 1 edit' }),
    it({ turn: 2, m: 38, kind: 'tool', tool: 'WebFetch', tool_kind: 'web', summary: 'Fetch developer.mozilla.org/en-US/docs/Web/HTML/Element/details', result: 'The <details> HTML element creates a disclosure widget…' }),
    it({ turn: 2, m: 36, kind: 'text', text: 'Added a usage chip with a breakdown of input, output and cache tokens.' }),
    it({ turn: 3, m: 30, kind: 'prompt', source: 'human', text: 'Run the e2e suite and fix whatever fails.' }),
    it({ turn: 3, m: 29, kind: 'subagent', summary: 'Explore: find the e2e harness and how it starts the devserver' }),
    it({ turn: 3, m: 28.8, kind: 'tool', tool: 'Glob', tool_kind: 'read', sidechain: true, summary: 'Find tests/e2e/web/*.mjs', result: '01-pair.mjs\n02-inbox.mjs\n06-screens.mjs\nlib.mjs' }),
    it({ turn: 3, m: 28.5, kind: 'tool', tool: 'Read', tool_kind: 'read', sidechain: true, summary: 'Read tests/e2e/web/lib.mjs', result: 'export const BASE = …' }),
    it({ turn: 3, m: 28.2, kind: 'text', sidechain: true, text: 'The harness pairs through the real UI and shells out to a QA env script.' }),
    it({ turn: 3, m: 28, kind: 'subagent', summary: 'Explore finished: 3 files read' }),
    it({ turn: 3, m: 26, kind: 'tool', tool: 'Bash', tool_kind: 'shell', summary: 'Run: node tests/e2e/web/20-session.mjs', is_error: true, result: 'FAIL  feed groups by turn — expected 4 turns, saw 3\nFAIL  pending spinner\n\n2 failed' }),
    it({ turn: 3, m: 25, kind: 'error', summary: 'API error', text: 'Overloaded (529). Retrying in 4 s.' }),
    it({ turn: 3, m: 24, kind: 'tool', tool: 'Edit', tool_kind: 'edit', summary: 'Edit internal/web/static/js/views/session.js', result: 'Applied 2 edits' }),
    it({ turn: 3, m: 22, kind: 'tool', tool: 'Bash', tool_kind: 'shell', summary: 'Run: node tests/e2e/web/20-session.mjs', result: '38/38 passed' }),
    it({ turn: 3, m: 21, kind: 'system', text: 'Context compacted' }),
    it({ turn: 4, m: 12, kind: 'prompt', source: 'human', text: 'Great. Now run the whole Go suite with the race detector and push the branch.' }),
    it({ turn: 4, m: 11, kind: 'text', text: 'Running the full suite first; I’ll push once it’s green.' }),
    it({ turn: 4, m: 10, kind: 'tool', tool: 'Bash', tool_kind: 'shell', summary: 'Run: gofmt -l .', result: '' }),
    it({ turn: 4, m: 9, kind: 'tool', tool: 'Bash', tool_kind: 'shell', summary: 'Run: go vet ./...', result: '' }),
    it({ turn: 4, m: 6, kind: 'tool', tool: 'mcp__github__list_prs', tool_kind: 'mcp', summary: 'GitHub: list open pull requests', result: '#212 web: inbox cards (open)\n#214 daemon: activity reader (draft)' }),
    it({ id: 'a_pending', turn: 4, m: 0.5, kind: 'tool', tool: 'Bash', tool_kind: 'shell', summary: 'Run: go test -race ./...', pending: true }),
  ];
  return {
    session_id: 's_web01', available: true, items, cursor: 'c1', usage: w.usageA,
    files: [
      { path: '/Users/dev/src/pilothouse/internal/web/static/js/views/session.js', op: 'edit', added: 412, removed: 96 },
      { path: '/Users/dev/src/pilothouse/internal/web/static/css/views.css', op: 'edit', added: 188, removed: 1 },
      { path: '/Users/dev/src/pilothouse/tests/e2e/web/20-session.mjs', op: 'create', added: 164, removed: 0 },
      { path: '/Users/dev/src/pilothouse/internal/web/static/js/views/old-timeline.js', op: 'delete', added: 0, removed: 37 },
    ],
    catch_up: {
      since: ago(55), tool_calls: 19, files: 4, errors: 1,
      lines: [
        'Built the activity feed and a usage chip on the session page.',
        'Tests: 20-session.mjs failed once, then passed (38/38).',
        'Now running go test -race ./... before pushing feat/session-page.',
        'Wants to run git push origin feat/session-page — waiting for you.',
      ],
    },
  };
}

/** What the next poll (cursor c1) returns: the pending call finished, plus a new step. */
export function activityWebNext(w) {
  const { ago } = w;
  return {
    session_id: 's_web01', available: true, cursor: 'c2', usage: { ...w.usageA, output_tokens: w.usageA.output_tokens + 900, cost_usd: 2.41 },
    items: [
      { id: 'a_pending', turn: 4, at: ago(0.5), kind: 'tool', tool: 'Bash', tool_kind: 'shell', summary: 'Run: go test -race ./...', result: 'ok  github.com/xasdta/pilothouse/internal/web\t4.112s\n… 31 packages ok' },
      { id: 'a_new1', turn: 4, at: ago(0.1), kind: 'text', text: 'All green. Pushing feat/session-page now.' },
    ],
  };
}

/** A long activity (for "show earlier"): the newest n items, per items to a turn. */
export function activityLong(w, n, sid = 's_api02', per = 10, total = n) {
  const items = [];
  for (let i = total - n; i < total; i++) {
    const turn = Math.floor(i / per) + 1;
    items.push(i % per === 0
      ? { id: `L${i}`, turn, at: w.ago(total - i), kind: 'prompt', source: 'human', text: `Prompt number ${turn}` }
      : { id: `L${i}`, turn, at: w.ago(total - i), kind: 'tool', tool: 'Bash', tool_kind: 'shell', summary: `Run: step ${i}`, result: `output ${i}` });
  }
  return { session_id: sid, available: true, items, cursor: 'L' + total, usage: null, files: [] };
}

// ---------- Insights ----------

function rnd(seed) {
  let s = seed >>> 0;
  return () => { s = (s * 1664525 + 1013904223) >>> 0; return s / 2 ** 32; };
}

export function insights(days, now = Date.now(), budget = { daily_usd: 10, spent_usd: 6.1, percent: 61, burn_usd_per_hour: 1.2 }) {
  const r = rnd(days * 7919);
  const list = [];
  const total = { input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, cache_write_tokens: 0, cost_usd: 0 };
  let auto = 0;
  let asked = 0;
  let blocked = 0;
  for (let i = days - 1; i >= 0; i--) {
    const d = new Date(now - i * 86400e3);
    const weekend = d.getDay() === 0 || d.getDay() === 6;
    const k = (weekend ? 0.25 : 1) * (0.55 + r() * 0.9);
    const u = {
      input_tokens: Math.round(160000 * k), output_tokens: Math.round(52000 * k),
      cache_read_tokens: Math.round(3400000 * k), cache_write_tokens: Math.round(180000 * k),
      cost_usd: Math.round(820 * k) / 100,
    };
    for (const key of Object.keys(total)) total[key] += u[key];
    const day = {
      date: d.toISOString().slice(0, 10), usage: u, sessions: Math.max(1, Math.round(9 * k)),
      auto_allowed: Math.round(180 * k), asked: Math.round(7 * k), blocked: Math.round(2 * k * r()),
    };
    auto += day.auto_allowed;
    asked += day.asked;
    blocked += day.blocked;
    list.push(day);
  }
  total.cost_usd = Math.round(total.cost_usd * 100) / 100;
  const part = (f, model) => ({
    input_tokens: Math.round(total.input_tokens * f), output_tokens: Math.round(total.output_tokens * f),
    cache_read_tokens: Math.round(total.cache_read_tokens * f), cache_write_tokens: Math.round(total.cache_write_tokens * f),
    cost_usd: Math.round(total.cost_usd * f * 100) / 100, model,
  });
  return {
    from: list[0].date + 'T00:00:00Z', to: list[list.length - 1].date + 'T23:59:59Z', total, days: list,
    by_agent: [
      { key: 'claude', usage: part(0.74), sessions: Math.round(days * 5.1) },
      { key: 'codex', usage: part(0.24), sessions: Math.round(days * 2.2) },
      { key: 'opencode', usage: part(0.02), sessions: Math.round(days * 0.3) },
    ],
    by_model: [
      { key: 'claude-sonnet-4-5', usage: part(0.61, 'claude-sonnet-4-5'), sessions: Math.round(days * 3.9) },
      { key: 'gpt-5-codex', usage: part(0.24, 'gpt-5-codex'), sessions: Math.round(days * 2.2) },
      { key: 'claude-haiku-4-5', usage: part(0.13, 'claude-haiku-4-5'), sessions: Math.round(days * 1.2) },
      { key: 'local-llama', usage: { ...part(0.02), cost_usd: 0 }, sessions: Math.round(days * 0.3) },
    ],
    by_repo: [
      { key: 'pilothouse', usage: part(0.46), sessions: Math.round(days * 3) },
      { key: 'proofline', usage: part(0.22), sessions: Math.round(days * 1.6) },
      { key: 'api-server', usage: part(0.17), sessions: Math.round(days * 1.4) },
      { key: 'site', usage: part(0.11), sessions: Math.round(days * 1.1) },
      { key: 'notes', usage: part(0.04), sessions: Math.round(days * 0.5) },
    ],
    rules: {
      auto_allowed: auto, asked, blocked, granted: Math.round(asked * 0.3), answered_by_device: Math.round(asked * 0.55),
      answered_at_terminal: Math.round(asked * 0.35), expired: Math.round(asked * 0.1),
      median_answer_secs: 38, blocked_minutes: Math.round(asked * 1.7),
      top_rules: [
        { key: 'allow-tests', count: Math.round(auto * 0.31) }, { key: 'allow-read-only-git', count: Math.round(auto * 0.22) },
        { key: 'allow-edits-in-repo', count: Math.round(auto * 0.18) }, { key: 'grant: npm run', count: Math.round(auto * 0.07) },
        { key: 'allow-package-scripts', count: Math.round(auto * 0.05) },
      ],
      top_blocked: [
        { key: 'deny-rm-rf', count: Math.max(1, Math.round(blocked * 0.5)) }, { key: 'no-secrets', count: Math.max(1, Math.round(blocked * 0.3)) },
        { key: 'protect-main', count: Math.max(0, Math.round(blocked * 0.2)) },
      ],
    },
    budget,
  };
}

// ---------- Tasks ----------

export const diffRetry = {
  task_id: 't_retry01', added: 61, removed: 9, truncated: false,
  files: [
    {
      path: 'internal/webhooks/deliver.go', op: 'edit', added: 34, removed: 8, lang: 'go',
      diff: '@@ -12,14 +12,30 @@ import (\n \t"time"\n )\n \n-// deliver posts the event once.\n-func deliver(ctx context.Context, c *http.Client, e Event) error {\n-\treturn post(ctx, c, e)\n+// backoff is the wait before each retry: 1s, 4s, 16s.\n+var backoff = []time.Duration{time.Second, 4 * time.Second, 16 * time.Second}\n+\n+// deliver posts the event, retrying failed deliveries with backoff.\n+func deliver(ctx context.Context, c *http.Client, e Event) error {\n+\tvar err error\n+\tfor i := 0; i <= len(backoff); i++ {\n+\t\tif err = post(ctx, c, e); err == nil {\n+\t\t\treturn nil\n+\t\t}\n+\t\tif i == len(backoff) {\n+\t\t\tbreak\n+\t\t}\n+\t\tselect {\n+\t\tcase <-ctx.Done():\n+\t\t\treturn ctx.Err()\n+\t\tcase <-time.After(backoff[i]):\n+\t\t}\n+\t}\n+\treturn fmt.Errorf("deliver %s: gave up after %d attempts: %w", e.ID, len(backoff)+1, err)\n }\n \n func post(ctx context.Context, c *http.Client, e Event) error {\n@@ -40,7 +56,9 @@ func post(ctx context.Context, c *http.Client, e Event) error {\n \tif err != nil {\n \t\treturn err\n \t}\n-\tresp.Body.Close()\n+\tdefer resp.Body.Close()\n+\tif resp.StatusCode >= 500 {\n+\t\treturn fmt.Errorf("endpoint answered %s", resp.Status)\n+\t}\n \treturn nil\n }\n',
    },
    {
      path: 'internal/webhooks/deliver_test.go', op: 'create', added: 26, removed: 0, lang: 'go',
      diff: '@@ -0,0 +1,26 @@\n+package webhooks\n+\n+import (\n+\t"context"\n+\t"testing"\n+)\n+\n+// The fourth failure gives up instead of retrying forever.\n+func TestDeliverGivesUpAfterThreeRetries(t *testing.T) {\n+\tbackoff = []time.Duration{0, 0, 0}\n+\tsrv, hits := failingServer(t)\n+\terr := deliver(context.Background(), srv.Client(), Event{ID: "e1"})\n+\tif err == nil || *hits != 4 {\n+\t\tt.Fatalf("err %v after %d hits", err, *hits)\n+\t}\n+}\n',
    },
    { path: 'CHANGELOG.md', op: 'edit', added: 1, removed: 1, lang: 'md', diff: '@@ -3,1 +3,1 @@\n-- Webhooks: deliveries are attempted once.\n+- Webhooks: failed deliveries are retried with backoff (1s, 4s, 16s).\n' },
  ],
};

export const diffDark = {
  task_id: 't_dark002', added: 18, removed: 2, truncated: true,
  files: [
    { path: 'src/pages/pricing.css', op: 'edit', added: 18, removed: 2, lang: 'css', truncated: true, diff: '@@ -1,4 +1,20 @@\n :root {\n-  --bg: #fff;\n-  --ink: #111;\n+  --bg: #fff;\n+  --ink: #111;\n+}\n+@media (prefers-color-scheme: dark) {\n+  :root { --bg: #0b0d10; --ink: #eef1f5; }\n }\n' },
  ],
};

// ---------- Mounting ----------

/** Registers the fixture routes on a v02-harness router. */
export function mount(apiR, w, opts = {}) {
  const sessions = new Map(w.sessions.map((s) => [s.id, s]));
  apiR.on('GET', /^\/api\/snapshot$/, () => ({ json: w.snapshot }));
  apiR.on('GET', /^\/api\/events$/, () => ({ sse: '/events' }));
  apiR.on('GET', /^\/api\/sessions\/([\w-]+)$/, ({ m }) => (sessions.has(m[1]) ? { json: sessions.get(m[1]) } : { status: 404, json: { error: 'not found', code: 'not_found' } }));
  apiR.on('POST', /^\/api\/sessions\/[\w-]+\/seen$/, () => ({ status: 204 }));
  apiR.on('GET', /^\/api\/sessions\/([\w-]+)\/events$/, ({ m }) => ({ json: fallbackEvents(w, m[1]) }));
  apiR.on('GET', /^\/api\/sessions\/([\w-]+)\/activity$/, ({ m, url }) => {
    const sid = m[1];
    if (opts.activity && opts.activity[sid]) return opts.activity[sid](url);
    if (!sessions.has(sid)) return { status: 404, json: { error: 'not found', code: 'not_found' } };
    if (sid === 's_web01') return { json: url.searchParams.get('cursor') ? activityWebNext(w) : activityWeb(w) };
    if (sid === 's_notes4') return { json: { session_id: sid, available: false, note: 'Codex keeps its transcript where Pilothouse can’t read it on this machine.', items: [], cursor: '' } };
    if (sid === 's_site03') return { status: 501, json: { error: 'not supported here', code: 'unsupported' } };
    return { json: activityLong(w, 24, sid) };
  });
  apiR.on('POST', /^\/api\/sessions\/[\w-]+\/reply$/, () => ({ json: { via: 'terminal' } }));
  apiR.on('GET', /^\/api\/insights$/, ({ url }) => ({ json: insights(Number(url.searchParams.get('days')) || 7, w.now) }));
  apiR.on('GET', /^\/api\/repos$/, () => ({ json: w.repos }));
  apiR.on('GET', /^\/api\/tasks$/, () => ({ json: w.tasks }));
  apiR.on('GET', /^\/api\/tasks\/([\w-]+)\/diff$/, ({ m }) => ({ json: m[1] === 't_dark002' ? diffDark : m[1] === 't_retry01' ? diffRetry : { task_id: m[1], files: [], added: 0, removed: 0 } }));
  return { sessions };
}

// ---------- Terminal ----------

const ESC = '\x1b[';
const rgb = (r, g, b) => `${ESC}38;2;${r};${g};${b}m`;
/** A screenful of a Claude-like TUI, with truecolor and 16-colour text. */
export const TERM_SCREEN = [
  `${rgb(215, 119, 87)}╭───────────────────────────────────────────╮${ESC}0m\r\n`,
  `${rgb(215, 119, 87)}│${ESC}0m ${rgb(215, 119, 87)}✻${ESC}0m Welcome to ${ESC}1mClaude Code${ESC}0m                    ${rgb(215, 119, 87)}│${ESC}0m\r\n`,
  `${rgb(215, 119, 87)}│${ESC}0m   ${ESC}2mcwd: ~/src/pilothouse${ESC}0m                   ${rgb(215, 119, 87)}│${ESC}0m\r\n`,
  `${rgb(215, 119, 87)}╰───────────────────────────────────────────╯${ESC}0m\r\n\r\n`,
  `${ESC}2m>${ESC}0m Run the whole Go suite with the race detector\r\n\r\n`,
  `${ESC}32m⏺${ESC}0m ${ESC}1mBash${ESC}0m(go test -race ./...)\r\n`,
  `  ⎿  ok   github.com/xasdta/pilothouse/internal/api      0.412s\r\n`,
  `     ok   github.com/xasdta/pilothouse/internal/web      4.112s\r\n`,
  `     ${ESC}31mFAIL${ESC}0m github.com/xasdta/pilothouse/internal/task     2.020s\r\n\r\n`,
  `${ESC}32m⏺${ESC}0m One test failed in internal/task. Fix it before pushing?\r\n\r\n`,
  `${rgb(120, 120, 120)}─────────────────────────────────────────────${ESC}0m\r\n`,
  `${ESC}1m❯${ESC}0m `,
].join('');

/**
 * Mocks the terminal routes for one session. The stream sends the screen
 * on every connect (a replay); returns what the test needs to inspect.
 */
export function mountTerminal(apiR, sse, sid, opts = {}) {
  const st = { tokens: 0, inputs: [] };
  apiR.on('POST', new RegExp(`^/api/sessions/${sid}/terminal/token$`), () => {
    st.tokens++;
    if (opts.tokenStatus) return { status: opts.tokenStatus, json: { error: opts.tokenError || 'typing is not allowed here', code: 'forbidden' } };
    return { json: { token: `tok-${st.tokens}-${'x'.repeat(24)}`, expires_at: new Date(Date.now() + 600e3).toISOString() } };
  });
  apiR.on('GET', new RegExp(`^/api/sessions/${sid}/terminal$`), () => ({ sse: `/term/${sid}` }));
  apiR.on('POST', new RegExp(`^/api/sessions/${sid}/terminal/input$`), ({ body, headers }) => {
    let b = {};
    try { b = JSON.parse(body); } catch { /* recorded as is */ }
    st.inputs.push({ token: headers['x-terminal-token'], csrf: headers['x-pilothouse'], ...b, text: b.data ? Buffer.from(b.data, 'base64').toString('utf8') : undefined });
    if (opts.inputStatus) return { status: opts.inputStatus, json: { error: 'input is not allowed for this session', code: 'forbidden' } };
    return { status: 204 };
  });
  if (!opts.noReplay) {
    sse.onConnect(`/term/${sid}`, (res) => {
      if (opts.size) sse.write(res, 'size', opts.size);
      sse.write(res, 'data', Buffer.from(opts.screen || TERM_SCREEN).toString('base64'));
    });
  }
  return st;
}

/** Hook events for the fallback timeline (newest first, like the API). */
export function fallbackEvents(w, sid) {
  const ev = (seq, m, type, data) => ({ seq, at: w.ago(m), type, session_id: sid, data });
  return [
    ev(9, 3, 'hook', { kind: 'stop', message: 'Done — notes are indexed.' }),
    ev(8, 9, 'hook', { kind: 'pre_tool', tool: 'shell', tool_kind: 'shell', command: 'npm run index', summary: 'Run: npm run index' }),
    ev(7, 14, 'hook', { kind: 'pre_tool', tool: 'apply_patch', tool_kind: 'edit', paths: ['/Users/dev/src/notes/src/index.ts'], cwd: '/Users/dev/src/notes', summary: 'Edit src/index.ts' }),
    ev(6, 18, 'hook', { kind: 'prompt_submit', message: 'Index every note on save.' }),
    ev(5, 20, 'session.started', {}),
  ];
}
