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

## Web: the 0.2 views (`web/20-*.mjs` … `web/24-*.mjs`)

The session page, live terminal, tasks and insights are checked against the
web devserver (`go build -tags devserver ./internal/web/devserver`: real
pairing, cookies, device keys and static files) while the 0.2 API calls are
answered from fixtures by request interception (`web/v02-fixtures.mjs`), so
they run before the daemon implements those calls. Streams (events, the
terminal) go to a local mock SSE server. Signed actions are verified against
the device's public key the way `internal/web/signed.go` does.

```sh
cd tests/e2e/web && npm ci
node 20-session.mjs      # feed, catch-up, polling, reply/interrupt (signed), fallbacks
node 21-terminal.mjs     # token, stream, input, quick keys, reconnect, read-only
node 22-tasks.mjs        # list, new task, diff, ship/discard (signed)
node 23-insights.mjs     # ranges, chart tooltip/keyboard/table, breakdowns, budget
PH_SHOTS=/tmp/shots node 24-screens-v02.mjs   # 390×844 + 1440×900, light + dark, axe
```

They need Go and Chrome (`CHROME=` to point at another binary) and use a
throwaway Chrome profile and temp dirs (`PH_TMP`, removed afterwards unless
`PH_KEEP=1`).
