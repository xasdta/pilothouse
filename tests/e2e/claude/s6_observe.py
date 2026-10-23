"""Scenario 6 — observability: TUI shows a held request live and answers it
with 'y'; CLI approve/deny run as the person; what happens when a desk
(stepped_aside) request is approved from the CLI; ls/log/explain accuracy."""
import os, shutil, subprocess, sys, time
from common import *

W = f"{ROOT}/w_obs"
shutil.rmtree(W, ignore_errors=True); os.makedirs(W)
results = {}
def sreqs(sid, **kw): return [r for r in requests() if r["session_id"] == sid and all(r.get(k) == v for k, v in kw.items())]
def sess(sid): return [x for x in ls()["sessions"] if x["id"] == sid][0]
def tui(rx, keys):
    out = f"{ROOT}/tui.out"
    subprocess.run([sys.executable, DETACH, sys.executable, os.path.join(HERE, "tui_driver.py"), out, rx, keys], env=env(), timeout=120)
    return open(out).read()

say("away:", ph_person("away"))
t = claude(W, log=f"{ROOT}/s6.raw")
try:
    pass_startup(t)
    sid = wait_until(lambda: [s for s in ls()["sessions"] if s["cwd"].endswith("w_obs") and s["state"] != "exited"], 20)[0]["id"]
    # 1. TUI shows held request live, 'y' allows
    t.type("Use the Write tool to create t1.txt containing tango. Do nothing else."); t.send("\r")
    r = wait_until(lambda: sreqs(sid, status="open"), 90, what="held")[0]
    print(ph("ls"))
    scr = tui(r"t1\.txt", "y")
    print(scr)
    wait_until(lambda: os.path.exists(f"{W}/t1.txt"), 30, what="t1 via TUI")
    r = [x for x in requests() if x["id"] == r["id"]][0]; say("t1:", r["status"], r.get("decision"))
    results["tui-y"] = r["status"] == "decided" and (r.get("decision") or {}).get("by") == "tui"
    wait_until(lambda: sess(sid)["state"] == "waiting", 60)
    # 2. CLI deny with message (as the person)
    t.type("Use the Write tool to create t2.txt containing two. If denied, do not retry; quote the reason and stop."); t.send("\r")
    r = wait_until(lambda: sreqs(sid, status="open"), 90, what="held t2")[0]
    say("deny:", ph_person("deny", r["id"][:6], "-m", "QA says no"))
    wait_until(lambda: sess(sid)["state"] == "waiting", 60)
    results["cli-deny"] = (not os.path.exists(f"{W}/t2.txt"), "QA says no" in t.text())
    # agent trying to approve itself via CLI from inside the agent tree is refused
    t.type("Use the Write tool to create t3.txt containing three."); t.send("\r")
    r = wait_until(lambda: sreqs(sid, status="open"), 90, what="held t3")[0]
    out = ph("approve", r["id"]); say("approve from agent tree:", out.strip()[:200])
    results["guard-refuses"] = "refused" in out or "agent" in out
    print(ph("explain", sid))
    say("approve as person:", ph_person("approve", r["id"]))
    wait_until(lambda: os.path.exists(f"{W}/t3.txt"), 30)
    wait_until(lambda: sess(sid)["state"] == "waiting", 60)
    # 3. desk mode: stepped_aside request approved from CLI — does Claude proceed?
    say("here:", ph_person("here"))
    t.type("Use the Write tool to create t4.txt containing four. Do nothing else."); t.send("\r")
    r = wait_until(lambda: sreqs(sid, status="stepped_aside"), 90, what="stepped aside t4")[0]
    rc, out = ph_person("approve", r["id"]); say("CLI approve of stepped_aside request:", rc, out.strip()[:300])
    time.sleep(5)
    r = [x for x in requests() if x["id"] == r["id"]][0]
    dialog = "Do you want to create t4.txt" in t.text()
    say("t4 after CLI approve: status", r["status"], "| native dialog still shown:", dialog, "| file:", os.path.exists(f"{W}/t4.txt"))
    results["approve-stepped-aside"] = (rc, r["status"], dialog, os.path.exists(f"{W}/t4.txt"))
    if dialog:
        t.send("3"); time.sleep(3)  # answer No in the dialog
        r = [x for x in requests() if x["id"] == r["id"]][0]; say("t4 after native No:", r["status"], r.get("decision"), "file:", os.path.exists(f"{W}/t4.txt"))
    print(ph("log", "--session", sid, "-n", "50"))
    print(ph("ls", "-a"))
finally:
    say("RESULTS", results)
    ph_person("here")
    t.send("\x03"); time.sleep(0.5); t.send("\x03"); t.wait_exit(10); t.close()
