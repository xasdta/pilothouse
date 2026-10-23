#!/usr/bin/env bash
# Robustness: shim fuzzing (invalid/oversized/binary/never-closing stdin,
# unknown events/agents), oversized-payload policy bypass, daemon SIGKILL
# mid-hold, corrupt policy (hot and at start), corrupt config, DB locked by
# another process, malformed local-API bodies, and (optional, DISKFULL=1) a
# full disk on a RAM volume.
here=$(cd "$(dirname "$0")" && pwd)
source "$here/common.sh"
unset PILOTHOUSE_SESSION_ID
R=$QA/rob; rm -rf "$R"; mkdir -p "$R"
pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; FAILS=$((${FAILS:-0}+1)); }
now_ms() { python3 -c 'import time;print(int(time.time()*1000))'; }
trap 'stop_daemon' EXIT
stop_daemon
mkdir -p "$QA/repo"

cat >"$PILOTHOUSE_HOME/policy.yaml" <<'EOF'
version: 1
rules:
  - name: no-secrets
    match: { path: "**/.env*" }
    action: deny
  - name: no-force-push
    match: { kind: shell, command: "git push*" }
    when: 'command.matches("(--force|-f)\\b")'
    action: deny
EOF
write_config 'notify:' '  desktop: false' 'update_check: false' 'approvals: {hold_limit: 60s}'
start_daemon "$R/daemon.log" || exit 1

# shim <name> <stdin-file> [args…] → prints "rc ms stdout-bytes"
shim() {
  local name=$1 in=$2; shift 2
  local t0 t1; t0=$(now_ms)
  ( cd "$QA/repo" && "$PH" hook claude "$@" <"$in" >"$R/$name.out" 2>"$R/$name.err" ); local rc=$?
  t1=$(now_ms)
  echo "$rc $((t1-t0)) $(wc -c <"$R/$name.out" | tr -d ' ')"
}
noop() { # expect exit 0, no stdout, under $2 ms
  read -r rc ms bytes <<<"$3"
  if [ "$rc" = 0 ] && [ "$bytes" = 0 ] && [ "$ms" -lt "$2" ]; then pass "$1 → no opinion, rc 0, ${ms} ms"; else fail "$1 → rc=$rc bytes=$bytes ms=$ms err=$(head -c 200 "$R/$1.err")"; fi
}

# ---- fuzz stdin ----
printf '{not json' >"$R/in.invalid";            noop invalid 1000 "$(shim invalid "$R/in.invalid")"
: >"$R/in.empty";                                noop empty 1000 "$(shim empty "$R/in.empty")"
head -c 65536 /dev/urandom >"$R/in.binary";      noop binary 1000 "$(shim binary "$R/in.binary")"
python3 -c 'print("["*200000 + "]"*200000)' >"$R/in.deep"; noop deep-nesting 2000 "$(shim deep-nesting "$R/in.deep")"
printf '{"hook_event_name":"FutureEvent","session_id":"rob-1","cwd":"/tmp"}' >"$R/in.unknown"; noop unknown-event 1000 "$(shim unknown-event "$R/in.unknown")"
printf '{"session_id":"rob-1"}' >"$R/in.noevent";  noop no-event-name 1000 "$(shim no-event-name "$R/in.noevent")"
printf '[1,2,3]' >"$R/in.array";                 noop json-array 1000 "$(shim json-array "$R/in.array")"
printf '{"hook_event_name":"PreToolUse","session_id":"rob-1","tool_name":"Bash","tool_input":{"command":"ls"}} trailing' >"$R/in.trailing"; noop trailing-garbage 1000 "$(shim trailing-garbage "$R/in.trailing")"
printf '{"hook_event_name":"PreToolUse","session_id":"rob-1","tool_name":"Bash","tool_input":"notanobject"}' >"$R/in.strinput"; noop tool_input-string 1000 "$(shim tool_input-string "$R/in.strinput")"
printf '{"hook_event_name":"PreToolUse","session_id":"rob-1","tool_name":"Bash","tool_input":{"command":"ls \\u0000 \\ud800 é"}}' >"$R/in.nul"; noop nul-and-bad-surrogate 1000 "$(shim nul-and-bad-surrogate "$R/in.nul")"
r=$(cd "$QA/repo" && "$PH" hook nosuchagent <"$R/in.invalid"; echo "rc=$?"); [ "$r" = "rc=0" ] && pass "unknown agent → rc 0, silent" || fail "unknown agent: $r"
r=$( (cd "$QA/repo" && "$PH" hook </dev/null; echo "rc=$?") 2>&1); [ "$r" = "rc=0" ] && pass "no agent arg → rc 0" || fail "no agent arg: $r"
t0=$(now_ms); (sleep 10 | (cd "$QA/repo" && "$PH" hook claude >"$R/hang.out")) & sp=$!
while kill -0 $sp 2>/dev/null && [ $(( $(now_ms) - t0 )) -lt 9000 ]; do
  pgrep -f "hook claude" -P $sp >/dev/null 2>&1 || true
  [ -z "$(pgrep -f "$PH hook claude")" ] && break; sleep 0.05
done
el=$(( $(now_ms) - t0 )); pkill -f "sleep 10" 2>/dev/null; wait $sp 2>/dev/null
[ "$el" -lt 3000 ] && pass "stdin never closes → shim gives up after ${el} ms" || fail "stdin never closes: shim took ${el} ms"

# 5 MB valid payload, and the policy bypass it implies
python3 - "$R" <<'EOF'
import json, sys
R = sys.argv[1]
def w(name, size):
    p = {"hook_event_name": "PreToolUse", "session_id": "rob-big", "cwd": "/tmp/qa3/repo", "tool_name": "Write",
         "tool_use_id": "tu_" + name, "tool_input": {"file_path": "/tmp/qa3/repo/.env", "content": "A" * size}}
    open(f"{R}/in.{name}", "w").write(json.dumps(p))
w("env-small", 100)
w("env-3mb", 3 << 20)
w("env-5mb", 5 << 20)
EOF
for n in env-small env-3mb env-5mb; do
  read -r rc ms bytes <<<"$(shim $n "$R/in.$n")"
  dec=$(jq -r '.hookSpecificOutput.permissionDecision // "no-opinion"' "$R/$n.out" 2>/dev/null || echo no-opinion)
  [ "$bytes" = 0 ] && dec=no-opinion
  echo "      Write .env with $n payload → $dec (rc $rc, ${ms} ms)"
  eval "dec_${n//-/_}=$dec"
done
[ "$dec_env_small" = deny ] && pass "deny rule applies to small Write .env" || fail "small .env not denied ($dec_env_small)"
[ "$dec_env_3mb" = deny ] && pass "deny rule applies to 3 MB Write .env" || fail "3 MB .env not denied ($dec_env_3mb)"
[ "$dec_env_5mb" = deny ] && pass "deny rule applies to 5 MB Write .env" || fail "POLICY BYPASS: 5 MB Write to .env gets '$dec_env_5mb' (shim stdin cap 4 MB → fail-open, deny rule never evaluated)"
write_config 'notify:' '  desktop: false' 'update_check: false' 'approvals: {hold_limit: 60s, fail_closed: true}'
read -r rc ms bytes <<<"$(shim env-5mb-fc "$R/in.env-5mb")"
echo "      same 5 MB payload with approvals.fail_closed: true → rc $rc stdout '$(head -c 120 "$R/env-5mb-fc.out")'"
[ "$bytes" != 0 ] && pass "fail_closed denies oversized pre_tool" || fail "fail_closed: oversized pre_tool still no-opinion (kind unknown before parse)"
write_config 'notify:' '  desktop: false' 'update_check: false' 'approvals: {hold_limit: 60s}'
api /v1/health >/dev/null && pass "daemon alive after fuzzing" || fail "daemon died during fuzzing"

# ---- local API: malformed and oversized bodies ----
c=$(api /v1/hook -o /dev/null -w '%{http_code}' -X POST -d '{bad'); [ "$c" = 400 ] && pass "POST /v1/hook bad JSON → 400" || fail "bad JSON → $c"
c=$(api /v1/hook -o "$R/unk.json" -w '%{http_code}' -X POST -d '{"agent":"nope","payload":{}}'); [ "$c" = 200 ] && pass "unknown agent envelope → 200 $(cat "$R/unk.json")" || fail "unknown agent envelope $c"
head -c $((20<<20)) /dev/zero | tr '\0' 'a' | sed 's/^/{"agent":"claude","payload":"/; s/$/"}/' >"$R/big.json"
c=$(api /v1/hook -o "$R/big.resp" -w '%{http_code}' -X POST --data-binary @"$R/big.json"); [ "$c" = 400 ] || [ "$c" = 413 ] && pass "20 MB envelope → $c" || fail "20 MB envelope → $c"
api /v1/health >/dev/null && pass "daemon alive after API abuse" || fail "daemon died on API abuse"

# ---- daemon SIGKILL mid-hold ----
detached "$R/away.out" "$PH" away >/dev/null
jq -cn '{hook_event_name:"PermissionRequest", session_id:"rob-hold", cwd:"/tmp/qa3/repo", tool_name:"Bash", tool_input:{command:"git push origin main"}}' >"$R/in.perm"
( cd "$QA/repo"; s=$(now_ms); "$PH" hook claude <"$R/in.perm" >"$R/hold.out" 2>"$R/hold.err"; echo "$? $(( $(now_ms) - s ))" >"$R/hold.rc" ) &
for _ in $(seq 1 50); do RID=$(api '/v1/requests?status=open' | jq -r '.[0].id // empty'); [ -n "$RID" ] && break; sleep 0.1; done
[ -n "$RID" ] && pass "request $RID held" || fail "no held request"
sleep 1; DP=$(api /v1/health | jq -r .pid); k0=$(now_ms); kill -9 "$DP"
for _ in $(seq 1 100); do [ -f "$R/hold.rc" ] && break; sleep 0.05; done
read -r hrc hms <<<"$(cat "$R/hold.rc" 2>/dev/null)"
el=$(( $(now_ms) - k0 ))
[ "$hrc" = 0 ] && [ ! -s "$R/hold.out" ] && [ "$el" -lt 2000 ] && pass "SIGKILL mid-hold → shim fails open in ~${el} ms (no opinion, rc 0)" || fail "SIGKILL mid-hold: rc=$hrc out=$(cat "$R/hold.out") after ${el} ms"
start_daemon "$R/daemon-after-kill.log" || fail "restart after SIGKILL"
st=$(api "/v1/requests/$RID" | jq -r '.status')
[ "$st" != open ] && pass "held request recovered as '$st' after restart" || fail "request still open after restart"
grep -iE 'recover|stale|expired' "$R/daemon-after-kill.log" | sed -E 's/time=[^ ]+ //' | head -3
detached "$R/auto.out" "$PH" auto >/dev/null

# ---- corrupt policy (hot reload) keeps last good rules ----
cp "$PILOTHOUSE_HOME/policy.yaml" "$R/policy.good"
printf 'version: 1\nrules:\n  - name: broken\n    match: { kind: shell, command: "[unterminated" }\n    when: "command.(("\n    action: deny\n  : : :\n\t\x00garbage' >"$PILOTHOUSE_HOME/policy.yaml"
sleep 4
api /v1/policy/status | jq -c '{errors: .errors, files: [.files[]? | {path, rules, error}]}' | cut -c1-400
jq -cn '{hook_event_name:"PreToolUse", session_id:"rob-pol", cwd:"/tmp/qa3/repo", tool_name:"Bash", tool_use_id:"tp1", tool_input:{command:"git push -f origin main"}}' >"$R/in.fp"
read -r rc ms bytes <<<"$(shim fp "$R/in.fp")"
[ "$(jq -r .hookSpecificOutput.permissionDecision "$R/fp.out" 2>/dev/null)" = deny ] && pass "corrupt policy hot-reload: last good rules still deny force-push" || fail "corrupt policy: force-push → '$(cat "$R/fp.out")'"
api /v1/policy/status | jq -e '(.errors|length>0) or ([.files[]?|select(.error!=null and .error!="")]|length>0)' >/dev/null && pass "policy status reports the error" || fail "policy status shows no error"
# corrupt policy at daemon start
stop_daemon; sleep 0.5
start_daemon "$R/daemon-badpolicy.log" && pass "daemon starts with corrupt policy file" || fail "daemon won't start with corrupt policy"
read -r rc ms bytes <<<"$(shim fp2 "$R/in.fp")"
echo "      force-push after cold start with corrupt policy → '$(jq -r '.hookSpecificOutput.permissionDecision // "no-opinion"' "$R/fp2.out" 2>/dev/null)' (no last-good rules exist)"
cp "$R/policy.good" "$PILOTHOUSE_HOME/policy.yaml"; sleep 3
read -r rc ms bytes <<<"$(shim fp3 "$R/in.fp")"
[ "$(jq -r .hookSpecificOutput.permissionDecision "$R/fp3.out" 2>/dev/null)" = deny ] && pass "fixed policy picked up without restart" || fail "fixed policy not reloaded"

# ---- corrupt config ----
cp "$PILOTHOUSE_HOME/config.yaml" "$R/config.good"
stop_daemon; sleep 0.5
printf 'web: [\n  this is: not: yaml\n' >"$PILOTHOUSE_HOME/config.yaml"
perl -e 'alarm 5; exec @ARGV' "$PH" daemon >"$R/badcfg.out" 2>&1; echo "      daemon with corrupt config: rc=$? $(head -c 200 "$R/badcfg.out")"
read -r rc ms bytes <<<"$(shim badcfg-shim "$R/in.fp")"
[ "$rc" = 0 ] && pass "shim with daemon down + corrupt config → rc 0 (${ms} ms)" || fail "shim with corrupt config rc=$rc"
cp "$R/config.good" "$PILOTHOUSE_HOME/config.yaml"
start_daemon "$R/daemon-3.log" || exit 1

# ---- DB locked by another process ----
python3 - "$PILOTHOUSE_HOME/pilothouse.db" 8 >"$R/lock.out" 2>&1 <<'EOF' &
import sqlite3, sys, time
c = sqlite3.connect(sys.argv[1], timeout=1, isolation_level=None)
c.execute("BEGIN EXCLUSIVE")
print("locked", flush=True)
time.sleep(float(sys.argv[2]))
c.execute("COMMIT")
print("released", flush=True)
EOF
LK=$!
for _ in $(seq 1 30); do grep -q locked "$R/lock.out" && break; sleep 0.1; done
lat=()
for i in 1 2 3; do read -r rc ms bytes <<<"$(shim lock$i "$R/in.fp")"; lat+=("$ms/rc$rc/$( [ "$bytes" = 0 ] && echo no-opinion || jq -r .hookSpecificOutput.permissionDecision "$R/lock$i.out")"); done
echo "      pre_tool while DB exclusively locked: ${lat[*]}"
read -r ms1 <<<"${lat[0]%%/*}"
[ "$ms1" -lt 2600 ] && pass "DB locked → shim answers within ~2 s" || fail "DB locked → shim took ${ms1} ms"
wait $LK
sleep 6  # let the busy_timeout (5 s) of the queued hooks drain
read -r rc ms bytes <<<"$(shim unlock "$R/in.fp")"
[ "$(jq -r .hookSpecificOutput.permissionDecision "$R/unlock.out" 2>/dev/null)" = deny ] && pass "daemon recovers after lock released (${ms} ms)" || fail "after unlock: '$(cat "$R/unlock.out")'"
grep -E 'level=(ERROR|WARN)' "$R/daemon-3.log" | sed -E 's/time=[^ ]+ //' | sort | uniq -c | sort -rn | head -5

# ---- optional: disk full on a small RAM volume ----
if [ "${DISKFULL:-0}" = 1 ]; then
  stop_daemon
  dev=$(hdiutil attach -nomount ram://16384 | awk '{print $1}')  # 8 MB
  newfs_hfs -v phqa3full "$dev" >/dev/null && mkdir -p "$QA/full" && mount -t hfs "$dev" "$QA/full" 2>/dev/null || diskutil mount -mountPoint "$QA/full" "$dev" >/dev/null
  if mount | grep -q "$QA/full"; then
    OLDHOME=$PILOTHOUSE_HOME; export PILOTHOUSE_HOME=$QA/full/home; mkdir -p "$PILOTHOUSE_HOME"; chmod 700 "$PILOTHOUSE_HOME"
    cp "$OLDHOME/config.yaml" "$OLDHOME/policy.yaml" "$PILOTHOUSE_HOME/"
    start_daemon "$R/daemon-full.log"
    dd if=/dev/zero of="$QA/full/filler" bs=1m count=100 2>/dev/null; df -h "$QA/full" | tail -1
    python3 -c "import json;print(json.dumps({'hook_event_name':'PreToolUse','session_id':'rob-full','cwd':'/tmp/qa3/repo','tool_name':'Bash','tool_input':{'command':'echo '+'x'*200000}}))" >"$R/in.fullbig"
    worst=0; codes=""
    for i in $(seq 1 40); do read -r rc ms bytes <<<"$(shim full$i "$R/in.fullbig")"; codes="$codes$rc"; [ "$ms" -gt "$worst" ] && worst=$ms; done
    echo "      40 hooks on a full disk: exit codes '$codes', worst ${worst} ms"
    read -r rc ms bytes <<<"$(shim full-fp "$R/in.fp")"
    echo "      force-push on full disk → $( [ "$bytes" = 0 ] && echo no-opinion || jq -r .hookSpecificOutput.permissionDecision "$R/full-fp.out") (${ms} ms)"
    api /v1/health >/dev/null && pass "daemon alive on full disk" || fail "daemon died on full disk"
    grep -E 'level=(ERROR|WARN)' "$R/daemon-full.log" | sed -E 's/time=[^ ]+ //' | cut -c1-200 | sort | uniq -c | sort -rn | head -4
    rm -f "$QA/full/filler"
    read -r rc ms bytes <<<"$(shim full-after "$R/in.fp")"
    [ "$(jq -r .hookSpecificOutput.permissionDecision "$R/full-after.out" 2>/dev/null)" = deny ] && pass "recovers after space freed" || fail "after freeing space: '$(cat "$R/full-after.out")'"
    stop_daemon; export PILOTHOUSE_HOME=$OLDHOME
    umount "$QA/full" 2>/dev/null || diskutil unmount "$QA/full" >/dev/null
  else
    echo "SKIP  disk full: could not mount RAM volume"
  fi
  hdiutil detach "$dev" >/dev/null 2>&1
fi
echo "FAILS=${FAILS:-0}"
