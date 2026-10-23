"""Scenario 3 — 'Allow for session' from the phone creates a grant; a second
similar command is auto-allowed with no new request; a deny rule still wins.
Needs the policy rule no-forbidden-commits (see README) and a paired phone."""
import os, shutil, subprocess, time
from common import *

W = f"{ROOT}/w_grant"
shutil.rmtree(W, ignore_errors=True); os.makedirs(W)
subprocess.run("git init -q && git -c user.email=a@b -c user.name=qa commit -q --allow-empty -m init", shell=True, cwd=W, check=True)
PHONE = ["node", os.path.join(HERE, "phone.mjs")]
STATE = f"{ROOT}/phone.json"
def sreqs(sid): return [r for r in requests() if r["session_id"] == sid]
def sess(sid): return [x for x in ls()["sessions"] if x["id"] == sid][0]
def commits(): return subprocess.run(["git", "log", "--format=%s"], cwd=W, capture_output=True, text=True).stdout.split()

results = {}
say("away:", ph_person("away"))
t = claude(W, log=f"{ROOT}/s3.raw")
try:
    pass_startup(t)
    sid = wait_until(lambda: [s for s in ls()["sessions"] if s["cwd"].endswith("w_grant") and s["state"] != "exited"], 20)[0]["id"]
    say("session", sid)
    t.type("Run exactly this with the Bash tool and nothing else: git -c user.email=a@b -c user.name=qa commit --allow-empty -m one"); t.send("\r")
    wait_until(lambda: [r for r in sreqs(sid) if r["status"] == "open"], 90, what="held")
    p = subprocess.run(PHONE + ["decide", STATE, "allow", "session", "commit", "30"], capture_output=True, text=True); say("phone:", p.stdout.strip()[:300])
    wait_until(lambda: "one" in commits(), 60, what="commit one")
    wait_until(lambda: sess(sid)["state"] == "waiting", 60)
    n1 = len(sreqs(sid)); say("requests so far:", [(r["id"], r["status"], (r.get("decision") or {}).get("scope")) for r in sreqs(sid)])
    t.type("Run exactly this with the Bash tool and nothing else: git -c user.email=a@b -c user.name=qa commit --allow-empty -m two"); t.send("\r")
    wait_until(lambda: "two" in commits() or [r for r in sreqs(sid) if r["status"] == "open"], 90, what="commit two or request")
    time.sleep(1)
    n2 = len(sreqs(sid)); results["grant-auto-allow"] = ("two" in commits(), n2 == n1)
    say("after second command: commit two?", "two" in commits(), "new requests:", n2 - n1)
    wait_until(lambda: sess(sid)["state"] == "waiting", 60)
    t.type("Run exactly this with the Bash tool and nothing else: git -c user.email=a@b -c user.name=qa commit --allow-empty -m forbidden . If it is blocked, do not retry, just stop."); t.send("\r")
    wait_until(lambda: sess(sid)["state"] == "waiting", 90, what="waiting after forbidden")
    time.sleep(1)
    results["deny-wins"] = ("forbidden" not in commits(), len(sreqs(sid)) == n2)
    say("forbidden committed?", "forbidden" in commits(), "| screen:", [l for l in t.text().splitlines() if "forbidden" in l.lower() or "block" in l.lower()][-4:])
    # an ask rule must also beat the grant
    t.type("Run exactly this with the Bash tool and nothing else: git -c user.email=a@b -c user.name=qa commit --allow-empty -m askme"); t.send("\r")
    r = wait_until(lambda: [r for r in sreqs(sid) if r["status"] == "open"] or ("askme" in commits() and "x"), 90, what="askme held or ran")
    results["ask-beats-grant"] = isinstance(r, list)
    say("askme:", "held for a person" if isinstance(r, list) else "RAN WITHOUT ASKING")
    if isinstance(r, list):
        subprocess.run(PHONE + ["decide", STATE, "deny", "once", "askme", "10", "no"], capture_output=True)
    wait_until(lambda: sess(sid)["state"] == "waiting", 60)
    # how wide is the grant? an unrelated git subcommand
    t.type("Run exactly this with the Bash tool and nothing else: git tag qa-v1"); t.send("\r")
    r = wait_until(lambda: [r for r in sreqs(sid) if r["status"] == "open"] or ("qa-v1" in subprocess.run(["git","tag"],cwd=W,capture_output=True,text=True).stdout and "x"), 90, what="tag held or ran")
    results["grant-covers-git-tag"] = not isinstance(r, list)
    say("git tag after granting 'git -c .. commit':", "held" if isinstance(r, list) else "AUTO-ALLOWED by the grant")
    if isinstance(r, list):
        subprocess.run(PHONE + ["decide", STATE, "deny", "once", "tag", "10", "no"], capture_output=True)
    wait_until(lambda: sess(sid)["state"] == "waiting", 60)
    # chained command must not ride on the grant
    t.type("Run exactly this with the Bash tool and nothing else: git -c user.email=a@b -c user.name=qa commit --allow-empty -m three && touch chained.txt"); t.send("\r")
    r = wait_until(lambda: [r for r in sreqs(sid) if r["status"] == "open"] or ("three" in commits() and "x"), 90, what="chained held or ran")
    results["chained-not-granted"] = isinstance(r, list)
    say("chained command:", "held for a person" if isinstance(r, list) else "RAN WITHOUT ASKING")
    if isinstance(r, list):
        subprocess.run(PHONE + ["decide", STATE, "deny", "once", "commit", "10", "no"], capture_output=True)
    wait_until(lambda: sess(sid)["state"] == "waiting", 60)
    print(ph("log", "--session", sid, "-n", "40"))
finally:
    say("RESULTS", results)
    ph_person("here")
    t.send("\x03"); time.sleep(0.5); t.send("\x03"); t.wait_exit(10); t.close()
