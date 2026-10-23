"""Scenario 5 — tasks with real Claude. A `claude` wrapper in $E2E_ROOT/bin
(first on PATH) adds --settings/--model so the task's agent uses the isolated
hooks. Approvals: approvals.always_hold + the simulated phone (scope=session)."""
import json, os, shutil, subprocess, threading, time, re
from common import *

R = f"{ROOT}/w_task"
shutil.rmtree(R, ignore_errors=True); os.makedirs(R)
subprocess.run("git init -q -b main && echo '# t' > README.md && git add . && git -c user.email=a@b -c user.name=qa commit -qm init", shell=True, cwd=R, check=True)
PHONE = ["node", os.path.join(HERE, "phone.mjs")]
STATE = f"{ROOT}/phone.json"
E = env(); E["PATH"] = f"{ROOT}/bin:" + E["PATH"]
E["GIT_AUTHOR_NAME"] = E["GIT_COMMITTER_NAME"] = "qa"; E["GIT_AUTHOR_EMAIL"] = E["GIT_COMMITTER_EMAIL"] = "qa@example.com"
def tph(*a, cwd=R):
    r = subprocess.run([PH, *a], env=E, cwd=cwd, capture_output=True, text=True, timeout=120); return r.returncode, r.stdout + r.stderr
def set_config(extra):
    open(f"{ROOT}/home/config.yaml", "w").write("web:\n  addr: 127.0.0.1:7591\nnotify:\n  desktop: false\nupdate_check: false\n" + extra)

results = {}
stop = threading.Event()
approved = []
def phone_loop():
    while not stop.is_set():
        p = subprocess.run(PHONE + ["decide", STATE, "allow", "session", "", "2"], capture_output=True, text=True)
        if p.returncode == 0: approved.append(p.stdout.split("\n")[0][:160])

say("stop:", ph_person("stop")); set_config("approvals:\n  always_hold: true\n"); say("start:", ph("start").strip())
say("auto:", ph_person("auto"))
th = threading.Thread(target=phone_loop, daemon=True); th.start()
A = None
try:
    rc, out = tph("task", "new", "create hello.go that prints hi and commit it"); say("task new:", rc, out)
    tid = re.search(r"\bt_[a-z0-9]+", out).group(0)
    rc, out = tph("task", "ls"); say("task ls:\n" + out)
    tasks = json.loads(ph_get("/v1/tasks")); t = [x for x in tasks if x["id"] == tid][0]; say("task:", t)
    sid = t.get("session_id"); wt = t.get("worktree"); br = t.get("branch")
    # attach to get past the folder-trust prompt
    A = Term([PH, "attach", sid], env=E, cwd=R, log=f"{ROOT}/s5.raw")
    time.sleep(4)
    scr = A.text(); say("attached screen (trust prompt?):", "trust" in scr.lower())
    if "Yes, I trust this folder" in scr:
        A.send("\x1b[B"); time.sleep(0.5); A.send("\r")
    time.sleep(3); A.send("\x1c"); A.wait_exit(5)
    def committed():
        return "hello.go" in subprocess.run(["git", "log", br, "--name-only", "--format="], cwd=R, capture_output=True, text=True).stdout
    try:
        wait_until(committed, 240, every=2, what="hello.go committed on task branch")
        results["agent-committed"] = True
    except TimeoutError as e:
        results["agent-committed"] = False; say(str(e))
        A = Term([PH, "attach", sid], env=E, cwd=R); time.sleep(3); say("screen:\n" + A.text()); A.send("\x1c"); A.wait_exit(5)
    say("phone approvals:", approved)
    wait_until(lambda: [s for s in ls()["sessions"] if s["id"] == sid][0]["state"] in ("waiting", "exited"), 120)
    rc, out = tph("task", "ls"); say("task ls:\n" + out)
    rc, out = tph("task", "diff", tid); say("task diff:", rc, "\n" + out[:800]); results["diff-has-hello"] = "hello.go" in out
    print(ph("log", "--session", sid, "-n", "40"))
    rc, out = tph("task", "done", tid, "--keep"); say("task done --keep (agent running):", rc, out.strip())
    results["done-refuses-running"] = rc != 0
    A = Term([PH, "attach", sid], env=E, cwd=R); time.sleep(2); A.type("/exit"); time.sleep(0.5); A.send("\r"); A.wait_exit(30); time.sleep(6)
    rc, out = tph("task", "done", tid, "--keep"); say("task done --keep:", rc, out.strip())
    results["done-keep"] = rc == 0 and os.path.isdir(wt)
    rc, out = tph("task", "ls"); say("task ls after done:\n" + out)
    say("branches:", subprocess.run(["git", "branch", "-a"], cwd=R, capture_output=True, text=True).stdout)
    # abandon a second task right away
    rc, out = tph("task", "new", "--name", "throwaway", "say hi"); say("task new 2:", rc, out.strip())
    tid2 = re.search(r"\bt_[a-z0-9]+", out).group(0)
    t2 = [x for x in json.loads(ph_get("/v1/tasks")) if x["id"] == tid2][0]
    rc, out = tph("task", "abandon", tid2); say("abandon (running):", rc, out.strip())
    if rc != 0:
        rc, out = tph("task", "abandon", tid2, "--force"); say("abandon --force:", rc, out.strip())
    time.sleep(2)
    br2 = subprocess.run(["git", "branch", "--list", t2["branch"]], cwd=R, capture_output=True, text=True).stdout.strip()
    results["abandon"] = rc == 0 and not os.path.exists(t2["worktree"]) and not br2
    say("abandon: worktree exists?", os.path.exists(t2["worktree"]), "branch left:", br2)
    s2 = [s for s in ls()["sessions"] if s["id"] == t2.get("session_id")]
    say("abandoned task session:", [(s["id"], s["state"]) for s in s2])
    hp = subprocess.run(["pgrep", "-fl", "__holder"], capture_output=True, text=True).stdout; say("holders left:", hp)
    rc, out = tph("task", "ls"); say("task ls final:\n" + out)
    say("worktree list:", subprocess.run(["git", "worktree", "list"], cwd=R, capture_output=True, text=True).stdout)
finally:
    stop.set(); say("RESULTS", results)
    if A: A.close()
    set_config("")
