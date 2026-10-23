# Reliability suite (manual, macOS)

Black-box reliability tests against a real `pilothouse` binary. They use only
`/tmp/qa3` (`PILOTHOUSE_HOME`, `PILOTHOUSE_USER_HOME`, `PILOTHOUSE_RUN_DIR`)
and web port 7593, and never touch `~/.claude`, `~/.pilothouse`,
`~/Library/LaunchAgents` or the real launchd label.

```sh
go build -ldflags "-X github.com/xasdta/pilothouse/internal/version.Version=0.1.1" -o /tmp/qa3/pilothouse ./cmd/pilothouse
tests/e2e/reliability/notify.sh        # ntfy.sh (random topic), webhooks ok/slow/fail, osascript injection, action tokens
tests/e2e/reliability/launchd.sh       # relabelled plist (dev.pilothouse.qa3): KeepAlive, kickstart -k, bootout, holder survival, attach
tests/e2e/reliability/update_check.sh  # live pilothouse.dev/api/version behind logging CONNECT proxies; opt-outs send nothing
tests/e2e/reliability/soak.sh          # 50 sessions × 20k PreToolUse, 50 rules, 200 phone-signed approvals, 20 SSE, SIGSTOP, retention
DISKFULL=1 tests/e2e/reliability/robustness.sh  # shim fuzzing, SIGKILL mid-hold, corrupt policy/config, DB lock, full disk
```

| File | Purpose |
|---|---|
| `common.sh` | env isolation, `start_daemon`/`stop_daemon`, `detached` (double fork so the agent-caller guard doesn't see an agent ancestor) |
| `webhook_server.py` | fake Slack/Discord webhook: `/ok`, `/slow`, `/fail`; logs JSON lines |
| `connect_proxy.py` | logging HTTPS CONNECT proxy, shows whether the daemon contacts the network |
| `renderplist/` | prints the plist `service install` would write, without loading it |
| `phone_many.mjs` | paired simulated phone (WebCrypto P-256) that answers every held request, survives resyncs |
| `soak.py` | load driver: shim processes, SIGSTOP window, SSE clients, 1 Hz RSS/CPU/threads/fds/DB sampling |
| `attach_check.py` | attaches in a real PTY, waits for output, detaches with ctrl+\ |

Notes
- Run from a normal terminal or under an agent: guarded calls (`away`, `/v1/pair`) go through `detached`.
- `notify.sh` sends ~60 messages to a random ntfy.sh topic and hits the public rate limit on purpose.
- `launchd.sh` always boots out `dev.pilothouse.qa3` on exit (trap).
- The scripts print `PASS`/`FAIL` lines and `FAILS=<n>`; outputs land in `/tmp/qa3/<suite>/`.
