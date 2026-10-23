# Agents: what Pilothouse can and cannot do with each one

Pilothouse talks to coding agents through their **hooks**: the agent runs
`pilothouse hook <agent>` at fixed points, pipes a JSON payload to it, and reads
the answer from its stdout (or, for Codex denies, its exit code). The shim forwards the payload to the daemon and
prints the daemon's answer; if the daemon is down or slow it prints nothing and
exits 0, so the agent behaves as if Pilothouse were not installed (fail open).

This page records what each agent really supports, with sources, as of
2026-10-22. It is the reference for `internal/adapters/claude` and
`internal/adapters/codex`.

## At a glance

| | Claude Code | Codex CLI |
|---|---|---|
| Hook config Pilothouse edits | `~/.claude/settings.json` | `~/.codex/hooks.json` + trust entries in `~/.codex/config.toml` |
| State detection | Full: session start/end, prompt, every tool call, permission prompts, notifications (idle, permission waiting), stop | Full except notifications: session start/end, prompt, tool calls, permission prompts, stop, **interrupt** |
| Policy `deny` (before every tool call) | Yes (`PreToolUse` → `permissionDecision: "deny"`) | Yes (`PreToolUse` → exit code 2, reason on stderr) |
| Policy `ask` (force the agent's prompt) | Yes (`permissionDecision: "ask"`) | **No prompt** — Codex has no `ask` at `PreToolUse`, so Pilothouse **denies** the call and tells the agent a person must do it (see Responses) |
| Policy / grant `allow` (skip the prompt) | Yes (`permissionDecision: "allow"`; the agent's own deny/ask rules still win) | **No** at `PreToolUse` (Codex rejects `allow` without `updatedInput`); see Caveats |
| Remote approvals (phone answers the prompt) | Yes (`PermissionRequest` → `decision.behavior` allow/deny), held up to 600 s | Yes (`PermissionRequest` → `decision.behavior` allow/deny), held up to 600 s |
| Resume a session | `claude --resume <session_id>` | `codex resume <session_id>` |
| OSC 7501 status (wrap mode, L1) | Emits it (v2.1.295+) when it detects terminal support | Emits it whenever stdout is a terminal |

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
