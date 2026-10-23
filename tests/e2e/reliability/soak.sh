#!/usr/bin/env bash
# Soak: 50 sessions × 20k PreToolUse hooks through the real shim, a 50-rule
# policy, 200 held PermissionRequests answered by a signed simulated phone,
# 20 SSE subscribers, a SIGSTOP of the daemon mid-load, then retention
# (retention: 1m) verified across a restart.
# Env knobs: PRETOOL (20000) PERM (200) WORKERS (50) SSE (20) STOP_AT (10000) STOP_SECS (6)
here=$(cd "$(dirname "$0")" && pwd)
source "$here/common.sh"
unset PILOTHOUSE_SESSION_ID
R=$QA/soak; rm -rf "$R"; mkdir -p "$R"
stop_daemon
rm -f "$PILOTHOUSE_HOME"/pilothouse.db*  # fresh DB for clean numbers
trap 'stop_daemon; [ -n "${PHONE:-}" ] && kill $PHONE 2>/dev/null' EXIT

# repo the sessions work in (git lookup is part of every hook)
SR=$QA/soakrepo; rm -rf "$SR"; mkdir -p "$SR"; git -C "$SR" init -q -b main && git -C "$SR" commit -q --allow-empty -m init

# 50-rule policy: 20 glob deny, 10 CEL ask, 10 path deny, 10 allow
{
  echo 'version: 1'; echo 'rules:'
  for i in $(seq 0 19); do printf '  - name: deny-rm-%d\n    match: { kind: shell, command: "rm -rf /data%d/*" }\n    action: deny\n    message: no rm %d\n' $i $i $i; done
  for i in $(seq 0 9); do printf '  - name: ask-secret-%d\n    match: { kind: shell }\n    when: '"'"'command.contains("secret%d") && !has_word(command, "nope")'"'"'\n    action: ask\n' $i $i; done
  for i in $(seq 0 9); do printf '  - name: deny-private-%d\n    match: { path: "**/private%d/**" }\n    action: deny\n' $i $i; done
  for i in $(seq 0 9); do printf '  - name: allow-safe-%d\n    match: { kind: shell, command: "echo safe%d *" }\n    action: allow\n' $i $i; done
} >"$PILOTHOUSE_HOME/policy.yaml"
"$PH" policy check "$PILOTHOUSE_HOME/policy.yaml" | tail -2

write_config 'notify:' '  desktop: false' 'update_check: false' 'retention: 1m' 'approvals: {hold_limit: 120s}'
start_daemon "$R/daemon.log" || exit 1
DPID=$(api /v1/health | jq -r .pid)
"$PH" policy status 2>&1 | head -5
detached "$R/away.out" "$PH" away >/dev/null
echo "presence: $(api /v1/presence | jq -r .state)"

# pair the phone: the guard refuses callers under an agent, so double-fork curl
detached "$R/pair.json" curl -s --unix-socket "$SOCK" -X POST http://x/v1/pair >/dev/null
CODE=$(jq -r .code "$R/pair.json"); echo "pair code: $CODE"
[ -n "$CODE" ] && [ "$CODE" != null ] || { echo "pairing failed: $(cat "$R/pair.json")"; exit 1; }
node "$here/phone_many.mjs" "http://$WEB_ADDR" "$CODE" "$R/phone.jsonl" allow &
PHONE=$!
for _ in $(seq 1 50); do grep -q paired "$R/phone.jsonl" 2>/dev/null && break; sleep 0.1; done

python3 -I "$here/soak.py" --ph "$PH" --sock "$SOCK" --pid "$DPID" --db "$PILOTHOUSE_HOME/pilothouse.db" --cwd "$SR" --out "$R" \
  --pretool "${PRETOOL:-20000}" --perm "${PERM:-200}" --workers "${WORKERS:-50}" --sse "${SSE:-20}" \
  --stop-at "${STOP_AT:-10000}" --stop-secs "${STOP_SECS:-6}" | tee "$R/summary.json"

echo "--- phone"
jq -s '{decides: map(select(.ev=="decide"))|length, ok: map(select(.ev=="decide" and .status==200))|length,
        non200: map(select(.ev=="decide" and .status!=200))|map(.status), resyncs: map(select(.ev=="resync"))|length,
        decide_ms_p50: (map(select(.ev=="decide").ms)|sort|.[length/2|floor]), decide_ms_max: (map(select(.ev=="decide").ms)|max)}' "$R/phone.jsonl"
echo "--- daemon log: warnings/errors"
grep -E 'level=(WARN|ERROR)' "$R/daemon.log" | sed -E 's/time=[^ ]+ //' | sort | uniq -c | sort -rn | head -10
echo "--- goroutine-ish: threads now $(($(ps -M -p "$DPID" | wc -l)-1)), fds $(($(lsof -p "$DPID" | wc -l)-1))"

# ---- retention (prune runs at start-up and then hourly) ----
DB=$PILOTHOUSE_HOME/pilothouse.db
counts() { sqlite3 "file:$DB?mode=ro" "select 'events='||(select count(*) from events)||' hook_events='||(select count(*) from events where type='hook')||' requests='||(select count(*) from requests)||' evidence='||(select count(*) from evidence)||' sessions='||(select count(*) from sessions)"; }
echo "rows before: $(counts)"; ls -la "$DB"* | awk '{print $5, $9}'
echo "waiting 70 s (retention 1m) …"; sleep 70
echo "rows after 70 s, no restart: $(counts)"
kill $PHONE 2>/dev/null; PHONE=
stop_daemon; sleep 1
start_daemon "$R/daemon2.log" || exit 1
sleep 2
echo "rows after restart: $(counts)"; grep 'pruned old records' "$R/daemon2.log" | sed -E 's/time=[^ ]+ //'
ls -la "$DB"* | awk '{print $5, $9}'
