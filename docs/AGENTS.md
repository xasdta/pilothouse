# Agents: what Pilothouse can and cannot do with each one

Pilothouse talks to coding agents through their **hooks**: the agent runs
`pilothouse hook <agent>` at fixed points, pipes a JSON payload to it, and reads
the answer from its stdout (or, for Codex denies, its exit code). The shim forwards the payload to the daemon and
prints the daemon's answer; if the daemon is down or slow it prints nothing and
exits 0, so the agent behaves as if Pilothouse were not installed (fail open).

This page records what each agent really supports, with sources, as of
2026-10-27. It is the reference for `internal/adapters/claude`,
`internal/adapters/codex`, `internal/adapters/opencode` and
`internal/adapters/grok`. OpenCode has no command hooks; Pilothouse reaches
it through a plugin instead (see OpenCode).

## At a glance

| | Claude Code | Codex CLI | OpenCode (1.x) | Grok Build |
|---|---|---|---|---|
| Hook config Pilothouse edits | `~/.claude/settings.json` | `~/.codex/hooks.json` + trust entries in `~/.codex/config.toml` | Installs a plugin: `~/.config/opencode/plugins/pilothouse.js` | `~/.grok/hooks/pilothouse.json` (personal; folder trust gate) |
| State detection | Full: session start/end, prompt, every tool call, permission prompts, notifications (idle, permission waiting), stop | Full except notifications: session start/end, prompt, tool calls, permission prompts, stop, **interrupt** | Session start/end, prompt, tool calls, permission prompts, questions, idle (with the last message), compaction | Session start/end, prompt, every tool call, permission denied, notifications, stop/failure/cancel, subagents, compaction (**no permission prompt**) |
| Policy `deny` (before every tool call) | Yes (`PreToolUse` → `permissionDecision: "deny"`) | Yes (`PreToolUse` → exit code 2, reason on stderr) | Yes (`tool.execute.before` throws; the model reads the reason) | Yes (`PreToolUse` → `decision: "deny"`; the model reads the reason) |
| Policy `ask` (force the agent's prompt) | Yes (`permissionDecision: "ask"`) | **No prompt** — Codex has no `ask` at `PreToolUse`, so Pilothouse **denies** the call and tells the agent a person must do it (see Responses) | **No prompt** a plugin can raise, so the call is **held for a person in Pilothouse, at the desk too** (inbox, CLI, phone); unanswered means denied | **At the desk only**: `decision: "ask"` forces Grok's own permission prompt. No remote answer (no `PermissionRequest` hook), so it waits at the terminal |
| Policy / grant `allow` (skip the prompt) | Yes (`permissionDecision: "allow"`; the agent's own deny/ask rules still win) | **No** at `PreToolUse` (Codex rejects `allow` without `updatedInput`); see Caveats | Policy allow: no (OpenCode's own permission settings decide). Grants: yes, at OpenCode's permission prompt | **No**: `allow` only means "not blocked"; Grok's rules, mode and saved grants still decide |
| Remote approvals (phone answers the prompt) | Yes (`PermissionRequest` → `decision.behavior` allow/deny), held up to 600 s | Yes (`PermissionRequest` → `decision.behavior` allow/deny), held up to 600 s | Yes (`permission.asked` → `/permission/{id}/reply`), held up to `approvals.hold_limit`; OpenCode asks only for what its `permission` settings say (setup offers bash/edit = ask) | **No** — Grok has no permission-request hook |
| Questions and plans from anywhere (0.2) | Yes: `AskUserQuestion` answers and `ExitPlanMode` approve / request changes through `PermissionRequest` (see Questions and plans) | Read-only: `request_user_input` shows as "blocked (question)"; answer it in Codex (no hook can) | Questions: yes (`question.asked` → `/question/{id}/reply`, multi-select and free text). OpenCode has no plan approval | **No**: a question or plan-approval tool can only be answered in Grok |
| Resume a session | `claude --resume <session_id>` | `codex resume <session_id>` | `opencode --session <session_id>` | `grok --resume <session_id>` |
| Replies (follow-up prompt from the phone/CLI) | Wrapped: typed into the terminal (interrupt with Esc). Started on its own: Claude's session messaging socket (2.1.224+; no interrupt) | Wrapped only (`pilothouse run codex`): hooks can't inject a prompt | Through its plugin (`session.promptAsync`; interrupt aborts the turn) | Wrapped only (`pilothouse run grok`): hooks can't inject a prompt |
| Activity feed and usage (read from the transcript, see [USAGE.md](USAGE.md)) | `~/.claude/projects/…/<session>.jsonl` + subagent files; usage per response | `~/.codex/sessions/…/rollout-*.jsonl`, found by thread id until hooks name it; usage from `token_count` | Activity from OpenCode's `opencode.db` (messages and parts); session usage from its session row, not in the global usage page yet | Activity from `$GROK_HOME/sessions/<encoded-cwd>/<session>/updates.jsonl`; usage not parsed yet (`usage.json`) |
| OSC 7501 status (wrap mode, L1) | Emits it (v2.1.295+) when it detects terminal support | Emits it whenever stdout is a terminal | No | Not yet verified |

"Remote approvals" means: when the person is away, the hook holds the agent's
permission request while Pilothouse pushes it to a paired device, and the answer
goes back through the hook. When the person is at the desk the hook returns
no opinion immediately and the agent's own dialog appears (Pilothouse notifies
if it waits longer than `presence.notify_after`).

## Claude Code

Source: <https://code.claude.com/docs/en/hooks> (hooks reference) and
<https://code.claude.com/docs/en/hooks-guide>. Payloads in
`internal/adapters/claude/testdata/hooks/` were captured from Claude Code
**2.1.309** headless runs (files named `doc_*` are copied from the reference
for events a headless run cannot trigger). An end-to-end run of 2.1.309
through the real shim confirmed that a `PreToolUse` deny blocks the call with
Pilothouse's reason and that a `PermissionRequest` allow lets a `Write` through
that `claude -p` would otherwise deny.

### What `pilothouse hooks install` writes

One matcher group per event, appended after the person's own groups, each
with a single handler `{"type": "command", "command": "<pilothouse> hook claude"}`:

| Event | Matcher | Timeout | Pilothouse kind |
|---|---|---|---|
| `SessionStart` | (all) | default | `session_start` |
| `SessionEnd` | (all) | default (Claude's whole SessionEnd budget is 1.5 s; the shim gives up after 1 s) | `session_end` |
| `UserPromptSubmit` | n/a | default (30 s) | `prompt_submit` |
| `PreToolUse` | `*` | **30 s** | `pre_tool` |
| `PermissionRequest` | `*` | **600 s** | `permission` |
| `PostToolUse` | `*` | default | `post_tool` |
| `PostToolUseFailure` | `*` | default | `post_tool` (the tool ran, so the prompt was answered) |
| `Notification` | (all) | default | `notification` |
| `Stop` | n/a | default | `stop` |
| `SubagentStop` | (all) | default | `subagent_stop` |

Our handlers are recognized by a command of the form `…/pilothouse* hook claude`.
Install is idempotent, keeps every other key and hook (key order and
indentation too), writes atomically, follows a symlinked `settings.json`, backs
the file up to `settings.json.pilothouse-bak-<unix>` before changing it, and
refuses to touch a file that is not valid JSON. Uninstall removes exactly our
handlers (and any group, event list or `hooks` key left empty by that).

### Payload fields used

Common: `session_id`, `transcript_path`, `cwd`, `hook_event_name`,
`permission_mode`. Tool events: `tool_name`, `tool_input`, `tool_use_id`
(absent on `PermissionRequest`). Also `prompt` (UserPromptSubmit), `message` +
`notification_type` (Notification), `last_assistant_message` (Stop,
SubagentStop), `source` (SessionStart), `reason` (SessionEnd).

Tool classification: `Bash`, `PowerShell` → shell (`tool_input.command`);
`Edit`, `MultiEdit`, `Write` (`file_path`), `NotebookEdit` (`notebook_path`)
→ edit; `Read` (`file_path`), `Glob`, `Grep`, `LS` (`path`) → read;
`WebFetch`, `WebSearch` → web; `mcp__<server>__<tool>` → mcp; anything else →
other. File paths arrive absolute.

### Responses

Everything exits 0; decisions are JSON on stdout.

- `PreToolUse`: `{"hookSpecificOutput": {"hookEventName": "PreToolUse",
  "permissionDecision": "allow"|"deny"|"ask", "permissionDecisionReason": "…"}}`.
  For deny the reason is shown to Claude; for ask, in the prompt. An `allow`
  does not override the person's own deny/ask permission rules.
- `PermissionRequest`: `{"hookSpecificOutput": {"hookEventName":
  "PermissionRequest", "decision": {"behavior": "allow"}}}` or
  `{"behavior": "deny", "message": "…"}`. No output = the native dialog shows.
- `SessionStart` / `UserPromptSubmit`: optional `additionalContext`.
- Others: no output. (`Stop` with `decision: "block"` would keep Claude
  working, which Pilothouse never wants.)

### Questions and plans

Verified end to end with Claude Code 2.1.309 and 2.1.310
(`tests/e2e/claude/s7_questions.py`, `s8_pretool_allow.py`,
`s9_tui_answers.py`); fixtures in `internal/adapters/claude/testdata/hooks/*ask_user_question*`
and `*exit_plan_mode*`.

- **`AskUserQuestion`** fires `PreToolUse` and then `PermissionRequest` (no
  `tool_use_id` on the latter), with
  `tool_input: {"questions": [{"question", "header", "options": [{"label", "description"}], "multiSelect"}]}`
  (1–4 questions, unique texts). Pilothouse opens a request with
  `checkpoint: "question"` (summary `Asks: <first question>`); each question
  is keyed by its text, `allow_other` is always true (Claude offers "Type
  something"). The answer is
  `{"decision": {"behavior": "allow", "updatedInput": <tool_input unchanged> + {"answers": {"<question>": "<label>"}}}}`.
  A multi-select answer is the labels joined with `", "`; any other text is
  taken as the person's own answer. Claude tells the model `Your questions
  have been answered: "<question>"="<answer>"`. Claude refuses an answer whose
  questions differ from what it asked, so the input is echoed exactly; if
  the answers would not cover every question, Pilothouse gives no opinion
  and Claude's own dialog stays. A deny ("decline") tells Claude the person
  declined, with the person's reason when given.
- **`ExitPlanMode`**: Claude adds `plan` (markdown) and `planFilePath` to
  the input before the hooks run (on `PostToolUse` the input is empty and
  the plan is in `tool_response.plan`). Pilothouse opens `checkpoint:
  "plan"` (summary `Plan ready: <first heading or line>`). Approve = allow
  with `updatedInput` = the input echoed: Claude leaves plan mode and does
  the work. Request changes = deny with the feedback as `message`: Claude
  revises the plan and asks again (a new request; the old one is closed).
- **Routing** is the same as for permissions: away, the request is held and
  pushed; at the desk Pilothouse steps aside. Claude renders its own
  question / plan dialog while the hook holds the request too, so the
  person can answer either place; the first answer wins. An answer given in
  Claude's dialog closes the request as `answered_locally` (matched on
  `PostToolUse` by tool and summary; a plan rejected in the dialog is closed
  when the revised plan arrives).
- **No `allow` at `PreToolUse`.** A `PreToolUse` allow without
  `updatedInput` is ignored for both tools in 2.1.310: the dialog still
  shows and `PermissionRequest` still fires (s8). Pilothouse never sends one
  anyway — it would answer nothing, and a future version honoring it would
  approve a plan nobody read — so allow rules and session grants do not
  apply to these tools, an `ask` rule adds nothing, and only a `deny` rule
  decides them at `PreToolUse`. Grants never answer a question or a plan at
  the permission checkpoint either.
- Validation (daemon): every question needs an answer (an option label in
  any case, a comma list of labels for multi-select, or free text), plans
  take allow or deny with an optional message, neither takes "allow for
  session", and answers on a permission request are refused.
- Plan files go to `~/.claude/plans` unless the project sets
  `plansDirectory` (must be inside the project); the e2e scripts set it so
  tests never write to the real home.

### Caveats

- **Folder trust.** The first time Claude Code runs in a project it asks
  "Do you trust the files in this folder?" before reading the prompt. Trust
  is stored as `projects[<path>].hasTrustDialogAccepted` in
  `$CLAUDE_CONFIG_DIR/.claude.json` or `~/.claude.json` (Claude's own error
  messages name that key), and for a git worktree Claude looks it up under
  the main repository's root, so a task (`pilothouse task new`) in a repo
  where the person already uses Claude starts straight away. In a repo
  Claude has never been trusted in, the detached task agent would sit at
  that question unseen. Pilothouse **does not answer it or pre-trust the
  folder** — trust is what stops a repo's own `.claude/settings.json` hooks
  and MCP servers from running unreviewed. Instead `task new` reads that
  file (never writes it), prints a note with the `pilothouse attach`
  command, and reports the session as blocked on a question ("Claude is
  asking whether to trust this folder — attach to answer") until the next
  hook arrives.
- Hooks run without a controlling terminal, so the shim cannot report a TTY.
  Inside **tmux** this does not matter for hooks: approvals travel through the
  hook, and the shim forwards `$TMUX`/`$TERM_PROGRAM` so the daemon can say
  where the session lives. (Inside tmux, Claude Code asks tmux rather than the
  outer terminal whether OSC 7501 is supported; see the terminal docs.)
- `"disableAllHooks": true` turns Pilothouse off; `pilothouse hooks status` reports it.
  Managed settings with `allowManagedHooksOnly` block user hooks entirely.
- `PermissionRequest` does not fire for a sandboxed command's network request;
  only the `Notification` `permission_prompt` does (after ~6 s without typing),
  so those prompts show as blocked but cannot be answered remotely.
- The `Notification` `permission_prompt` / `idle_prompt` types fire only after
  ~6 s / ~60 s without keystrokes.
- Anyone who can edit a settings file can add a hook that approves everything;
  `ProtectedPaths()` lists the user, project and local settings files so the
  built-in `pilothouse-self-protection` rule forces a person to approve such edits.
  Hooks can also come from plugins, skills and subagent frontmatter, which
  Pilothouse does not police in v0.1.
- Settings edits are normally picked up by running sessions; restart a session
  if the hooks do not appear.

### Messaging socket (replies to sessions not started with `pilothouse run`)

Verified end to end with 2.1.310 (`tests/e2e/claude/s7_reply.py`). Hooks run
with `CLAUDE_CODE_MESSAGING_SOCKET` (`/tmp/cc-socks/<pid>.sock`, mode 0600 in
a 0700 directory) and `CLAUDE_CODE_MESSAGING_TOKEN`. Writing the NDJSON lines
`{"type":"auth","token":…}` and `{"type":"user","message":{"role":"user","content":…}}`
starts a turn at once in an idle session (UserPromptSubmit fires with the
content as the prompt). The socket answers nothing. Claude shows the model the
text under "Another Claude session sent a message: … not typed by your user",
with the rule that a peer can't grant permissions or approve prompts; tool calls
still go through the session's permission settings and hooks. 2.1.310 also
accepted a missing or wrong token, so the socket's file mode is what keeps other
users out. Claude's setting `crossSessionInbound: refuse` turns the socket off (not tested here).
Details of Pilothouse's framing: [REPLIES.md](REPLIES.md).

## Codex CLI

Source: `github.com/openai/codex` at commit `8531184` (2026-10-23), mostly
`codex-rs/hooks`. Codex is not installed on the machine this was built on, so
the fixtures in `internal/adapters/codex/testdata/hooks/` are built from
Codex's generated input schemas (`codex-rs/hooks/schema/generated/`) and its
payload builders; the trust hash is checked against vectors from Codex's own
test suite (`codex-rs/core/tests/suite/hooks.rs`).

### Hooks exist, are stable and on by default

Codex implements Claude-style lifecycle hooks: the feature `hooks` (legacy key
`codex_hooks`) is `Stage::Stable`, `default_enabled: true`
(`codex-rs/features/src/lib.rs`). Events (`codex-rs/hooks/src/lib.rs`):
`PreModelRequest`, `PostModelRequest`, `PreToolUse`, `PermissionRequest`,
`PostToolUse`, `PreCompact`, `PostCompact`, `SessionStart`, `SessionEnd`,
`UserPromptSubmit`, `SubagentStart`, `SubagentStop`, `Stop`, `Interrupt`.
There is **no Notification event**.

Hooks are read from `hooks.json` next to each config layer (`~/.codex/hooks.json`
for the user; `<repo>/.codex/hooks.json` for projects) or from `[hooks]` tables
in `config.toml`, in the same JSON shape as Claude Code
(`codex-rs/hooks/src/engine/discovery.rs`). Pilothouse writes `hooks.json` (Codex
warns if a layer uses both forms). Command hooks run through the person's
shell in a new session, with stdin JSON and stdout JSON: Codex **0.164**
runs `$SHELL -c "<command>"` (verified end to end, see
`tests/e2e/codex/README.md`; zsh then reads `~/.zshenv` for every hook),
and `main` at `8531184` uses `$SHELL -lc` (login profiles too;
`build_command` in `codex-rs/hooks/src/engine/command_runner.rs`). `$SHELL`
unset means `/bin/sh`. Default timeout 600 s;
`SessionEnd` and `Interrupt` default to 1 s and are clamped to 3 s.

### Trust: why install also edits config.toml

Codex runs a non-managed hook only if it is **trusted**: `config.toml` must
contain `[hooks.state."<hooks.json path>:<event label>:<group>:<handler>"]`
with `trusted_hash = "sha256:…"`, a SHA-256 over the normalized declaration
(event label, matcher, `{type, command, timeout, async}`). Untrusted hooks are
listed as "new — review required" and skipped (`hook_trust_status`,
`hook_hash` in `discovery.rs`; `hook_key` in `hooks/src/lib.rs`;
`version_for_toml` in `codex-rs/config/src/fingerprint.rs`).

`pilothouse hooks install --agent codex` computes those hashes and adds one table
per Pilothouse handler, each preceded by an `# pilothouse-managed` comment, using a
small line-based TOML editor that keeps every comment and unrelated line.
Uninstall removes them and renames the trust entries of the person's own hooks
whose position in `hooks.json` shifted, so their trust survives. If
`config.toml` declares hook state with inline tables or dotted keys, Pilothouse
leaves it alone, installs the hooks untrusted and says so; Codex then asks the
person to review them at startup (or in `/hooks`). `pilothouse hooks status`
reports untrusted or modified Pilothouse hooks, and `[features] hooks = false`.

`~/.codex` is `$CODEX_HOME` when set, **canonicalized** (symlinks
resolved, so `/tmp/x` is `/private/tmp/x` on macOS), else `$HOME/.codex` as
spelled, not resolved (`find_codex_home` in `codex-rs/utils/home-dir`).
The trust key embeds that path, so Pilothouse resolves it the same way.
(Under `PILOTHOUSE_USER_HOME`, i.e. tests and the e2e harness, `CODEX_HOME`
counts only when it lies inside that directory.) `pilothouse hooks status`
recomputes each installed handler's key and hash the way Codex will — key
from its real position in `hooks.json` under Codex's spelling of the path,
hash from the declaration as written — and reports "not trusted" when any
differs, and names trust entries recorded under another spelling of the
Codex home.

### What `pilothouse hooks install --agent codex` writes

| Event | Matcher | Timeout | Pilothouse kind |
|---|---|---|---|
| `SessionStart` | (all) | default | `session_start` |
| `SessionEnd` | (all) | 3 s | `session_end` |
| `UserPromptSubmit` | n/a | default | `prompt_submit` |
| `PreToolUse` | `*` | 30 s | `pre_tool` |
| `PermissionRequest` | `*` | 600 s | `permission` |
| `PostToolUse` | `*` | default | `post_tool` |
| `Stop` | n/a | default | `stop` |
| `SubagentStop` | (all) | default | `subagent_stop` |
| `Interrupt` | n/a | 3 s | `stop` (the person pressed Esc; the session now waits) |

### Payloads

Common: `session_id`, `turn_id`, `transcript_path` (the rollout file
`~/.codex/sessions/YYYY/MM/DD/rollout-…-<uuid>.jsonl`; may be `null`), `cwd`,
`hook_event_name`, `model`, `permission_mode` (`default` or
`bypassPermissions`, derived from `approval_policy`).

Tool names (`codex-rs/core/src/tools/hook_names.rs` and handlers):
`Bash` with `{"command": "<string>"}` for `exec_command` (and for network-access
approvals, with `description: "network-access <host>"`); `apply_patch` with
`{"command": "<patch>"}` (Pilothouse extracts the file list from the `*** Add/
Update/Delete File:` and `*** Move to:` lines). When the model has no
`apply_patch` tool (any model slug Codex does not know), it patches through
`exec_command`: `Bash` with `apply_patch <<'EOF'` … `EOF` (or `applypatch`,
optionally after `cd <dir> &&`). When that is the whole command, Codex
intercepts it and applies the patch itself
(`extract_apply_patch_from_bash` in `codex-rs/apply-patch/src/invocation.rs`),
so Pilothouse classifies the call as an **edit** with the patch's files as
`paths` (relative to the `cd` directory if any). A longer command that also
contains such a heredoc stays a shell command; its patch files are still
listed in `paths` for deny/ask rules, and allow rules never decide on them
(POLICY.md). The same extraction applies to Claude Code `Bash` calls; `mcp__<server>__<tool>` with
the raw arguments; `write_stdin` (`chars` typed into a running command,
classified as shell); `spawn_agent`, `request_permissions` and extension tools.
`PermissionRequest` has no `tool_use_id`.

### Responses (`codex-rs/hooks/src/events/*.rs`, `engine/output_parser.rs`)

Because hooks run through the person's shell, anything a startup file
prints to stdout lands in front of the JSON answer, Codex cannot parse it,
and the answer is silently dropped (QA saw a policy deny lost this way and a
force-push run). So **every deny is an exit code**: exit 2 with the reason on
stderr, which Codex reads whatever stdout holds — the `Some(2)` arms in
`pre_tool_use.rs`, `permission_request.rs` and `user_prompt_submit.rs`
(stderr must not be empty, or the hook counts as failed and the call runs).
Only answers without an exit-code form use JSON on stdout.

- `PreToolUse`: deny → exit 2. Codex shows it to the model as
  `Command blocked by PreToolUse hook: <reason>. Command: …`
  (`codex-rs/core/src/hook_runtime.rs`), so the reason is sent without a
  final period. `allow` without `updatedInput` and `ask` are reported as
  unsupported and the hook counts as failed, so Pilothouse never sends them:
  - policy **`ask`** (the built-in self-protection rule or a person's own ask
    rule) is sent as a **deny** whose reason starts "Pilothouse: a person must
    approve this, and Codex cannot ask them from a hook" and asks the agent
    to have the user run it themselves (self-protection; no rule can loosen
    it) or allow it with a Pilothouse rule (the person's own rules), followed
    by the rule's message. Leaving it unanswered would let the call run
    without the person the rule demands (QA: writes into `~/.pilothouse`
    succeeded). The daemon passes the deciding rule in
    `HookOutcome.Rule`; a "Pilothouse self-protection:" message is
    recognized as well.
  - `allow` (rule or grant) prints nothing: Codex's own approval policy
    applies.
  `continue: false`, `stopReason` and `suppressOutput` are rejected too.
- `PermissionRequest`: allow → `decision.behavior: "allow"` JSON (there is
  no exit-code allow; if shell noise garbles it, Codex shows its own prompt,
  which is safe); deny → exit 2 + message on stderr. Codex fails closed on
  `updatedInput`, `updatedPermissions` and `interrupt: true`, so Pilothouse
  never sends them. Any deny wins over allows from other hooks.
- `UserPromptSubmit`: deny → exit 2; else optional `additionalContext`.
- `SessionStart`: optional `additionalContext`.
- `approvals.fail_closed` with the daemon down: `PreToolUse` exits 2 with
  the explanation.

### Caveats

- Policy `allow` cannot skip Codex's prompt at `PreToolUse`, and `ask`
  cannot force it (Pilothouse denies instead, see Responses).
  "Allow for session" grants are applied at `pre_tool` (BUILD-SPEC §5), where
  Codex ignores `allow`; for them to take effect on Codex the daemon must also
  apply a matching grant at the `permission` checkpoint (answer `allow`
  without routing to a person). The adapter encodes that answer correctly.
- No `Notification` event: "blocked" comes from `PermissionRequest` (it fires
  before Codex shows its prompt), "waiting" from `Stop`/`Interrupt`.
- **Questions (`request_user_input`) are read-only.** Its `PreToolUse` hook
  fires with the model's arguments (`{"questions": [{"id", "header",
  "question", "options": [{"label", "description"}]}]}`,
  `request_user_input_spec.rs`), but no `PermissionRequest` follows and a
  hook can only deny it; the answer travels through Codex's own UI or its
  app-server (`codex-rs/core/src/tools/handlers/request_user_input.rs`).
  Pilothouse reads the questions so the session shows "blocked (question):
  Asks: …" until the next hook, and the inbox says to answer in the
  terminal. A policy `ask` on it is dropped (its deny form would cancel the
  question); a `deny` rule still blocks it.
- Hooks run through `$SHELL -c` (0.164; `-lc` on main): anything a startup
  file (`~/.zshenv`, and login profiles with `-lc`) prints to stdout lands in
  front of Pilothouse's JSON. Denies are unaffected (exit code 2), but a
  remote **allow** is lost (Codex then shows its own prompt) and SessionStart
  context is dropped. `pilothouse doctor` runs `$SHELL -c true` and warns
  when it prints anything (`codex.ShellNoise`). Keep shell startup files
  quiet for non-interactive shells.
- Hooks need `[features] hooks` on (the default) and are ignored when the
  admin sets `allow_managed_hooks_only = true` in `requirements.toml`.
- tmux: hooks do not depend on the terminal; Codex's OSC 7501 output is emitted
  whenever stdout is a terminal (including inside tmux).
- `notify` (`notify = ["program", …]` in config.toml) is a legacy fire-and-forget
  program run after each turn with one JSON argument
  (`codex-rs/hooks/src/legacy_notify.rs`, marked for removal). It cannot block
  or answer anything and hooks cover it, so Pilothouse does not install it.

## Grok Build

Source: <https://docs.x.ai/build/features/hooks> and the Grok Build **1.0.70**
user guide (`~/.grok/docs/user-guide/*.md`, especially `10-hooks.md`,
`17-sessions.md` and `24-monitoring-usage.md`). The adapter fixtures are built
from that documentation and a local install; they have **not** yet been
captured from a live authenticated session.

### What `pilothouse hooks install --agent grok` writes

One personal file, `~/.grok/hooks/pilothouse.json`, merged with anything else
there and backed up first. Grok also reads `~/.claude/settings.json` and
`~/.cursor/hooks.json`; Pilothouse never writes those for Grok.

| Event | Matcher | Timeout | Pilothouse kind |
|---|---|---|---|
| `SessionStart` | (all) | default | `session_start` |
| `SessionEnd` | (all) | default | `session_end` |
| `UserPromptSubmit` | n/a | default | `prompt_submit` |
| `PreToolUse` | `*` | 30 s | `pre_tool` |
| `PostToolUse` | `*` | default | `post_tool` |
| `PostToolUseFailure` | `*` | default | `post_tool` (the tool ran, so the prompt was answered) |
| `PermissionDenied` | `*` | default | `unknown` (informational; no dialog is waiting) |
| `Stop` | n/a | default | `stop` |
| `StopFailure` | n/a | default | `stop` (the error is the message) |
| `StopCancelled` | n/a | default | `stop` (the reason is the message) |
| `Notification` | (all) | default | `notification` |
| `SubagentStart` | (all) | default | `unknown` |
| `SubagentStop` | (all) | default | `subagent_stop` |
| `PreCompact` | (all) | default | `pre_compact` |
| `PostCompact` | (all) | default | `unknown` |

There is **no `PermissionRequest`** event. Grok's hook timeout defaults to 5 s;
Pilothouse's shim is fast except when a person is answering, so 30 s on
`PreToolUse` is ample. A malformed answer, crash or timeout **fails open**;
only an explicit `deny` blocks.

### Folder trust gates every hook

Grok records folder trust in `~/.grok/trusted_folders.toml`. A folder that is
not trusted loads no personal hooks — including this file — and no project
hooks. Trust it with `/hooks-trust`, or launch with `--trust` (the grant covers
MCP, LSP, hooks and project instructions for that repository and its
subdirectories). The person's home directory, filesystem roots and shared
temporary directories count as trusted. A headless run (`grok -p …`) counts its
folder as trusted **for personal hooks only**; project hooks and a project
agent's own hooks still need trust. `pilothouse hooks install` prints this
reminder, and `pilothouse hooks status` checks the file itself, not folder
trust, so a trusted folder is still a per-project step for interactive use.

### Payloads

Grok's native fields are camelCase: `hookEventName`, `sessionId`, `cwd`,
`workspaceRoot`, `permissionMode`, `toolName`, `toolInput`, `toolUseId`,
`timestamp`, `promptId`, `transcript_path` and the event-specific fields. It
also sends Claude's snake_case spellings (`session_id`, `tool_name`,
`tool_input`, `tool_use_id`, …). The two event-name keys differ:

- `hookEventName` carries Grok's snake_case name (`pre_tool_use`, `stop_failure`).
- `hook_event_name` carries Claude's PascalCase name (`PreToolUse`).

Pilothouse accepts either and normalizes (`pre_tool_use` → `PreToolUse`).
`transcript_path` points at the session's `updates.jsonl`.

Tool names are Grok's own, with Claude aliases for matchers:
`run_terminal_command` (`Bash`), `read_file` (`Read`), `search_replace`,
`hashline_edit`, `write` (`Edit`/`Write`/`MultiEdit`), `grep` (`Grep`),
`list_dir` (`Glob`/`LS`), `web_search` (`WebSearch`), `web_fetch` (`Fetch`),
`spawn_subagent` (`Task`) and the internal `use_tool` / `search_tool`
dispatchers. MCP calls arrive as `server__tool` (no `mcp__` prefix). Under the
Cursor preset, `StrReplace` and `Write` use `path` + `contents`, and
`EditNotebook` uses `target_notebook`; the adapter reads those keys too.
`toolInput` is capped at 128 KiB with `toolInputTruncated: true` when strings
were cut.

### Responses

- **`deny`**: `{"decision":"deny","reason":"…"}`; the reason is shown to the
  model and the call does not run.
- **`ask`**: `{"decision":"ask","reason":"…"}`; this forces Grok's own
  permission prompt. It overrides always-approve, auto mode, a saved "always
  allow" grant and safe-command handling, but a client that blanket-approves
  every prompt still approves it. An ask cannot widen a permission policy
  deny, an auto-mode block or plan mode.
- **`allow`**: `{"decision":"allow"}` only means "not blocked"; it does **not**
  auto-approve. Pilothouse sends no opinion instead.
- **`defer`** is "no opinion"; Pilothouse writes no output, which has the same
  effect without a warning in Grok's log.
- Exit 2 with a `deny` on stdout keeps the deny. A crash, timeout or malformed
  reply fails open.
- `PreToolUse` runs before Grok's plan-mode and permission checks. A bash-mode
  (`!`) command typed in the TUI raises no hooks at all, so no hook can deny it.
- Grok's `Stop` can be blocked with `decision: "block"` to keep working;
  Pilothouse never does that. `UserPromptSubmit` can block a prompt; Pilothouse
  does not.

Policy `ask` is therefore honest at the desk and **not remote-answerable**:
there is no permission hook to hold. Away from the desk the terminal prompt
waits; Pilothouse does not pretend it can answer it from a phone.

### Activity and sessions

Sessions live under `$GROK_HOME/sessions/<url-encoded-cwd>/<session-id>/`.
`updates.jsonl` is the event stream: each line has
`{timestamp, method, params:{sessionId, update, _meta:{eventId, agentTimestampMs}}}`,
where `method` is `session/update` or `_x.ai/session/update` and `update` is an
ACP update (`user_message_chunk`, `agent_message_chunk`, `agent_thought_chunk`,
`tool_call`, `tool_call_update`, `plan`, …) or an xAI extension
(`subagent_spawned`, `task_backgrounded`, `auto_compact_started`,
`compaction_checkpoint`, `rewind_marker`, …). Older bare `{sessionId, update}`
lines are accepted. Tool calls carry `toolCallId`, `title`, `kind`, `status`
(`pending`/`in_progress`/`completed`/`failed`), `rawInput`, `rawOutput` and
`locations[].path`; diff content blocks carry `{type:"diff",path,oldText,newText}`.
`usage.json` (camelCase tokens/cost/turns) is not parsed yet, so the activity
feed has no token usage for Grok.

### Caveats

- No remote approvals and no way to answer Grok's questions or plan approval
  from Pilothouse.
- No usage/token numbers yet (`usage.json` is documented but unparsed).
- Live hook payloads, tool names and the trust flow still need an end-to-end
  run against an authenticated Grok Build; the parser accepts documented
  camelCase and Claude-compatible shapes in the meantime.
- Personal hooks are skipped until each interactive project is trusted; use
  `/hooks-trust` or `--trust`. Headless `grok -p` runs do not hit this for
  personal hooks.

## OpenCode

Source: OpenCode **1.18.25** (`opencode`, the native binary; its bundled
plugin loader and server routes) and the `@opencode-ai/plugin` /
`@opencode-ai/sdk` 1.18.25 type definitions. Everything below was run end to
end against the real CLI with a mock model (`tests/e2e/opencode`).

### A plugin, not hooks

OpenCode has no command hooks. It loads JavaScript plugins from
`{plugin,plugins}/*.{js,ts}` in every config directory — the global one
(`$XDG_CONFIG_HOME/opencode`, else `~/.config/opencode`, on every OS),
`~/.opencode`, the project's `.opencode/` directories and
`$OPENCODE_CONFIG_DIR` — and runs them **inside its own process** (the TUI,
`opencode run` and `opencode serve` alike). `pilothouse hooks install --agent
opencode` writes Pilothouse's plugin to
`<global config>/plugins/pilothouse.js`. The global directory is loaded
whether or not `OPENCODE_CONFIG_DIR` is set, so the plugin works from any
shell; `hooks status` reports a second copy in another plugin directory
(it would load twice). The file carries an `@pilothouse-managed` marker:
install refuses to replace a `pilothouse.js` without it, uninstall removes
only ours, and other plugins (herdr's, your own) are never read or touched.
The plugin has no npm dependencies; OpenCode still installs
`@opencode-ai/plugin` into the config directory at startup, as it does for
every plugin directory. A plugin exports `default { id, server }`, the
shape 1.x loads for file plugins; it does not export the 2.x
(`@opencode/cli`) `setup` definition, whose plugin API is different and
was not available to test.

The plugin talks to the daemon over its Unix socket (Bun's `fetch` with the
`unix` option; the path is resolved like Pilothouse's own:
`$PILOTHOUSE_RUN_DIR`, else `$PILOTHOUSE_HOME/run`, else
`~/.pilothouse/run`):

- `POST /v1/plugin/hook` — a `HookEnvelope` (`agent: "opencode"`) whose
  payload is the plugin's own normalized event (`internal/adapters/opencode`,
  `parse.go`); the answer is a `HookResponse` whose stdout is a small JSON
  object (`encode.go`). Timeouts: 2 s (plus 1 s per MB of payload, at most
  20 s) for everything that doesn't wait for a person.
- `POST /v1/plugin/commands` — a 25-second long poll that delivers replies a
  person sends to the plugin's sessions (see Replies).

With the daemon down every call fails at once and the plugin does nothing:
OpenCode behaves as if it were not installed. `approvals.fail_closed: true`
(read from `config.yaml`, as the hook shim does) makes it fail tool calls
instead.

### What the plugin forwards

| OpenCode | Pilothouse kind | Notes |
|---|---|---|
| `tool.execute.before` (plugin hook) | `pre_tool` | Runs before every tool call and before OpenCode's own permission check; awaited |
| `tool.execute.after` | `post_tool` | |
| `chat.message` (plugin hook) | `prompt_submit` | The prompt's text parts |
| `permission.asked` (event) | `permission` | Enriched with the tool call's arguments seen at `tool.execute.before` |
| `question.asked` (event) | `permission` with questions | Becomes a question request |
| `permission.replied`, `question.replied`, `question.rejected` | `post_tool`, matched by the dialog's id | The person answered OpenCode's own dialog |
| `session.created` / `session.deleted` | `session_start` / `session_end` | |
| `session.idle` | `stop` | With the turn's last assistant text (or `Error: …` after `session.error`) |
| `session.updated` (title changed) | notification `title` | Logged; Pilothouse keeps the repo name as the title |
| `experimental.session.compacting` | `pre_compact` | |

Tools (`packages/opencode/src/tool`): `bash {command}` → shell; `edit`,
`write {filePath}`, `apply_patch {patchText}` → edit (paths resolved
against the session's directory); `read`, `glob`, `grep`, `list` → read;
`webfetch`, `websearch`, `codesearch` → web; MCP tools are named
`<server>_<tool>` → mcp. A `bash` call that is a whole `apply_patch` heredoc
counts as an edit, as for Codex. Subagents (the `task` tool) run in child
sessions; their calls are reported as the parent session's.

### Responses

- **`deny`** (policy, before a tool call): the plugin throws; OpenCode fails
  the call with the rule's message, which the model reads.
- **`ask`** (policy): OpenCode has no way for a plugin to show a permission
  prompt (the `permission.ask` plugin hook exists in the types but is never
  called), and its defaults ask for almost nothing. So the plugin **holds
  the call** and asks the daemon again (`pilothouse.ask`); the daemon holds
  that request for a person **even at the desk** (`HookEvent.Hold`): it
  shows in the inbox (`pilothouse approve`/`deny`, the TUI) and on paired
  devices, and OpenCode shows a "waiting for a person" toast. A session
  grant never answers it. Allow lets the call run (and answers OpenCode's
  own prompt for that same call, so the person isn't asked twice); deny or
  no answer within `approvals.hold_limit` fails it.
- **`allow`** (policy) is no opinion: OpenCode's own `permission` settings
  still apply. Session grants ("allow for this session") are applied at
  OpenCode's permission prompt, which the daemon answers without asking.
- **Permission prompts** (`permission.asked`): at the desk the request
  steps aside and OpenCode's own dialog is answered there; when you answer
  it, `permission.replied` closes the request. Away, it is held for your
  devices while OpenCode's dialog stays open too — whichever is answered
  first wins. A device allow is sent as `reply: "once"`, a deny as
  `reply: "reject"` with the message, which OpenCode passes to the model as
  feedback (`POST /permission/{id}/reply`, through the plugin's in-process
  client: the TUI has no listening server).
- **Questions** (`question.asked`): one request per OpenCode question
  request, one `Question` per question with id `<que_id>#<n>`; options,
  `multiple` (multi-select) and `custom` (free text, OpenCode's default) are
  kept. Answers go back in question order as lists of labels
  (`POST /question/{id}/reply {"answers": [["Blue"], ["Lint", "E2E"]]}`); a
  decline rejects the question. Sets Pilothouse can't ask (more than 8
  questions, more than 16 options, unbounded labels) are left to OpenCode's
  dialog and shown as "waiting on a question".

### Permissions: what OpenCode asks by default

OpenCode 1.18's default rules are `{"*": "allow", "doom_loop": "ask",
"external_directory": "ask"}` (plus `.env` reads): it runs shell commands
and edits files without asking, so there is no prompt Pilothouse could hold
for you while you're away. `pilothouse setup` offers to set
`"permission": {"bash": "ask", "edit": "ask"}` in the global config file
that wins (`opencode.jsonc`, else `opencode.json`, else `config.json`, else
a new `opencode.json`): it prints the exact diff, asks, writes a backup next
to the file, and keeps your comments and formatting. A permission you set to
`deny`, `ask` or a per-pattern object is left alone; `pilothouse doctor`
mentions the suggestion until it is applied. (Project configs and the
`OPENCODE_PERMISSION` environment variable can still override the global
file.)

### Replies

A person can reply to an OpenCode session from the phone (a signed `reply`
action) or any local client of `POST /v1/sessions/{id}/reply` (guarded
against agents like decisions): the daemon queues the text for the session's plugin,
whose long poll picks it up within a moment, runs
`client.session.promptAsync` (or `client.session.abort` for an interrupt)
and acknowledges it; the reply call reports OpenCode's error if it refused.
`caps.reply` (via `plugin`) is set only while the plugin's channel is
connected.

Security: the plugin routes are open to any local process of the user,
agents included (the plugin runs inside one). Each OpenCode process makes a
random token and sends it with its hooks and polls; the daemon keeps only
its SHA-256. The first token to report a session owns it and is the only
one that can receive (or acknowledge) that session's replies. Another token
reporting the same session while the owner is connected makes it
**contested**: replies are refused and the daemon logs it. A new process
takes over once the old one's channel has been gone for 15 s (OpenCode quit
and the session was resumed). After a daemon restart the plugin re-announces
its sessions with its first poll. The text of a reply is in the event log
(`session.reply`), as for every agent; the token never is.

### Caveats

- OpenCode must be able to load plugins: `--pure` turns external plugins
  off, and so does a config that can't be parsed.
- Policy `allow` can't skip OpenCode's own prompt (only a person's grant
  does, at the prompt).
- Activity and session tokens/cost are read from OpenCode's own database
  (`opencode.db`); OpenCode is not yet included in the global usage page.
- `pilothouse run opencode` names the session OpenCode and the plugin
  forwards `PILOTHOUSE_SESSION_ID`, so its hooks join the wrapped session
  (one session, not a second observed one); replies to it go through the
  plugin.
- Tasks (`pilothouse task new --agent opencode`, or from the phone) pass
  the prompt with `--prompt`: OpenCode reads a bare first argument as a
  project path. From the phone, OpenCode is found on the daemon's PATH or
  in `~/.opencode/bin`, and only once the plugin is installed.
- An `opencode serve` shared by several clients is one process: its
  sessions share a PID and are expired after `session_idle_expiry` of
  silence rather than by process liveness.

## OSC 7501 (for wrap mode)

- **Codex** (`codex-rs/tui/src/terminal_program_status.rs`) writes
  `ESC ] 7501 ; state=<idle|working|blocked|done|error>:app=codex[:kind=<permission|question|auth>] ESC \`
  whenever stdout is a terminal, and `ESC ] 7501 ; state=clear ESC \` on exit.
  Note the **colon** between fields, and the extra `clear` state.
- **Claude Code** (v2.1.295+) reports working / waiting / done through OSC 7501
  only after it detects that the terminal supports the protocol (inside tmux it
  asks tmux, not the outer terminal), and not when
  `CLAUDE_CODE_DISABLE_TERMINAL_TITLE=1`
  (<https://code.claude.com/docs/en/terminal-config>). The detection handshake
  (observed in Claude Code 2.1.309): Claude writes the query `ESC ] 7501 ; ? ST`
  and treats any OSC 7501 reply whose data starts with `?` as support. The
  wrap holder answers it with `ESC ] 7501 ; ? ESC \`, so wrapped Claude
  sessions report `state=…:app=claude-code[:kind=…][:msg=<base64>]`
  (colon-separated). Statuses with an `id=` field describe background tasks,
  not the session, and are ignored; `state=clear` is reported as idle.
- **Codex wrap mode**: Codex's TUI normally runs sessions on a shared
  `codex app-server --managed-daemon`, which runs the hooks with its own
  environment, so `PILOTHOUSE_SESSION_ID` would never reach them.
  `pilothouse run codex` therefore adds `--no-daemon` (TUI, `resume`, `fork`;
  only when `codex --help` lists it) so hooks join the wrapped session. Set
  `PILOTHOUSE_CODEX_SHARED_DAEMON=1` to keep the shared server.
