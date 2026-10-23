# End-to-end tests with the real Claude Code CLI

Manual/QA scripts that drive a real, interactive `claude` (haiku) in a PTY
against an isolated Pilothouse daemon. They cost real model calls (~12 short
sessions for the whole set) and are not part of `go test`.

## Setup (once)

```sh
R=/tmp/qa1                                   # or export E2E_ROOT to use another dir
mkdir -p $R/{home,uhome,run,bin}
go build -o $R/pilothouse ./cmd/pilothouse
python3 -m venv $R/venv && $R/venv/bin/pip install pyte
export PILOTHOUSE_HOME=$R/home PILOTHOUSE_USER_HOME=$R/uhome PILOTHOUSE_RUN_DIR=$R/run PILOTHOUSE_NO_UPDATE_CHECK=1
printf 'web:\n  addr: 127.0.0.1:7591\nnotify:\n  desktop: false\nupdate_check: false\n' > $R/home/config.yaml
$R/pilothouse hooks install                  # writes only $R/uhome/.claude/settings.json
$R/pilothouse start
# pair the simulated phone (pair must run outside any agent process tree)
CODE=$(./detach.py $R/pilothouse pair --json --no-wait --no-qr | python3 -c 'import json,sys;print(json.load(sys.stdin)["code"])')
node phone.mjs pair http://127.0.0.1:7591 $CODE $R/phone.json
# for s5 (tasks): a `claude` wrapper first on PATH so the task's agent uses the isolated hooks
printf '#!/bin/sh\nexec claude --settings %s/uhome/.claude/settings.json --model haiku --permission-mode default "$@"\n' $R > $R/bin/claude   # use the absolute path of the real claude
chmod +x $R/bin/claude
```

s3 needs these rules in `$R/home/policy.yaml`:

```yaml
version: 1
rules:
  - { name: no-forbidden-commits, match: { kind: shell, command: "git *" }, when: 'command.contains("forbidden")', action: deny, message: Commits mentioning forbidden are blocked by QA policy. }
  - { name: ask-askme, match: { kind: shell, command: "git *" }, when: 'command.contains("askme")', action: ask }
```

## Run

```sh
cd tests/e2e/claude
$R/venv/bin/python s1_desk.py      # here: step aside, native dialog, answered_locally; deny closes on next prompt
$R/venv/bin/python s2_away.py      # away: held + phone allow/deny; answer locally while held; here while held; hold_limit expiry
$R/venv/bin/python s3_grant.py     # allow-for-session grant; deny/ask rules beat it; chained commands don't ride on it
$R/venv/bin/python s4_wrap.py      # pilothouse run: OSC 7501, detach/attach, daemon kill/restart, exit codes
$R/venv/bin/python s5_task.py      # task new / ls / diff / done --keep / abandon (always_hold + phone)
$R/venv/bin/python s6_observe.py   # TUI shows held request live and answers with y; CLI approve/deny; guard
```

Each prints `[e2e] RESULTS {...}` at the end, plus `pilothouse log`/`explain` output.

## Files

- `ptyterm.py` – PTY + `pyte` virtual screen; `wait(regex)`, `send`, `type`.
- `common.py` – env isolation (strips the parent's `CLAUDECODE`/`CLAUDE_*` vars), `ph()`, `ph_person()`, `claude()` launcher (`--permission-mode default` so dialogs appear even if the user's default mode is auto), folder-trust handling.
- `detach.py` – runs a command double-forked so it is not a descendant of an agent. Pilothouse refuses `approve/deny/here/away/pair/stop` from inside an agent's process tree by design; a harness running under Claude Code acts as "the person" through this.
- `phone.mjs` – simulated paired phone (WebCrypto P-256, signed decisions, replay check).
- `tui_driver.py` – drives the inbox TUI in a PTY (run via `detach.py`).

Isolation: never run these without the `PILOTHOUSE_*` variables; `claude` is
started with `--settings $R/uhome/.claude/settings.json`, so the real
`~/.claude/settings.json` is not modified (Claude Code still records the
folder-trust answer and transcripts under the real `~/.claude*`, as any run does).
