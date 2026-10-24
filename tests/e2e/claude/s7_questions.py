"""Scenario 7 — questions and plans (0.2 F1) with real Claude Code.

Run phone.mjs pair first (state in $E2E_ROOT/phone.json).
7a  away: AskUserQuestion is held as a question request and answered from
    the simulated phone through the signed v2 respond endpoint: an option
    label, then the person's own words; a decline reaches Claude too.
7a' away: two questions (one multi-select) answered with `pilothouse answer`
    using option numbers.
7b  at the desk: Pilothouse steps aside, Claude's own question dialog
    shows, the answer given there closes the request as answered_locally.
7c  plan mode, away: ExitPlanMode is held as a plan request; the phone
    asks for changes (Claude revises the plan), then approves (Claude
    proceeds and does the plan).
Plan files go to <workdir>/plans (project setting plansDirectory), never
to ~/.claude/plans."""
import json, os, re, shutil, subprocess, time
from common import *

PHONE = ["node", os.path.join(HERE, "phone.mjs")]
STATE = f"{ROOT}/phone.json"


def workdir(name):
    w = f"{ROOT}/{name}"
    shutil.rmtree(w, ignore_errors=True)
    os.makedirs(f"{w}/.claude")
    with open(f"{w}/.claude/settings.json", "w") as f:
        json.dump({"plansDirectory": "plans"}, f)
    return w


def respond(checkpoint, match, payload, timeout=90):
    p = subprocess.run(PHONE + ["respond", STATE, checkpoint, match, str(timeout), json.dumps(payload)],
                       capture_output=True, text=True, timeout=timeout + 30)
    out = (p.stdout + p.stderr).strip()
    say("phone:", out)
    return p.returncode, out


def reqs(sid, **kw):
    return [r for r in requests() if r["session_id"] == sid and all(r.get(k) == v for k, v in kw.items())]


def req(rid):
    return [r for r in requests() if r["id"] == rid][0]


def sess(sid):
    return [x for x in ls()["sessions"] if x["id"] == sid][0]


def held(sid, checkpoint, not_ids=(), timeout=120):
    return wait_until(lambda: [r for r in reqs(sid, status="open", checkpoint=checkpoint) if r["id"] not in not_ids],
                      timeout, what=f"held {checkpoint}")[0]


def prompt(t, text):
    t.type(text)
    time.sleep(0.3)
    t.send("\r")


def tail(t, n=14):
    return "\n".join(l for l in t.text().splitlines()[-n:] if l.strip())


def start(w, log, mode="default"):
    t = claude(w, log=log, mode=mode)
    pass_startup(t)
    sid = wait_until(lambda: [s for s in ls()["sessions"] if s["cwd"] == w and s["state"] != "exited"], 30,
                     what="session")[0]["id"]
    say("session", sid, "in", w)
    return t, sid


results = {}
W = workdir("w_ask")
t, sid = start(W, f"{ROOT}/s7a.raw")
try:
    say("away:", ph_person("away"))

    # 7a: a label from the phone.
    prompt(t, "Use the AskUserQuestion tool exactly once with one question 'Which color do you prefer?' "
              "(header 'Color') and two options: Red and Blue. After you receive the answer, reply with only "
              "the chosen color in capital letters and nothing else.")
    r = held(sid, "question")
    s = sess(sid)
    say("held:", r["id"], r["summary"], "route=", r["route"], "questions=", json.dumps(r["questions"]))
    say("session while held:", s["state"], s.get("block_kind"), s.get("state_detail"))
    say("screen while held:\n" + tail(t))
    rc, out = respond("question", "Which color", {"behavior": "allow", "answers_list": ["Blue"]})
    t.wait(r"\bBLUE\b", 90)
    r = req(r["id"])
    say("after phone answer:", r["status"], r.get("decision"))
    results["7a-label"] = (rc == 0 and "replay 403" in out, r["status"], r["decision"]["answers"],
                           s["block_kind"], s.get("state_detail"))
    wait_until(lambda: sess(sid)["state"] == "waiting", 60, what="waiting")

    # 7a: the person's own words (AllowOther).
    prompt(t, "Use the AskUserQuestion tool exactly once with one question 'Which city should the trip start in?' "
              "(header 'City') and two options: Paris and Rome. After you receive the answer, reply with only the "
              "city you were given, in capital letters, and nothing else.")
    held(sid, "question")
    rc, _ = respond("question", "Which city", {"behavior": "allow", "answers_list": ["Lisbon"]})
    t.wait(r"\bLISBON\b", 90)
    results["7a-free-text"] = rc == 0
    wait_until(lambda: sess(sid)["state"] == "waiting", 60, what="waiting")

    # 7a': two questions, one multi-select, answered from the CLI by number.
    prompt(t, "Use the AskUserQuestion tool exactly once, asking two questions in the same call: first "
              "'Which fruit?' (header 'Fruit', options Apple, Banana, Cherry); second 'Which toppings?' (header "
              "'Toppings', multiSelect true, options Nuts, Honey, Cream). After you receive the answers, reply with "
              "exactly one line and nothing else: FRUIT=<fruit>; TOPPINGS=<toppings exactly as given>")
    r = held(sid, "question")
    say("held 2 questions:", r["summary"], [q["question"] for q in r["questions"]],
        [q.get("multi_select", False) for q in r["questions"]])
    rc0, out0 = ph_person("answer", r["id"][:8])
    say("answer without answers:", rc0, out0)
    rc, out = ph_person("answer", r["id"][:8], "3", "1,2")
    say("pilothouse answer:", rc, out)
    m = t.wait(r"FRUIT=(\w+); TOPPINGS=([^\n]+)", 90)
    say("claude replied:", m.group(0))
    results["7a-cli"] = (rc0 != 0 and "1) Apple" in out0, rc == 0, m.group(1), m.group(2).strip(),
                         req(r["id"])["decision"]["answers"])
    wait_until(lambda: sess(sid)["state"] == "waiting", 60, what="waiting")

    # 7a: declining from the phone.
    prompt(t, "Use the AskUserQuestion tool exactly once with one question 'Which size?' (header 'Size') and "
              "two options: Small and Large. If the question is declined, reply with only the word DECLINED.")
    r = held(sid, "question")
    rc, _ = respond("question", "Which size", {"behavior": "deny", "message": "Not now, I'll decide later."})
    t.wait(r"DECLINED|decide later|declined", 90)
    results["7a-decline"] = (rc == 0, req(r["id"])["status"], req(r["id"])["decision"]["behavior"])
    wait_until(lambda: sess(sid)["state"] == "waiting", 60, what="waiting")

    # 7b: at the desk — Claude's own dialog, answered there.
    say("here:", ph_person("here"))
    prompt(t, "Use the AskUserQuestion tool exactly once with one question 'Which shape?' (header 'Shape') and "
              "two options: Circle and Square. After you receive the answer, reply with only the chosen shape in "
              "capital letters and nothing else.")
    r = wait_until(lambda: reqs(sid, checkpoint="question", status="stepped_aside"), 90, what="stepped-aside question")[0]
    time.sleep(2)
    screen = t.text()
    say("native dialog:\n" + tail(t, 20))
    s = sess(sid)
    dialog = "Circle" in screen and "Square" in screen
    t.send("\x1b[B"); time.sleep(0.4)  # down to the second option
    t.send("\r"); time.sleep(1.5)
    if "Submit" in t.text():  # review step of the question dialog
        say("review step:\n" + tail(t, 12))
        t.send("\r")
    t.wait(r"\bSQUARE\b", 60)
    st = wait_until(lambda: req(r["id"])["status"] != "stepped_aside" and req(r["id"])["status"], 30, what="closed")
    results["7b-desk"] = (dialog, r["route"], s["block_kind"], st)
    wait_until(lambda: sess(sid)["state"] == "waiting", 60, what="waiting")
    print(ph("log", "--session", sid, "-n", "40"))
finally:
    t.send("\x03"); time.sleep(0.5); t.send("\x03"); t.wait_exit(10); t.close()

# 7c: plan mode.
W2 = workdir("w_plan")
t, sid = start(W2, f"{ROOT}/s7c.raw", mode="plan")
try:
    say("away:", ph_person("away"))
    prompt(t, "This is a test of plan approval. The whole task is: reply with the word DONE. Do not read, create "
              "or edit any files other than the plan file, and do not run commands. Write a one-step plan to the "
              "plan file and call ExitPlanMode. After the plan is approved, just do the plan's steps by replying "
              "with the words it says.")
    r1 = held(sid, "plan", timeout=180)
    say("plan held:", r1["id"], r1["summary"], "path=", r1["plan"].get("path"), "\n" + r1["plan"]["markdown"])
    s = sess(sid)
    rc1, _ = respond("plan", "Plan ready", {"behavior": "deny", "message": "Add a final step that says the word KIWI."})
    r2 = held(sid, "plan", not_ids=(r1["id"],), timeout=180)
    say("revised plan:", r2["id"], r2["summary"], "\n" + r2["plan"]["markdown"])
    rc2, _ = respond("plan", "Plan ready", {"behavior": "allow"})
    t.wait(r"KIWI", 120)
    time.sleep(3)
    say("screen after approval:\n" + tail(t, 16))
    plan_path = r2["plan"].get("path", "")
    results["7c-plan"] = (rc1 == 0, req(r1["id"])["decision"], "KIWI" in r2["plan"]["markdown"], rc2 == 0,
                          req(r2["id"])["status"], s["block_kind"], plan_path.startswith(W2 + "/plans/"),
                          "DONE" in t.text())
    print(ph("log", "--session", sid, "-n", "30"))
finally:
    say("RESULTS", results)
    ph_person("here")
    t.send("\x03"); time.sleep(0.5); t.send("\x03"); t.wait_exit(10); t.close()
