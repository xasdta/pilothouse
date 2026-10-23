#!/usr/bin/env bash
# Update check against the live https://pilothouse.dev/api/version.
# The URL is not overridable at runtime (daemon/update.go: package var), so
# every daemon runs behind its own logging CONNECT proxy (HTTPS_PROXY) to see
# whether a request leaves the machine at all. Four daemons in parallel:
#   A 0.1.0, check on        → CONNECT pilothouse.dev:443 + "newer … available"
#   B 0.1.1, NO_UPDATE_CHECK=1 → nothing
#   C 0.1.1, update_check:false → nothing
#   D 0.1.1, check on        → CONNECT, no "newer" line, meta update_checked_at set
here=$(cd "$(dirname "$0")" && pwd); repo=$(cd "$here/../../.." && pwd)
QA=${QA:-/tmp/qa3}; R=$QA/upd; rm -rf "$R"; mkdir -p "$R"
PH=$QA/pilothouse
( cd "$repo" && go build -ldflags "-X github.com/xasdta/pilothouse/internal/version.Version=0.1.0" -o "$R/pilothouse-0.1.0" ./cmd/pilothouse ) || exit 1
pids=()
cleanup() { for p in "${pids[@]}"; do kill "$p" 2>/dev/null; done; }
trap cleanup EXIT
i=0
for spec in "A $R/pilothouse-0.1.0 on" "B $PH env" "C $PH cfg" "D $PH on"; do
  set -- $spec; n=$1 bin=$2 mode=$3; i=$((i+1))
  d=$R/$n; mkdir -p "$d/home" "$d/run" "$d/uhome"; chmod 700 "$d/home" "$d/run"
  { printf 'web:\n  addr: 127.0.0.1:%d\nnotify:\n  desktop: false\n' $((7600+i)); [ "$mode" = cfg ] && echo 'update_check: false'; } >"$d/home/config.yaml"
  python3 -I "$here/connect_proxy.py" $((18600+i)) "$d/proxy.log" & pids+=($!)
  extra=(); [ "$mode" = env ] && extra=(PILOTHOUSE_NO_UPDATE_CHECK=1)
  env PILOTHOUSE_HOME=$d/home PILOTHOUSE_RUN_DIR=$d/run PILOTHOUSE_USER_HOME=$d/uhome \
      HTTPS_PROXY=http://127.0.0.1:$((18600+i)) HTTP_PROXY=http://127.0.0.1:$((18600+i)) "${extra[@]}" \
      "$bin" daemon --debug >"$d/daemon.log" 2>&1 & pids+=($!)
done
echo "waiting 75 s (first check is 1 min after start)…"; sleep 75
for n in A B C D; do
  d=$R/$n
  echo "== $n: proxy: $(tr '\n' ';' <"$d/proxy.log" 2>/dev/null)"
  grep -h 'newer pilothouse' "$d/daemon.log" | cut -c1-200
  echo "   meta: $(sqlite3 "file:$d/home/pilothouse.db?mode=ro" "select key||'='||value from meta where key like 'update%'" 2>&1 | tr '\n' ' ')"
done
