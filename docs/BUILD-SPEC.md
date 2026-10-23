# Pilothouse v0.1 — binding build spec

Pilothouse is one inbox for every coding agent. A single Go binary (`pilothouse`)
runs a per-user daemon that sees every Claude Code and Codex session on the
machine, applies the user's rules **before every tool call**, and sends what
needs a person to wherever they are: the terminal at the desk, or a paired
phone/browser when they're away. Apache-2.0, no account, no cloud.

The product plan is in `../docs/` (00-overview … 05-build-plan). This spec is
the v0.1 cut and overrides the plan where they differ.

## 1. v0.1 scope

In:
- Daemon `pilothouse daemon` (Unix socket API + loopback web listener), SQLite event log.
- Observe mode: hooks for **Claude Code** (full: policy + remote approvals) and **Codex** (whatever its hooks support — see §6.3).
- Wrap mode: `pilothouse run <agent>` PTY holder with detach/attach, OSC 7501 (L1), survives daemon restarts and terminal close.
- Policy engine: YAML rules with glob `match:` and/or CEL `when:`; user + repo (trust-gated) + built-in rules; `pilothouse policy test|replay`.
- Presence-aware routing (OS idle on macOS/Linux, manual override).
- Approvals: first answer wins; allow once / allow for session / deny with message.
- Web app (PWA) served by the daemon; pairing by QR/one-time code; per-device P-256 keys sign every remote decision; Web Push (VAPID); works over Tailscale Serve HTTPS (`pilothouse remote tailscale`).
- Notifications: desktop, Web Push, ntfy (with signed Allow/Deny action buttons), generic webhooks.
- Inbox TUI (Bubble Tea), CLI (`ls`, `approve`, `deny`, `log`, `explain`, `here/away/auto`, `setup`, `doctor`, `service`).
- Tasks: `pilothouse task new "prompt"` → git worktree + branch + wrapped agent; `task ls|diff|done --pr`.
- launchd/systemd user service install.
- macOS + Linux, amd64 + arm64. No Windows in v0.1.

Out (roadmap, say so honestly in docs): E2E relay, hosted Cloud, team policies/SSO, OpenCode/Copilot/Gemini adapters, headless protocol mode (Codex app-server/ACP), screen-rule detection (L4), Windows.

## 2. Invariants (every workstream)

1. **Tests never touch the real home.** Set `PILOTHOUSE_HOME`, `PILOTHOUSE_USER_HOME` and `PILOTHOUSE_RUN_DIR` to temp dirs in every test (`t.Setenv`). `PILOTHOUSE_RUN_DIR` must be short (`os.MkdirTemp("/tmp", "ag")`) — macOS socket paths max ~104 bytes. **Never** write to `~/.claude`, `~/.codex`, `~/.pilothouse`, `~/Library/LaunchAgents` or systemd dirs of the real user, and never install hooks for real — the developer machine is running Claude Code right now and its hooks would break it.
2. **Fail open by default.** If the daemon is down or slow, the hook shim prints the adapter's no-opinion response and exits 0 within 2 s (except blocking permission hooks, which have the daemon's own hold limit). `approvals.fail_closed: true` flips pre_tool to deny.
3. **Pre_tool is fast.** Policy evaluation p99 < 5 ms; shim round trip p99 < 25 ms. Never block at pre_tool waiting for a person.
4. **First answer wins.** Closing a request is a compare-and-set (`UPDATE … WHERE status IN ('open','stepped_aside')`); losers get `already_decided` (HTTP 409).
5. **Agents cannot approve themselves.**
   - Decisions are accepted only on the Unix socket (local) or the web API with a valid device signature. The TCP listener never accepts unsigned decisions.
   - On the Unix socket, `decide`, `pair`, `login-link`, device revoke, presence change and policy reload are rejected with 403 `agent_caller` if any ancestor of the peer process is an agent process (any adapter's `ProcessNames()`).
   - Built-in policy rule `pilothouse-self-protection` forces `ask` for shell commands that run pilothouse admin subcommands and for edits to protected paths (Pilothouse home, agent settings files).
6. **Event log is the source of truth for clients.** Every state change is written with its event in one transaction (outbox). Seq is strictly increasing. Clients load `/v1/snapshot`, then stream `/v1/events?follow=1&after=<seq>`; if `after` is too old or the client is slow, the server sends `event: control` `{"resync_required":true}` and closes.
7. **No telemetry.** Only the anonymous update check (`version, os, arch, monthly-rotating random id`) — off with `update_check: false` or `PILOTHOUSE_NO_UPDATE_CHECK=1`. Nothing about sessions, prompts, commands or paths ever leaves the machine except notification channels the user configured.
8. **Secrets.** Data dir 0700, DB/keys/config 0600. Device tokens and action tokens stored hashed (SHA-256). Constant-time compares.
9. **Logging:** `log/slog`, text to stderr in foreground, file `logs/daemon.log` (rotate at 10 MB, keep 3) when run as a service. Never log tool inputs at info level beyond the one-line summary.
10. **Errors are actionable.** CLI errors say what to do next ("daemon not running — run `pilothouse start`").

## 3. Layout and ownership

See `docs/WORKSTREAMS.md` for who owns what. Shared files (`internal/api`, `internal/core`, `internal/client`, `internal/paths`, `internal/config`, `internal/adapters/adapter.go`, `internal/cli/root.go`) accept **additive** changes only (new types/fields/methods/routes), and every such change is logged in `docs/SHARED-CHANGES.md` with the reason.

```
cmd/pilothouse/            main → cli.Main
internal/api            wire types (JSON)
internal/core           Core / Notifier interfaces (daemon boundary)
internal/client         Unix-socket client (CLI, TUI, shim, holders)
internal/paths, config  locations, config.yaml
internal/adapters/...   adapter interface, claude, codex, all
internal/proc           ancestry, liveness, peer PID
internal/store          SQLite: sessions, requests, events, grants, tasks
internal/state          resolver
internal/daemon         server, hook routing, broker, presence loop
internal/presence       OS idle detection
internal/policy         rules engine
internal/wrap           PTY holder + attach client + OSC parser
internal/task           worktree tasks
internal/web            PWA + device API + pairing + push
internal/notify         desktop, ntfy, webhooks, fan-out
internal/tui            inbox
internal/service        launchd / systemd units
internal/cli            commands
```

## 4. Daemon local API (Unix socket `paths.Socket()`, mode 0600)

HTTP/1.1 over the socket. JSON bodies. Errors: `api.Error{error, code}` with codes `bad_request` 400, `agent_caller` 403, `not_found` 404, `already_decided` 409, `internal` 500.

| Method & path | Body → response | Notes |
|---|---|---|
| GET /v1/health | → `api.Health` | |
| POST /v1/shutdown | → 204 | graceful stop; guarded (agent_caller) |
| GET /v1/snapshot | → `api.Snapshot` | sessions not exited + exited in last 24 h; open/stepped-aside requests + last 50 closed |
| GET /v1/events?after=N | → `[]api.Event` (≤1000) | |
| GET /v1/events?follow=1&after=N | SSE `event: event` / `event: control`, `: ping` every 15 s | |
| POST /v1/hook | `adapters.HookEnvelope` → `adapters.HookResponse` | may block (permission) |
| GET /v1/sessions | → `[]api.Session` | |
| GET /v1/sessions/{id} | → `api.Session` | id or unique prefix |
| GET /v1/sessions/{id}/events?limit= | → `[]api.Event` newest first | |
| POST /v1/sessions/{id}/seen | → 204 | clears Unseen |
| GET /v1/sessions/{id}/explain | → `api.Explain` | |
| POST /v1/sessions/wrapped | `api.WrappedSessionBody` → `api.Session` | idempotent on session_id |
| POST /v1/sessions/{id}/osc | `api.OSCBody` → 204 | L1 evidence |
| POST /v1/sessions/{id}/exit | `api.ExitBody` → 204 | |
| GET /v1/requests?status= | → `[]api.Request` | |
| GET /v1/requests/{id} | → `api.Request` | id or unique prefix |
| POST /v1/requests/{id}/decide | `api.DecideBody` → `api.Request` | guarded; By = "cli" or "tui" (header `X-Pilothouse-Client`) |
| GET/PUT /v1/presence | `api.PresenceBody` → `api.Presence` | PUT guarded |
| GET /v1/tasks, GET /v1/tasks/{id}, POST /v1/tasks | `api.Task` | POST upserts |
| GET /v1/policy/status, POST /v1/policy/reload | → `api.PolicyStatus` | reload guarded |
| POST /v1/notify/test | → 204 | |
| POST /v1/pair, POST /v1/login-link, GET /v1/devices, DELETE /v1/devices/{id} | web package (`RegisterLocal`) | guarded |

IDs: sessions `s_` + 10 lowercase base32 chars; requests `r_…`; tasks `t_…`; devices `d_…`; grants `g_…`. Crypto-random.

## 5. Hook routing (daemon)

```
POST /v1/hook(env):
  ad := all.Get(env.Agent); ev := ad.ParseHook(env)            // parse error → no-opinion response
  sess := find by env.PilothouseSessionID (wrapped) or (agent, ev.AgentSessionID); else create observed session
          PID = nearest ancestor whose Comm ∈ ad.ProcessNames(); repo root + branch via git (cache 30 s per cwd)
  append EventHook (normalized ev, no raw payload) + state evidence → resolver → session.updated if changed
  switch ev.Kind:
  pre_tool:   self-protection/policy/grants → outcome (deny > ask > grant-allow > policy-allow > none)
              emit policy.decision when a rule or grant matched
  permission: req := open Request{checkpoint: permission}
              holdMax := min(config hold_limit, ad.MaxHold(permission) - 15s)
              route := devices if always_hold or presence == away, else terminal
              terminal: status=stepped_aside; return no-opinion (native dialog shows);
                        if still blocked after presence.notify_after → desktop notification (Remote=false)
              devices:  notify Remote=true; wait for: decision | holdMax | presence→present | shim gone (ctx)
                        decision → encode allow/deny (+ Grant if scope=session)
                        presence→present → status=stepped_aside, no-opinion (person is back: use the terminal)
                        timeout/ctx → status=expired, no-opinion, notify "answer in the terminal"
  post_tool:  stepped_aside request with same tool_use_id (else same tool+summary, newest) → answered_locally
  prompt_submit / stop / session_end: remaining stepped_aside requests of the session → answered_locally
  notification: evidence only (permission_prompt → blocked/permission, idle_prompt → waiting, elicitation → blocked/question, auth → blocked/auth)
  return ad.EncodeResponse(ev, outcome)
```

Grants (`scope: session`): shell tools store `Prefix` = the program plus its subcommand, skipping global flags and their values (`git -c k=v commit` → `git commit`); if the subcommand can't be determined safely, the grant covers only the exact command. Chained commands (`;`, `&&`, `|`, `$(…)`) are never covered. Other tools store the tool name only. Grants also answer a matching permission request directly (needed for Codex, whose pre-tool hook can't allow). Applied at pre_tool **after** deny/ask rules (deny always wins).

## 6. Adapters

### 6.1 Shim
`pilothouse hook <agent>` reads stdin (≤ 4 MB), builds `HookEnvelope` (ancestors via `proc.Ancestors(os.Getppid() chain)`, env allow-list: `TERM_PROGRAM, TMUX, PILOTHOUSE_SESSION_ID, PILOTHOUSE_TASK_ID, SSH_CONNECTION`), POSTs to the daemon with timeout = `ad.MaxHold(kind)` for permission, 2 s otherwise; prints `Stdout` to stdout, `Stderr` to stderr, exits `ExitCode`. Daemon unreachable → `ad.FailResponse(kind, cfg.fail_closed)`.

### 6.2 Claude Code
Verify against https://code.claude.com/docs/en/hooks (fetch it). Settings file `~/.claude/settings.json` (via `paths.UserHome()`); merge — never clobber the user's hooks; back up to `settings.json.pilothouse-bak-<ts>`; identify our entries by the command containing `pilothouse hook claude`. Wire: SessionStart, SessionEnd, UserPromptSubmit, PreToolUse (timeout 30), PermissionRequest (timeout 600), PostToolUse, Notification, Stop, SubagentStop. `MaxHold(permission)` = 600 s; others 0.

### 6.3 Codex
Research the current Codex CLI (github.com/openai/codex) for: hooks support and its config format/events/response schema, the `notify` program setting, OSC 7501, session files. Implement the strongest supported integration; document precisely what works (policy? remote approvals? state only?) in `docs/AGENTS.md`. Don't invent APIs — cite source files.

## 7. State resolver (`internal/state`)

Evidence: `{at, layer, state, kind, detail}`. Rules, in order (the first that applies wins; Explain reports the rule):
1. `exited`: holder reported exit, SessionEnd hook, or L0 says PID gone (checked every 2 s).
2. `blocked/permission`: an open or stepped_aside permission request exists.
3. Latest fresh hook evidence (L2) — hooks are authoritative.
4. Latest OSC evidence (L1) if no hook evidence newer than it, or the session has no hook adapter; if L1 contradicts an L2 event < 2 s older, keep L2 and record the disagreement.
5. `unknown` if the last evidence is `working` and older than 20 min while the process is alive.
6. `starting` otherwise.
Mapping: PreToolUse/PostToolUse/UserPromptSubmit → working; Stop → waiting; OSC `idle|done` → waiting, `working` → working, `blocked` → blocked(kind), `error` → errored.
Unseen = set when entering blocked/waiting/errored/exited; cleared by `seen`.

## 8. Policy

```yaml
# ~/.pilothouse/policy.yaml  (also <repo>/.pilothouse/policy.yaml)
version: 1
rules:
  - name: no-force-push
    description: Never force-push
    match: { kind: shell, command: "git push*" }   # globs; all fields AND-ed
    when: 'command.matches("(--force|-f)\\b")'      # optional CEL, AND-ed with match
    action: deny          # allow | ask | deny
    message: Force-push is blocked by policy. Open a PR instead.
    agents: [claude, codex]  # optional
```
- `match` fields: `agent, tool, kind, command, path, cwd, repo, branch` — glob syntax (`*`, `**`, `?`, `{a,b}`); `path` matches if any path matches.
- CEL vars: `agent, tool, kind, command, paths (list), path (first or ""), input (map), cwd, repo, branch, mode, hour, weekday`; functions `glob(s, pattern)`, `has_word(s, word)` plus CEL standard + strings ext.
- Precedence: any `deny` wins, then `ask`, then `allow`. Built-in rules can't be disabled.
- Repo files: `<repo>/.pilothouse/policy.yaml`. Untrusted repos (not in `trusted-repos.yaml`) may only contribute `deny`/`ask` — their `allow` rules are reported as ignored.
- Invalid file → keep last good rules for that file + report error in status; never crash.
- CLI: `policy init` (starter file with examples), `policy check [file]`, `policy test --tool --command --path --agent --cwd`, `policy replay [--since 7d]` (re-evaluate logged pre_tool hook events, show what changes), `policy trust|untrust [repo]`, `policy status`.

## 9. Wrap mode (`pilothouse run`)

- `pilothouse run [-n name] [-d] [--task id] [--] <cmd> [args…]` picks the agent kind from `basename(cmd)` (`claude`, `codex`, else `generic`).
- Spawns a holder: re-exec `pilothouse __holder --id <sid> -- <cmd…>` with `Setsid`, stdio → /dev/null, log `logs/holder-<sid>.log`. The holder owns the PTY (creack/pty), exports `PILOTHOUSE_SESSION_ID=<sid>` and `PILOTHOUSE=1` to the child, listens on `paths.HolderSocket(sid)` (0600), registers with the daemon (retry every 2 s; works with no daemon), parses OSC 7501 from output and reports it, reports exit and exits 5 s after the child.
- Attach client: raw mode, forwards stdin/resize, prints output; **detach key `ctrl+\`** (0x1c) — press twice to send a literal ctrl+\ to the agent. On attach, nudge a redraw (resize rows-1 then rows). One active client; a new attach detaches the old one with a message.
- Frame protocol on the holder socket: `[type u8][len u32 BE][payload]`; types: hello, data, resize(cols u16, rows u16), detach, exit(code i32), error(text).
- `pilothouse attach <id-prefix>`; `pilothouse ls` marks attachable sessions.
- Holders are not children of the daemon or the service manager: restarting the daemon never kills agents.

## 10. Tasks

`pilothouse task new [--agent claude] [--base <ref>] [--name slug] [-a|--attach] "<prompt>"`: requires a git repo; creates branch `pilothouse/<slug>` and worktree `$PILOTHOUSE_WORKTREES/<repo>/<slug>` (default `~/.pilothouse-worktrees`, outside the self-protected Pilothouse home) from base (default: current branch); registers `api.Task`; starts `pilothouse run -d --task <id>` with the agent and the prompt as its first message (`claude "<prompt>"`, `codex "<prompt>"`). `task ls` (status derived from its session), `task diff <id>` (`git diff base...branch`), `task done <id> [--pr] [--keep]` (`--pr` pushes and runs `gh pr create --fill`), `task abandon <id>`.

## 11. Web app and device API (TCP `127.0.0.1:7517`)

- Host allow-list (DNS-rebinding defense): `127.0.0.1:<port>`, `localhost:<port>`, and the host of `web.public_url`.
- Pairing: `pilothouse pair` → one-time code (10 min) → URL `<base>/pair#<code>` + terminal QR. The page generates a non-extractable ECDSA P-256 key in IndexedDB, POSTs `/api/pair {code, name, public_key}` → device id + HttpOnly cookie `ph_dev` (SameSite=Strict, Secure on https). `pilothouse open` does the same for this machine's browser via a login link.
- Auth: cookie on every `/api/*`; state-changing requests also need header `X-Pilothouse: 1` and a same-origin `Origin`.
- Decisions: `POST /api/requests/{id}/decide {behavior, scope, message, ts, nonce, sig}`; `sig` = WebCrypto ECDSA-P256-SHA256 (P1363) over `pilothouse-decide:v1\n{id}\n{behavior}\n{scope}\n{ts}\n{nonce}`; |now−ts| ≤ 120 s; nonce single-use. By = `device:<id>`.
- `GET /api/snapshot`, `GET /api/events?after=` (SSE), session detail/events/seen, presence, push subscribe/unsubscribe, `GET /api/vapid`, device rename/sign-out.
- ntfy actions: `POST /api/actions/{token}` where token = random 32 bytes stored hashed with (request, behavior, expiry = request expiry); single use.
- Security headers: strict CSP (`default-src 'self'`), `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`, `Permissions-Policy`, HSTS on https.
- PWA: mobile-first, installable (manifest + service worker), dark and light, no build step (vanilla JS modules embedded with `go:embed`). Screens: Pair, Inbox (Needs you → big Allow / Allow for session / Deny), Sessions, Session detail (timeline), Settings (notifications, presence, device name, sign out). Service worker shows pushes and, where the platform supports notification actions, signs and posts Allow/Deny directly.
- `pilothouse remote tailscale`: verify `tailscale` exists and HTTPS is enabled for the tailnet; run `tailscale serve --bg` to proxy HTTPS 443 → 127.0.0.1:7517; save `web.public_url`; `remote off|status`.

## 12. Storage (SQLite via modernc.org/sqlite)

WAL, `busy_timeout=5000`, `foreign_keys=on`, one writer connection + reader pool. Migrations are numbered and idempotent. Tables (store): `sessions`, `requests`, `events`, `grants`, `tasks`, `evidence`, `meta`. Web owns `web_devices`, `web_push_subs`, `web_pair_codes`, `web_nonces`, `web_action_tokens` (migrated by `web.New`). Retention job hourly: closed requests, hook events and evidence older than `retention` are pruned; sessions/tasks kept.

## 13. Quality bar

- `go vet`, `gofmt`, `staticcheck` clean (install with `go install honnef.co/go/tools/cmd/staticcheck@latest` into GOBIN if missing). `go test -race` for your packages passes.
- Every package has table-driven unit tests; daemon has integration tests over a real socket in a temp dir; wrap has PTY tests with a fake agent (a small Go test binary or `/bin/sh -c` script that prints OSC 7501 and reads input).
- Only test **your own packages** while others are in flight (`go test -race ./internal/<pkg>/...`); `go build ./...` must stay green — if someone else's package breaks the build, don't edit it: report it.
- Comments: package doc + exported identifiers; explain *why*, not *what*. Match the existing style.
- No new dependencies without logging them in `docs/SHARED-CHANGES.md` (prefer stdlib).
