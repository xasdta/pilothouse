"""Shared helpers for Pilothouse + real Codex CLI e2e scripts (mock model).

Layout under E2E_ROOT (default /tmp/qa6): pilothouse binary, home/ run/ uhome/
(PILOTHOUSE_*), uhome/.codex is CODEX_HOME, npm/ holds the Codex install,
repo/ is a scratch git repo the sessions run in. The mock model server
(mock_responses.py) listens on MOCK_PORT (default 7597)."""
import json, os, subprocess, sys, time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(HERE, "..", "claude"))
from ptyterm import Term  # noqa: E402,F401

ROOT = os.path.realpath(os.environ.get("E2E_ROOT", "/tmp/qa6"))  # canonical: Codex canonicalizes CODEX_HOME in trust keys
PH = os.path.join(ROOT, "pilothouse")
CODEX_BIN = os.environ.get("CODEX_BIN", os.path.join(ROOT, "npm/node_modules/.bin/codex"))
CODEX_HOME = os.path.join(ROOT, "uhome/.codex")
DETACH = os.path.join(HERE, "..", "claude", "detach.py")
PHONE = os.path.join(HERE, "..", "claude", "phone.mjs")
REPO = os.path.join(ROOT, "repo")
STRIP = ["CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SESSION_ID", "CLAUDE_PID",
         "CLAUDE_CODE_EXECPATH", "AI_AGENT", "OPENAI_API_KEY", "CODEX_API_KEY", "OPENAI_BASE_URL"]


def env(**extra):
    e = {k: v for k, v in os.environ.items() if k not in STRIP and not k.startswith("CLAUDE_CODE_")}
    e.update(PILOTHOUSE_HOME=f"{ROOT}/home", PILOTHOUSE_USER_HOME=f"{ROOT}/uhome",
             PILOTHOUSE_RUN_DIR=f"{ROOT}/run", PILOTHOUSE_NO_UPDATE_CHECK="1",
             CODEX_HOME=CODEX_HOME, HOME=f"{ROOT}/uhome", MOCK_API_KEY="dummy-not-a-key",
             TERM="xterm-256color")
    e.update(extra)
    return e


def ph(*args, timeout=60):
    r = subprocess.run([PH, *args], env=env(), capture_output=True, text=True, timeout=timeout)
    return r.stdout + r.stderr


def ph_person(*args):
    """Run a pilothouse admin command as 'the person' (double-forked, outside
    any agent's process tree, so the agent-caller guard lets it through)."""
    r = subprocess.run([sys.executable, DETACH, PH, *args], env=env(), capture_output=True, text=True, timeout=90)
    return r.returncode, r.stdout + r.stderr


def sock_get(path):
    r = subprocess.run(["curl", "-s", "--unix-socket", f"{ROOT}/run/pilothouse.sock", f"http://x{path}"],
                       capture_output=True, text=True)
    return r.stdout


def ls():
    return json.loads(ph("ls", "--all", "--json"))


def requests(status=""):
    return json.loads(sock_get(f"/v1/requests?status={status}") or "[]")


def codex_exec(prompt, *flags, cwd=REPO, timeout=120, extra_env=None):
    """Run `codex exec` non-interactively; returns (rc, stdout+stderr)."""
    argv = [CODEX_BIN, "exec", *flags, prompt]
    try:
        r = subprocess.run(argv, env=env(**(extra_env or {})), cwd=cwd, stdin=subprocess.DEVNULL,
                           capture_output=True, text=True, timeout=timeout)
        return r.returncode, r.stdout + r.stderr
    except subprocess.TimeoutExpired as ex:
        out = (ex.stdout or b"").decode() if isinstance(ex.stdout, bytes) else (ex.stdout or "")
        return 124, out


def codex_tui(*flags, cwd=REPO, log=None, cols=160, rows=50, extra_env=None):
    return Term([CODEX_BIN, *flags], env=env(**(extra_env or {})), cwd=cwd, log=log, cols=cols, rows=rows)


def wait_until(fn, timeout=60, every=0.5, what="condition"):
    end = time.time() + timeout
    last = None
    while time.time() < end:
        last = fn()
        if last:
            return last
        time.sleep(every)
    raise TimeoutError(f"timeout waiting for {what}; last={last!r}")


def say(*a):
    print("[e2e]", *a, flush=True)


MOCK_LOG = os.environ.get("MOCK_LOG", os.path.join(ROOT, "mock/requests.jsonl"))


def tool_output(call_prefix):
    """What Codex reported back to the model for the tool call whose call_id
    starts with call_prefix (e.g. "call_touch_esc2_0"), from the mock log."""
    out = None
    with open(MOCK_LOG) as f:
        for line in f:
            body = json.loads(line).get("body") or {}
            for it in body.get("input") or []:
                if it.get("type", "").endswith("call_output") and str(it.get("call_id", "")).startswith(call_prefix):
                    o = it.get("output")
                    out = o if isinstance(o, str) else json.dumps(o)
    return out
