"""Away path with the real Codex TUI (-a on-request) and a simulated phone.

Needs: daemon running, hooks installed, phone paired (state in $ROOT/phone.json).
  A1  away: escalated `touch escalated.txt` is held (route=devices, no native
      dialog); phone allows → command runs, file exists.
  A2  away: phone denies with a message → command not run, model gets message.
  A3  away: phone answers `mkdir -p sess_a` with allow+session → the next
      `mkdir -p sess_b` is allowed by the grant without a person.
  A4  away → here while held: request handed back, native dialog appears.
Prints PASS/FAIL lines."""
import os, re, subprocess, time
from common import *

DIALOG = r"Would you like to run the following command"
STATE = f"{ROOT}/phone.json"
results = {}


def phone(behavior, scope, match, timeout=60, message=""):
    return subprocess.Popen(["node", PHONE, "decide", STATE, behavior, scope, match, str(timeout), message],
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)


def prompt(t, text):
    t.send(text); time.sleep(0.4); t.send("\r")


def done(t, name, timeout=60):
    return t.wait(rf"MOCK-DONE scenario={name}\b.*?'", timeout=timeout)


def reqs_for(sid):
    return [r for r in requests() if r["session_id"] == sid]


def no_dialog_for(t, secs):
    end = time.time() + secs
    while time.time() < end:
        if re.search(DIALOG, t.text()):
            return False
        time.sleep(0.3)
    return True


def check(name, ok, detail=""):
    results[name] = ok
    say(("PASS " if ok else "FAIL ") + name, detail)


for f in ("escalated.txt", "escalated2.txt"):
    if os.path.exists(os.path.join(REPO, f)):
        os.unlink(os.path.join(REPO, f))
for d in ("sess_a", "sess_b"):
    if os.path.isdir(os.path.join(REPO, d)):
        os.rmdir(os.path.join(REPO, d))

before = {s["id"] for s in ls()["sessions"]}
say("away:", ph_person("away"))
t = codex_tui("--no-alt-screen", "-a", "on-request", log=f"{ROOT}/s_away.raw")
try:
    t.wait(r"Ask Codex to do anything", 30)
    # A1 ------------------------------------------------------------------
    p = phone("allow", "once", "touch escalated.txt")
    prompt(t, "scenario=touch_esc")
    sid = wait_until(lambda: [s for s in ls()["sessions"] if s["agent"] == "codex" and s["id"] not in before],
                     20, what="new session")[-1]["id"]
    say("session", sid)
    out = p.communicate(timeout=90)[0]
    say("phone:", out.strip())
    m = done(t, "touch_esc")
    r = [x for x in reqs_for(sid) if "touch escalated.txt" in x["summary"]]
    check("A1 held for devices, phone allow runs command",
          os.path.exists(os.path.join(REPO, "escalated.txt")) and r and r[-1]["route"] == "devices"
          and r[-1]["status"] == "decided" and not re.search(DIALOG, t.text()),
          f"route={r[-1]['route'] if r else None} status={r[-1]['status'] if r else None} reply={m.group(0)[:160]!r}")

    # A2 ------------------------------------------------------------------
    p = phone("deny", "once", "touch escalated2.txt", message="QA6 phone says no")
    prompt(t, "scenario=touch_esc2")
    out = p.communicate(timeout=90)[0]
    say("phone:", out.strip())
    done(t, "touch_esc2")
    got = tool_output("call_touch_esc2_0") or ""
    check("A2 phone deny blocks command and model sees message",
          not os.path.exists(os.path.join(REPO, "escalated2.txt")) and "QA6 phone says no" in got,
          f"tool output to model={got[:200]!r}")

    # A3 ------------------------------------------------------------------
    p = phone("allow", "session", "mkdir -p sess_a")
    prompt(t, "scenario=grant1")
    say("phone:", p.communicate(timeout=90)[0].strip())
    done(t, "grant1")
    prompt(t, "scenario=grant2")
    m = done(t, "grant2", timeout=60)
    r = [x for x in reqs_for(sid) if "sess_b" in x["summary"]]
    check("A3 allow-for-session grant answers the next similar request",
          os.path.isdir(os.path.join(REPO, "sess_b")) and r and (r[-1].get("decision") or {}).get("by", "").startswith("grant:"),
          f"req={r[-1] if r else None}")

    # A4 ------------------------------------------------------------------
    prompt(t, "scenario=ls_esc")
    wait_until(lambda: [x for x in reqs_for(sid) if "ls -la /" in x["summary"] and x["status"] == "open"], 30,
               what="held ls request")
    held_ok = no_dialog_for(t, 3)
    say("here:", ph_person("here"))
    t.wait(DIALOG, 20)
    r = [x for x in reqs_for(sid) if "ls -la /" in x["summary"]]
    t.send("y")
    done(t, "ls_esc")
    r2 = [x for x in reqs_for(sid) if "ls -la /" in x["summary"]]
    check("A4 coming back hands a held request to the native dialog",
          held_ok and r and r[-1]["status"] in ("stepped_aside", "answered_locally"),
          f"after here={r[-1]['status'] if r else None} after y={r2[-1]['status'] if r2 else None}")
finally:
    try:
        t.send("\x03"); time.sleep(0.5); t.send("\x03"); time.sleep(1)
    except OSError:
        pass
    try:
        os.kill(t.pid, 9)
    except OSError:
        pass
    ph_person("auto")
print("SUMMARY", results)
