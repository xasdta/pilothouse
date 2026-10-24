"""Scenario 7 (0.2) — replies, interrupts, the live terminal and keep-awake
with real Claude Code (haiku):

  A. wrapped (`pilothouse run claude`, a terminal attached the whole time):
     a multi-line reply from the CLI arrives as one prompt and the agent
     acts on it; --interrupt stops a running tool call and the new
     instruction runs; keep-awake holds caffeinate tied to the daemon pid
     while the agent works.
  B. the simulated phone drives the terminal API on the same session:
     signed token, SSE stream with replay, typed input, token scoping.
  C. observed (plain `claude` with Pilothouse hooks): the reply goes through
     Claude's messaging socket and the agent acts on it; --interrupt is
     refused with a reason.

Needs a paired phone state at $E2E_ROOT/phone.json (see README) and the web
listener on $E2E_WEB (default http://127.0.0.1:7593)."""
import json, os, re, shutil, subprocess, sys, time
from common import *

PHONE = f"{ROOT}/phone.json"
results = {}
ARGV = ["claude", "--setting-sources", "project,local", "--settings", SETTINGS,
        "--model", "haiku", "--permission-mode", "default"]


def project(name, allow):
    d = f"{ROOT}/{name}"
    shutil.rmtree(d, ignore_errors=True)
    os.makedirs(f"{d}/.claude")
    with open(f"{d}/.claude/settings.local.json", "w") as f:
        json.dump({"permissions": {"allow": allow}}, f)
    return d


def sess(sid):
    return [x for x in ls()["sessions"] if x["id"] == sid][0]


def hooks(sid, kind):
    evs = json.loads(ph_get(f"/v1/sessions/{sid}/events?limit=500"))
    return [e["data"] for e in evs if e["type"] == "hook" and e["data"].get("kind") == kind]


def prompts(sid):
    return [d.get("message", "") for d in hooks(sid, "prompt_submit")]


def long_calls(sid, kind):
    return [d for d in hooks(sid, kind) if "ping -c 100" in (d.get("command") or "")]


def caffeinates(pid):
    out = subprocess.run(["ps", "-axo", "pid=,ppid=,command="], capture_output=True, text=True).stdout
    return [l.strip() for l in out.splitlines() if "caffeinate" in l and f"-w {pid}" in l]


def daemon_pid():
    return json.loads(ph_get("/v1/health"))["pid"]


def wait_file(path, timeout=120):
    wait_until(lambda: os.path.exists(path) and os.path.getsize(path) > 0, timeout, what=path)
    return open(path).read()


# ---------------------------------------------------------------- A: wrapped
WA = project("w_reply", ["Write", "Read", "Bash(ping:*)", "Bash(ls:*)"])
NAME = f"e2e-reply-{int(time.time())}"
A = Term([PH, "run", "-n", NAME, "--", *ARGV], env=env(), cwd=WA, log=f"{ROOT}/s7a.raw")
sid = None
try:
    pass_startup(A)
    sid = wait_until(lambda: [s for s in ls()["sessions"] if s["title"] == NAME and s["state"] != "exited"], 30)[0]["id"]
    caps = wait_until(lambda: (lambda c: c if c.get("reply") and c.get("watch") else None)(sess(sid)["caps"]), 30, what="caps")
    say("wrapped caps:", caps)
    results["A-caps"] = caps.get("reply_via") == "terminal" and caps.get("interrupt") is True

    rc, out = ph_person("reply", sid, "Use the Write tool to create a file named wrap-reply.txt containing the word gamma.\n"
                        "When it is written, answer with just the word DONE.")
    say("reply:", rc, out.strip())
    results["A-reply-cli"] = rc == 0 and "typed into its terminal" in out
    results["A-acted"] = "gamma" in wait_file(f"{WA}/wrap-reply.txt")
    ps = [p for p in prompts(sid) if "wrap-reply" in p]
    say("prompts with the reply:", ps)
    results["A-one-prompt"] = len(ps) == 1 and "DONE" in ps[0]
    results["A-attached-kept"] = A.alive() and "detached" not in A.text()
    wait_until(lambda: sess(sid)["state"] == "waiting", 90, what="waiting")

    # Interrupt a long-running tool call.
    rc, out = ph_person("reply", sid, "Run exactly this Bash command in the foreground (run_in_background: false) and wait for it "
                        "to finish: ping -c 100 127.0.0.1")
    wait_until(lambda: long_calls(sid, "pre_tool"), 90, what="the long tool call")
    time.sleep(2)
    results["A-tool-running-at-interrupt"] = not long_calls(sid, "post_tool") and sess(sid)["state"] == "working"
    dp = daemon_pid()
    cf = caffeinates(dp)
    say("caffeinate while working:", cf)
    results["keep-awake-held"] = len(cf) == 1 and "caffeinate -i -w" in cf[0]
    t0 = time.time()
    rc, out = ph_person("reply", sid, "--interrupt", "Stop that. Use the Write tool to create interrupted.txt containing the word delta.")
    say("interrupt:", rc, out.strip())
    results["A-interrupted-acted"] = "delta" in wait_file(f"{WA}/interrupted.txt", 100) and time.time() - t0 < 100
    results["A-interrupted-shown"] = bool(re.search(r"Interrupted", A.text() + A.rawtext().decode("utf8", "replace")))
    wait_until(lambda: sess(sid)["state"] == "waiting", 90, what="waiting")

    # A permission dialog is up: a plain reply is refused (its Enter would
    # answer the dialog); with --interrupt, Esc cancels it first.
    ph_person("here")
    rc, out = ph_person("reply", sid, "Run this Bash command: touch blocked.txt")
    wait_until(lambda: sess(sid)["state"] == "blocked", 90, what="permission dialog")
    rc, out = ph_person("reply", sid, "yes")
    say("reply while blocked:", rc, out.strip())
    results["A-blocked-refused"] = rc != 0 and "waiting for an answer" in out
    rc, out = ph_person("reply", sid, "--interrupt", "Don't run that. Use the Write tool to create blocked-alt.txt containing the word eta.")
    results["A-blocked-interrupt-acted"] = "eta" in wait_file(f"{WA}/blocked-alt.txt")
    results["A-blocked-not-approved"] = not os.path.exists(f"{WA}/blocked.txt")
    wait_until(lambda: sess(sid)["state"] == "waiting", 90, what="waiting")

    # ------------------------------------------------------------ B: phone terminal
    if os.path.exists(PHONE):
        r = subprocess.run(["node", f"{HERE}/phone_terminal.mjs", PHONE, sid,
                            "Use the Write tool to create a file named term-input.txt containing the word zeta."],
                           capture_output=True, text=True, timeout=120)
        say("phone terminal:", r.stdout.strip(), r.stderr.strip()[-500:])
        t = json.loads(r.stdout.strip().splitlines()[-1])
        results["B-token-scoping"] = t["no_token"] == 403 and t["other_session"] in (403, 404) and t["mint"] == 200 and 590 <= t["expires_in_s"] <= 600
        results["B-stream"] = t["stream"] == 200 and t["replay_bytes"] > 0 and t["replay_shows_claude"] and t["events"].get("size", 0) >= 1
        results["B-input"] = t["type"] == 204 and t["enter"] == 204 and t["resize"] == 204 and t["live_bytes"] > 0
        results["B-acted"] = "zeta" in wait_file(f"{WA}/term-input.txt")
        results["B-attached-kept"] = A.alive() and "detached" not in A.text()
    else:
        say("no paired phone; skipping the terminal API part")
finally:
    if A.alive():
        A.send("\x1b"); time.sleep(1)
        A.type("/exit"); A.send("\r")
        A.wait_exit(20)
    A.close()
    if sid:
        # The holder outlives the attached terminal; end the agent we
        # started if /exit didn't.
        try:
            wait_until(lambda: sess(sid)["state"] == "exited", 15, what="wrapped session exit")
        except TimeoutError:
            pid = sess(sid).get("pid")
            say("stopping leftover agent", pid)
            if pid:
                os.kill(pid, 15)

# ---------------------------------------------------------------- C: observed
WC = project("w_observed", ["Write", "Read"])
C = claude(WC, log=f"{ROOT}/s7c.raw")
try:
    pass_startup(C)
    sidC = wait_until(lambda: [s for s in ls()["sessions"] if s["cwd"].endswith("w_observed") and s["mode"] == "observed"
                                and s["state"] != "exited"], 40)[0]["id"]
    # The endpoint arrives with the first hook (SessionStart).
    capsC = wait_until(lambda: (lambda c: c if c.get("reply") else None)(sess(sidC)["caps"]), 30, what="observed caps")
    say("observed caps:", capsC)
    results["C-caps"] = capsC.get("reply_via") == "message" and not capsC.get("interrupt")
    rc, out = ph_person("reply", sidC, "Use the Write tool to create a file named observed-reply.txt containing the word epsilon.")
    say("reply:", rc, out.strip())
    results["C-reply-cli"] = rc == 0 and "messaging socket" in out
    results["C-acted"] = "epsilon" in wait_file(f"{WC}/observed-reply.txt")
    results["C-framed"] = "[Pilothouse]" in C.text() or b"[Pilothouse]" in C.rawtext()
    rc, out = ph_person("reply", sidC, "--interrupt", "stop")
    results["C-interrupt-refused"] = rc != 0 and "pilothouse run claude" in out
    # The token never shows up in what clients can read.
    snap = ph_get("/v1/snapshot") + ph_get(f"/v1/sessions/{sidC}/events?limit=500") + ph_get(f"/v1/sessions/{sidC}/explain")
    results["C-no-socket-in-api"] = "cc-socks" not in snap
finally:
    C.send("\x03"); time.sleep(0.5); C.send("\x03")
    C.close()

say("RESULTS", json.dumps(results))
