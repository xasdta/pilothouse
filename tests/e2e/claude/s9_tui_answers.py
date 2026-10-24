"""Scenario 9 — the inbox TUI answers questions and plans, end to end
without a model: Claude Code payloads (captured from 2.1.309, as in the
adapter's testdata) go through the real `pilothouse hook claude` shim to the
daemon, are held while away, answered in the TUI (in a PTY, run through
detach.py so the agent-caller guard lets it decide), and the shim prints
the answer Claude Code would get."""
import json, os, subprocess, sys, time
from common import *

W = f"{ROOT}/w_tui"
os.makedirs(W, exist_ok=True)


def payload(tool, tool_input, sid):
    return json.dumps({"session_id": sid, "transcript_path": f"{W}/t.jsonl", "cwd": W, "permission_mode": "default",
                       "hook_event_name": "PermissionRequest", "tool_name": tool, "tool_input": tool_input})


def shim(body):
    p = subprocess.Popen([PH, "hook", "claude"], env=env(), cwd=W, stdin=subprocess.PIPE,
                         stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    p.stdin.write(body)
    p.stdin.close()
    return p


def tui(rx, keys, name):
    out = f"{ROOT}/{name}.screen"
    subprocess.run([sys.executable, DETACH, sys.executable, os.path.join(HERE, "tui_driver.py"), out, rx, keys],
                   env=env(), timeout=180)
    return open(out).read()


def decision(p):
    stdout, stderr = p.communicate(timeout=60)
    say("shim:", p.returncode, stdout.strip(), stderr.strip())
    return json.loads(stdout)["hookSpecificOutput"]["decision"] if stdout.strip() else None


results = {}
say("away:", ph_person("away"))
try:
    questions = {"questions": [
        {"question": "Which database?", "header": "Database", "multiSelect": False,
         "options": [{"label": "Postgres 16", "description": "what prod runs"}, {"label": "Postgres 17", "description": "newer"}]},
        {"question": "Which checks?", "header": "Checks", "multiSelect": True,
         "options": [{"label": "Lint", "description": ""}, {"label": "Unit tests", "description": ""}, {"label": "E2E", "description": "slow"}]},
    ]}
    p = shim(payload("AskUserQuestion", questions, "tui-e2e-1"))
    wait_until(lambda: [r for r in requests("open") if r["checkpoint"] == "question"], 20, what="held question")
    # enter opens the panel · 2 = Postgres 17 · 1 and 3 toggle Lint and E2E · enter sends
    screen = tui(r"Which database\?", "\r213\r", "s9-question")
    say("TUI:\n" + screen)
    d = decision(p)
    results["question"] = d is not None and d["behavior"] == "allow" and d["updatedInput"]["answers"] == {
        "Which database?": "Postgres 17", "Which checks?": "Lint, E2E"} and d["updatedInput"]["questions"] == questions["questions"]

    plan = {"plan": "# Add OAuth login\n\n1. Register the app\n2. Add the callback\n", "planFilePath": f"{W}/plans/p.md"}
    p = shim(payload("ExitPlanMode", plan, "tui-e2e-2"))
    wait_until(lambda: [r for r in requests("open") if r["checkpoint"] == "plan"], 20, what="held plan")
    # enter reads the plan · c asks for changes · type · enter sends
    screen = tui(r"Plan ready: Add OAuth login", "\rcPKCE\r", "s9-plan-changes")
    say("TUI:\n" + screen)
    d = decision(p)
    results["plan-changes"] = d is not None and d["behavior"] == "deny" and d["message"] == "PKCE"

    p = shim(payload("ExitPlanMode", plan, "tui-e2e-2"))
    wait_until(lambda: [r for r in requests("open") if r["checkpoint"] == "plan"], 20, what="held plan 2")
    screen = tui(r"Plan ready: Add OAuth login", "y", "s9-plan-approve")
    d = decision(p)
    results["plan-approve"] = d is not None and d["behavior"] == "allow" and d["updatedInput"] == plan
    print(ph("log", "-n", "30"))
finally:
    say("RESULTS", results)
    ph_person("here")
