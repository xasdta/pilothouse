#!/bin/sh
# Isolated Pilothouse + real Codex CLI + mock model. Never touches ~/.codex,
# ~/.claude or ~/.pilothouse. Usage: E2E_ROOT=/tmp/qa6 sh setup.sh
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
REPO_ROOT=$(cd "$HERE/../../.." && pwd)
mkdir -p "${E2E_ROOT:-/tmp/qa6}"
ROOT=$(cd "${E2E_ROOT:-/tmp/qa6}" && pwd -P)   # canonical: Codex canonicalizes CODEX_HOME in hook trust keys
PORT=${MOCK_PORT:-7597}
export PILOTHOUSE_HOME=$ROOT/home PILOTHOUSE_USER_HOME=$ROOT/uhome PILOTHOUSE_RUN_DIR=$ROOT/run \
       PILOTHOUSE_NO_UPDATE_CHECK=1 CODEX_HOME=$ROOT/uhome/.codex HOME=$ROOT/uhome
mkdir -p "$ROOT/npm" "$ROOT/home" "$ROOT/run" "$ROOT/uhome/.codex" "$ROOT/mock" "$ROOT/repo"

[ -x "$ROOT/npm/node_modules/.bin/codex" ] || npm i --prefix "$ROOT/npm" @openai/codex >/dev/null
"$ROOT/npm/node_modules/.bin/codex" --version
(cd "$REPO_ROOT" && go build -o "$ROOT/pilothouse" ./cmd/pilothouse)
[ -d "$ROOT/venv" ] || { python3 -m venv "$ROOT/venv" && "$ROOT/venv/bin/pip" -q install pyte; }

[ -d "$ROOT/repo/.git" ] || (cd "$ROOT/repo" && git init -q -b main && git -c user.name=qa -c user.email=qa@example.invalid commit -q --allow-empty -m init)

cat > "$ROOT/uhome/.codex/config.toml" <<EOF
model = "mock-model"
model_provider = "mock"
check_for_update_on_startup = false

[model_providers.mock]
name = "Mock"
base_url = "http://127.0.0.1:$PORT/v1"
env_key = "MOCK_API_KEY"
wire_api = "responses"

[analytics]
enabled = false

[features]
plugins = false
remote_plugin = false
apps = false
in_app_updates = false

[projects."$ROOT/repo"]
trust_level = "trusted"
EOF

cat > "$ROOT/home/config.yaml" <<EOF
web:
  addr: 127.0.0.1:${WEB_PORT:-7596}
notify:
  desktop: false
update_check: false
EOF

nohup python3 "$HERE/mock_responses.py" "$PORT" "$ROOT/mock" > "$ROOT/mock/server.out" 2>&1 &
echo $! > "$ROOT/mock/pid"
"$ROOT/pilothouse" start
"$ROOT/pilothouse" hooks install --agent codex | grep -v '^    '
"$ROOT/pilothouse" hooks status

# Pair the simulated phone. /v1/pair is refused for callers inside an agent's
# process tree, so call it double-forked.
"$ROOT/venv/bin/python" "$HERE/../claude/detach.py" curl -s -X POST --unix-socket "$ROOT/run/pilothouse.sock" http://x/v1/pair > "$ROOT/pair.json"
CODE=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["code"])' "$ROOT/pair.json")
node "$HERE/../claude/phone.mjs" pair "http://127.0.0.1:${WEB_PORT:-7596}" "$CODE" "$ROOT/phone.json"
