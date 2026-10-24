"""Scenario 7 — a task started from the phone, with real Claude Code (haiku).

The simulated phone (phone.mjs + phone_task.mjs, signed v2 actions) lists
repos, starts a Claude task in a repo with a .pilothouse/task.yaml, approves
its permission requests, reads the diff and discards the task. Checks: the
signed start (replay refused, unknown repo refused), started_by, task.yaml
copy (and untrusted setup not run), the prompt starting with "-" reaching
Claude as the prompt, "Ready for review" (via a local webhook), the diff,
and discard (worktree, branch and agent gone).

Self-contained setup (needs $E2E_ROOT/pilothouse built from this tree):
  E2E_ROOT  scratch dir for homes, repo and phone state
  E2E_RUN   short dir for sockets (Unix socket paths are limited to ~104 bytes)
  E2E_PORT  web port (default: a free one; never the real 7517)
  CLAUDE_BIN  real claude (default: from PATH)
claude runs with --setting-sources project,local and explicit --settings, so
the user's own ~/.claude/settings.json (and its hooks) never loads.
"""
import http.server, json, os, re, shutil, socket, subprocess, sys, threading, time
from common import HERE, STRIP, Term, say, wait_until

ROOT = os.environ["E2E_ROOT"]
RUN = os.environ.get("E2E_RUN", f"{ROOT}/run")
def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]
# Other test daemons may hold the usual 759x ports: take a free one.
PORT = int(os.environ.get("E2E_PORT") or free_port())
assert PORT != 7517, "never the real daemon's port"
PH = f"{ROOT}/pilothouse"
BASE = f"http://127.0.0.1:{PORT}"
SETTINGS = f"{ROOT}/uhome/.claude/settings.json"
STATE = f"{ROOT}/phone.json"
REPO = f"{ROOT}/w_phone"
CLAUDE = os.environ.get("CLAUDE_BIN") or shutil.which("claude")
PHONE = ["node", os.path.join(HERE, "phone.mjs")]
PTASK = ["node", os.path.join(HERE, "phone_task.mjs")]
DETACH = [sys.executable, os.path.join(HERE, "detach.py")]

for d in ("home", "uhome", "bin"):
    os.makedirs(f"{ROOT}/{d}", exist_ok=True)
os.makedirs(RUN, exist_ok=True)


def env():
    e = {k: v for k, v in os.environ.items() if k not in STRIP}
    e.update(PILOTHOUSE_HOME=f"{ROOT}/home", PILOTHOUSE_USER_HOME=f"{ROOT}/uhome", PILOTHOUSE_RUN_DIR=RUN,
             PILOTHOUSE_NO_UPDATE_CHECK="1", TERM="xterm-256color")
    e["PATH"] = f"{ROOT}/bin:{ROOT}:" + e["PATH"]  # the claude wrapper first
    return e


def ph(*a, cwd=None):
    r = subprocess.run([PH, *a], env=env(), cwd=cwd, capture_output=True, text=True, timeout=120)
    return r.returncode, r.stdout + r.stderr


def person(*a):
    """An admin command run as 'the person': outside this agent's process tree."""
    r = subprocess.run(DETACH + [PH, *a], env=env(), capture_output=True, text=True, timeout=90)
    return r.returncode, r.stdout + r.stderr


def node(argv):
    r = subprocess.run(argv, env=env(), capture_output=True, text=True, timeout=60)
    try:
        return json.loads(r.stdout.strip().splitlines()[-1])
    except Exception:
        raise RuntimeError(f"{argv[1:3]}: {r.stdout} {r.stderr}")


def get(path):
    r = subprocess.run(["curl", "-s", "--unix-socket", f"{RUN}/pilothouse.sock", f"http://x{path}"],
                       capture_output=True, text=True)
    return json.loads(r.stdout)


# A local webhook receiver stands in for a notification channel.
hooks = []
class Hook(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        hooks.append(self.rfile.read(int(self.headers.get("Content-Length", 0))).decode())
        self.send_response(204); self.end_headers()
    def log_message(self, *a): pass
hook_srv = http.server.HTTPServer(("127.0.0.1", 0), Hook)
threading.Thread(target=hook_srv.serve_forever, daemon=True).start()

open(f"{ROOT}/home/config.yaml", "w").write(
    f"web:\n  addr: 127.0.0.1:{PORT}\nnotify:\n  desktop: false\n  webhooks: ['http://127.0.0.1:{hook_srv.server_port}/hook']\n"
    f"update_check: false\napprovals:\n  always_hold: true\ntasks:\n  repos: ['{REPO}']\n")
open(f"{ROOT}/bin/claude", "w").write(
    f"#!/bin/sh\nexec {CLAUDE} --setting-sources project,local --settings {SETTINGS} --model haiku --permission-mode default \"$@\"\n")
os.chmod(f"{ROOT}/bin/claude", 0o755)

shutil.rmtree(REPO, ignore_errors=True); os.makedirs(f"{REPO}/.pilothouse")
open(f"{REPO}/.gitignore", "w").write("")
open(f"{REPO}/README.md", "w").write("# phone task e2e\n")
open(f"{REPO}/.pilothouse/task.yaml", "w").write("copy: [.env.local]\nsetup: ['touch SETUP_RAN']\n")
G = ["git", "-c", "user.email=qa@example.com", "-c", "user.name=qa", "-c", "commit.gpgsign=false"]
subprocess.run("git init -q -b main && git add -A", shell=True, cwd=REPO, check=True)
subprocess.run(G + ["commit", "-qm", "init"], cwd=REPO, check=True)
open(f"{REPO}/.env.local", "w").write("E2E_SECRET=not-for-the-phone\n")  # untracked, copied into the worktree

results = {}
stop = threading.Event()
approved = []
def approve_loop():
    while not stop.is_set():
        p = subprocess.run(PHONE + ["decide", STATE, "allow", "session", "", "2"], capture_output=True, text=True)
        if p.returncode == 0:
            approved.append(p.stdout.split("\n")[0][:160])

A = None
try:
    say("hooks install:", ph("hooks", "install")[1].strip().splitlines()[-1:])
    say("start:", ph("start")[1].strip())
    say("away:", person("away")[1].strip())  # so "ready for review" goes out
    rc, out = person("pair", "--json", "--no-wait", "--no-qr")
    code = json.loads(out[out.index("{"):])["code"]
    p = subprocess.run(PHONE + ["pair", BASE, code, STATE], capture_output=True, text=True)
    say("pair:", p.returncode, p.stdout.strip()[:120])

    repos = node(PTASK + ["repos", STATE])
    results["repos-listed"] = repos["status"] == 200 and any(r["root"] == os.path.realpath(REPO) for r in repos["body"])
    say("repos:", repos)

    prompt = "- Create a file named hello.txt containing exactly the word hi. Do not run git or any other command."
    start = node(PTASK + ["start", STATE, os.path.realpath(REPO), "claude", prompt])
    say("start:", {k: v for k, v in start.items() if k != "body"}, start["body"] if start["status"] != 200 else "")
    t = start["body"]
    results["start-200"] = start["status"] == 200
    results["replay-refused"] = start["replay"] == 403 and start["replay_code"] == "replayed"
    results["unknown-repo-refused"] = start["unknown"] == 400 and "not a repository Pilothouse knows" in (start["unknown_error"] or "")
    results["started-by-device"] = t.get("by", "").startswith("device:d_")
    results["env-copied"] = (t.get("setup") or {}).get("copied") == [".env.local"]
    results["untrusted-setup-skipped"] = (t.get("setup") or {}).get("status") == "skipped" and not os.path.exists(f"{t['worktree']}/SETUP_RAN")
    sid, wt, br, tid = t["session_id"], t["worktree"], t["branch"], t["id"]
    say("task:", tid, sid, wt, "note:", t.get("note"))

    th = threading.Thread(target=approve_loop, daemon=True); th.start()
    # Claude asks whether to trust a folder it has never seen; answer it in the terminal.
    A = Term([PH, "attach", sid], env=env(), cwd=REPO, log=f"{ROOT}/s7.raw")
    time.sleep(5)
    scr = A.text()
    results["trust-prompt-surfaced"] = ("trust" in scr.lower()) == ("trust this folder" in (t.get("note") or ""))
    if "Yes, I trust this folder" in scr:
        if "❯ Yes, I trust" not in scr:
            A.send("\x1b[B"); time.sleep(0.5)
        A.send("\r"); time.sleep(2)
    time.sleep(3); A.send("\x1c"); A.wait_exit(5); A = None

    try:
        wait_until(lambda: os.path.exists(f"{wt}/hello.txt"), 240, every=2, what="hello.txt in the worktree")
        results["agent-wrote-file"] = True
    except TimeoutError as e:
        results["agent-wrote-file"] = False; say(str(e))
        A = Term([PH, "attach", sid], env=env(), cwd=REPO); time.sleep(3); say("screen:\n" + A.text()); A.send("\x1c"); A.wait_exit(5); A = None
    say("phone approvals:", approved)
    wait_until(lambda: [s for s in get("/v1/sessions") if s["id"] == sid][0]["state"] in ("waiting", "exited"), 120, what="turn finished")

    wait_until(lambda: any("Ready for review" in h for h in hooks), 30, what="ready-for-review notification")
    results["ready-for-review-notified"] = True
    say("webhooks:", hooks)
    # A freshly started Claude idles at its input box; that is not "ready for review".
    first = lambda s: next((i for i, h in enumerate(hooks) if s in h), len(hooks))
    results["no-early-review"] = first("wants to edit") < first("Ready for review")
    tasks = node(PTASK + ["tasks", STATE])["body"]
    results["task-in-review"] = [x for x in tasks if x["id"] == tid][0]["status"] == "review"

    diff = node(PTASK + ["diff", STATE, tid])
    files = {f["path"]: f for f in diff["body"].get("files", [])}
    say("diff:", diff["status"], {p: (f["op"], f["added"], f["removed"]) for p, f in files.items()})
    results["diff-has-hello"] = diff["status"] == 200 and files.get("hello.txt", {}).get("op") == "create" and "+hi" in files["hello.txt"].get("diff", "")
    results["diff-hides-copied-env"] = ".env.local" not in files and "not-for-the-phone" not in json.dumps(diff)

    rc, out = ph("task", "ls", cwd=REPO); say("task ls:\n" + out)
    results["cli-ls-shows-device"] = bool(re.search(rf"{tid}\s+\S+\s+review\s+device:d_", out))

    fin = node(PTASK + ["finish", STATE, tid, "discard"])
    say("discard:", fin["status"], fin["body"].get("status"), fin["body"].get("note"))
    time.sleep(2)
    branches = subprocess.run(["git", "branch", "--list", br], cwd=REPO, capture_output=True, text=True).stdout.strip()
    results["discarded"] = fin["status"] == 200 and fin["body"]["status"] == "abandoned" and not os.path.exists(wt) and not branches
    wait_until(lambda: [s for s in get("/v1/sessions") if s["id"] == sid][0]["state"] == "exited", 30, what="agent stopped")
    results["agent-stopped"] = True
finally:
    stop.set()
    if A: A.close()
    say("RESULTS", json.dumps(results))
    say("stop:", person("stop")[1].strip())
    hook_srv.shutdown()
    ok = all(results.values()) and len(results) >= 17
    say("PASS" if ok else "FAIL")
    sys.exit(0 if ok else 1)
