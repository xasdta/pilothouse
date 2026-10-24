# 0.2 workstreams

Read `docs/PLAN-0.2.md` first. The contract is on `main` (commits
"0.2 contract" and "0.2 hook points"): wire types in `internal/api/v02.go`,
optional capabilities in `internal/core/v02.go`, device routes in
`internal/web/v02.go` (+ `signed.go`, `terminal.go`), local routes in
`internal/daemon/server_v02.go`, client methods in `internal/client/v02.go`,
daemon stubs (`reply.go`, `activity.go`, `rules.go`, `taskrun.go`,
`plugin.go`, `enrich.go`, `caps.go`), the 0.2 store migration
(`session_secrets`, `usage_daily`), config sections (`notify.telegram`,
`usage`, `keep_awake`, `tasks`), stub PWA views and `internal/demo`.

## Rules for every workstream

**Isolation and safety (non-negotiable)**
- You work in your own git worktree. Commit there, on your branch. Never
  push, merge or rebase other branches, and never touch the main checkout.
- The user runs a real Pilothouse daemon (`~/.pilothouse`, launchd job
  `dev.pilothouse.daemon`) with hooks installed in their real
  `~/.claude/settings.json`. Never touch `~/.pilothouse`, `~/.claude`,
  `~/.codex`, `~/.config/opencode` or the launchd job. Every daemon you
  run uses temporary `PILOTHOUSE_HOME`, `PILOTHOUSE_USER_HOME` and
  `PILOTHOUSE_RUN_DIR` (see `internal/paths`) and a web port other than
  7517.
- Real Claude Code runs must not load the user's settings (their hooks
  would report to the real daemon). Follow `tests/e2e/claude/README.md`:
  isolated config/settings, `--setting-sources` or a temp
  `CLAUDE_CONFIG_DIR`, explicit `--settings` with your test hooks. Use the
  cheapest model (haiku) and as few runs as you need. Afterwards confirm,
  read-only, that `~/.pilothouse/logs/daemon.log` gained no sessions from
  your tests.
- Only stop processes you started, by PID. Never `pkill`/`killall` by
  name: that would hit the user's daemon or agents.
- Never `rm` with globs, and never `rm -rf` relative paths after `cd`.
  Make fresh dirs with `mktemp -d` under the scratchpad
  (`/private/tmp/claude-501/-Users-xasdta-Projects-ideas/3a83cd2e-5914-49d3-b149-9aa627f8cfb3/scratchpad`)
  and delete exactly those.
- No new paid services or accounts. No network calls to third parties
  except package downloads you need (`go get`, `npm i` in test folders).

**Engineering bar**
- Match the surrounding code: naming, comment density and voice, error
  messages a person can act on. Read neighbouring files before writing.
- Security first: everything a device does that changes what an agent may
  or will do is a signed action (`internal/web/signed.go`). Secrets never
  reach events, evidence, `explain`, logs or the API. Validate all input
  from devices, agents and files; bound every read and every output.
- Fail open where the product fails open (hooks, plugins), fail closed for
  security checks.
- Test as you code: unit tests for every behavior including the negative
  and security cases, integration tests through the daemon or the HTTP
  handlers, and e2e with real software where it exists (Claude Code,
  OpenCode, headless Chrome via `tests/e2e/web`'s puppeteer). Timing tests
  can flake when nine workstreams run `-race` at once: re-run before you
  conclude, and fix real flakes rather than loosening asserts blindly.
- Before you finish: `gofmt -l .` empty, `go vet ./...` clean,
  `go test -race ./...` green in your worktree.
- Keep the contract stable. If you must change a shared file
  (`internal/api/*`, `internal/core/*`, `internal/web/v02.go`,
  `internal/daemon/server_v02.go`, `internal/client/v02.go`,
  `internal/config/config.go`, `internal/adapters/adapter.go`), keep the
  change additive and minimal and list it in your report.
- Docs: update the user docs your feature touches (`README.md`,
  `docs/*.md`) in your branch.

**Hand-back report** (your final message): branch name and commits; what
you built; how you tested it (commands, e2e results); what you could not
test; every shared-file change; follow-ups. Be factual: say plainly what
failed or was skipped.

## Ownership

| WS | Owns (others don't edit) | May touch lightly |
|---|---|---|
| A | `adapters/claude`, `adapters/hookkit` (responses), question/plan routing in `daemon/hook.go`, answer validation in `daemon/core.go` `Decide`, `internal/tui`, `cli/answer.go`, `tests/e2e/claude` | notification wording for questions/plans |
| B | `internal/wrap`, `daemon/reply.go`, `daemon/terminal*.go`, `daemon/keepawake*.go`, `web/terminal.go`, `store/secrets.go`, messaging capture in `cli/hooks.go`, `cli/reply.go` | one call in `daemon/hook.go` to store the messaging endpoint |
| C | `internal/transcript`, `internal/insights`, `daemon/activity.go`, `daemon/usage*.go`, `store/usage.go`, `cli/usage.go` | one line in `daemon/loops.go` to start the usage loop |
| D | `internal/risk`, `internal/preview`, `daemon/enrich.go`, `daemon/rules.go`, `internal/policy` (managed file, presets), `cli/policy.go` | — |
| E | `adapters/opencode` (+ plugin JS), `daemon/plugin.go`, `adapters/all` registration, `tests/e2e/opencode` | `cli` setup lists adapters generically |
| F | `notify/telegram`, `daemon/telegram.go`, server side of push withdrawal in `web/push.go` and `notify/*`, setup flow in `cli/inbox.go`, `cli/demo.go`, `cli/notify*.go` | — |
| G | `internal/task` (task.yaml, start from daemon), `daemon/taskrun.go` | — |
| H | PWA shell and inbox: `static/index.html`, `css/app.css`, `js/{app,api,ui,dom,card,push,keystore}.js`, `sw.js`, `manifest`, `views/{inbox,request,rules,settings,pair,sessions,rows}.js`, new shared components (`js/diff.js`, `js/markdown.js`), `internal/demo`, `web/devserver`, `web/webtest` | — |
| I | PWA session/terminal/tasks/insights: `views/{session,terminal,tasks,insights}.js`, `css/views.css`, `static/vendor/` | imports H's shared components |

## Cross-workstream contracts

- **Questions/plans (A ↔ E, H, F).** A request with
  `checkpoint: "question"` carries `questions`; `"plan"` carries `plan`.
  An answer is `Decision{behavior: allow, answers: {Question.Key(): value}}`;
  multi-select values are labels joined by `", "`; free text is allowed
  when `allow_other`. A plan is approved with allow and revised with deny +
  `message`. `Decide` validates answers (A). Devices answer through
  `POST /api/requests/{id}/respond` (signed `decide`); Telegram (F) and
  the TUI/CLI (A) call `Decide` directly.
- **Replies (B ↔ E, A, F, H, I).** `core.Replier.Reply` dispatches:
  wrapped → PTY (B), observed Claude with a messaging endpoint → socket
  (B), plugin agents → `pluginReply` (E). Every reply emits
  `session.reply` with an `api.ReplyRecord`. `Session.Caps` says what is
  possible; UIs show the reply box only when `caps.reply`.
- **Caps.** `capsFor` (`daemon/caps.go`) calls `replyCaps` (B),
  `pluginCaps` (E) and `activityCaps` (C). Caps are stored with the
  session, so a change in what is possible must re-save the session
  (`touchSession`).
- **Enrichment (D).** `enrichRequest` fills `Risk`, `Preview` and
  `Context` for every request before it is stored. It must stay under a
  few milliseconds: bounded file reads, no network, no git subprocess on
  the hot path.
- **Activity (C ↔ H, I, A).** `Activity` returns at most `Limit` newest
  items; `Cursor` returns only newer items. The PWA polls it while a
  session view is open and re-fetches on `session.updated` for that
  session.
- **Terminal (B ↔ I).** `POST /api/sessions/{id}/terminal/token` (signed
  `terminal`, payload `{}`) → `{"token": "...", "expires_at": "..."}`.
  `GET /api/sessions/{id}/terminal?replay=1` is SSE: `event: data` with
  base64 output, `event: exit` with `{"code": n}`. `POST
  /api/sessions/{id}/terminal/input` with header `X-Terminal-Token` and
  body `{"data": "<base64>"}` (or `{"resize": {"cols": c, "rows": r}}`).
  B documents any deviation in `internal/web/terminal.go`.
- **Push withdrawal (F ↔ H).** When a request closes, the server sends a
  push with the same notification tag (`req-<id>`) and data
  `{"type": "resolved", "request_id", "status", "by"}`. The service worker
  (H) replaces the notification with a short "Answered ✓ …" one using the
  same tag and `renotify: false` — never a silent push (iOS revokes
  subscriptions that receive pushes without a notification).
- **Android/desktop notification actions (H).** Request pushes carry
  `data.request` (id, checkpoint, summary). The service worker offers
  Allow / Deny actions for `permission` requests only and signs with
  `PilothouseKeys.respond` (keystore.js is importable in the SW).
- **Demo (H ↔ F).** `internal/demo.New(...)` returns a core with every
  0.2 capability implemented on believable canned data; `Run(ctx)` keeps
  it alive. H implements it (moving the devserver's seed/demo loop into
  it); F builds `pilothouse demo` (sandboxed home, port, pairing QR,
  Ctrl-C cleanup) on top.
