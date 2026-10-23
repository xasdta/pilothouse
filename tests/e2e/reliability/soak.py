"""Soak/load driver: real hook shim processes against a running daemon.

- W concurrent fake Claude Code sessions send N PreToolUse hooks through
  `pilothouse hook claude` (one process per hook, like the agent does);
- P PermissionRequest hooks (held: presence is away) answered by a paired
  simulated phone (phone_many.mjs, started by soak.sh);
- S SSE subscribers on /v1/events?follow=1;
- optional SIGSTOP of the daemon mid-load (fail-open timing);
- 1 Hz sampling of daemon RSS / CPU / threads / fds / DB size.
Writes JSON results + CSV samples to --out.
Usage: python3 -I soak.py --ph BIN --sock SOCK --pid DAEMON_PID --cwd DIR --out DIR [...]
"""
import argparse, http.client, json, os, signal, socket, statistics, subprocess, threading, time

ap = argparse.ArgumentParser()
ap.add_argument("--ph", required=True)
ap.add_argument("--sock", required=True)
ap.add_argument("--pid", type=int, required=True)
ap.add_argument("--db", required=True)
ap.add_argument("--cwd", required=True)
ap.add_argument("--out", required=True)
ap.add_argument("--workers", type=int, default=50)
ap.add_argument("--pretool", type=int, default=20000)
ap.add_argument("--perm", type=int, default=200)
ap.add_argument("--perm-conc", type=int, default=20)
ap.add_argument("--sse", type=int, default=20)
ap.add_argument("--stop-at", type=int, default=10000)
ap.add_argument("--stop-secs", type=float, default=6.0)
ap.add_argument("--direct", type=int, default=2000)
a = ap.parse_args()
os.makedirs(a.out, exist_ok=True)


class UConn(http.client.HTTPConnection):
    def __init__(self, path, timeout=None):
        super().__init__("x", timeout=timeout)
        self.path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        if self.timeout:
            self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)


def api(path):
    c = UConn(a.sock, timeout=10)
    c.request("GET", path)
    r = c.getresponse()
    b = r.read()
    c.close()
    return json.loads(b)


def pct(xs, p):
    if not xs:
        return None
    xs = sorted(xs)
    return round(xs[min(len(xs) - 1, int(len(xs) * p / 100))], 1)


def summary(xs):
    return {"n": len(xs), "p50": pct(xs, 50), "p90": pct(xs, 90), "p99": pct(xs, 99), "max": round(max(xs), 1) if xs else None}


# Commands that hit the 50-rule policy in different ways (see soak.sh).
def command_for(i):
    m = i % 10
    if m == 0:
        return f"rm -rf /data{i % 20}/x", "deny"
    if m == 1:
        return f"echo safe{i % 10} hi", "allow"
    if m == 2:
        return f"cat notes-secret{i % 10}.txt", "ask"
    return f"ls -la dir{i}", ""


stop = threading.Event()
samples = []


def sampler():
    with open(os.path.join(a.out, "samples.csv"), "w") as f:
        f.write("t,rss_kb,cpu_pct,cputime,threads,fds,db_bytes,wal_bytes\n")
        t0 = time.time()
        n = 0
        while not stop.is_set():
            try:
                o = subprocess.run(["ps", "-o", "rss=,%cpu=,time=", "-p", str(a.pid)], capture_output=True, text=True).stdout.split()
                th = len(subprocess.run(["ps", "-M", "-p", str(a.pid)], capture_output=True, text=True).stdout.strip().splitlines()) - 1
                fds = ""
                if n % 5 == 0:
                    fds = len(subprocess.run(["lsof", "-p", str(a.pid)], capture_output=True, text=True).stdout.splitlines()) - 1
                db = os.path.getsize(a.db) if os.path.exists(a.db) else 0
                wal = os.path.getsize(a.db + "-wal") if os.path.exists(a.db + "-wal") else 0
                row = [round(time.time() - t0, 1), o[0], o[1], o[2], th, fds, db, wal]
                samples.append(row)
                f.write(",".join(map(str, row)) + "\n")
                f.flush()
            except Exception as e:  # daemon gone, etc.
                f.write(f"# {e}\n")
            n += 1
            stop.wait(1.0)


sse_stats = []


def sse_client(k, after):
    st = {"client": k, "events": 0, "last_seq": after, "controls": 0, "pings": 0, "ended": False, "gaps": 0}
    sse_stats.append(st)
    try:
        c = UConn(a.sock)
        c.request("GET", f"/v1/events?follow=1&after={after}", headers={"Accept": "text/event-stream"})
        r = c.getresponse()
        ev = None
        while not stop.is_set():
            line = r.readline()
            if not line:
                st["ended"] = True
                break
            line = line.decode().rstrip("\n")
            if line.startswith("event:"):
                ev = line[6:].strip()
            elif line.startswith(":"):
                st["pings"] += 1
            elif line.startswith("data:"):
                if ev == "control":
                    st["controls"] += 1
                    st["control"] = line[5:].strip()
                else:
                    d = json.loads(line[5:])
                    if d["seq"] != st["last_seq"] + 1:
                        st["gaps"] += 1  # seq is strictly increasing but not necessarily dense
                    if d["seq"] <= st["last_seq"]:
                        st["dup_or_reorder"] = st.get("dup_or_reorder", 0) + 1
                    st["last_seq"] = d["seq"]
                    st["events"] += 1
    except Exception as e:
        st["error"] = str(e)


env = dict(os.environ)
env.pop("PILOTHOUSE_SESSION_ID", None)


def run_shim(payload, timeout=700):
    t = time.time()
    p = subprocess.run([a.ph, "hook", "claude"], input=payload.encode(), capture_output=True, cwd=a.cwd, env=env, timeout=timeout)
    return t, (time.time() - t) * 1000, p.returncode, p.stdout.decode(), p.stderr.decode()


counter = {"i": 0}
clock = threading.Lock()
pre_results = []
stop_window = {}


def pre_worker(w):
    sid = f"soak-{w:02d}"
    # announce the session like Claude Code does
    run_shim(json.dumps({"hook_event_name": "SessionStart", "session_id": sid, "cwd": a.cwd, "source": "startup"}))
    while True:
        with clock:
            i = counter["i"]
            if i >= a.pretool:
                return
            counter["i"] += 1
        cmd, want = command_for(i)
        payload = json.dumps({"hook_event_name": "PreToolUse", "session_id": sid, "cwd": a.cwd, "tool_name": "Bash",
                              "tool_use_id": f"tu_{i}", "tool_input": {"command": cmd}})
        t, ms, rc, out, err = run_shim(payload)
        got = ""
        if out.strip():
            try:
                got = json.loads(out)["hookSpecificOutput"]["permissionDecision"]
            except Exception:
                got = "BADJSON:" + out[:80]
        pre_results.append({"i": i, "t": t, "ms": ms, "rc": rc, "want": want, "got": got, "err": err[:200]})


def stopper():
    if a.stop_secs <= 0 or a.stop_at >= a.pretool:
        return
    while counter["i"] < a.stop_at and not stop.is_set():
        time.sleep(0.01)
    if stop.is_set() or a.stop_secs <= 0:
        return
    stop_window["start"] = time.time()
    os.kill(a.pid, signal.SIGSTOP)
    time.sleep(a.stop_secs)
    os.kill(a.pid, signal.SIGCONT)
    stop_window["end"] = time.time()


perm_results = []
pcount = {"i": 0}


def perm_worker(k):
    while True:
        with clock:
            i = pcount["i"]
            if i >= a.perm:
                return
            pcount["i"] += 1
        sid = f"soak-{i % a.workers:02d}"
        payload = json.dumps({"hook_event_name": "PermissionRequest", "session_id": sid, "cwd": a.cwd, "tool_name": "Bash",
                              "tool_input": {"command": f"git push origin feature-{i}"}})
        t, ms, rc, out, err = run_shim(payload)
        beh = ""
        try:
            beh = json.loads(out)["hookSpecificOutput"]["decision"]["behavior"] if out.strip() else "no-opinion"
        except Exception:
            beh = "BADJSON:" + out[:80]
        perm_results.append({"i": i, "t": t, "ms": ms, "rc": rc, "behavior": beh})


def direct_bench(n):
    """Daemon-side latency without process spawn: POST /v1/hook on one keep-alive connection."""
    c = UConn(a.sock, timeout=10)
    lat = []
    for i in range(n):
        cmd, _ = command_for(i)
        env_ = {"agent": "claude", "payload": json.loads(json.dumps({"hook_event_name": "PreToolUse", "session_id": "soak-direct", "cwd": a.cwd,
                "tool_name": "Bash", "tool_use_id": f"d_{i}", "tool_input": {"command": cmd}})), "cwd": a.cwd}
        body = json.dumps(env_)
        t = time.perf_counter()
        c.request("POST", "/v1/hook", body=body, headers={"Content-Type": "application/json"})
        r = c.getresponse()
        r.read()
        lat.append((time.perf_counter() - t) * 1000)
    c.close()
    return lat


# ---- run ----
seq0 = api("/v1/snapshot")["seq"]
threading.Thread(target=sampler, daemon=True).start()
sse_threads = [threading.Thread(target=sse_client, args=(k, seq0), daemon=True) for k in range(a.sse)]
for t in sse_threads:
    t.start()
time.sleep(1)
t_start = time.time()
ths = [threading.Thread(target=pre_worker, args=(w,)) for w in range(a.workers)]
ths += [threading.Thread(target=perm_worker, args=(k,)) for k in range(a.perm_conc)]
st_thread = threading.Thread(target=stopper)
for t in ths:
    t.start()
st_thread.start()
for t in ths:
    t.join()
t_load = time.time() - t_start
st_thread.join()
direct = direct_bench(a.direct) if a.direct else []
time.sleep(3)
final_seq = api("/v1/snapshot")["seq"]
time.sleep(2)
stop.set()

# ---- analyse ----
sw = stop_window
in_stop = [r for r in pre_results if sw and sw["start"] - 0.05 <= r["t"] <= sw["end"]]
outside = [r for r in pre_results if not (sw and sw["start"] - 2.5 <= r["t"] <= sw["end"] + 0.5)]
wrong = [r for r in outside if r["got"] != r["want"]]
res = {
    "load_seconds": round(t_load, 1),
    "pretool_total": len(pre_results),
    "pretool_throughput_per_s": round(len(pre_results) / t_load, 1),
    "pretool_shim_ms_outside_stop": summary([r["ms"] for r in outside]),
    "pretool_wrong_decisions_outside_stop": len(wrong),
    "pretool_wrong_examples": wrong[:5],
    "pretool_nonzero_exit": sum(1 for r in pre_results if r["rc"] != 0),
    "stop_window_s": round(sw["end"] - sw["start"], 2) if sw else None,
    "stop_window": sw,
    "pretool_during_stop": summary([r["ms"] for r in in_stop]),
    "pretool_during_stop_outputs": sorted({(r["got"] or "no-opinion") for r in in_stop}),
    "pretool_during_stop_rc": sorted({r["rc"] for r in in_stop}),
    "perm_total": len(perm_results),
    "perm_ms": summary([r["ms"] for r in perm_results]),
    "perm_behaviors": {b: sum(1 for r in perm_results if r["behavior"] == b) for b in {r["behavior"] for r in perm_results}},
    "direct_hook_ms": summary(direct),
    "seq_start": seq0,
    "seq_final": final_seq,
    "sse": sse_stats,
}
if samples:
    rss = [int(s[1]) for s in samples]
    th = [int(s[4]) for s in samples]
    cpu = [float(s[2]) for s in samples]
    res["daemon"] = {"rss_mb_start": round(rss[0] / 1024, 1), "rss_mb_max": round(max(rss) / 1024, 1), "rss_mb_end": round(rss[-1] / 1024, 1),
                     "cpu_pct_max": max(cpu), "cpu_pct_median": statistics.median(cpu), "threads_max": max(th), "threads_end": th[-1],
                     "fds": [s[5] for s in samples if s[5] != ""], "cputime_end": samples[-1][3],
                     "db_mb_end": round(samples[-1][6] / 1e6, 2), "wal_mb_max": round(max(int(s[7]) for s in samples) / 1e6, 2)}
json.dump({"results": res, "pre": pre_results, "perm": perm_results}, open(os.path.join(a.out, "soak.json"), "w"))
r2 = dict(res)
r2["sse"] = {"clients": len(sse_stats), "events_min": min((s["events"] for s in sse_stats), default=0),
             "events_max": max((s["events"] for s in sse_stats), default=0),
             "last_seq_min": min((s["last_seq"] for s in sse_stats), default=0), "controls": sum(s["controls"] for s in sse_stats),
             "ended": sum(1 for s in sse_stats if s["ended"]), "errors": [s.get("error") for s in sse_stats if s.get("error")],
             "dup_or_reorder": sum(s.get("dup_or_reorder", 0) for s in sse_stats)}
if "daemon" in r2:
    r2["daemon"] = dict(r2["daemon"]); r2["daemon"]["fds"] = r2["daemon"]["fds"][:3] + ["…"] + r2["daemon"]["fds"][-3:]
print(json.dumps(r2, indent=1))
