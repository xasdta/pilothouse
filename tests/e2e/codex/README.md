# Codex end-to-end tests (real Codex CLI, mock model)

These scripts drive the **real** Codex CLI (`@openai/codex`, installed into a
temp prefix) against Pilothouse without an OpenAI account. Codex is pointed at
`mock_responses.py`, a local stand-in for the OpenAI Responses API, through a
custom model provider in `$CODEX_HOME/config.toml`:

```toml
model_provider = "mock"
[model_providers.mock]
base_url = "http://127.0.0.1:7597/v1"
env_key  = "MOCK_API_KEY"      # any dummy value
wire_api = "responses"
```

The mock picks a scenario from `scenario=<name>` in the user prompt. Its
first reply is the scenario's tool call (`exec_command`, optionally with
`sandbox_permissions: "require_escalated"` so `-a on-request` asks for
approval). Once the tool result comes back, it sends a final message
`MOCK-DONE …`. Every request is logged to `$E2E_ROOT/mock/requests.jsonl`,
and `common.tool_output()` reads back what Codex told the model.

Everything is isolated under `E2E_ROOT` (default `/tmp/qa6`, canonicalized).
That covers `PILOTHOUSE_HOME`, `PILOTHOUSE_USER_HOME`, `PILOTHOUSE_RUN_DIR`,
and `CODEX_HOME` = `HOME/.codex`, with `HOME` = `$E2E_ROOT/uhome`.
Never run Codex without `CODEX_HOME`: even `codex --version` writes to
`~/.codex/tmp`.

## Run

```sh
sh setup.sh                 # install Codex, build, mock + daemon, hooks, pair phone
$E2E_ROOT/venv/bin/python s_exec.py
$E2E_ROOT/venv/bin/python s_away.py
$E2E_ROOT/venv/bin/python s_policy.py
$E2E_ROOT/venv/bin/python s_wrap.py              # CODEX_FLAGS="--no-daemon …" for the control run
go run ./tests/e2e/codex/osccheck                # (from the repo root) OSC parser vs Codex's sequences
sh teardown.sh
```

| Script | What it checks |
|---|---|
| `s_exec.py` | `codex exec`: session tracking, deny, a noisy `~/.zshenv`, self-protection, `.env` path rule vs a shell `apply_patch`, fail-open |
| `s_away.py` | TUI `-a on-request`: held for phone → allow / deny, allow-for-session grant, handback on `here` |
| `s_policy.py` | TUI: policy `allow` / `ask` are ignored by Codex, deny still blocks |
| `s_wrap.py` | `pilothouse run codex`: OSC 7501 emitted / recorded, hooks join the wrapped session |

## Things to know about Codex 0.164

- `codex exec` always runs with `approval: never`. It ignores
  `-c approval_policy=…`, so PermissionRequest only happens in the TUI.
  Drive the TUI with `--no-alt-screen -a on-request` in a PTY
  (`../claude/ptyterm.py`, needs `pyte`).
- The TUI starts a shared background `codex app-server --managed-daemon`
  under `$CODEX_HOME/packages/…`. It outlives the TUI and runs the hooks, so
  hooks inherit the daemon's environment and its PID. `teardown.sh` kills it.
- Hooks run as `$SHELL -c "<command>"` (not `-lc`). zsh sources `~/.zshenv`
  for every hook. `~/.zprofile` is only read for Codex's shell snapshot.
- The TUI also sends a tool-less "generate a title" request each turn. The
  mock answers it with plain text.
- With an unknown model slug, Codex offers no `apply_patch` tool. Patches go
  through `exec_command` (`apply_patch <<'EOF' …`), which Codex intercepts.
- Hook trust keys use the canonical `CODEX_HOME` path (`/private/tmp/…` on
  macOS). That is why `E2E_ROOT` is canonicalized.
- Other outbound traffic: the TUI fetches
  `raw.githubusercontent.com/openai/codex/main/announcement_tip.toml`. With
  `[features] plugins = false` Codex no longer calls `chatgpt.com`.
