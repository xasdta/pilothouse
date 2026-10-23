#!/usr/bin/env bash
# Real notification channels: ntfy.sh (random topic), local webhooks
# (ok/slow/fail), macOS desktop (osascript injection), `notify test`, and the
# ntfy action-button tokens (well-formed, single use, Host allow-list).
here=$(cd "$(dirname "$0")" && pwd)
source "$here/common.sh"
unset PILOTHOUSE_SESSION_ID
R=$QA/notify; rm -rf "$R" "$QA/pwned"; mkdir -p "$R"
pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; FAILS=$((${FAILS:-0}+1)); }

TOPIC=pilothouse-qa-$(openssl rand -hex 16)
PUB=https://phqa3.example.test
WHPORT=${WHPORT:-18593}
echo "topic: $TOPIC"
echo "$TOPIC" >"$R/topic"

python3 -I "$here/webhook_server.py" "$WHPORT" "$R/webhooks.jsonl" 30 &
WH=$!
trap 'stop_daemon; kill $WH 2>/dev/null' EXIT

write_config \
  "  public_url: $PUB" \
  'notify:' '  desktop: true' "  ntfy: {server: https://ntfy.sh, topic: $TOPIC}" \
  '  webhooks:' "    - http://127.0.0.1:$WHPORT/ok" "    - http://127.0.0.1:$WHPORT/slow" "    - http://127.0.0.1:$WHPORT/fail" \
  'approvals: {hold_limit: 60s}' 'update_check: false'
start_daemon "$R/daemon.log" || exit 1

# Hostile repo name (the notification title) and command (the body). No '/'
# is allowed in a directory name, hence the cd chain.
EVIL_DIR="$QA/repo-x\" & (do shell script \"cd;cd ..;cd ..;cd tmp;cd qa3;touch pwned\") & \"y"
mkdir -p "$EVIL_DIR"
EVIL_CMD='echo "; do shell script "touch /tmp/qa3/pwned"; " & (do shell script "touch /tmp/qa3/pwned") & "'"'"'$(touch /tmp/qa3/pwned)'

# ---- notify test ----
"$PH" notify test >"$R/notify-test.out" 2>&1 && pass "pilothouse notify test exit 0" || fail "notify test: $(cat "$R/notify-test.out")"

# ---- go away (guarded: run detached from this process tree) ----
detached "$R/away.out" "$PH" away >/dev/null
[ "$(api /v1/presence | jq -r .state)" = away ] && pass "presence away" || fail "presence not away: $(cat "$R/away.out")"

# ---- held permission request ----
payload=$(jq -cn --arg cwd "$EVIL_DIR" --arg c "$EVIL_CMD" \
  '{hook_event_name:"PermissionRequest", session_id:"qa-notify-1", cwd:$cwd, tool_name:"Bash", tool_input:{command:$c}}')
( cd "$EVIL_DIR" && echo "$payload" | "$PH" hook claude >"$R/shim.out" 2>"$R/shim.err"; echo $? >"$R/shim.code" ) &
SHIM=$!
for _ in $(seq 1 50); do
  RID=$(api '/v1/requests?status=open' | jq -r '.[0].id // empty'); [ -n "$RID" ] && break; sleep 0.1
done
[ -n "$RID" ] && pass "request held: $RID" || fail "no open request"

# ---- ntfy: poll the topic ----
for _ in $(seq 1 30); do
  curl -s "https://ntfy.sh/$TOPIC/json?poll=1" >"$R/ntfy.jsonl"
  grep -q '"event":"message"' "$R/ntfy.jsonl" && [ "$(grep -c '"event":"message"' "$R/ntfy.jsonl")" -ge 2 ] && break
  sleep 1
done
echo "--- ntfy messages"; jq -c '{title,message,priority,tags,click,actions:[.actions[]?|{action,label,url,method,clear}]}' "$R/ntfy.jsonl"
msg=$(jq -c 'select(.tags|index("lock"))' "$R/ntfy.jsonl" | head -1)
[ -n "$msg" ] && pass "ntfy request message received" || fail "ntfy request message missing"
[ "$(jq -r .priority <<<"$msg")" = 4 ] && pass "ntfy priority high(4)" || fail "ntfy priority $(jq -r .priority <<<"$msg")"
jq -e '.title | test("Claude Code")' <<<"$msg" >/dev/null && pass "ntfy title" || fail "ntfy title: $(jq -r .title <<<"$msg")"
jq -e '.message | test("wants to run")' <<<"$msg" >/dev/null && pass "ntfy body" || fail "ntfy body"
jq -c 'select(.title=="Pilothouse test notification")' "$R/ntfy.jsonl" | grep -q . && pass "ntfy test message received" || fail "ntfy test message missing"
ALLOW=$(jq -r '.actions[]? | select(.label=="Allow") | .url' <<<"$msg")
DENY=$(jq -r '.actions[]? | select(.label=="Deny") | .url' <<<"$msg")
OPEN=$(jq -r '.actions[]? | select(.label=="Open") | .url' <<<"$msg")
echo "allow=$ALLOW deny=$DENY open=$OPEN"
[[ $ALLOW =~ ^$PUB/api/actions/[A-Za-z0-9_-]{40,64}$ ]] && pass "allow URL well-formed" || fail "allow URL: $ALLOW"
[[ $DENY =~ ^$PUB/api/actions/[A-Za-z0-9_-]{40,64}$ ]] && pass "deny URL well-formed" || fail "deny URL: $DENY"
[ "$OPEN" = "$PUB/r/$RID" ] && pass "open/click URL" || fail "open URL: $OPEN"
[ "$(jq -r '.actions[]?|select(.label=="Allow")|.method' <<<"$msg")" = POST ] && pass "action method POST" || fail "action method"
# secrets: no action token, device token or cookie in the visible text
tokA=${ALLOW##*/}; tokD=${DENY##*/}
if grep -F -e "$tokA" -e "$tokD" <(jq -r '.title, .message' "$R/ntfy.jsonl") >/dev/null; then fail "action token leaked into title/body"; else pass "no token in ntfy title/body"; fi

# ---- action tokens against the local daemon ----
host=${PUB#https://}
code() { curl -s -o "$R/act.$1.json" -w '%{http_code}' -X POST -H "Host: $2" "http://$WEB_ADDR/api/actions/$3"; }
c=$(code badhost evil.example "$tokA"); [ "$c" = 421 ] && pass "action with foreign Host → 421" || fail "foreign Host gave $c"
c=$(code short "$host" abc); [ "$c" = 404 ] && pass "short token → 404" || fail "short token $c"
c=$(code forged "$host" "$(openssl rand -hex 24)"); [ "$c" = 410 ] && pass "forged token → 410" || fail "forged token $c"
c=$(code allow "$host" "$tokA"); [ "$c" = 200 ] && pass "allow action → 200 ($(jq -r '.status+" by "+.decision.by' "$R/act.allow.json"))" || fail "allow action $c $(cat "$R/act.allow.json")"
c=$(code replay "$host" "$tokA"); [ "$c" = 410 ] && pass "allow replay → 410" || fail "allow replay $c"
c=$(code sibling "$host" "$tokD"); [ "$c" = 410 ] && pass "sibling deny after allow → 410" || fail "sibling deny $c"
wait $SHIM
echo "shim exit=$(cat "$R/shim.code") stdout=$(cat "$R/shim.out")"
jq -e '.hookSpecificOutput.decision.behavior=="allow"' "$R/shim.out" >/dev/null && pass "shim got allow" || fail "shim output"

# ---- webhooks ----
sleep 2
echo "--- webhook hits"; jq -c '{path, ct: .headers["Content-Type"], body}' "$R/webhooks.jsonl"
ok=$(grep '"/ok"' "$R/webhooks.jsonl" | grep 'wants to run' | head -1)
[ -n "$ok" ] && jq -e '.body|fromjson|(.text|length>0) and (.content==.text)' <<<"$ok" >/dev/null && pass "webhook JSON {text,content}" || fail "webhook payload"
[ "$(jq -r '.headers["Content-Type"]' <<<"$ok")" = application/json ] && pass "webhook content-type" || fail "webhook content-type"

# ---- slow/failing webhook must not block hooks; bounded queue ----
lat=()
for i in $(seq 1 60); do
  s=$(python3 -c 'import time;print(time.time())')
  hook_json Stop qa-notify-1 "" "" | "$PH" hook claude >/dev/null 2>&1
  e=$(python3 -c 'import time;print(time.time())')
  lat+=("$(python3 -c "print(round(($e-$s)*1000))")")
done
printf '%s\n' "${lat[@]}" | sort -n >"$R/stop-lat.txt"
echo "Stop hook latency ms (60 calls while slow webhook stalls): p50=$(sed -n 30p "$R/stop-lat.txt") max=$(tail -1 "$R/stop-lat.txt")"
[ "$(tail -1 "$R/stop-lat.txt")" -lt 1000 ] && pass "hooks not blocked by slow webhook" || fail "hook latency with slow webhook"
sleep 3
okn=$(grep -c '"/ok"' "$R/webhooks.jsonl"); slown=$(grep -c '"/slow"' "$R/webhooks.jsonl"); failn=$(grep -c '"/fail"' "$R/webhooks.jsonl")
drops=$(grep -c 'queue full' "$R/daemon.log"); fails=$(grep -c 'notification failed.*webhook' "$R/daemon.log")
echo "webhook hits ok=$okn slow=$slown fail=$failn; 'queue full' drops=$drops; failure logs=$fails"
[ "$okn" -ge 60 ] && pass "fast webhook unaffected by slow one" || fail "fast webhook got $okn"
[ "$drops" -gt 0 ] && pass "bounded queue drops (slow channel)" || fail "no queue-full drops seen"
grep -m2 'notification failed' "$R/daemon.log"

# ---- desktop: osascript argv, no injection ----
pgrep -fl osascript | head -3
sleep 3
[ -e "$QA/pwned" ] && fail "INJECTION: $QA/pwned exists" || pass "osascript injection: $QA/pwned absent"
grep -i 'desktop' "$R/daemon.log" | grep -iv debug | head -3
grep -c 'channel=desktop' "$R/daemon.log" | xargs echo "desktop log lines:"
echo "FAILS=${FAILS:-0}"
