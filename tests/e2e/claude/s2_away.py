"""Scenario 2 — away path with a simulated phone (run phone.mjs pair first;
state in $E2E_ROOT/phone.json). 2a: held, no native dialog, phone allows.
2a': phone denies with a message. 2b: away → here while held → handed back.
2c: hold_limit 15s (daemon restarted with new config) → expired → native dialog."""
import os, re, shutil, subprocess, sys, time
from common import *

W = f"{ROOT}/w_away"
shutil.rmtree(W, ignore_errors=True); os.makedirs(W)
PHONE = [ "node", os.path.join(HERE, "phone.mjs") ]
STATE = f"{ROOT}/phone.json"
DIALOG = r"Do you want to (create|make this edit|proceed)"

def phone_decide(behavior, scope, match, timeout=60, message=""):
    return subprocess.Popen(PHONE + ["decide", STATE, behavior, scope, match, str(timeout), message],
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)

def reqs(sid, **kw):
    return [r for r in requests() if r["session_id"] == sid and all(r.get(k) == v for k, v in kw.items())]

def sess(sid):
    return [x for x in ls()["sessions"] if x["id"] == sid][0]

def no_dialog_for(t, secs):
    end = time.time() + secs
    while time.time() < end:
        if re.search(DIALOG, t.text()):
            return False
        time.sleep(0.3)
    return True

def set_config(extra):
    open(f"{ROOT}/home/config.yaml", "w").write(
        "web:\n  addr: 127.0.0.1:7591\nnotify:\n  desktop: false\nupdate_check: false\n" + extra)

results = {}
t = claude(W, log=f"{ROOT}/s2.raw")
try:
    pass_startup(t)
    sid = wait_until(lambda: [s for s in ls()["sessions"] if s["cwd"].endswith("w_away") and s["state"] != "exited"], 20)[0]["id"]
    say("session", sid)
    say("away:", ph_person("away"))

    # 2a: held, phone allows
    t.type("Use the Write tool to create a.txt containing the word alpha. Do nothing else."); t.send("\r")
    r = wait_until(lambda: reqs(sid, status="open"), 90, what="held request")[0]
    say("held:", r["id"], r["summary"], "route=", r["route"], "expires", r["expires_at"])
    say("session state:", sess(sid)["state"], sess(sid).get("block_kind"))
    nd = no_dialog_for(t, 8); say("no native dialog while held (8s):", nd)
    say("screen while held:\n" + "\n".join(l for l in t.text().splitlines()[-12:] if l.strip()))
    p = phone_decide("allow", "once", "a.txt"); out = p.communicate(timeout=90)[0]; say("phone:", out.strip())
    ok = wait_until(lambda: os.path.exists(f"{W}/a.txt"), 60, what="a.txt")
    r = [x for x in requests() if x["id"] == r["id"]][0]; say("after phone allow:", r["status"], r.get("decision"))
    wait_until(lambda: sess(sid)["state"] == "waiting", 60, what="waiting")
    results["2a"] = (nd, bool(ok), r["status"])  #  and r["status"] not in ("open", "stepped_aside")

    # 2a': phone denies with a message
    t.type("Use the Write tool to create b.txt containing the word beta. If the write is denied, do not retry; just tell me the denial reason and stop."); t.send("\r")
    wait_until(lambda: reqs(sid, status="open"), 90, what="held request b")
    p = phone_decide("deny", "once", "b.txt", 60, "not now, use c.txt"); say("phone:", p.communicate(timeout=90)[0].strip())
    wait_until(lambda: sess(sid)["state"] == "waiting", 90, what="waiting after deny")
    time.sleep(1)
    rb = [x for x in requests() if x["session_id"] == sid and "b.txt" in x["summary"]]
    say("b requests:", [(x["id"], x["status"], x.get("decision")) for x in rb])
    say("b.txt exists?", os.path.exists(f"{W}/b.txt"), "| screen tail:", t.text().strip().splitlines()[-12:])
    results["2a-deny"] = (not os.path.exists(f"{W}/b.txt"), rb[0]["status"], "c.txt" in t.text())

    # 2a-local: away, held, but the person answers the (visible) native dialog
    t.type("Use the Write tool to create l.txt containing the word lima. Do nothing else."); t.send("\r")
    r = wait_until(lambda: reqs(sid, status="open"), 90, what="held request l")[0]
    t.wait(DIALOG, 10); time.sleep(1)
    t.send("1")
    wait_until(lambda: os.path.exists(f"{W}/l.txt"), 60, what="l.txt")
    time.sleep(3)
    r = [x for x in requests() if x["id"] == r["id"]][0]; say("l after local answer while held:", r["status"], r.get("decision"))
    p = phone_decide("allow", "once", "l.txt", 3); say("phone late decide:", p.communicate(timeout=30)[0].strip())
    print(ph("log", "--session", sid, "-n", "12"))
    results["2a-local"] = r["status"]
    wait_until(lambda: sess(sid)["state"] == "waiting", 60, what="waiting l")

    # 2b: away → here while held
    t.type("Use the Write tool to create d.txt containing the word delta. Do nothing else."); t.send("\r")
    r = wait_until(lambda: reqs(sid, status="open"), 90, what="held request d")[0]
    time.sleep(2)
    say("here:", ph_person("here"))
    t.wait(DIALOG, 20); say("native dialog present after here")
    time.sleep(1)
    r = [x for x in requests() if x["id"] == r["id"]][0]; say("d after here:", r["status"], r.get("route"))
    results["2b"] = r["status"] == "stepped_aside"
    t.send("1")
    wait_until(lambda: os.path.exists(f"{W}/d.txt"), 60, what="d.txt")
    wait_until(lambda: [x for x in requests() if x["id"] == r["id"]][0]["status"] == "answered_locally", 30, what="d answered_locally")
    say("d -> answered_locally")
    print(ph("log", "--session", sid, "-n", "60"))

    # 2c: hold limit 15s
    say("stop:", ph_person("stop"))
    set_config("approvals:\n  hold_limit: 15s\n")
    say("start:", ph("start"))
    time.sleep(1)
    say("away:", ph_person("away"))
    wait_until(lambda: sess(sid)["state"] == "waiting", 60, what="waiting")
    t.type("Use the Write tool to create e.txt containing the word echo. Do nothing else."); t.send("\r")
    r = wait_until(lambda: reqs(sid, status="open"), 90, what="held request e")[0]
    t0 = time.time(); say("held e:", r["id"], "created", r["created_at"], "expires", r["expires_at"])
    nd = no_dialog_for(t, 5)
    wait_until(lambda: [x for x in requests() if x["id"] == r["id"]][0]["status"] != "open", 40, every=0.2, what="e expired")
    dt = time.time() - t0
    r = [x for x in requests() if x["id"] == r["id"]][0]
    say(f"request left 'open' after {dt:.1f}s; no dialog for first 5s: {nd}; status={r['status']}")
    t.wait(DIALOG, 10)
    results["2c"] = (r["status"], round(dt, 1), nd)
    t.send("1")
    wait_until(lambda: os.path.exists(f"{W}/e.txt"), 60, what="e.txt")
    time.sleep(2)
    r = [x for x in requests() if x["id"] == r["id"]][0]; say("e after local answer:", r["status"])
    print(ph("log", "--session", sid, "-n", "30"))
    print(ph("explain", sid))
finally:
    say("RESULTS", results)
    ph_person("here")
    set_config("")
    t.send("\x03"); time.sleep(0.5); t.send("\x03"); t.wait_exit(10); t.close()
