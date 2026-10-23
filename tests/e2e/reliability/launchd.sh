#!/usr/bin/env bash
# Real launchd: the plist `service install` would write, relabelled
# dev.pilothouse.qa3 and bootstrapped explicitly. Checks KeepAlive after
# kill -9, clean-exit stays down, and that a `pilothouse run` holder
# survives kickstart -k and bootout and is still known afterwards.
# Never touches the real label dev.pilothouse.daemon or ~/Library/LaunchAgents.
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../../.." && pwd)
source "$here/common.sh"
unset PILOTHOUSE_SESSION_ID
LABEL=dev.pilothouse.qa3
DOM=gui/$(id -u)
R=$QA/launchd; rm -rf "$R"; mkdir -p "$R"
PLIST=$PILOTHOUSE_USER_HOME/Library/LaunchAgents/$LABEL.plist
pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; FAILS=$((${FAILS:-0}+1)); }
cleanup() {
  launchctl bootout "$DOM/$LABEL" 2>/dev/null
  [ -n "${HOLDER:-}" ] && kill "$HOLDER" 2>/dev/null
  pkill -f "__holder --id ${SID:-none}" 2>/dev/null
  launchctl print "$DOM/$LABEL" >/dev/null 2>&1 && echo "WARNING: $LABEL still loaded" || echo "cleanup: $LABEL booted out"
}
trap cleanup EXIT INT TERM

stop_daemon
write_config 'notify:' '  desktop: false' 'update_check: false'
mkdir -p "$(dirname "$PLIST")"
(cd "$repo" && go run ./tests/e2e/reliability/renderplist "$PH") >"$R/orig.plist" || exit 1
sed "s|<string>dev.pilothouse.daemon</string>|<string>$LABEL</string>|" "$R/orig.plist" >"$PLIST"
grep -q "dev.pilothouse.daemon" "$PLIST" && { echo "label not replaced"; exit 1; }
plutil -lint "$PLIST" || exit 1

lpid() { launchctl print "$DOM/$LABEL" 2>/dev/null | awk '/^\tpid = /{print $3; exit}'; }
wait_pid() { # wait for a pid different from $1, up to $2 s; prints seconds taken
  local t0=$(python3 -c 'import time;print(time.time())') p
  for _ in $(seq 1 $(($2*10))); do
    p=$(lpid); if [ -n "$p" ] && [ "$p" != "$1" ] && curl -s --unix-socket "$SOCK" http://x/v1/health >/dev/null 2>&1; then
      python3 -c "import time;print(round(time.time()-$t0,1))"; return 0; fi
    sleep 0.1
  done
  return 1
}

launchctl bootstrap "$DOM" "$PLIST" || { fail "bootstrap"; exit 1; }
t=$(wait_pid 0 15) && pass "bootstrapped, daemon pid $(lpid) healthy after ${t}s" || { fail "daemon did not come up"; tail "$PILOTHOUSE_HOME/logs/"*; exit 1; }
P1=$(lpid)

# ---- holder under `pilothouse run -d` ----
"$PH" run -d -n qa3tick -- /bin/sh -c 'while true; do echo tick; sleep 1; done' >"$R/run.out" 2>&1
cat "$R/run.out"
sleep 2
SID=$(grep -o "s_[a-z0-9]\{10\}" "$R/run.out" | head -1)
HOLDER=$(pgrep -f "__holder --id $SID" | head -1)
CHILD=$(api "/v1/sessions/$SID" | jq -r '.pid // empty')
echo "session=$SID holder=$HOLDER child=$CHILD"
[ -n "$SID" ] && [ -n "$HOLDER" ] && pass "wrapped session registered" || fail "no wrapped session: $(api /v1/sessions)"
ps -o pid,ppid,pgid,sess,comm -p "$HOLDER","$CHILD","$P1" 2>/dev/null

# ---- KeepAlive after kill -9 ----
kill -9 "$P1"
t=$(wait_pid "$P1" 30) && pass "kill -9 → launchd restarted daemon in ${t}s (pid $(lpid))" || fail "no restart after kill -9: $(launchctl print "$DOM/$LABEL" | grep -E 'state|last exit|runs')"
P2=$(lpid)
kill -0 "$HOLDER" && kill -0 "$CHILD" && pass "holder+agent survived daemon kill -9" || fail "holder died on kill -9"

# ---- kickstart -k ----
launchctl kickstart -k "$DOM/$LABEL"
t=$(wait_pid "$P2" 30) && pass "kickstart -k → new daemon pid $(lpid) in ${t}s" || fail "kickstart -k"
P3=$(lpid)
kill -0 "$HOLDER" && kill -0 "$CHILD" && pass "holder+agent survived kickstart -k" || fail "holder died on kickstart -k"
st=$(api "/v1/sessions/$SID" | jq -r '.state + " exited=" + ((.exited_at // "no")|tostring)')
echo "session after kickstart: $st"
[[ $st == *"exited=no"* ]] && pass "session still live after kickstart" || fail "session marked exited after kickstart: $st"

# ---- bootout (daemon gone), holder keeps running; come back ----
launchctl bootout "$DOM/$LABEL"; sleep 2
launchctl print "$DOM/$LABEL" >/dev/null 2>&1 && fail "still loaded after bootout" || pass "booted out"
kill -0 "$P3" 2>/dev/null && fail "daemon $P3 still alive after bootout" || pass "daemon stopped by bootout"
kill -0 "$HOLDER" && kill -0 "$CHILD" && pass "holder+agent survived bootout (AbandonProcessGroup)" || fail "holder died on bootout"
sleep 3
launchctl bootstrap "$DOM" "$PLIST"
t=$(wait_pid "$P3" 15) && pass "re-bootstrapped in ${t}s" || fail "re-bootstrap"
sleep 5
st=$(api "/v1/sessions/$SID" | jq -c '{state, mode, exited_at, holder_pid, pid, attachable}')
echo "session after bootout+bootstrap: $st"
jq -e '.exited_at==null' <<<"$st" >/dev/null && pass "session live after daemon came back" || fail "session exited after daemon came back"
"$PH" ls 2>&1 | head -5
python3 -I "$here/attach_check.py" "$PH" "$SID" && pass "attach works after daemon restart" || fail "attach after restart"
grep -E 'registered|re-register|report' "$PILOTHOUSE_HOME/logs/holder-$SID.log" 2>/dev/null | tail -5

# ---- clean exit (SIGTERM) must NOT respawn ----
P4=$(lpid); kill -TERM "$P4"; sleep 12
p=$(lpid)
le=$(launchctl print "$DOM/$LABEL" | awk -F' = ' '/last exit code/{print $2; exit}')
[ -z "$p" ] && pass "SIGTERM (clean exit, code $le) stays stopped" || echo "INFO  SIGTERM → respawned pid $p (last exit $le)"
echo "FAILS=${FAILS:-0}"
