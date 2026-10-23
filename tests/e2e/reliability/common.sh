# Shared helpers for the reliability suite. Source it; never run it.
# Everything lives under $QA (default /tmp/qa3): no real home is touched.
set -u
QA=${QA:-/tmp/qa3}
export PILOTHOUSE_HOME=$QA/home PILOTHOUSE_USER_HOME=$QA/uhome PILOTHOUSE_RUN_DIR=$QA/run
PH=${PH:-$QA/pilothouse}
WEB_ADDR=${WEB_ADDR:-127.0.0.1:7593}
SOCK=$PILOTHOUSE_RUN_DIR/pilothouse.sock
mkdir -p "$PILOTHOUSE_HOME" "$PILOTHOUSE_USER_HOME" "$PILOTHOUSE_RUN_DIR" "$QA/logs"
chmod 700 "$PILOTHOUSE_HOME" "$PILOTHOUSE_RUN_DIR"

# write_config <extra yaml lines…>: base config + extras.
write_config() {
  {
    printf 'web:\n  addr: %s\n' "$WEB_ADDR"
    printf '%s\n' "$@"
  } >"$PILOTHOUSE_HOME/config.yaml"
  chmod 600 "$PILOTHOUSE_HOME/config.yaml"
}

daemon_pid() { "$PH" status --json 2>/dev/null | jq -r '.pid // empty' 2>/dev/null; }

# start_daemon [logfile] [extra env…]: foreground daemon detached with nohup.
start_daemon() {
  local log=${1:-$QA/logs/daemon.log}
  nohup "$PH" daemon --debug >>"$log" 2>&1 </dev/null &
  DAEMON_PID=$!
  for _ in $(seq 1 100); do
    [ -S "$SOCK" ] && curl -s --unix-socket "$SOCK" http://x/v1/health >/dev/null 2>&1 && return 0
    sleep 0.1
  done
  echo "daemon did not start; log:" >&2; tail -20 "$log" >&2; return 1
}

stop_daemon() {
  local p
  p=$(curl -s --unix-socket "$SOCK" http://x/v1/health 2>/dev/null | jq -r .pid 2>/dev/null)
  [ -n "${p:-}" ] && kill "$p" 2>/dev/null
  [ -n "${DAEMON_PID:-}" ] && kill "$DAEMON_PID" 2>/dev/null
  for _ in $(seq 1 50); do
    [ -n "${p:-}" ] && kill -0 "$p" 2>/dev/null || break
    sleep 0.1
  done
  [ -n "${p:-}" ] && kill -9 "$p" 2>/dev/null
  return 0
}

# detached <outfile> <cmd…>: run a command double-forked so it is reparented
# to launchd and no ancestor is an agent process (the daemon's agent-caller
# guard refuses decide/pair/presence from inside an agent's process tree,
# and this suite itself may run under Claude Code). Waits for it to finish.
detached() {
  local out=$1; shift
  rm -f "$out" "$out.done"
  ( nohup sh -c 'sleep 0.3; "$@"; echo $? >"$0.done"' "$out" "$@" >"$out" 2>&1 </dev/null & )
  for _ in $(seq 1 150); do [ -f "$out.done" ] && break; sleep 0.1; done
  cat "$out"
  return "$(cat "$out.done" 2>/dev/null || echo 99)"
}

api() { curl -s --unix-socket "$SOCK" "http://x$1" "${@:2}"; }

# hook_json <event> <session> <tool> <command> [tool_use_id]
hook_json() {
  jq -cn --arg e "$1" --arg s "$2" --arg t "$3" --arg c "$4" --arg u "${5:-tu_$RANDOM$RANDOM}" --arg cwd "$QA/repo" \
    '{hook_event_name:$e, session_id:$s, cwd:$cwd, transcript_path:null, tool_name:$t, tool_use_id:$u, tool_input:{command:$c}}'
}
