"""Wrap mode: `pilothouse run codex` in a PTY.
  W1  Codex emits OSC 7501 (colon-separated, plus state=clear) into the PTY.
  W2  the daemon records L1 (OSC) evidence for the wrapped session.
  W3  hook events land on the wrapped session (not a second observed one).
Prints PASS/FAIL lines; leaves the raw PTY log in $ROOT/s_wrap.raw."""
import json, re, time
from common import *

results = {}


def check(name, ok, detail=""):
    results[name] = ok
    say(("PASS " if ok else "FAIL ") + name, detail)


before = {s["id"] for s in ls()["sessions"]}
say("here:", ph_person("here"))
flags = os.environ.get("CODEX_FLAGS", "--no-alt-screen -a on-request").split()
t = Term([PH, "run", "-n", "wrapqa", "--", CODEX_BIN, *flags], env=env(), cwd=REPO, log=f"{ROOT}/s_wrap.raw",
         cols=160, rows=50)
try:
    t.wait(r"Ask Codex to do anything", 40)
    t.send("scenario=touch_esc"); time.sleep(0.4); t.send("\r")
    t.wait(r"Would you like to run the following command", 30)
    time.sleep(1.5)
    new = [s for s in ls()["sessions"] if s["id"] not in before]
    say("new sessions:", [(s["id"], s.get("mode") or s.get("origin"), s["state"], s.get("pid")) for s in new])
    t.send("y")
    t.wait(r"MOCK-DONE scenario=touch_esc", 30)
    time.sleep(1.5)
    raw = t.rawtext()
    seqs = re.findall(rb"\x1b\]7501;[^\x07\x1b]*", raw)
    check("W1 Codex emits OSC 7501 through the wrapper", bool(seqs), str(sorted(set(seqs))[:6]))
    new = [s for s in ls()["sessions"] if s["id"] not in before]
    wrapped = [s for s in new if "wrapqa" in json.dumps(s)]
    detail = {}
    for s in new:
        ex = ph("explain", s["id"])
        detail[s["id"]] = ("L1" in ex, s["state"], [l for l in ex.splitlines() if "L1" in l][:4])
    say("explain:", detail)
    check("W2 daemon records L1 OSC evidence for the wrapped session",
          any(v[0] for v in detail.values()), json.dumps(detail)[:400])
    check("W3 hook events land on the wrapped session (one session, not two)", len(new) == 1,
          f"{len(new)} new sessions: {[(s['id'], s['state']) for s in new]}")
finally:
    try:
        t.send("\x03"); time.sleep(0.4); t.send("\x03"); time.sleep(2)
    except OSError:
        pass
    try:
        os.kill(t.pid, 9)
    except OSError:
        pass
    ph_person("auto")
print("SUMMARY", results)
