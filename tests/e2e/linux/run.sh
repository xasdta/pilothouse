#!/usr/bin/env bash
# Pilothouse Linux end-to-end suite.
#
# Runs on a fresh Ubuntu box (GitHub Actions ubuntu-latest / ubuntu-24.04-arm).
# Everything lives in an isolated PILOTHOUSE_HOME / PILOTHOUSE_USER_HOME /
# PILOTHOUSE_RUN_DIR under a temp dir; the only thing that touches the real
# user's home is the optional systemd --user check, which runs only when
# PH_E2E_SYSTEMD=1 (set by CI on a throwaway runner).
#
# Environment:
#   PH_E2E_SUBJECT   release (default): drive the binary from the real installer
#                    source: drive the binary built from this checkout
#   PH_E2E_BIN_DIR   where binaries go (default: $WORK/bin). Must not be under
#                    /tmp if you want `doctor`'s binary check to pass.
#   PH_E2E_VERSION   version the installer should fetch (default: latest)
#   PH_E2E_SYSTEMD   1 = install the real systemd --user service (CI only)
#   PH_E2E_KEEP      1 = keep the work dir
#
# Needs: bash, curl, jq, git, node >= 20, go (for the source build), script(1).
set -u
set -o pipefail

REPO=$(cd "$(dirname "$0")/../../.." && pwd)
HERE="$REPO/tests/e2e/linux"
FIX_CLAUDE="$REPO/internal/adapters/claude/testdata/hooks"
FIX_CODEX="$REPO/internal/adapters/codex/testdata/hooks"

WORK=$(mktemp -d /tmp/phe2e.XXXXXX) # short: Unix socket paths
BIN_DIR=${PH_E2E_BIN_DIR:-$WORK/bin}
SUBJECT=${PH_E2E_SUBJECT:-release}
mkdir -p "$BIN_DIR" "$WORK/logs"

export PILOTHOUSE_HOME="$WORK/home"
export PILOTHOUSE_USER_HOME="$WORK/uhome"
export PILOTHOUSE_RUN_DIR="$WORK/run"
export PILOTHOUSE_NO_UPDATE_CHECK=1
# Headless: no X, no Wayland, no session bus → presence detector "none".
unset DISPLAY WAYLAND_DISPLAY DBUS_SESSION_BUS_ADDRESS XDG_SESSION_TYPE
mkdir -p "$PILOTHOUSE_HOME" "$PILOTHOUSE_USER_HOME" "$PILOTHOUSE_RUN_DIR"

PORT=$((20000 + RANDOM % 20000))
BASE="http://127.0.0.1:$PORT"
SOCK="$PILOTHOUSE_RUN_DIR/pilothouse.sock"
PROJ="$WORK/proj"
RESULTS="$WORK/results.tsv"
: >"$RESULTS"
PH="" # binary under test, set in the install step

# ------------------------------------------------------------------ harness

CUR=""
CUR_FAIL=0
CUR_NOTE=""
section() { printf '\n\033[1m=== %s\033[0m\n' "$*"; }
begin() {
	CUR=$1
	CUR_FAIL=0
	CUR_NOTE=""
	printf -- '--- %s\n' "$CUR"
}
ok() { printf '    ok   %s\n' "$*"; }
bad() {
	printf '    FAIL %s\n' "$*"
	CUR_FAIL=1
	[ -z "$CUR_NOTE" ] && CUR_NOTE=$*
}
note() { printf '    ..   %s\n' "$*"; }
end() {
	if [ "$CUR_FAIL" = 0 ]; then
		printf '%s\tPASS\t%s\n' "$CUR" "${1:-}" >>"$RESULTS"
	else
		printf '%s\tFAIL\t%s\n' "$CUR" "$CUR_NOTE" >>"$RESULTS"
	fi
}
skip() {
	printf '    SKIP %s\n' "$2"
	printf '%s\tSKIP\t%s\n' "$1" "$2" >>"$RESULTS"
}
# expect_eq what got want
expect_eq() {
	if [ "$2" = "$3" ]; then ok "$1 = $3"; else bad "$1: want '$3', got '$2'"; fi
}
# expect_match what text regex
expect_match() {
	if printf '%s' "$2" | grep -Eq -- "$3"; then ok "$1 ~ /$3/"; else
		bad "$1: want /$3/, got '$(printf '%s' "$2" | head -c 400)'"
	fi
}
expect_nomatch() {
	if printf '%s' "$2" | grep -Eq -- "$3"; then bad "$1: must not match /$3/, got '$(printf '%s' "$2" | head -c 300)'"; else ok "$1 !~ /$3/"; fi
}

# sock METHOD PATH [BODY] — call the daemon's Unix-socket API.
sock() {
	if [ $# -ge 3 ]; then
		curl -sS --max-time 10 --unix-socket "$SOCK" -X "$1" -H 'Content-Type: application/json' -d "$3" "http://pilothouse$2"
	else
		curl -sS --max-time 10 --unix-socket "$SOCK" -X "$1" "http://pilothouse$2"
	fi
}
now_ms() { date +%s%3N; }

# payload FIXTURE SESSION_ID [jq-filter] — fixture with our cwd/session id.
payload() {
	jq -c --arg sid "$2" --arg cwd "$PROJ" \
		'.session_id=$sid | (if has("cwd") then .cwd=$cwd else . end) | (if has("transcript_path") then .transcript_path=null else . end)'"${3:+ | $3}" "$1"
}

# hook AGENT JSON → runs the hook command exactly as installed in the agent's
# settings file; sets HOOK_OUT, HOOK_ERR, HOOK_RC, HOOK_MS.
hook() {
	local agent=$1 json=$2 cmd t0
	if [ "$agent" = claude ]; then cmd=$CLAUDE_HOOK_CMD; else cmd=$CODEX_HOOK_CMD; fi
	t0=$(now_ms)
	HOOK_OUT=$(printf '%s' "$json" | sh -c "$cmd" 2>"$WORK/hook.err")
	HOOK_RC=$?
	HOOK_MS=$(($(now_ms) - t0))
	HOOK_ERR=$(cat "$WORK/hook.err")
}
# hook_bg AGENT JSON NAME → background hook; output in $WORK/NAME.{out,err,rc}
hook_bg() {
	local agent=$1 json=$2 name=$3 cmd
	if [ "$agent" = claude ]; then cmd=$CLAUDE_HOOK_CMD; else cmd=$CODEX_HOOK_CMD; fi
	(
		t0=$(now_ms)
		printf '%s' "$json" | sh -c "$cmd" >"$WORK/$name.out" 2>"$WORK/$name.err"
		echo $? >"$WORK/$name.rc"
		echo $(($(now_ms) - t0)) >"$WORK/$name.ms"
	) &
	BG_PID=$!
}
# wait_for SECONDS CMD… — poll until CMD succeeds.
wait_for() {
	local secs=$1 i=0
	shift
	while [ $i -lt $((secs * 5)) ]; do
		"$@" >/dev/null 2>&1 && return 0
		sleep 0.2
		i=$((i + 1))
	done
	return 1
}
open_request() { # prints the newest open/stepped request id for an agent
	"$PH" ls --json 2>/dev/null | jq -r --arg a "$1" '[.requests[] | select(.agent==$a)] | last | .id // empty'
}
has_open_request() { [ -n "$(open_request "$1")" ]; }
req_json() { sock GET "/v1/requests/$1"; }

cleanup() {
	set +e
	[ -n "$PH" ] && "$PH" stop >/dev/null 2>&1
	pkill -f '[_]_holder --id' >/dev/null 2>&1
	pkill -f "$WORK/agents/" >/dev/null 2>&1
	if [ "${PH_E2E_KEEP:-0}" != 1 ]; then rm -rf "$WORK"; else echo "kept $WORK"; fi
}
trap cleanup EXIT

dump_logs() {
	section "logs"
	for f in "$PILOTHOUSE_HOME"/logs/*.log "$PILOTHOUSE_HOME"/daemon.log; do
		[ -f "$f" ] || continue
		echo "----- $f (tail)"
		tail -n 60 "$f"
	done
}

# ------------------------------------------------------------------ fake agents

mkdir -p "$WORK/agents"
# A wrapped "agent": prints OSC 7501 status, echoes each input line, exits
# with code 3 on "quit".
cat >"$WORK/agents/fakeagent" <<'EOF'
#!/usr/bin/env bash
log=${FAKE_LOG:-/dev/null}
printf 'FAKEAGENT READY pid=%s sid=%s\r\n' "$$" "${PILOTHOUSE_SESSION_ID:-none}"
echo "start pid=$$ sid=${PILOTHOUSE_SESSION_ID:-none} pilothouse=${PILOTHOUSE:-}" >>"$log"
printf '\033]7501;state=idle;app=fakeagent\007'
while IFS= read -r line; do
  printf '\033]7501;state=working;app=fakeagent\007'
  echo "GOT:$line" >>"$log"
  printf 'GOT:%s\r\n' "$line"
  [ "$line" = quit ] && exit 3
  printf '\033]7501;state=idle;app=fakeagent\007'
done
echo "eof" >>"$log"
EOF
# A task agent: takes the prompt as its argument, writes a file in its cwd.
cat >"$WORK/agents/fake-task-agent" <<'EOF'
#!/usr/bin/env bash
printf '# task\n\n%s\n' "$1" > TASK_NOTES.md
echo "task agent wrote TASK_NOTES.md in $PWD"
sleep 120
EOF
chmod +x "$WORK/agents/fakeagent" "$WORK/agents/fake-task-agent"
export FAKE_LOG="$WORK/fake-input.log"

# ------------------------------------------------------------------ 1. install

section "install"
begin "install: real installer (curl | sh)"
REL_DIR="$BIN_DIR/release"
inst_out=$(curl -fsSL https://pilothouse.dev/install.sh |
	env PILOTHOUSE_INSTALL_DIR="$REL_DIR" ${PH_E2E_VERSION:+PILOTHOUSE_VERSION=$PH_E2E_VERSION} sh 2>&1)
inst_rc=$?
echo "$inst_out" | sed 's/^/    | /'
expect_eq "installer exit" "$inst_rc" 0
if [ -x "$REL_DIR/pilothouse" ]; then ok "binary at $REL_DIR/pilothouse"; else bad "no binary at $REL_DIR/pilothouse"; fi
expect_match "installer verified checksum" "$inst_out" "checksum ok"
end

begin "install: version of released binary"
if [ -x "$REL_DIR/pilothouse" ]; then
	vj=$("$REL_DIR/pilothouse" version --json 2>&1)
	echo "$vj" | sed 's/^/    | /'
	want_os=linux
	case $(uname -m) in x86_64) want_arch=amd64 ;; aarch64 | arm64) want_arch=arm64 ;; *) want_arch=$(uname -m) ;; esac
	expect_eq "os" "$(echo "$vj" | jq -r .os)" "$want_os"
	expect_eq "arch" "$(echo "$vj" | jq -r .arch)" "$want_arch"
	rel_ver=$(echo "$vj" | jq -r .version)
	if [ -n "${PH_E2E_VERSION:-}" ]; then
		expect_match "version" "$rel_ver" "^v?${PH_E2E_VERSION#v}$"
	else
		latest=$(curl -fsSL ${GITHUB_TOKEN:+-H "Authorization: Bearer $GITHUB_TOKEN"} https://api.github.com/repos/xasdta/pilothouse/releases/latest | jq -r .tag_name)
		expect_match "version matches latest release $latest" "$rel_ver" "^v?${latest#v}$"
	fi
	expect_nomatch "commit stamped" "$(echo "$vj" | jq -r .commit)" "^(none|)$"
	file "$REL_DIR/pilothouse" | sed 's/^/    | /'
	if file "$REL_DIR/pilothouse" | grep -q 'statically linked'; then ok "static binary"; else note "not reported as statically linked"; fi
	end "$rel_ver"
else
	bad "no released binary"
	end
fi

begin "install: build from source"
if command -v go >/dev/null 2>&1; then
	SRC_DIR="$BIN_DIR/source"
	mkdir -p "$SRC_DIR"
	if (cd "$REPO" && CGO_ENABLED=0 go build -o "$SRC_DIR/pilothouse" ./cmd/pilothouse) >"$WORK/logs/build.log" 2>&1; then
		ok "go build"
		sv=$("$SRC_DIR/pilothouse" version 2>&1)
		expect_match "source version" "$sv" "linux/(amd64|arm64)"
	else
		bad "go build failed: $(tail -5 "$WORK/logs/build.log")"
	fi
else
	bad "go not installed"
fi
end

if [ "$SUBJECT" = source ]; then PH="$BIN_DIR/source/pilothouse"; else PH="$REL_DIR/pilothouse"; fi
if [ ! -x "$PH" ]; then
	echo "no binary under test ($PH); aborting"
	exit 1
fi
note "binary under test: $PH ($SUBJECT)"
# `pilothouse` on PATH for the hooks' self-protection tests and for task/run.
PATH="$(dirname "$PH"):$WORK/agents:$PATH"
export PATH

# ------------------------------------------------------------------ 2. doctor (no daemon)

section "doctor / presence (headless)"
begin "doctor: before setup, headless presence fallback"
dj=$("$PH" doctor --json 2>/dev/null)
drc=$?
echo "$dj" | jq -c '.[] | {name,level,detail}' | sed 's/^/    | /'
expect_eq "doctor exit (problems expected)" "$drc" 1
expect_eq "binary check" "$(echo "$dj" | jq -r '.[]|select(.name=="binary")|.level')" ok
expect_eq "daemon check" "$(echo "$dj" | jq -r '.[]|select(.name=="daemon")|.level')" fail
expect_eq "presence level" "$(echo "$dj" | jq -r '.[]|select(.name=="presence")|.level')" warn
expect_match "presence detail" "$(echo "$dj" | jq -r '.[]|select(.name=="presence")|.detail')" "can't be measured"
dt=$("$PH" doctor 2>&1)
expect_match "human doctor output" "$dt" "problem"
end

# ------------------------------------------------------------------ 3. config, policy, daemon

section "daemon"
cat >"$PILOTHOUSE_HOME/config.yaml" <<EOF
web:
  addr: 127.0.0.1:$PORT
presence:
  notify_after: 600s
approvals:
  hold_limit: 12s
notify:
  desktop: false
update_check: false
EOF

begin "policy init (starter policy)"
pi=$("$PH" policy init 2>&1)
echo "$pi" | sed 's/^/    | /'
if [ -f "$PILOTHOUSE_HOME/policy.yaml" ]; then ok "policy.yaml written"; else bad "no policy.yaml"; fi
"$PH" policy check >/dev/null 2>&1
expect_eq "policy check exit" "$?" 0
pt=$("$PH" policy test --tool Bash --command "git push -f origin main" 2>&1)
echo "$pt" | sed 's/^/    | /'
expect_match "policy test force-push" "$pt" "deny"
expect_match "policy test names rule" "$pt" "no-force-push"
pt2=$("$PH" policy test --tool Read --path "$PROJ/.env" 2>&1)
expect_match "policy test .env read" "$pt2" "deny"
end

begin "daemon: start, status, permissions"
st=$("$PH" start 2>&1)
echo "$st" | sed 's/^/    | /'
expect_match "start" "$st" "started|already running"
expect_match "web url" "$st" "127.0.0.1:$PORT"
sj=$("$PH" status --json 2>&1)
expect_eq "running" "$(echo "$sj" | jq -r .running)" true
if [ -S "$SOCK" ]; then ok "socket at $SOCK"; else bad "no socket at $SOCK ($(ls -la "$PILOTHOUSE_RUN_DIR"))"; fi
expect_eq "socket mode" "$(stat -c %a "$SOCK" 2>/dev/null)" 600
expect_eq "home mode" "$(stat -c %a "$PILOTHOUSE_HOME")" 700
# Files the product created (config.yaml above was written by this script).
for f in "$PILOTHOUSE_HOME"/pilothouse.db $(find "$PILOTHOUSE_HOME/keys" -type f 2>/dev/null); do
	[ -f "$f" ] && expect_match "$(basename "$f") mode" "$(stat -c %a "$f")" "^600$"
done
h=$(sock GET /v1/health)
echo "    | $h"
expect_match "health" "$h" '"pid"'
ss -ltnp 2>/dev/null | grep ":$PORT " | sed 's/^/    | /'
if ss -ltn 2>/dev/null | grep -q "127.0.0.1:$PORT "; then ok "web listener on loopback only"; else bad "web listener not on 127.0.0.1:$PORT"; fi
end

begin "presence: headless auto mode → unknown/present"
pj=$(sock GET /v1/presence)
echo "    | $pj"
expect_eq "mode" "$(echo "$pj" | jq -r .mode)" auto
expect_eq "state" "$(echo "$pj" | jq -r .state)" present
expect_match "source" "$(echo "$pj" | jq -r .source)" "^(unknown|none)$"
pa=$("$PH" auto 2>&1)
echo "    | $pa"
expect_match "auto sentence" "$pa" "can't be measured"
end

# ------------------------------------------------------------------ 4. hooks install

section "hooks"
mkdir -p "$PILOTHOUSE_USER_HOME/.claude" "$PILOTHOUSE_USER_HOME/.codex"
cat >"$PILOTHOUSE_USER_HOME/.claude/settings.json" <<'EOF'
{
  "model": "opus",
  "permissions": {"allow": ["Bash(ls:*)"]},
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "echo user-own-hook"}]}
    ]
  }
}
EOF

begin "hooks install: Claude Code (merge, backup)"
hi=$("$PH" hooks install --agent claude 2>&1)
hrc=$?
echo "$hi" | head -3 | sed 's/^/    | /'
expect_eq "exit" "$hrc" 0
CS="$PILOTHOUSE_USER_HOME/.claude/settings.json"
expect_eq "model kept" "$(jq -r .model "$CS")" opus
expect_eq "permissions kept" "$(jq -r '.permissions.allow[0]' "$CS")" 'Bash(ls:*)'
expect_eq "user hook kept" "$(jq -r '[.. | .command? // empty | select(.=="echo user-own-hook")] | length' "$CS")" 1
ncl=$(jq -r '[.. | .command? // empty | select(test("hook claude"))] | length' "$CS")
expect_match "pilothouse hook entries" "$ncl" "^(9|10|11)$"
if ls "$CS".pilothouse-bak-* >/dev/null 2>&1; then ok "backup written"; else bad "no backup next to settings.json"; fi
CLAUDE_HOOK_CMD=$(jq -r '[.hooks.PermissionRequest[].hooks[].command | select(test("hook claude"))][0]' "$CS")
expect_match "hook command is absolute path of binary" "$CLAUDE_HOOK_CMD" "^'?$(dirname "$PH")/pilothouse'? hook claude$"
expect_eq "PermissionRequest timeout" "$(jq -r '[.hooks.PermissionRequest[].hooks[] | select(.command|test("hook claude")) | .timeout][0]' "$CS")" 600
end

begin "hooks install: Codex (hooks.json + trust)"
"$PH" hooks install --agent codex >/dev/null 2>&1
expect_eq "exit" "$?" 0
XH="$PILOTHOUSE_USER_HOME/.codex/hooks.json"
CODEX_HOOK_CMD=$(jq -r '[.hooks.PermissionRequest[].hooks[].command | select(test("hook codex"))][0]' "$XH" 2>/dev/null)
expect_match "codex hook command" "$CODEX_HOOK_CMD" "pilothouse'? hook codex$"
if grep -q trusted_hash "$PILOTHOUSE_USER_HOME/.codex/config.toml" 2>/dev/null; then ok "config.toml trust entries"; else bad "no trust entries in config.toml"; fi
end

begin "hooks status + idempotent reinstall"
hs=$("$PH" hooks status --json 2>&1)
expect_eq "claude current" "$(echo "$hs" | jq -r '.[]|select(.agent=="claude")|.status.current')" true
expect_eq "codex current" "$(echo "$hs" | jq -r '.[]|select(.agent=="codex")|.status.current')" true
nb=$(ls "$CS".pilothouse-bak-* | wc -l)
"$PH" hooks install --agent claude >/dev/null 2>&1
"$PH" hooks install --agent codex >/dev/null 2>&1
expect_eq "no duplicate claude entries" "$(jq -r '[.. | .command? // empty | select(test("hook claude"))] | length' "$CS")" "$ncl"
expect_eq "no new backup on no-op" "$(ls "$CS".pilothouse-bak-* | wc -l)" "$nb"
end

begin "hooks: uninstall restores user settings, reinstall"
"$PH" hooks uninstall --agent claude >/dev/null 2>&1
expect_eq "no pilothouse entries" "$(jq -r '[.. | .command? // empty | select(test("hook claude"))] | length' "$CS")" 0
expect_eq "user hook still there" "$(jq -r '[.. | .command? // empty | select(.=="echo user-own-hook")] | length' "$CS")" 1
"$PH" hooks install --agent claude >/dev/null 2>&1
expect_eq "reinstalled" "$(jq -r '[.. | .command? // empty | select(test("hook claude"))] | length' "$CS")" "$ncl"
end

begin "hooks: binary installed under another name (e.g. ph)"
# Not every install is called `pilothouse` (renamed copies, wrappers).
RH="$WORK/rh"
mkdir -p "$RH/bin" "$RH/.claude"
cp "$PH" "$RH/bin/ph"
PILOTHOUSE_USER_HOME=$RH "$RH/bin/ph" hooks install --agent claude >/dev/null 2>&1
PILOTHOUSE_USER_HOME=$RH "$RH/bin/ph" hooks install --agent claude >/dev/null 2>&1
n1=$(jq -r '[.. | .command? // empty | select(test("hook claude"))] | length' "$RH/.claude/settings.json")
expect_eq "entries after two installs (no duplicates)" "$n1" "$ncl"
rs=$(PILOTHOUSE_USER_HOME=$RH "$RH/bin/ph" hooks status --json --agent claude 2>&1)
expect_eq "status sees the install" "$(echo "$rs" | jq -r '.[0].status.installed')" true
PILOTHOUSE_USER_HOME=$RH "$RH/bin/ph" hooks uninstall --agent claude >/dev/null 2>&1
expect_eq "uninstall removes them" "$(jq -r '[.. | .command? // empty | select(test("hook claude"))] | length' "$RH/.claude/settings.json")" 0
end

# ------------------------------------------------------------------ 5. hooks driven with fixtures

section "observe mode: Claude Code + Codex payloads"
mkdir -p "$PROJ"
(
	cd "$PROJ" && git init -q -b main && git config user.email e2e@example.com && git config user.name e2e &&
		echo hello >README.md && git add . && git commit -qm init
) || echo "git init failed"

CSID="e2e-claude-$(date +%s)-$RANDOM"
XSID="019a3b2c-$(printf '%04x' $RANDOM)-7f10-9a1b-$(date +%s)"

begin "claude: session tracking (SessionStart → working → waiting → exited)"
hook claude "$(payload "$FIX_CLAUDE/session_start.json" "$CSID")"
expect_eq "SessionStart rc" "$HOOK_RC" 0
expect_eq "SessionStart stderr" "$HOOK_ERR" ""
sess=$("$PH" ls --json | jq -c --arg s "$CSID" '.sessions[] | select(.agent_session_id==$s)')
echo "    | $sess" | head -c 600
echo
expect_eq "session agent" "$(echo "$sess" | jq -r .agent)" claude
expect_eq "session mode" "$(echo "$sess" | jq -r .mode)" observed
expect_eq "repo root" "$(echo "$sess" | jq -r .repo_root)" "$PROJ"
expect_eq "branch" "$(echo "$sess" | jq -r .branch)" main
CLAUDE_S=$(echo "$sess" | jq -r .id)
expect_match "session id format" "$CLAUDE_S" '^s_[a-z2-7]{10}$'
hook claude "$(payload "$FIX_CLAUDE/user_prompt_submit.json" "$CSID")"
hook claude "$(payload "$FIX_CLAUDE/pre_tool_bash.json" "$CSID")"
expect_eq "PreToolUse(echo) rc" "$HOOK_RC" 0
expect_nomatch "PreToolUse(echo) not denied" "$HOOK_OUT" '"deny"'
note "pre_tool round trip ${HOOK_MS} ms"
[ "$HOOK_MS" -lt 1500 ] && ok "pre_tool < 1.5 s" || bad "pre_tool took ${HOOK_MS} ms"
expect_eq "state after PreToolUse" "$(sock GET "/v1/sessions/$CLAUDE_S" | jq -r .state)" working
hook claude "$(payload "$FIX_CLAUDE/post_tool_bash.json" "$CSID")"
hook claude "$(payload "$FIX_CLAUDE/stop.json" "$CSID")"
expect_eq "state after Stop" "$(sock GET "/v1/sessions/$CLAUDE_S" | jq -r .state)" waiting
ex=$("$PH" explain "$CLAUDE_S" 2>&1)
expect_match "explain" "$ex" "waiting"
end

begin "claude: policy deny / ask / allow at PreToolUse"
hook claude "$(payload "$FIX_CLAUDE/pre_tool_bash.json" "$CSID" '.tool_input.command="git push --force origin feature"')"
echo "    | $HOOK_OUT"
expect_eq "force-push decision" "$(echo "$HOOK_OUT" | jq -r .hookSpecificOutput.permissionDecision)" deny
expect_match "force-push reason" "$(echo "$HOOK_OUT" | jq -r .hookSpecificOutput.permissionDecisionReason)" "Force-push is blocked"
hook claude "$(payload "$FIX_CLAUDE/pre_tool_bash.json" "$CSID" '.tool_input.command="git push origin main"')"
expect_eq "push-to-main decision" "$(echo "$HOOK_OUT" | jq -r .hookSpecificOutput.permissionDecision)" ask
hook claude "$(payload "$FIX_CLAUDE/pre_tool_bash.json" "$CSID" '.tool_input.command="rm -rf ~/"')"
expect_eq "rm -rf ~ decision" "$(echo "$HOOK_OUT" | jq -r .hookSpecificOutput.permissionDecision)" deny
hook claude "$(payload "$FIX_CLAUDE/pre_tool_read.json" "$CSID" ".tool_input.file_path=\"$PROJ/README.md\"")"
expect_eq "read decision (allow-read-only)" "$(echo "$HOOK_OUT" | jq -r .hookSpecificOutput.permissionDecision)" allow
hook claude "$(payload "$FIX_CLAUDE/pre_tool_read.json" "$CSID" ".tool_input.file_path=\"$PROJ/.env\"")"
expect_eq ".env read decision" "$(echo "$HOOK_OUT" | jq -r .hookSpecificOutput.permissionDecision)" deny
hook claude "$(payload "$FIX_CLAUDE/pre_tool_bash.json" "$CSID" '.tool_input.command="pilothouse approve r_abcdefghij"')"
expect_eq "self-protection (agent runs pilothouse approve)" "$(echo "$HOOK_OUT" | jq -r .hookSpecificOutput.permissionDecision)" ask
hook claude "$(payload "$FIX_CLAUDE/pre_tool_write.json" "$CSID" ".tool_input.file_path=\"$PILOTHOUSE_USER_HOME/.claude/settings.json\"")"
expect_eq "self-protection (edit agent settings)" "$(echo "$HOOK_OUT" | jq -r .hookSpecificOutput.permissionDecision)" ask
ev=$("$PH" log --json -n 200 2>/dev/null | grep -c '"policy.decision"')
[ "$ev" -ge 4 ] && ok "policy.decision events logged ($ev)" || bad "policy.decision events: $ev"
end

begin "codex: session tracking + deny (Codex can't ask)"
hook codex "$(payload "$FIX_CODEX/session_start.json" "$XSID")"
expect_eq "SessionStart rc" "$HOOK_RC" 0
xs=$("$PH" ls --json | jq -c --arg s "$XSID" '.sessions[] | select(.agent_session_id==$s)')
expect_eq "codex session agent" "$(echo "$xs" | jq -r .agent)" codex
CODEX_S=$(echo "$xs" | jq -r .id)
hook codex "$(payload "$FIX_CODEX/pre_tool_bash.json" "$XSID" '.tool_input.command="git push -f origin main"')"
echo "    | $HOOK_OUT"
expect_eq "codex force-push" "$(echo "$HOOK_OUT" | jq -r .hookSpecificOutput.permissionDecision)" deny
hook codex "$(payload "$FIX_CODEX/pre_tool_bash.json" "$XSID")"
expect_eq "codex ask rule → no opinion (exit 0, empty)" "$HOOK_RC/$HOOK_OUT" "0/"
hook codex "$(payload "$FIX_CODEX/pre_tool_apply_patch.json" "$XSID")"
expect_eq "codex apply_patch rc" "$HOOK_RC" 0
hook codex "$(payload "$FIX_CODEX/stop.json" "$XSID")"
expect_eq "codex state after Stop" "$(sock GET "/v1/sessions/$CODEX_S" | jq -r .state)" waiting
expect_match "codex last message" "$(sock GET "/v1/sessions/$CODEX_S" | jq -r .last_message)" "Fixed the test"
end

begin "hooks: unknown events and garbage never break the agent"
hook claude "$(payload "$FIX_CLAUDE/doc_unknown_event.json" "$CSID")"
expect_eq "unknown event rc" "$HOOK_RC/$HOOK_OUT" "0/"
hook codex "$(payload "$FIX_CODEX/unknown_post_model_request.json" "$XSID")"
expect_eq "codex unknown rc" "$HOOK_RC/$HOOK_OUT" "0/"
hook claude 'not json at all {'
expect_eq "garbage rc" "$HOOK_RC/$HOOK_OUT" "0/"
end

# ------------------------------------------------------------------ 6. approvals / presence routing

section "approvals"
begin "routing at the desk (headless auto → terminal, no hold)"
hook claude "$(payload "$FIX_CLAUDE/permission_bash.json" "$CSID")"
expect_eq "rc/out (no opinion → native dialog)" "$HOOK_RC/$HOOK_OUT" "0/"
[ "$HOOK_MS" -lt 2000 ] && ok "returned in ${HOOK_MS} ms" || bad "terminal-routed permission took ${HOOK_MS} ms"
rid=$(open_request claude)
rj=$(req_json "$rid")
expect_eq "route" "$(echo "$rj" | jq -r .route)" terminal
expect_eq "status" "$(echo "$rj" | jq -r .status)" stepped_aside
expect_eq "session blocked" "$(sock GET "/v1/sessions/$CLAUDE_S" | jq -r '.state + "/" + .block_kind')" "blocked/permission"
# Claude Code's PostToolUse for the same call closes it as answered locally.
hook claude "$(payload "$FIX_CLAUDE/post_tool_bash.json" "$CSID" '.tool_input.command="echo hi > out.txt"')"
expect_eq "after PostToolUse" "$(req_json "$rid" | jq -r .status)" answered_locally
end

begin "pair a device over the Unix socket + web auth"
pj=$(sock POST /v1/pair '{}')
echo "    | $pj"
CODE=$(echo "$pj" | jq -r .code)
expect_match "pair code" "$CODE" '^[A-Z0-9-]{8,}$'
expect_match "pair url" "$(echo "$pj" | jq -r .url)" "^$BASE/pair#"
PST="$WORK/phone.json"
po=$(node "$HERE/phone.mjs" pair "$BASE" "$PST" "$CODE" 2>&1)
echo "    | $po"
expect_eq "pair status" "$(echo "$po" | jq -r .pair_status)" 200
expect_eq "cookie" "$(echo "$po" | jq -r .cookie_set)" true
expect_eq "code is single use" "$(echo "$po" | jq -r .reuse_status)" 401
dv=$("$PH" devices --json 2>&1)
expect_eq "devices listed" "$(echo "$dv" | jq -r 'length')" 1
pr=$(node "$HERE/phone.mjs" probe "$BASE" "$PST" 2>&1)
echo "    | $pr"
expect_eq "snapshot without cookie" "$(echo "$pr" | jq -r .snapshot_nocookie)" 401
expect_eq "snapshot with cookie" "$(echo "$pr" | jq -r .snapshot_cookie)" 200
expect_match "decide without CSRF header refused" "$(echo "$pr" | jq -r .decide_nocsrf)" '^40[0-9]$'
expect_match "CSP header" "$(echo "$pr" | jq -r .csp)" "default-src 'self'"
expect_eq "X-Frame-Options" "$(echo "$pr" | jq -r .xfo)" DENY
# DNS-rebinding defense (node's fetch can't override Host, so use curl).
ck=$(jq -r .cookie "$PST")
eh=$(curl -s -o /dev/null -w '%{http_code}' -H "Host: evil.example:$PORT" -H "Cookie: $ck" "$BASE/api/snapshot")
expect_match "foreign Host refused" "$eh" '^4[0-9][0-9]$'
lh=$(curl -s -o /dev/null -w '%{http_code}' -H "Host: localhost:$PORT" -H "Cookie: $ck" "$BASE/api/snapshot")
expect_eq "localhost Host accepted" "$lh" 200
end

begin "away: held request answered by a signed device decision (claude, allow)"
pa=$("$PH" away 2>&1)
echo "    | $pa"
expect_match "away sentence" "$pa" "away"
expect_eq "presence state" "$(sock GET /v1/presence | jq -r .state)" away
hook_bg claude "$(payload "$FIX_CLAUDE/permission_bash.json" "$CSID" '.tool_input.command="make deploy-staging"')" held1
ph1=$(node "$HERE/phone.mjs" decide "$BASE" "$PST" allow once "" 20 2>&1)
echo "    | $ph1"
wait "$BG_PID"
o1=$(cat "$WORK/held1.out")
echo "    | hook out: $o1 (rc $(cat "$WORK/held1.rc"), $(cat "$WORK/held1.ms") ms)"
expect_eq "phone saw request" "$(echo "$ph1" | jq -r .request.agent)" claude
expect_eq "decide" "$(echo "$ph1" | jq -r .decide.status)" 200
expect_match "decided by device" "$(echo "$ph1" | jq -r .decide.by)" '^device:d_'
expect_eq "bad signature" "$(echo "$ph1" | jq -r '"\(.badsig.status)/\(.badsig.code)"')" "403/bad_signature"
expect_eq "stale timestamp" "$(echo "$ph1" | jq -r '"\(.stale.status)/\(.stale.code)"')" "403/stale_signature"
expect_eq "replay" "$(echo "$ph1" | jq -r '"\(.replay.status)/\(.replay.code)"')" "403/replayed"
expect_eq "second answer" "$(echo "$ph1" | jq -r '"\(.second.status)/\(.second.code)"')" "409/already_decided"
expect_eq "hook rc" "$(cat "$WORK/held1.rc")" 0
expect_eq "hook behavior" "$(echo "$o1" | jq -r .hookSpecificOutput.decision.behavior)" allow
end

begin "away: codex permission denied from the device with a message"
hook_bg codex "$(payload "$FIX_CODEX/permission_bash.json" "$XSID")" held2
ph2=$(node "$HERE/phone.mjs" decide "$BASE" "$PST" deny once "use the lockfile" 20 2>&1)
echo "    | $ph2"
wait "$BG_PID"
o2=$(cat "$WORK/held2.out")
echo "    | hook out: $o2"
expect_eq "phone saw codex request" "$(echo "$ph2" | jq -r .request.agent)" codex
expect_eq "codex behavior" "$(echo "$o2" | jq -r .hookSpecificOutput.decision.behavior)" deny
expect_match "codex message" "$(echo "$o2" | jq -r .hookSpecificOutput.decision.message)" "use the lockfile"
end

begin "away: allow for session → grant applies at PreToolUse"
hook_bg claude "$(payload "$FIX_CLAUDE/permission_bash.json" "$CSID" '.tool_input.command="make build"')" held3
ph3=$(node "$HERE/phone.mjs" decide "$BASE" "$PST" allow session "" 20 2>&1)
wait "$BG_PID"
expect_eq "decide (session)" "$(echo "$ph3" | jq -r .decide.status)" 200
expect_eq "hook behavior" "$(jq -r .hookSpecificOutput.decision.behavior "$WORK/held3.out" 2>/dev/null)" allow
hook claude "$(payload "$FIX_CLAUDE/pre_tool_bash.json" "$CSID" '.tool_input.command="make build -j8"')"
expect_eq "grant → allow" "$(echo "$HOOK_OUT" | jq -r .hookSpecificOutput.permissionDecision)" allow
hook claude "$(payload "$FIX_CLAUDE/pre_tool_bash.json" "$CSID" '.tool_input.command="make build && git push --force"')"
expect_eq "deny still beats grant" "$(echo "$HOOK_OUT" | jq -r .hookSpecificOutput.permissionDecision)" deny
end

begin "away: approve from the CLI (local socket)"
hook_bg claude "$(payload "$FIX_CLAUDE/permission_write.json" "$CSID" ".tool_input.file_path=\"$PROJ/notes.txt\"")" held4
wait_for 10 has_open_request claude
rid=$(open_request claude)
ap=$("$PH" approve "$rid" 2>&1)
echo "    | $ap"
wait "$BG_PID"
expect_eq "hook behavior" "$(jq -r .hookSpecificOutput.decision.behavior "$WORK/held4.out" 2>/dev/null)" allow
expect_eq "decided by" "$(req_json "$rid" | jq -r .decision.by)" cli
hook_bg claude "$(payload "$FIX_CLAUDE/permission_bash.json" "$CSID" '.tool_input.command="make clean"')" held5
wait_for 10 has_open_request claude
rid=$(open_request claude)
"$PH" deny "$rid" -m "not now" >/dev/null 2>&1
wait "$BG_PID"
expect_eq "deny behavior" "$(jq -r .hookSpecificOutput.decision.behavior "$WORK/held5.out" 2>/dev/null)" deny
expect_match "deny message" "$(jq -r .hookSpecificOutput.decision.message "$WORK/held5.out" 2>/dev/null)" "not now"
end

begin "away: unanswered request expires at hold_limit (12 s) → terminal"
hook_bg claude "$(payload "$FIX_CLAUDE/permission_bash.json" "$CSID" '.tool_input.command="make expire"')" held6
wait_for 10 has_open_request claude
rid=$(open_request claude)
wait "$BG_PID"
ms=$(cat "$WORK/held6.ms")
note "held for $ms ms"
expect_eq "rc/out (no opinion)" "$(cat "$WORK/held6.rc")/$(cat "$WORK/held6.out")" "0/"
[ "$ms" -ge 10000 ] && [ "$ms" -le 20000 ] && ok "held ~12 s" || bad "held $ms ms, want ~12000"
expect_eq "status" "$(req_json "$rid" | jq -r .status)" expired
end

begin "presence: \`here\` releases a held request at once"
hook_bg claude "$(payload "$FIX_CLAUDE/permission_bash.json" "$CSID" '.tool_input.command="make release"')" held7
wait_for 10 has_open_request claude
rid=$(open_request claude)
sleep 1
ph=$("$PH" here 2>&1)
echo "    | $ph"
expect_match "here sentence" "$ph" "Presence: here"
wait "$BG_PID"
ms=$(cat "$WORK/held7.ms")
expect_eq "rc/out (no opinion)" "$(cat "$WORK/held7.rc")/$(cat "$WORK/held7.out")" "0/"
[ "$ms" -lt 6000 ] && ok "released after $ms ms" || bad "released only after $ms ms"
expect_eq "status" "$(req_json "$rid" | jq -r .status)" stepped_aside
expect_eq "presence state" "$(sock GET /v1/presence | jq -r '.state + "/" + .mode')" "present/here"
hook claude "$(payload "$FIX_CLAUDE/permission_bash.json" "$CSID" '.tool_input.command="make here"')"
expect_eq "here → not held" "$HOOK_RC/$HOOK_OUT" "0/"
"$PH" auto >/dev/null 2>&1
hook claude "$(payload "$FIX_CLAUDE/stop.json" "$CSID")"
end

# ------------------------------------------------------------------ 7. wrap mode

section "wrap mode"
begin "run -d: fake agent in a PTY holder"
WS=$(cd "$PROJ" && "$PH" run -d -n fake1 -- fakeagent 2>"$WORK/run1.err")
echo "    | $WS / $(cat "$WORK/run1.err")"
expect_match "session id" "$WS" '^s_[a-z2-7]{10}$'
wait_for 10 grep -q "sid=$WS" "$FAKE_LOG"
expect_match "agent saw PILOTHOUSE_SESSION_ID" "$(cat "$FAKE_LOG" 2>/dev/null)" "sid=$WS pilothouse=1"
lj=$("$PH" ls --json)
expect_eq "wrapped mode" "$(echo "$lj" | jq -r --arg s "$WS" '.sessions[]|select(.id==$s)|.mode')" wrapped
expect_eq "attachable" "$(echo "$lj" | jq -r --arg s "$WS" '.attachable|index($s) != null')" true
if pgrep -f "__holder --id $WS" >/dev/null; then ok "holder process running"; else bad "no holder process"; fi
hp=$(pgrep -f "__holder --id $WS" | head -1)
if [ -n "$hp" ]; then
	expect_eq "holder is its own session leader (setsid)" "$(ps -o sid= -p "$hp" | tr -d ' ')" "$hp"
fi
end

begin "attach via script(1), type, detach with ctrl+\\"
(
	sleep 2
	printf 'hello-from-attach\r'
	sleep 2
	printf '\034'
	sleep 3
) | timeout 25 script -qfec "$PH attach $WS" "$WORK/attach1.typescript" >/dev/null 2>&1
arc=$?
expect_eq "attach exit" "$arc" 0
wait_for 5 grep -q "GOT:hello-from-attach" "$FAKE_LOG"
expect_match "agent got input" "$(cat "$FAKE_LOG")" "GOT:hello-from-attach"
tsc=$(tr -d '\r' <"$WORK/attach1.typescript" 2>/dev/null)
expect_match "terminal showed agent output" "$tsc" "GOT:hello-from-attach"
expect_match "detach message" "$tsc" "detached from"
expect_eq "still attachable after detach" "$("$PH" ls --json | jq -r --arg s "$WS" '.attachable|index($s) != null')" true
st=$(sock GET "/v1/sessions/$WS" | jq -r .state)
expect_match "state from OSC 7501" "$st" "^(waiting|working)$"
end

begin "run (attached) in a PTY, detach"
(
	sleep 2
	printf 'second-session\r'
	sleep 2
	printf '\034'
	sleep 3
) | timeout 25 script -qfec "cd $PROJ && $PH run -n fake2 -- fakeagent" "$WORK/run2.typescript" >/dev/null 2>&1
expect_eq "run exit after detach" "$?" 0
expect_match "fake2 got input" "$(cat "$FAKE_LOG")" "GOT:second-session"
WS2=$("$PH" ls --json | jq -r '.sessions[]|select(.title=="fake2")|.id' | head -1)
expect_match "fake2 session" "$WS2" '^s_'
end

begin "holder survives daemon restart; reattach"
hpid=$(pgrep -f "__holder --id $WS" | head -1)
apid=$(grep "sid=$WS" "$FAKE_LOG" | sed -n 's/.*pid=\([0-9]*\).*/\1/p' | head -1)
"$PH" stop 2>&1 | sed 's/^/    | /'
sleep 1
if [ -n "$hpid" ] && kill -0 "$hpid" 2>/dev/null; then ok "holder alive with daemon down"; else bad "holder died with the daemon"; fi
if [ -n "$apid" ] && kill -0 "$apid" 2>/dev/null; then ok "agent alive with daemon down"; else bad "agent died with the daemon"; fi
# attach works without a daemon (holder probing)
(
	sleep 2
	printf 'while-daemon-down\r'
	sleep 2
	printf '\034'
	sleep 2
) | timeout 25 script -qfec "$PH attach $WS" "$WORK/attach2.typescript" >/dev/null 2>&1
expect_eq "attach with daemon down" "$?" 0
expect_match "input while daemon down" "$(cat "$FAKE_LOG")" "GOT:while-daemon-down"
"$PH" start >/dev/null 2>&1
expect_eq "daemon back" "$("$PH" status --json | jq -r .running)" true
if wait_for 15 sh -c "\"$PH\" ls --json | jq -e --arg s $WS '.attachable|index(\$s) != null'"; then
	ok "session re-registered and attachable"
else
	bad "session not attachable after restart: $("$PH" ls --json | jq -c .attachable)"
fi
expect_match "state after restart" "$(sock GET "/v1/sessions/$WS" | jq -r .state)" "^(waiting|working|starting)$"
(
	sleep 2
	printf 'after-restart\r'
	sleep 2
	printf '\034'
	sleep 2
) | timeout 25 script -qfec "$PH attach $WS" "$WORK/attach3.typescript" >/dev/null 2>&1
expect_match "input after restart" "$(cat "$FAKE_LOG")" "GOT:after-restart"
end

begin "agent exit code propagates through attach; session exited"
(
	sleep 2
	printf 'quit\r'
	sleep 4
) | timeout 25 script -qfec "$PH attach $WS; echo ATTACH_RC=\$?" "$WORK/attach4.typescript" >/dev/null 2>&1
tsc=$(tr -d '\r' <"$WORK/attach4.typescript")
expect_match "attach exit = agent exit (3)" "$tsc" "ATTACH_RC=3"
if wait_for 10 sh -c "[ \"\$(curl -s --unix-socket $SOCK http://x/v1/sessions/$WS | jq -r .state)\" = exited ]"; then ok "state exited"; else bad "state: $(sock GET "/v1/sessions/$WS" | jq -r .state)"; fi
expect_eq "exit code recorded" "$(sock GET "/v1/sessions/$WS" | jq -r .exit_code)" 3
end

# ------------------------------------------------------------------ 8. tasks

section "tasks"
begin "task new with a fake agent (worktree + branch + wrapped session)"
to=$(cd "$PROJ" && "$PH" task new --agent fake-task-agent "add task notes please" 2>&1)
echo "$to" | sed 's/^/    | /'
TID=$(echo "$to" | sed -n 's/^task \(t_[a-z0-9]*\).*/\1/p' | head -1)
expect_match "task id" "$TID" '^t_'
WT=$(echo "$to" | sed -n 's/^ *worktree: *//p')
BR=$(echo "$to" | sed -n 's/^ *branch: *\([^ ]*\).*/\1/p')
expect_match "worktree under PILOTHOUSE_HOME" "$WT" "^$PILOTHOUSE_HOME/worktrees/proj/"
expect_match "branch" "$BR" '^pilothouse/'
if git -C "$PROJ" rev-parse --verify -q "$BR" >/dev/null; then ok "branch exists"; else bad "branch $BR missing"; fi
if wait_for 10 test -f "$WT/TASK_NOTES.md"; then ok "agent ran in the worktree"; else bad "agent didn't write TASK_NOTES.md in $WT"; fi
tl=$("$PH" task ls 2>&1)
echo "$tl" | sed 's/^/    | /'
expect_match "task ls" "$tl" "$TID"
td=$("$PH" task diff "$TID" 2>&1)
expect_match "task diff shows the agent's file" "$td" "TASK_NOTES.md"
TS=$(echo "$to" | sed -n 's/^ *session: *//p')
expect_eq "task session linked" "$(sock GET "/v1/sessions/$TS" | jq -r .task_id)" "$TID"
ta=$("$PH" task abandon "$TID" 2>&1)
tarc=$?
echo "$ta" | sed 's/^/    | /'
expect_eq "abandon refused while the agent runs" "$tarc" 1
expect_match "actionable refusal" "$ta" "still running.*--force"
ta=$("$PH" task abandon --force "$TID" 2>&1)
echo "$ta" | sed 's/^/    | /'
expect_match "abandon --force" "$ta" "abandoned"
if wait_for 8 sh -c "! pgrep -f '[_]_holder --id $TS' >/dev/null"; then ok "task agent stopped"; else bad "task holder still running after abandon --force"; fi
if [ ! -d "$WT" ]; then ok "worktree removed"; else bad "worktree still there"; fi
if git -C "$PROJ" rev-parse --verify -q "$BR" >/dev/null; then bad "branch not deleted"; else ok "branch deleted"; fi
end

# ------------------------------------------------------------------ 9. fail-open

section "fail-open"
begin "daemon down → hooks fail open fast"
"$PH" stop >/dev/null 2>&1
hook claude "$(payload "$FIX_CLAUDE/pre_tool_bash.json" "$CSID" '.tool_input.command="git push --force"')"
expect_eq "pre_tool rc/out" "$HOOK_RC/$HOOK_OUT" "0/"
[ "$HOOK_MS" -lt 2500 ] && ok "answered in $HOOK_MS ms" || bad "took $HOOK_MS ms"
hook claude "$(payload "$FIX_CLAUDE/permission_bash.json" "$CSID")"
expect_eq "permission rc/out" "$HOOK_RC/$HOOK_OUT" "0/"
[ "$HOOK_MS" -lt 2500 ] && ok "answered in $HOOK_MS ms" || bad "took $HOOK_MS ms"
hook codex "$(payload "$FIX_CODEX/pre_tool_bash.json" "$XSID")"
expect_eq "codex pre_tool rc/out" "$HOOK_RC/$HOOK_OUT" "0/"
end

# ------------------------------------------------------------------ 10. service

section "systemd --user service"
begin "service install / status / restart / uninstall"
if [ "${PH_E2E_SYSTEMD:-0}" = 1 ] && systemctl --user show-environment >/dev/null 2>&1; then
	# systemd only reads units from the real ~/.config/systemd/user.
	export PILOTHOUSE_USER_HOME="$HOME"
	si=$("$PH" service install 2>&1)
	src=$?
	echo "$si" | sed 's/^/    | /'
	expect_eq "install exit" "$src" 0
	UNIT="$HOME/.config/systemd/user/pilothouse.service"
	if [ -f "$UNIT" ]; then ok "unit written"; else bad "no unit at $UNIT"; fi
	sed 's/^/    | /' "$UNIT" 2>/dev/null
	va=$(systemd-analyze --user verify "$UNIT" 2>&1)
	expect_eq "systemd-analyze verify" "$?" 0
	[ -n "$va" ] && echo "$va" | sed 's/^/    | /'
	expect_eq "is-active" "$(systemctl --user is-active pilothouse.service)" active
	expect_eq "is-enabled" "$(systemctl --user is-enabled pilothouse.service)" enabled
	sj=$("$PH" service status --json 2>&1)
	echo "    | $sj"
	expect_eq "status running" "$(echo "$sj" | jq -r .running)" true
	expect_eq "status current" "$(echo "$sj" | jq -r .current)" true
	expect_eq "daemon answers" "$("$PH" status --json | jq -r .running)" true
	# A wrapped agent started now must survive a service restart (KillMode=process).
	WS3=$(cd "$PROJ" && "$PH" run -d -n fake3 -- fakeagent 2>/dev/null)
	sleep 1
	h3=$(pgrep -f "__holder --id $WS3" | head -1)
	"$PH" service restart >/dev/null 2>&1
	sleep 2
	if [ -n "$h3" ] && kill -0 "$h3" 2>/dev/null; then ok "holder survives service restart"; else bad "holder died on service restart"; fi
	expect_eq "daemon back after restart" "$("$PH" status --json | jq -r .running)" true
	si2=$("$PH" service install 2>&1)
	expect_nomatch "reinstall is a no-op (no rewrite)" "$si2" "wrote "
	dj=$("$PH" doctor --json 2>/dev/null)
	expect_eq "doctor: service" "$(echo "$dj" | jq -r '.[]|select(.name=="service")|.level')" ok
	su=$("$PH" service uninstall 2>&1)
	echo "$su" | sed 's/^/    | /'
	if [ ! -f "$UNIT" ]; then ok "unit removed"; else bad "unit still there"; fi
	expect_match "daemon stopped" "$("$PH" status 2>&1)" "not running"
	pkill -f "__holder --id $WS3" 2>/dev/null
	export PILOTHOUSE_USER_HOME="$WORK/uhome"
	end
else
	note "no systemd --user manager (or PH_E2E_SYSTEMD!=1): checking the generated unit only"
	si=$("$PH" service install 2>&1)
	echo "$si" | sed 's/^/    | /'
	UNIT="$PILOTHOUSE_USER_HOME/.config/systemd/user/pilothouse.service"
	if [ -f "$UNIT" ]; then
		ok "unit written"
		sed 's/^/    | /' "$UNIT"
		if command -v systemd-analyze >/dev/null; then
			va=$(systemd-analyze verify "$UNIT" 2>&1)
			vrc=$?
			echo "$va" | sed 's/^/    | /'
			expect_eq "systemd-analyze verify" "$vrc" 0
		fi
		expect_match "ExecStart" "$(cat "$UNIT")" "^ExecStart=$PH daemon --log-file"
		expect_match "KillMode=process" "$(cat "$UNIT")" "KillMode=process"
	else
		bad "no unit file written"
	fi
	expect_match "actionable error without a user manager" "$si" "systemctl|systemd"
	end
fi

# ------------------------------------------------------------------ 11. doctor with everything

section "doctor (final)"
begin "doctor after setup"
"$PH" start >/dev/null 2>&1
dj=$("$PH" doctor --json 2>/dev/null)
echo "$dj" | jq -c '.[] | {name,level,detail}' | sed 's/^/    | /'
expect_eq "daemon" "$(echo "$dj" | jq -r '.[]|select(.name=="daemon")|.level')" ok
expect_eq "hooks: Claude Code" "$(echo "$dj" | jq -r '.[]|select(.name=="hooks: Claude Code")|.level')" ok
expect_eq "hooks: Codex" "$(echo "$dj" | jq -r '.[]|select(.name=="hooks: Codex")|.level')" ok
"$PH" stop >/dev/null 2>&1
end

# ------------------------------------------------------------------ 12. setup --yes

section "setup --yes (fresh home, real user manager)"
if [ "${PH_E2E_SYSTEMD:-0}" = 1 ] && systemctl --user show-environment >/dev/null 2>&1; then
	begin "setup --yes: hooks + service + daemon in one go"
	(
		# A fresh data dir, the runner's real (throwaway) home, default port.
		export PILOTHOUSE_HOME="$WORK/home2" PILOTHOUSE_RUN_DIR="$WORK/run2" PILOTHOUSE_USER_HOME="$HOME"
		mkdir -p "$HOME/.claude" "$HOME/.codex"
		so=$("$PH" setup --yes </dev/null 2>&1)
		src=$?
		echo "$so" | sed 's/^/    | /'
		[ "$src" = 0 ] && echo "RC_OK" || echo "RC=$src"
		echo "STATUS=$("$PH" status --json | jq -r .running)"
		echo "SERVICE=$(systemctl --user is-active pilothouse.service)"
		echo "CLAUDE=$("$PH" hooks status --json | jq -r '.[]|select(.agent=="claude")|.status.current')"
		echo "CODEX=$("$PH" hooks status --json | jq -r '.[]|select(.agent=="codex")|.status.current')"
		echo "HOMEMODE=$(stat -c %a "$PILOTHOUSE_HOME")"
		[ -f "$PILOTHOUSE_HOME/config.yaml" ] && echo "CONFMODE=$(stat -c %a "$PILOTHOUSE_HOME/config.yaml")"
		so2=$("$PH" setup --yes </dev/null 2>&1)
		echo "RERUN_RC=$?"
		echo "$so2" | sed 's/^/    | (rerun) /'
		"$PH" doctor --json 2>/dev/null | jq -r '.[] | "DOCTOR \(.name)=\(.level)"'
		"$PH" service uninstall >/dev/null 2>&1
		"$PH" hooks uninstall >/dev/null 2>&1
		"$PH" stop >/dev/null 2>&1
	) >"$WORK/setup.out" 2>&1
	sed -n '/^    |/p' "$WORK/setup.out"
	so=$(cat "$WORK/setup.out")
	expect_match "setup exit" "$so" "^RC_OK$"
	expect_match "daemon running" "$so" "^STATUS=true$"
	expect_match "service active" "$so" "^SERVICE=active$"
	expect_match "claude hooks current" "$so" "^CLAUDE=true$"
	expect_match "codex hooks current" "$so" "^CODEX=true$"
	expect_match "data dir 0700" "$so" "^HOMEMODE=700$"
	if printf '%s' "$so" | grep -q '^CONFMODE='; then expect_match "config.yaml 0600" "$so" "^CONFMODE=600$"; fi
	expect_match "setup is re-runnable" "$so" "^RERUN_RC=0$"
	# Headless runner: no notify-send and no paired device in this fresh home,
	# so nothing can actually be delivered.
	if ! command -v notify-send >/dev/null 2>&1; then
		expect_nomatch "setup doesn't claim a test notification it couldn't deliver" "$so" "test notification sent \(desktop"
	fi
	for c in daemon "hooks: Claude Code" "hooks: Codex" service; do
		expect_match "doctor $c" "$so" "^DOCTOR $c=ok$"
	done
	end
else
	skip "setup --yes: hooks + service + daemon in one go" "needs PH_E2E_SYSTEMD=1 and a systemd --user manager"
fi

# ------------------------------------------------------------------ summary

section "processes still running (expected: detached fake agent sessions only)"
pgrep -af '[_]_holder' | sed 's/^/    | /'
FAILS=$(grep -c $'\tFAIL\t' "$RESULTS")
[ "$FAILS" -gt 0 ] && dump_logs
section "summary ($(uname -m), subject=$SUBJECT, $("$PH" version | head -1))"
printf '%-62s %-5s %s\n' TEST RESULT NOTE
printf '%-62s %-5s %s\n' "$(printf '%.0s-' {1..62})" ----- ----
while IFS=$'\t' read -r name res msg; do
	printf '%-62s %-5s %s\n' "$name" "$res" "${msg:0:110}"
done <"$RESULTS"
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
	{
		echo "### Pilothouse Linux e2e — $(uname -m), subject=$SUBJECT"
		echo
		echo "| Test | Result | Note |"
		echo "|---|---|---|"
		while IFS=$'\t' read -r name res msg; do
			printf '| %s | %s | %s |\n' "$name" "$res" "$(printf '%s' "${msg:0:200}" | tr '|' '/')"
		done <"$RESULTS"
	} >>"$GITHUB_STEP_SUMMARY"
fi
echo
echo "$(grep -c $'\tPASS\t' "$RESULTS") passed, $FAILS failed, $(grep -c $'\tSKIP\t' "$RESULTS") skipped"
[ "$FAILS" -eq 0 ]
