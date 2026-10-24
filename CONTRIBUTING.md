# Contributing

Thanks for helping. Bug reports with exact steps are as valuable as code.

## Develop

```sh
go build -o pilothouse ./cmd/pilothouse   # Go 1.27+, no cgo
go test -race ./...
gofmt -l . && go vet ./... && staticcheck ./...
```

Tests must never touch your real `~/.claude`, `~/.codex` or `~/.pilothouse`:
set `PILOTHOUSE_HOME`, `PILOTHOUSE_USER_HOME` and a short `PILOTHOUSE_RUN_DIR`
(`/tmp/...`, Unix socket paths are limited to ~104 bytes) with `t.Setenv`.

To try a change against a real agent without touching your setup, run a daemon
with those variables pointing at a temp dir, `pilothouse hooks install` into the
temp user home, and start Claude Code with `claude --settings <temp>/.claude/settings.json`.

## Web app

The phone/desktop app is plain ES modules in `internal/web/static` (no build
step; strict CSP, so no inline scripts or styles). Work on it against the demo
core, which has believable agents, requests, rules and tasks:

```sh
go run -tags devserver ./internal/web/devserver -addr 127.0.0.1:7600 [-live]
```

Open the login link it prints. End-to-end checks run the same devserver in
headless Chrome (they never touch your own browser profile):

```sh
cd tests/e2e/web && npm ci
node 10-demo-cards.mjs && node 11-demo-shell.mjs && node 12-demo-screens.mjs
```

`PH_TMP` and `PH_SHOTS` choose where temp files and screenshots go.

## Layout

`docs/BUILD-SPEC.md` describes the architecture; `docs/AGENTS.md` what each
agent supports; `docs/POLICY.md` the rules engine.

## Pull requests

- One change per PR, with tests. Keep the hook path fast: policy evaluation
  must stay well under a millisecond.
- Explain *why* in comments, not *what*.
- Fail open: if Pilothouse breaks, agents must behave as if it weren't installed
  (unless the user set `approvals.fail_closed`).
