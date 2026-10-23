"""Scenario 4 — wrap mode with real Claude: `pilothouse run claude` in a PTY,
registration as wrapped, state, detach (ctrl+\\), attach, daemon kill/restart
survival, second attach steals, exit code propagation (generic + claude)."""
import os, shutil, signal, subprocess, time
from common import *

W = f"{ROOT}/w_wrap"
shutil.rmtree(W, ignore_errors=True); os.makedirs(W)
results = {}
def sess(sid): return [x for x in ls()["sessions"] if x["id"] == sid][0]
def daemon_pid():
    out = ph("status"); import re
    m = re.search(r"pid (\d+)", out); return int(m.group(1)) if m else None

# generic exit-code propagation and L1 OSC parsing first (no Claude needed)
g = Term([PH, "run", "-n", "oscprobe", "--", "sh", "-c",
          r"printf '\033]7501;state=working\033\\'; sleep 3; printf '\033]7501;state=blocked;kind=permission\007'; sleep 3; printf '\033]7501;state=idle\033\\'; sleep 3; exit 7"],
         env=env(), cwd=W)
time.sleep(1.5)
gs = [s for s in ls()["sessions"] if s["title"] == "oscprobe"]
say("generic wrapped session:", [(s["id"], s["mode"], s["state"], s.get("state_layer")) for s in gs])
time.sleep(3); s = sess(gs[0]["id"]); say("after OSC blocked:", s["state"], s.get("block_kind"), s.get("state_layer"))
results["osc-blocked"] = s["state"] == "blocked"
time.sleep(3); s = sess(gs[0]["id"]); say("after OSC idle:", s["state"], s.get("state_layer"))
results["osc-idle-waiting"] = s["state"] == "waiting"
code = g.wait_exit(20); time.sleep(1)
s = sess(gs[0]["id"]); say("run exit code:", code, "| session:", s["state"], s.get("exit_code"))
results["exit-code-generic"] = (code, s.get("exit_code"))

A = Term([PH, "run", "--", "claude", "--settings", SETTINGS, "--model", "haiku", "--permission-mode", "default"],
         env=env(), cwd=W, log=f"{ROOT}/s4a.raw")
B = None
try:
    pass_startup(A)
    sid = wait_until(lambda: [s for s in ls()["sessions"] if s["cwd"].endswith("w_wrap") and s["state"] != "exited" and s["title"] != "oscprobe"], 20)
    say("sessions in w_wrap:", [(s["id"], s["mode"], s["state"], bool(s.get("holder_sock"))) for s in sid])
    results["single-wrapped-session"] = len(sid) == 1 and sid[0]["mode"] == "wrapped"
    sid = sid[0]["id"]
    print(ph("ls"))
    A.type("Reply with just the word pong."); A.send("\r")
    A.wait(r"pong", 60)
    wait_until(lambda: sess(sid)["state"] == "waiting", 60, what="waiting")
    s = sess(sid); say("after turn:", s["state"], s.get("state_layer"), "| agent_session_id", s.get("agent_session_id"))
    raw = A.rawtext(); results["claude-emitted-osc7501"] = b"\x1b]7501" in raw
    say("claude emitted OSC 7501 in wrap?", results["claude-emitted-osc7501"])
    # detach
    A.send("\x1c"); code = A.wait_exit(10)
    say("detach: run exited", code, "| tail:", A.text().strip().splitlines()[-2:] if A.text().strip() else "")
    time.sleep(1); s = sess(sid); say("after detach:", s["state"], "pid", s.get("pid"))
    results["detach"] = code is not None and s["state"] != "exited"
    # attach
    B = Term([PH, "attach", sid[:6]], env=env(), cwd=W, log=f"{ROOT}/s4b.raw")
    B.wait(r"pong", 15); say("attached; screen shows prior conversation")
    B.type("Reply with just the word ping2."); B.send("\r")
    B.wait(r"ping2.*ping2|⏺ ping2", 60)
    wait_until(lambda: sess(sid)["state"] == "waiting", 60)
    results["attach"] = True
    # second attach steals
    C = Term([PH, "attach", sid], env=env(), cwd=W)
    time.sleep(3); bc = B.wait_exit(5)
    say("second attach: first client exit", bc, "msg:", [l for l in B.text().splitlines() if l.strip()][-2:])
    results["second-attach-steals"] = bc is not None
    C.send("\x1c"); C.wait_exit(5)
    # kill the daemon hard
    dp = daemon_pid(); say("killing daemon", dp); os.kill(dp, signal.SIGKILL); time.sleep(2)
    hp = subprocess.run(["pgrep", "-f", f"__holder --id {sid}"], capture_output=True, text=True).stdout.split()
    say("holder pids after daemon kill:", hp)
    results["holder-survives-kill"] = bool(hp)
    # attach works with no daemon, and claude still works (hooks fail open)
    D = Term([PH, "attach", sid], env=env(), cwd=W, log=f"{ROOT}/s4d.raw")
    time.sleep(2)
    D.type("Reply with just the word nodaemon."); D.send("\r")
    try:
        D.wait(r"⏺ nodaemon", 60); results["works-without-daemon"] = True
    except TimeoutError as e:
        results["works-without-daemon"] = False; say("no-daemon turn failed:", str(e)[-800:])
    D.send("\x1c"); D.wait_exit(5)
    say("start:", ph("start").strip()); time.sleep(5)
    s = sess(sid); say("after restart:", s["mode"], s["state"], s.get("holder_sock"))
    print(ph("ls"))
    results["reregistered"] = s["state"] != "exited"
    # exit claude from an attached client: /exit, check exit code propagates to attach
    E = Term([PH, "attach", sid], env=env(), cwd=W, log=f"{ROOT}/s4e.raw")
    time.sleep(2); E.type("/exit"); time.sleep(0.5); E.send("\r")
    ec = E.wait_exit(30); time.sleep(6)
    s = sess(sid); say("attach exit:", ec, "| session:", s["state"], s.get("exit_code"))
    results["claude-exit"] = (ec, s["state"], s.get("exit_code"))
    hp = subprocess.run(["pgrep", "-f", f"__holder --id {sid}"], capture_output=True, text=True).stdout.split()
    say("holder still running after child exit +6s:", hp)
    print(ph("log", "--session", sid, "-n", "40"))
finally:
    say("RESULTS", results)
    for t in (A, B):
        if t: t.close()
    subprocess.run(["pkill", "-f", "__holder --id"], capture_output=True)
