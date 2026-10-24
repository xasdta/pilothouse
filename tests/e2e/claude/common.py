"""Shared helpers for the Pilothouse + real Claude Code e2e scripts.
Environment comes from E2E_ROOT (default /tmp/qa1): binary at $E2E_ROOT/pilothouse,
PILOTHOUSE_* dirs under it, hooks installed into $E2E_ROOT/uhome."""
import json, os, subprocess, sys, time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from ptyterm import Term  # noqa: E402

ROOT = os.environ.get("E2E_ROOT", "/tmp/qa1")
PH = os.path.join(ROOT, "pilothouse")
DETACH = os.path.join(HERE, "detach.py")
SETTINGS = os.path.join(ROOT, "uhome/.claude/settings.json")
STRIP = ["CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_MESSAGING_SOCKET",
         "CLAUDE_CODE_MESSAGING_TOKEN", "CLAUDE_CODE_BRIDGE_SESSION_ID", "CLAUDE_CODE_SESSION_ID",
         "CLAUDE_CODE_CHILD_SESSION", "CLAUDE_CODE_SESSION_ATTENDED", "CLAUDE_PID", "CLAUDE_EFFORT",
         "CLAUDE_CODE_EXECPATH", "AI_AGENT"]


def env():
    e = {k: v for k, v in os.environ.items() if k not in STRIP}
    e.update(PILOTHOUSE_HOME=f"{ROOT}/home", PILOTHOUSE_USER_HOME=f"{ROOT}/uhome",
             PILOTHOUSE_RUN_DIR=f"{ROOT}/run", PILOTHOUSE_NO_UPDATE_CHECK="1",
             TERM="xterm-256color")
    e["PATH"] = ROOT + ":" + e["PATH"]
    return e


def ph(*args, check=False):
    r = subprocess.run([PH, *args], env=env(), capture_output=True, text=True, timeout=60)
    if check and r.returncode:
        raise RuntimeError(f"pilothouse {args}: {r.stderr}")
    return r.stdout + r.stderr


def ph_person(*args):
    """Run a pilothouse admin command as 'the person' (outside the agent tree)."""
    r = subprocess.run([sys.executable, DETACH, PH, *args], env=env(), capture_output=True, text=True, timeout=90)
    return r.returncode, r.stdout + r.stderr


def ls():
    return json.loads(ph("ls", "--all", "--json"))


def requests(status=""):
    return json.loads(ph_get(f"/v1/requests?status={status}"))


def ph_get(path):
    r = subprocess.run(["curl", "-s", "--unix-socket", f"{ROOT}/run/pilothouse.sock", f"http://x{path}"],
                       capture_output=True, text=True)
    return r.stdout


def wait_until(fn, timeout=60, every=0.5, what="condition"):
    end = time.time() + timeout
    last = None
    while time.time() < end:
        last = fn()
        if last:
            return last
        time.sleep(every)
    raise TimeoutError(f"timeout waiting for {what}; last={last!r}")


def claude(cwd, *extra, log=None):
    # --setting-sources leaves out the user's own ~/.claude/settings.json:
    # with Pilothouse installed there, its hooks would also fire and report
    # these test sessions to the user's real daemon.
    argv = ["claude", "--setting-sources", "project,local", "--settings", SETTINGS,
            "--model", "haiku", "--permission-mode", "default", *extra]
    return Term(argv, env=env(), cwd=cwd, log=log)


def pass_startup(t, timeout=40):
    """Get through first-run prompts (folder trust etc.) to the input box."""
    end = time.time() + timeout
    while time.time() < end:
        s = t.text()
        if "Yes, I trust this folder" in s:
            if "❯ Yes, I trust" not in s:
                t.send("\x1b[B"); time.sleep(0.6)
                continue
            t.send("\r"); time.sleep(2); continue
        if not t.alive():
            raise RuntimeError("claude exited during startup:\n" + s)
        if "for shortcuts" in s or ("❯" in s and "────" in s and "trust" not in s.lower()):
            return s
        time.sleep(0.5)
    return t.text()


def say(*a):
    print("[e2e]", *a, flush=True)
