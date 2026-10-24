"""Scenario 8 — probe: what Claude Code does with a PreToolUse "allow" for
AskUserQuestion and ExitPlanMode (why Pilothouse never sends one).

No Pilothouse hooks here: a throwaway hook script answers PreToolUse with
permissionDecision "allow" (no updatedInput) for both tools and logs every
PreToolUse / PermissionRequest / PostToolUse payload. The script reports
whether Claude still shows its own dialog, whether PermissionRequest still
fires, and what the tool returned. Plan mode, so both tools are reachable
in one session."""
import json, os, re, shutil, stat, time
from common import *

W = f"{ROOT}/w_probe"
LOG = f"{ROOT}/probe-log"
for d in (W, LOG):
    shutil.rmtree(d, ignore_errors=True)
os.makedirs(f"{W}/.claude")
os.makedirs(LOG)
json.dump({"plansDirectory": "plans"}, open(f"{W}/.claude/settings.json", "w"))

hook = f"{ROOT}/probe_hook.py"
open(hook, "w").write(f'''#!/usr/bin/env python3
import json, sys
d = json.load(sys.stdin)
ev, tool = d.get("hook_event_name", "x"), d.get("tool_name", "")
with open("{LOG}/" + ev + "-" + tool + ".jsonl", "a") as f:
    f.write(json.dumps(d) + "\\n")
if ev == "PreToolUse" and tool in ("AskUserQuestion", "ExitPlanMode"):
    print(json.dumps({{"hookSpecificOutput": {{"hookEventName": "PreToolUse", "permissionDecision": "allow",
                                              "permissionDecisionReason": "probe allow"}}}}))
''')
os.chmod(hook, os.stat(hook).st_mode | stat.S_IEXEC)
settings = f"{ROOT}/probe-settings.json"
entry = [{"matcher": "*", "hooks": [{"type": "command", "command": hook}]}]
json.dump({"hooks": {"PreToolUse": entry, "PermissionRequest": entry, "PostToolUse": entry}}, open(settings, "w"))


def logged(name):
    p = f"{LOG}/{name}.jsonl"
    return [json.loads(l) for l in open(p)] if os.path.exists(p) else []


def tail(t, n=16):
    return "\n".join(l for l in t.text().splitlines()[-n:] if l.strip())


results = {}
t = Term(["claude", "--setting-sources", "project,local", "--settings", settings, "--model", "haiku",
          "--permission-mode", "plan"], env=env(), cwd=W, log=f"{ROOT}/s8.raw")
try:
    pass_startup(t)
    t.type("Use the AskUserQuestion tool exactly once with one question 'Which color do you prefer?' (header "
           "'Color') and two options: Red and Blue. After you receive the answer, reply with ANSWER= followed by "
           "exactly what the tool returned."); time.sleep(0.3); t.send("\r")
    wait_until(lambda: logged("PreToolUse-AskUserQuestion"), 90, what="AUQ pre_tool")
    time.sleep(8)
    screen = t.text()
    say("screen 8s after a pre_tool allow for AskUserQuestion:\n" + tail(t, 22))
    dialog = bool(re.search(r"Red", screen) and re.search(r"Blue", screen) and re.search(r"Enter|select|Esc", screen))
    perm = bool(logged("PermissionRequest-AskUserQuestion"))
    post = logged("PostToolUse-AskUserQuestion")
    if dialog and not post:
        t.send("\x1b[B"); time.sleep(0.4); t.send("\r"); time.sleep(1.5)
        if "Submit" in t.text():
            t.send("\r")
        wait_until(lambda: logged("PostToolUse-AskUserQuestion"), 60, what="AUQ post_tool")
        post = logged("PostToolUse-AskUserQuestion")
    m = t.wait(r"⏺\s*ANSWER=[^\n]*", 90)  # Claude's reply, not the prompt echo
    results["auq"] = {"dialog_shown": dialog, "permission_request_fired": perm,
                      "tool_response": post[-1].get("tool_response") if post else None, "claude": m.group(0)}
    say("AUQ:", results["auq"])

    t.type("This is a test of plan approval. The whole task is: reply with the word DONE. Do not read, create or "
           "edit any files other than the plan file, and do not run commands. Write a one-step plan to the plan "
           "file and call ExitPlanMode. After the plan is approved, reply with the word DONE."); time.sleep(0.3); t.send("\r")
    wait_until(lambda: logged("PreToolUse-ExitPlanMode"), 180, what="EPM pre_tool")
    time.sleep(8)
    screen = t.text()
    say("screen 8s after a pre_tool allow for ExitPlanMode:\n" + tail(t, 22))
    perm = bool(logged("PermissionRequest-ExitPlanMode"))
    post = logged("PostToolUse-ExitPlanMode")
    plan_dialog = bool(re.search(r"(?i)would you like to proceed|approve|keep planning", screen)) and not post
    results["epm"] = {"dialog_shown": plan_dialog, "permission_request_fired": perm, "post_tool_fired": bool(post),
                      "approved_without_dialog": bool(post) and not plan_dialog,
                      "claude_said_done": bool(re.search(r"⏺\s*DONE", t.text()))}
    if plan_dialog:
        t.send("\x1b"); time.sleep(1)  # reject: nothing to do anyway
    say("EPM:", results["epm"])
finally:
    say("RESULTS", json.dumps(results))
    t.send("\x03"); time.sleep(0.5); t.send("\x03"); t.wait_exit(10); t.close()
