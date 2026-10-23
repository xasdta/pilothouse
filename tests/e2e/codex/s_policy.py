"""Policy allow / ask on Codex (TUI, -a on-request, presence here).
  P1  allow rule on `touch allowpol*`: Codex ignores PreToolUse allow, so its
      own approval still happens: PermissionRequest reaches Pilothouse
      (route=terminal) and the native dialog shows.
  P2  ask rule on `echo*`: Codex can't "ask" at PreToolUse, so Pilothouse
      turns ask into a deny that tells the agent a person must approve it.
  P3  deny rule on `git push*` in the TUI: blocked before any prompt.
Writes $ROOT/home/policy.yaml (restored afterwards) and reloads policy."""
import os, re, shutil, time
from common import *

DIALOG = r"Would you like to run the following command"
POL = f"{ROOT}/home/policy.yaml"
results = {}


def check(name, ok, detail=""):
    results[name] = ok
    say(("PASS " if ok else "FAIL ") + name, detail)


def prompt(t, text):
    t.send(text); time.sleep(0.4); t.send("\r")


shutil.copy(POL, POL + ".s_policy-bak")
open(POL, "w").write("""version: 1
rules:
  - name: allow-touch
    match: { kind: shell, command: "touch allowpol*" }
    action: allow
  - name: ask-echo
    match: { kind: shell, command: "echo*" }
    action: ask
  - name: no-push
    match: { kind: shell, command: "git push*" }
    action: deny
    message: "QA6: pushing is blocked by Pilothouse policy."
""")
time.sleep(1.5)  # the daemon reloads policy.yaml on change
for f in ("allowpol.txt", "asked.txt"):
    if os.path.exists(os.path.join(REPO, f)):
        os.unlink(os.path.join(REPO, f))
before = {s["id"] for s in ls()["sessions"]}
say("here:", ph_person("here"))
t = codex_tui("--no-alt-screen", "-a", "on-request", log=f"{ROOT}/s_policy.raw")
try:
    t.wait(r"Ask Codex to do anything", 30)
    prompt(t, "scenario=allowpol")
    t.wait(DIALOG, 30)
    sid = [s for s in ls()["sessions"] if s["id"] not in before][-1]["id"]
    rq = [r for r in requests() if r["session_id"] == sid and "allowpol" in r["summary"]]
    t.send("y")
    t.wait(r"MOCK-DONE scenario=allowpol", 30)
    log = ph("log", "--session", sid) if False else ph("log")
    check("P1 policy allow is ignored by Codex; permission still routed to the person",
          bool(rq) and rq[-1]["route"] == "terminal" and os.path.exists(os.path.join(REPO, "allowpol.txt"))
          and "rule=allow-touch action=allow" in log,
          f"request={rq[-1]['status'] if rq else None}/{rq[-1]['route'] if rq else None}")

    prompt(t, "scenario=askpol")
    t.wait(r"MOCK-DONE scenario=askpol", 30)
    rq = [r for r in requests() if r["session_id"] == sid and "asked" in r["summary"]]
    screen = t.text()
    got = tool_output("call_askpol_0") or ""
    check("P2 policy ask becomes a deny on Codex (a person must approve)",
          not os.path.exists(os.path.join(REPO, "asked.txt")) and "a person must approve" in got
          and "rule=ask-echo action=ask" in ph("log"),
          f"requests={len(rq)} hook-failure-shown={'failed' in screen.lower()}")

    prompt(t, "scenario=push")
    t.wait(r"MOCK-DONE scenario=push", 30)
    got = tool_output("call_push_0") or ""
    check("P3 deny rule blocks in the TUI too", "QA6: pushing is blocked" in got, got[:160])
finally:
    try:
        t.send("\x03"); time.sleep(0.4); t.send("\x03"); time.sleep(1)
    except OSError:
        pass
    try:
        os.kill(t.pid, 9)
    except OSError:
        pass
    shutil.move(POL + ".s_policy-bak", POL)
    time.sleep(1.5)
    ph_person("auto")
print("SUMMARY", results)
