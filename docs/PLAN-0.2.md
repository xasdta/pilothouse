# Pilothouse 0.2 — "Command Center"

Plan date: 2026-10-23. Inputs: two research passes run the same day — a
competitor/pain-point survey (18 products, GitHub issues, HN, Reddit) and an
agent-integration study with end-to-end tests against Claude Code 2.1.309
and OpenCode 1.18.25. Findings that this plan depends on are marked
**(e2e)** when they were run, **(src)** when read from source.

## 1. Where we stand

Phone approval alone stopped being a differentiator this autumn: Claude
Remote Control pushes "action required" and even skips the push while you
are at the terminal; Codex controls agents from the ChatGPT app; herdr has
~8 phone clients; Coucou (lock-screen approvals) reached 6.3k★ in a month.

What nobody else has: **rules before every tool call, across vendors, with
no account and no vendor relay.** Pilothouse 0.1 already does that. 0.2
keeps that core and closes every gap users list against us:

| Gap (0.1) | Every competitor has it | 0.2 |
|---|---|---|
| Can't answer an agent's question from the phone | Remote Control, Happy, Kelpie | Questions + plan approval, held while you're away |
| Can't reply / redirect | All of them | Reply to Claude (observed or wrapped), OpenCode, any wrapped agent |
| Session page is empty | All of them | Live activity feed, files changed, "catch me up" |
| Edit approvals show only file names | Remote Control, Superset | Diff preview, risk labels, the rule that fired |
| Allow-for-session is the only "remember" | — (nobody turns approvals into rules) | One-tap rules, suggested rules, guarded autopilot |
| No cost view | Agent Deck, Superset | Usage & cost across Claude and Codex, budgets |
| Can't start work from the phone | Codex, Claude, Superset | New task from phone → worktree → diff → PR |
| Two agents | herdr 22, Happy 4 | + OpenCode (most-requested next agent) |
| iPhone pushes can't carry buttons | — | Telegram two-way, ntfy first-class, Android action buttons |

## 2. Pain points this release answers (evidence in research notes)

1. YOLO-mode incidents and prompt fatigue (Anthropic: 93 % of prompts are
   accepted; claude-code#28240 209 👍) → **one-tap rules, suggested rules,
   guarded autopilot preset, weekly "what the rules caught".**
2. Blocked prompts and questions nobody sees; vendor questions time out
   after 60 s (codex#28969 221 👍) → **questions held while away, answered
   from phone/Telegram/TUI.**
3. Losing track of sessions ("can no longer tell which session is waiting")
   → **activity feed, catch-me-up, desktop dashboard.**
4. Notifications without context, never withdrawn (claude-code#92528) →
   **context in every notification; resolved notifications replaced.**
5. Usage limits and no cost visibility (claude-code#16157 726 👍) →
   **usage meter, burn rate, budget alerts.**
6. Diff review overload (codex#2998 240 👍) → **diffs on cards and per task.**
7. Worktree chores (copy .env, install deps) → **`.pilothouse/task.yaml`.**
8. Laptop sleep kills runs → **keep-awake while agents work.**

## 3. Features

### F1 Questions and plans from anywhere (Claude, OpenCode)
- Claude's `AskUserQuestion` and `ExitPlanMode` reach the PermissionRequest
  hook we already install (600 s timeout). Answer with
  `decision{behavior:"allow", updatedInput:{questions, answers:{<question>:<label>}}}`;
  multi-select = comma-joined labels; free text accepted **(e2e)**.
  Plans: approve = allow + echo input; request changes = deny + message
  (Claude revises the plan) **(e2e)**.
- Same presence routing as permissions: at the desk Pilothouse steps aside;
  away, the question is held and pushed. First answer wins.
- Cards: option buttons/chips (multi-select), "Other…" free text, rendered
  plan markdown with *Approve* / *Request changes*.
- TUI and CLI answer too (`pilothouse answer <req> --option …`).
- Codex `request_user_input` is not answerable through hooks (src) — shown
  read-only with "answer in the terminal".

### F2 Reply, redirect, interrupt
- **Wrapped sessions (any agent):** type into the PTY through the holder.
- **Observed Claude ≥ 2.1.224:** the session's messaging socket
  (`CLAUDE_CODE_MESSAGING_SOCKET` + token, captured by the hook shim, kept
  out of every log) **(e2e)**. Claude frames it as a message "not typed by
  your user", so the UI labels it "sent from Pilothouse".
- **OpenCode:** `session.promptAsync` from the plugin **(e2e)**.
- Interrupt (Esc) and Ctrl-C for wrapped sessions.
- Every reply is signed by the paired device key (§5).

### F3 See what agents are doing
- `internal/transcript`: incremental, size-capped readers for Claude
  transcripts (dedupe usage by `message.id`, subagents as sidechains) and
  Codex rollouts (`response_item`, `token_count`) **(src/local)**.
- Session page: activity feed (prompts, replies, tool calls with results,
  errors, subagents) collapsed per turn; files changed (+N/−M); token use;
  **Catch me up** — a deterministic summary since you last looked (tool
  calls, files, tests run and last result, questions asked).
- Live terminal mirror for wrapped sessions (xterm.js), with a quick-keys
  bar; a read-only *watch* mode never steals the attached terminal.

### F4 Approval cards people can trust at a glance
- Diff preview for Edit / MultiEdit / Write / apply_patch with +N/−M.
- Risk labels from `internal/risk`: destructive, force-push, deploy,
  secrets, network, install, sudo, outside-repo.
- The rule that fired, the agent's last sentence ("why"), and stale-request
  protection (cards disable the moment a request closes anywhere).

### F5 Rules without YAML
- Every card offers suggested rules: *Always allow `npm test` in api-server*,
  *Always ask before `git push`*, *Never allow `rm -rf`*, *Allow edits under
  `src/**` in this repo*.
- Repeated approvals become suggestions ("allowed 5× this week").
- Rules page: every rule with its source, managed rules can be disabled or
  removed, presets toggled, and a *Would this be allowed?* tester.
- Presets: **guarded-autopilot** (a safe alternative to
  `--dangerously-skip-permissions`: allow routine work, deny destructive,
  ask on push/deploy/secrets), protect-main, no-secrets, deploys-need-approval.
- Managed rules live in `~/.pilothouse/rules.yaml` (machine-written, kept
  apart from the hand-written `policy.yaml`). Creating a rule from a device
  is signed.

### F6 Usage and insights
- Tokens and estimated cost per session / day / agent / model / repo, burn
  rate, optional daily budget with 80 % / 100 % alerts.
- What the rules did: auto-allowed, asked, blocked, grants, median time to
  answer, time agents spent blocked.
- Insights page with charts; `pilothouse usage`.

### F7 Start work from the phone
- New task: repo (from repos Pilothouse has seen), agent, prompt, base
  branch → worktree + branch, wrapped agent. Then: activity, diff per file,
  **Ship** (push + PR via `gh`) or **Discard**. All signed.
- `.pilothouse/task.yaml`: files to copy into the worktree (`.env*`), setup
  commands (`npm ci`), cleanup.

### F8 OpenCode
- A small plugin (`~/.config/opencode/plugins/pilothouse.js`) forwards
  events and runs replies through OpenCode's in-process client **(e2e)**.
- Policy in `tool.execute.before` (throw = deny) **(e2e)**; permissions via
  `permission.asked` → `/permission/{id}/reply`; questions via
  `question.asked`; replies via `promptAsync`.
- OpenCode allows almost everything by default; setup offers
  `permission: {bash: ask, edit: ask}`.

### F9 Reach you anywhere
- **Telegram** two-way: buttons for allow / deny / session / question
  options, reply to a message to prompt the agent, messages edited when a
  request resolves elsewhere. Works without Tailscale.
- **ntfy** first-class in setup (its iPhone app shows action buttons).
- **Web Push**: action buttons on Android/desktop (signed in the service
  worker), app badge, context in the body, resolved notifications replaced
  (never silently dropped — iOS revokes subscriptions that do that).

### F10 Effortless start
- `pilothouse setup` shows the settings diff, detects Tailscale and offers
  remote access + the pairing QR inline, sends a test notification.
- `pilothouse demo`: a sandboxed daemon with live fake agents and a QR code
  — try the phone flow in 30 seconds without touching your agents.
- Keep-awake while agents work (`caffeinate` / `systemd-inhibit`).

### F11 Visual overhaul
- Dark mode, desktop three-pane dashboard (sessions · session · needs-you),
  keyboard shortcuts (j/k, a/d, r reply, n new task, ⌘K palette), motion
  that respects reduced-motion, agent brand colours, skeleton loading,
  empty states that teach.

### Deliberately not in 0.2
- Tailscale identity auto-pairing: `Tailscale-User-Login` can be forged by
  any local process (an agent), which would let it pair itself. QR pairing
  stays.
- Multi-machine hub, native iOS app / Live Activities, Slack/Discord bots,
  Gemini / Copilot / Droid adapters (cheap next steps; see research).

## 4. Architecture

Contract first: wire types, core interfaces, routes and stubs land on
`main` before any feature work, so workstreams fill in files they own and
merge without fighting over shared ones.

- **api**: Request gains `Questions`, `Plan`, `Risk`, `Preview`,
  `Context`; Decision gains `Answers`; Session gains `Caps` (reply,
  interrupt, watch) and `Usage`; new `Activity`, `Insights`, `RuleInfo`,
  `RuleSuggestion`, `TaskStart`.
- **core**: optional interfaces (`Replier`, `ActivityReader`, `RuleEditor`,
  `InsightsReader`, `TaskStarter`, `TerminalProxy`) so fakes stay small.
- **Signed actions v2** (§5) shared by decisions-with-answers, replies,
  terminal input, rule edits and tasks.
- **Holder protocol**: `ServerHello.Caps`; new hello modes `watch` and
  `input`. Old holders (no caps) never see the new modes — an unknown mode
  would otherwise be treated as attach and steal the terminal.

## 5. Security model additions

- Device-signed actions: `pilothouse-action:v2\n{action}\n{target}\n{sha256(payload)}\n{ts}\n{nonce}`,
  ECDSA P-256 with the non-extractable WebCrypto key, ±120 s, single-use
  nonce. Actions: `decide`, `reply`, `terminal`, `rule`, `task`.
- Terminal input uses one signed `terminal` action to mint a 10-minute,
  session-bound token instead of signing keystrokes.
- Messaging-socket tokens are stored in a private table, never in events,
  `explain`, logs or the API.
- Transcript reads are confined to `~/.claude/projects` and
  `$CODEX_HOME/sessions`, regular `.jsonl` files, bounded reads.
- Rule edits are restricted to the managed file and validated before the
  engine reloads; a rule from a device can never touch built-in rules.
- Telegram decisions are bound to the one chat that completed the pairing
  code; callback data are single-use tokens.
- The agent-caller guard keeps covering every new local socket route.

## 6. Workstreams

| WS | Scope | Owns |
|---|---|---|
| A | F1 questions/plans, TUI answers + reply box, CLI `answer` | adapters/claude, daemon/hook question path, tui |
| B | F2 replies, F3 terminal mirror, keep-awake | wrap (caps/watch/input), daemon/reply, messaging socket |
| C | F3 activity, F6 usage/insights | internal/transcript, internal/insights, daemon/activity |
| D | F4 risk + previews, F5 rules | internal/risk, internal/preview, policy managed file, presets |
| E | F8 OpenCode | adapters/opencode + plugin |
| F | F9 Telegram, ntfy/Web Push withdrawal, F10 setup + demo | notify/telegram, cli/setup, cli/demo |
| G | F7 tasks from phone, task.yaml | task, daemon/tasks |
| H | F11 PWA: shell, inbox cards, rules, settings, SW | web/static (core files) |
| I | F11 PWA: session page, terminal, tasks, insights | web/static (new views) |

Each workstream works in its own git worktree, writes tests as it codes
(unit + integration + e2e where a real agent exists), and runs
`go test -race ./...` and `go vet ./...` before handing back.

## 7. Validation

1. Per workstream: unit/integration tests, race detector, vet.
2. Real-software QA after merge: Claude Code (questions, plans, replies,
   activity) in an isolated home; OpenCode with a mock model; the PWA in
   real Chrome at phone and desktop sizes; Telegram against a mock Bot API;
   Linux CI e2e.
3. Fix, re-run, release 0.2.0 (GoReleaser + Homebrew), update the site.

## 8. Marketing site

New sections: "Answer, reply and steer from anywhere", "Rules without
YAML" (with the guarded-autopilot story), "See everything" (activity,
usage), OpenCode support, Telegram, and an honest comparison table against
Claude Remote Control, Happy, herdr and Conductor, plus a `pilothouse demo`
call to action.
