#!/bin/sh
# Stops everything setup.sh and the scenarios started. Leaves $E2E_ROOT on disk.
ROOT=$(cd "${E2E_ROOT:-/tmp/qa6}" && pwd -P)
export PILOTHOUSE_HOME=$ROOT/home PILOTHOUSE_USER_HOME=$ROOT/uhome PILOTHOUSE_RUN_DIR=$ROOT/run PILOTHOUSE_NO_UPDATE_CHECK=1 CODEX_HOME=$ROOT/uhome/.codex
"$ROOT/pilothouse" hooks uninstall --agent codex | grep -v '^    '
"$ROOT/venv/bin/python" "$(dirname "$0")/../claude/detach.py" "$ROOT/pilothouse" stop
pkill -f "$ROOT/uhome/.codex/packages/app-server-daemon" 2>/dev/null   # Codex's shared app-server daemon
pkill -f "$ROOT/npm/node_modules" 2>/dev/null
[ -f "$ROOT/mock/pid" ] && kill "$(cat "$ROOT/mock/pid")" 2>/dev/null
true
