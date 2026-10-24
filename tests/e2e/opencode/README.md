# OpenCode end-to-end tests (real OpenCode, mock model)

`e2e.py` drives the **real** OpenCode CLI (1.18.25 when written) with
Pilothouse's plugin installed by `pilothouse hooks install --agent opencode`,
against an isolated Pilothouse daemon and `mock_openai.py`, a scripted
OpenAI-compatible model (no account, no network for the model).

```sh
R=$(mktemp -d)                                   # an empty directory you made
E2E_ROOT=$R python3 tests/e2e/opencode/e2e.py    # the OpenCode-server scenarios
# the TUI scenario needs pyte:
python3 -m venv $R/venv && $R/venv/bin/pip install pyte
E2E_ROOT=$R $R/venv/bin/python tests/e2e/opencode/e2e.py --tui s_tui s_wrap
rm -rf $R
```

Optional: `OPENCODE_BIN` (default `~/.opencode/bin/opencode`) and
`OC_NODE_MODULES`, a `node_modules` that already holds `@opencode-ai/plugin`
(OpenCode installs it into every config directory at startup and waits for
it before loading plugins; pointing at a copy saves the download).

## Isolation

Everything lives in `E2E_ROOT`: the binary, `PILOTHOUSE_HOME`,
`PILOTHOUSE_USER_HOME` (= `HOME` for OpenCode, so its global config is
`$E2E_ROOT/uhome/.config/opencode`), OpenCode's XDG data/cache/state dirs and
the project. The daemon's socket dir is a short `mktemp` dir under `/tmp`
(Unix socket paths are limited to ~104 bytes) and is removed at the end.
OpenCode runs with `OPENCODE_DISABLE_CLAUDE_CODE`, `…_MODELS_FETCH`,
`…_AUTOUPDATE`, `…_SHARE`, `…_LSP_DOWNLOAD` and `…_EXTERNAL_SKILLS`. Ports are
picked free at start. Processes are stopped by PID. Admin commands
(`pilothouse here/away/approve/deny/pair`) run through `../claude/detach.py`
because the harness may itself run under an agent, which the agent-caller
guard refuses.

The global OpenCode config gets `"permission": {"bash": "ask", "edit": "ask"}`
from `suggest/` — the same change `pilothouse setup` offers — applied to a
JSONC file with comments, so each run also checks that OpenCode accepts the
edited file and asks.

## Scenarios

| Scenario | What it checks |
|---|---|
| `s_policy_deny` | A deny rule fails the tool call in `tool.execute.before`; the model reads the rule's message; the session shows as an `opencode` session, waiting |
| `s_desk_step_aside` | At the desk the permission request steps aside and OpenCode's own dialog stays up; answering it in OpenCode closes the request `answered_locally` |
| `s_away_allow` / `s_away_deny` | Away: the permission is held; a signed v2 `respond` from the simulated phone allows it, or denies it with feedback the model reads; replaying the signed request is refused |
| `s_question` | A question (single, then multi-select + yes/no) held while away and answered from the phone; OpenCode's question tool gets the labels in order |
| `s_ask_held_at_desk` | A policy `ask` (which OpenCode can't express) is held for a person even at the desk, approved with `pilothouse approve` (OpenCode's own prompt for that call is then answered too) and denied with `pilothouse deny -m` |
| `s_reply` | `caps.reply` via plugin; a signed reply from the phone reaches the model through `promptAsync`, and the session's last message follows |
| `s_daemon_down` | With the daemon stopped OpenCode keeps working (a call the policy would deny runs: fail open); after a restart the plugin reconnects and replies work again |
| `s_tui` (`--tui`) | The interactive TUI, where the plugin's client is in-process (no listening server): held + allowed from the phone, then a phone reply |
| `s_wrap` (`--tui`) | `pilothouse run -d -- opencode --prompt …`: the plugin's hooks join the wrapped session (no second session), held + allowed from the phone, `caps.reply` via plugin |

Each run prints `[e2e] RESULTS {...}` and exits non-zero on any failure.
Logs stay in `E2E_ROOT`: `daemon.log`, `serve.log`, `tui.log`, `mock.jsonl`.
