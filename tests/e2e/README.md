# End-to-end tests

Real-software tests, run outside `go test`. Each folder has its own README.

| Folder | What it drives |
|---|---|
| `claude/` | Real Claude Code (interactive and wrapped) against an isolated daemon |
| `codex/` | Real Codex CLI against a local mock of the Responses API (no account) |
| `opencode/` | Real OpenCode (server and TUI) with Pilothouse's plugin, a mock model and a simulated phone |
| `web/` | The phone/browser app in real Chrome (puppeteer), incl. real Web Push |
| `reliability/` | ntfy, webhooks, launchd, update check, soak and robustness |
| `linux/` | Full Linux suite; runs in CI (`.github/workflows/e2e-linux.yml`) |

All of them use temporary `PILOTHOUSE_HOME`, `PILOTHOUSE_USER_HOME` and
`PILOTHOUSE_RUN_DIR`, and never touch your real agent settings.
