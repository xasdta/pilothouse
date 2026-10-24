#!/usr/bin/env python3
"""End-to-end tests: real OpenCode with Pilothouse's plugin, a scripted mock
model (mock_openai.py) and an isolated Pilothouse daemon. See README.md.

    E2E_ROOT=$(mktemp -d) python3 tests/e2e/opencode/e2e.py [--tui] [scenario ...]

Everything lives under E2E_ROOT (Pilothouse home, the user home OpenCode
runs in, its XDG dirs, the project) plus a short socket dir under /tmp.
Nothing outside them is read or written; processes are stopped by PID."""
import json, os, shutil, signal, subprocess, sys, tempfile, threading, time, traceback
import urllib.parse, urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
REPO_ROOT = os.path.abspath(os.path.join(HERE, "..", "..", ".."))
ROOT = os.path.realpath(os.environ.get("E2E_ROOT") or sys.exit("set E2E_ROOT to an empty directory you made"))
OPENCODE = os.environ.get("OPENCODE_BIN", os.path.expanduser("~/.opencode/bin/opencode"))
NODE_MODULES = os.environ.get("OC_NODE_MODULES", "")  # optional: a node_modules with @opencode-ai/plugin (saves OpenCode a download)
def free_port():
    import socket
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


# Free ports: other test daemons may be running on this machine.
WEB_PORT, MOCK_PORT, OC_PORT = free_port(), free_port(), free_port()
PH = os.path.join(ROOT, "pilothouse")
UHOME = os.path.join(ROOT, "uhome")
PROJECT = os.path.join(ROOT, "repo")
MOCK_LOG = os.path.join(ROOT, "mock.jsonl")
DETACH = os.path.join(HERE, "..", "claude", "detach.py")
PHONE = os.path.join(HERE, "phone.mjs")
PHONE_STATE = os.path.join(ROOT, "phone.json")
RUN = None  # socket dir: unix socket paths must stay short
procs = {}
results = {}


def say(*a):
    print("[e2e]", *a, flush=True)


# ---------- environments ----------

def ph_env(**extra):
    e = {k: v for k, v in os.environ.items() if not k.startswith(("CLAUDE", "OPENCODE", "PILOTHOUSE", "XDG_"))}
    e.update(PILOTHOUSE_HOME=os.path.join(ROOT, "home"), PILOTHOUSE_USER_HOME=UHOME, PILOTHOUSE_RUN_DIR=RUN,
             PILOTHOUSE_NO_UPDATE_CHECK="1", HOME=UHOME, XDG_CONFIG_HOME=os.path.join(UHOME, ".config"))
    e.update(extra)
    return e


def oc_env():
    e = ph_env()
    for k in list(e):
        if k.startswith("PILOTHOUSE_USER_HOME"):
            del e[k]  # OpenCode doesn't need it; the plugin only reads the socket location
    e.update(XDG_DATA_HOME=os.path.join(ROOT, "xdg", "data"), XDG_CACHE_HOME=os.path.join(ROOT, "xdg", "cache"),
             XDG_STATE_HOME=os.path.join(ROOT, "xdg", "state"), OPENCODE_DISABLE_AUTOUPDATE="1",
             OPENCODE_DISABLE_MODELS_FETCH="1", OPENCODE_DISABLE_SHARE="1", OPENCODE_DISABLE_LSP_DOWNLOAD="1",
             OPENCODE_DISABLE_CLAUDE_CODE="1", OPENCODE_DISABLE_EXTERNAL_SKILLS="1", TERM="xterm-256color")
    return e


def ph(*args, timeout=60, env=None):
    r = subprocess.run([PH, *args], env=env or ph_env(), capture_output=True, text=True, timeout=timeout)
    return r.returncode, r.stdout + r.stderr


def ph_person(*args):
    """A pilothouse admin command as "the person": double-forked out of this
    harness's process tree, which may descend from an agent."""
    r = subprocess.run([sys.executable, DETACH, PH, *args], env=ph_env(), capture_output=True, text=True, timeout=90)
    return r.returncode, r.stdout + r.stderr


def sock(path, method="GET", body=None):
    cmd = ["curl", "-s", "--max-time", "10", "--unix-socket", os.path.join(RUN, "pilothouse.sock"), "-X", method, "http://x" + path]
    if body is not None:
        cmd += ["-H", "Content-Type: application/json", "-d", json.dumps(body)]
    out = subprocess.run(cmd, capture_output=True, text=True).stdout
    return json.loads(out) if out.strip() else None


def phone(*args, timeout=90):
    r = subprocess.run(["node", PHONE, *args], capture_output=True, text=True, timeout=timeout)
    return r.returncode, r.stdout + r.stderr


def wait_until(fn, timeout=30, every=0.2, what="condition"):
    end = time.time() + timeout
    last = None
    while time.time() < end:
        last = fn()
        if last:
            return last
        time.sleep(every)
    raise AssertionError(f"timed out waiting for {what}; last={last!r}")


def check(cond, msg):
    if not cond:
        raise AssertionError(msg)


# ---------- OpenCode server driver ----------

class OpenCode:
    def __init__(self, port):
        self.base = f"http://127.0.0.1:{port}"
        self.q = urllib.parse.urlencode({"directory": PROJECT})
        self.events, self.cond = [], threading.Condition()
        threading.Thread(target=self._sse, daemon=True).start()

    def req(self, method, path, body=None):
        url = self.base + path + ("&" if "?" in path else "?") + self.q
        r = urllib.request.Request(url, method=method, data=None if body is None else json.dumps(body).encode(),
                                   headers={"Content-Type": "application/json"})
        with urllib.request.urlopen(r, timeout=30) as resp:
            raw = resp.read()
            return resp.status, (json.loads(raw) if raw else None)

    def _sse(self):
        while True:
            try:
                with urllib.request.urlopen(self.base + "/event?" + self.q, timeout=600) as r:
                    for line in r:
                        line = line.decode().strip()
                        if line.startswith("data:"):
                            ev = json.loads(line[5:])
                            with self.cond:
                                self.events.append(ev)
                                self.cond.notify_all()
            except Exception:
                time.sleep(0.5)

    def mark(self):
        with self.cond:
            return len(self.events)

    def wait_event(self, pred, since, timeout=60):
        end = time.time() + timeout
        with self.cond:
            while time.time() < end:
                for ev in self.events[since:]:
                    if pred(ev):
                        return ev
                self.cond.wait(0.5)
        return None

    def session(self, title):
        _, s = self.req("POST", "/session", {"title": title})
        return s["id"]

    def prompt(self, sid, text):
        n = self.mark()
        st, _ = self.req("POST", f"/session/{sid}/prompt_async", {"parts": [{"type": "text", "text": text}]})
        check(st in (200, 204), f"prompt_async {st}")
        return n

    def idle(self, sid, since, timeout=60):
        return self.wait_event(lambda e: e["type"] == "session.idle" and e["properties"].get("sessionID") == sid, since, timeout)

    def tool_parts(self, sid):
        _, msgs = self.req("GET", f"/session/{sid}/message")
        out = []
        for m in msgs:
            for p in m["parts"]:
                if p["type"] == "tool":
                    out.append(p)
        return out

    def last_tool(self, sid, tool=None):
        parts = [p for p in self.tool_parts(sid) if tool is None or p.get("tool") == tool]
        return parts[-1] if parts else None

    def pending_permissions(self):
        _, ps = self.req("GET", "/permission")
        return ps or []


def tool_result(part):
    st = part.get("state", {})
    return st.get("status"), st.get("output") or st.get("error") or ""


def mock_saw(substr):
    try:
        with open(MOCK_LOG) as f:
            return any(substr in json.loads(l).get("last_text", "") for l in f)
    except FileNotFoundError:
        return False


def requests(status=""):
    return sock("/v1/requests" + (f"?status={status}" if status else "")) or []


def ph_session(agent_session):
    for s in sock("/v1/sessions") or []:
        if s.get("agent_session_id") == agent_session:
            return s
    return None


def presence(mode):
    rc, out = ph_person(mode)
    check(rc == 0, f"pilothouse {mode}: {out}")
    want = "away" if mode == "away" else "present"
    wait_until(lambda: (sock("/v1/presence") or {}).get("state") == want, 10, what=f"presence {want}")


# ---------- setup / teardown ----------

def setup():
    global RUN
    RUN = tempfile.mkdtemp(prefix="phoc", dir="/tmp")
    for d in ["home", "uhome/.config/opencode", "xdg/data", "xdg/cache", "xdg/state", "repo"]:
        os.makedirs(os.path.join(ROOT, d), exist_ok=True)
    say("building pilothouse")
    subprocess.run(["go", "build", "-o", PH, "./cmd/pilothouse"], cwd=REPO_ROOT, check=True)
    with open(os.path.join(ROOT, "home", "config.yaml"), "w") as f:
        f.write(f"web:\n  addr: 127.0.0.1:{WEB_PORT}\nnotify:\n  desktop: false\nupdate_check: false\n"
                "approvals:\n  hold_limit: 90s\n")
    with open(os.path.join(ROOT, "home", "policy.yaml"), "w") as f:
        f.write("version: 1\nrules:\n"
                "  - { name: e2e-no-forbidden, match: { kind: shell, command: \"echo *\" }, when: 'command.contains(\"forbidden\")', action: deny, message: Blocked by the e2e policy. }\n"
                "  - { name: e2e-ask-askme, match: { kind: shell, command: \"echo *\" }, when: 'command.contains(\"askme\")', action: ask, message: askme needs a person. }\n")
    with open(os.path.join(PROJECT, "opencode.json"), "w") as f:
        json.dump({"$schema": "https://opencode.ai/config.json",
                   "provider": {"mock": {"npm": "@ai-sdk/openai-compatible", "name": "Mock",
                                         "options": {"baseURL": f"http://127.0.0.1:{MOCK_PORT}/v1", "apiKey": "x"},
                                         "models": {"m1": {"name": "m1", "tool_call": True}}}},
                   "model": "mock/m1", "small_model": "mock/m1"}, f, indent=2)
    cfg = os.path.join(UHOME, ".config", "opencode")
    with open(os.path.join(cfg, "opencode.jsonc"), "w") as f:
        f.write('// e2e global config\n{\n  "$schema": "https://opencode.ai/config.json", // schema\n  "autoupdate": false\n}\n')
    if NODE_MODULES and not os.path.lexists(os.path.join(cfg, "node_modules")):
        os.symlink(NODE_MODULES, os.path.join(cfg, "node_modules"))
        with open(os.path.join(cfg, "package.json"), "w") as f:
            json.dump({"dependencies": {"@opencode-ai/plugin": "1.18.25"}}, f)

    procs["mock"] = subprocess.Popen([sys.executable, os.path.join(HERE, "mock_openai.py"), str(MOCK_PORT), MOCK_LOG])
    procs["daemon"] = start_daemon()

    rc, out = ph("hooks", "install", "--agent", "opencode")
    say("hooks install:", out.strip().splitlines()[0] if out.strip() else rc)
    check(rc == 0 and os.path.exists(os.path.join(cfg, "plugins", "pilothouse.js")), "plugin not installed: " + out)
    r = subprocess.run(["go", "run", "./tests/e2e/opencode/suggest"], cwd=REPO_ROOT, env=ph_env(), capture_output=True, text=True)
    check(r.returncode == 0, "suggest: " + r.stdout + r.stderr)
    text = open(os.path.join(cfg, "opencode.jsonc")).read()
    check('"bash": "ask"' in text and "// e2e global config" in text, "suggest result:\n" + text)
    say("permissions set by the setup suggestion:", " ".join(text.split()))

    rc, out = ph_person("pair", "--json", "--no-wait", "--no-qr")
    code = json.loads(out[out.index("{"):])["code"]
    rc, out = phone("pair", f"http://127.0.0.1:{WEB_PORT}", code, PHONE_STATE)
    check(rc == 0, "pair: " + out)
    presence("here")


def start_daemon():
    p = subprocess.Popen([PH, "daemon", "--log-file", os.path.join(ROOT, "daemon.log")], env=ph_env(),
                         stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    h = wait_until(lambda: sock("/v1/health"), 15, what="daemon health")
    check(h.get("web_url", "").endswith(f":{WEB_PORT}"), f"the daemon's web app is not on port {WEB_PORT}: {h}")
    return p


def stop(name, sig=signal.SIGTERM):
    p = procs.pop(name, None)
    if p and p.poll() is None:
        p.send_signal(sig)
        try:
            p.wait(10)
        except subprocess.TimeoutExpired:
            p.kill()
            p.wait(5)


def start_serve():
    log = open(os.path.join(ROOT, "serve.log"), "ab")
    procs["serve"] = subprocess.Popen([OPENCODE, "serve", "--port", str(OC_PORT), "--print-logs", "--log-level", "INFO"],
                                      cwd=PROJECT, env=oc_env(), stdin=subprocess.DEVNULL, stdout=log, stderr=log)
    oc = OpenCode(OC_PORT)
    wait_until(lambda: _alive(oc), 60, 0.5, what="opencode serve")
    return oc


def _alive(oc):
    try:
        return oc.req("GET", "/session")[0] == 200
    except Exception:
        return False


def teardown():
    for name in list(procs):
        stop(name)
    if RUN and os.path.isdir(RUN):
        shutil.rmtree(RUN)


# ---------- scenarios (OpenCode serve: the plugin talks to the real HTTP server) ----------

def s_policy_deny(oc):
    sid = oc.session("policy")
    n = oc.prompt(sid, "RUNBASH echo forbidden-1")
    check(oc.idle(sid, n), "no idle")
    st, out = tool_result(oc.last_tool(sid, "bash"))
    check(st == "error" and "Blocked by the e2e policy" in out, f"tool {st}: {out}")
    check(mock_saw("Blocked by the e2e policy"), "the model never saw the deny reason")
    s = wait_until(lambda: ph_session(sid), 10, what="pilothouse session")
    check(s["agent"] == "opencode" and s["state"] == "waiting", f"session {s['agent']} {s['state']}")
    check(not oc.pending_permissions(), "OpenCode still asks after a policy deny")
    return f"deny reached the model; session {s['id']} {s['state']}"


def s_desk_step_aside(oc):
    presence("here")
    sid = oc.session("desk")
    n = oc.prompt(sid, "RUNBASH echo desk-1")
    ev = oc.wait_event(lambda e: e["type"] == "permission.asked", n)
    check(ev, "OpenCode asked nothing (permission bash: ask from the suggestion?)")
    req = wait_until(lambda: [r for r in requests("stepped_aside") if r["summary"] == "Run: echo desk-1"], 10, what="stepped-aside request")[0]
    time.sleep(1)
    check(any(p["id"] == ev["properties"]["id"] for p in oc.pending_permissions()), "OpenCode's own dialog went away at the desk")
    # The person answers in OpenCode.
    oc.req("POST", f"/permission/{ev['properties']['id']}/reply", {"reply": "once"})
    check(oc.idle(sid, n), "no idle")
    st, out = tool_result(oc.last_tool(sid, "bash"))
    check(st == "completed" and "desk-1" in out, f"tool {st}: {out}")
    r = wait_until(lambda: sock(f"/v1/requests/{req['id']}")["status"] == "answered_locally" and sock(f"/v1/requests/{req['id']}"), 10, what="answered_locally")
    return f"request {r['id']} route={req['route']} → {r['status']}"


def s_away_allow(oc):
    presence("away")
    sid = oc.session("away-allow")
    n = oc.prompt(sid, "RUNBASH echo away-allow")
    rc, out = phone("respond", PHONE_STATE, "permission", "Run: echo away-allow", '{"behavior":"allow"}')
    check(rc == 0, "phone: " + out)
    check(oc.idle(sid, n), "no idle")
    st, res = tool_result(oc.last_tool(sid, "bash"))
    check(st == "completed" and "away-allow" in res, f"tool {st}: {res}")
    check(not oc.pending_permissions(), "OpenCode's dialog still open")
    return out.splitlines()[0]


def s_away_deny(oc):
    presence("away")
    sid = oc.session("away-deny")
    n = oc.prompt(sid, "RUNBASH echo away-deny")
    rc, out = phone("respond", PHONE_STATE, "permission", "Run: echo away-deny", '{"behavior":"deny","message":"Not from the phone, use a dry run"}')
    check(rc == 0, "phone: " + out)
    check(oc.idle(sid, n), "no idle")
    st, res = tool_result(oc.last_tool(sid, "bash"))
    check(st == "error" and "Not from the phone" in res, f"tool {st}: {res}")
    check(mock_saw("Not from the phone"), "the model never saw the phone's feedback")
    return "deny feedback reached the model"


def s_question(oc):
    presence("away")
    sid = oc.session("question")
    n = oc.prompt(sid, "ASKQ")
    rc, out = phone("respond", PHONE_STATE, "question", "Which color?", '{"behavior":"allow","answers":{"#1":"Blue"}}')
    check(rc == 0, "phone: " + out)
    check(oc.idle(sid, n), "no idle")
    st, res = tool_result(oc.last_tool(sid, "question"))
    check(st == "completed" and "Blue" in res, f"question tool {st}: {res}")
    n = oc.prompt(sid, "ASKMULTI")
    rc, out2 = phone("respond", PHONE_STATE, "question", "Which checks?", '{"behavior":"allow","answers":{"#1":"Lint, E2E","#2":"Yes"}}')
    check(rc == 0, "phone: " + out2)
    check(oc.idle(sid, n), "no idle")
    st, res = tool_result(oc.last_tool(sid, "question"))
    check(st == "completed" and "Lint" in res and "E2E" in res and "Yes" in res and "Unit" not in res, f"multi {st}: {res}")
    return res.replace("\n", " ")[:160]


def s_ask_held_at_desk(oc):
    presence("here")
    sid = oc.session("ask")
    n = oc.prompt(sid, "RUNBASH echo askme-1")
    req = wait_until(lambda: [r for r in requests("open") if r["summary"] == "Run: echo askme-1"], 20, what="held ask")[0]
    check(req["route"] == "devices", f"ask at the desk routed {req['route']}")
    rc, out = ph_person("approve", req["id"])
    check(rc == 0, "approve: " + out)
    check(oc.idle(sid, n), "no idle")
    st, res = tool_result(oc.last_tool(sid, "bash"))
    check(st == "completed" and "askme-1" in res, f"tool {st}: {res} (OpenCode's own prompt should be answered by the approval)")
    n = oc.prompt(sid, "RUNBASH echo askme-2")
    req = wait_until(lambda: [r for r in requests("open") if r["summary"] == "Run: echo askme-2"], 20, what="held ask 2")[0]
    rc, out = ph_person("deny", req["id"], "--message", "No askme today")
    check(rc == 0, "deny: " + out)
    check(oc.idle(sid, n), "no idle")
    st, res = tool_result(oc.last_tool(sid, "bash"))
    check(st == "error" and "No askme today" in res, f"tool {st}: {res}")
    return "approved via `pilothouse approve`, denied via `pilothouse deny`"


def s_reply(oc):
    presence("here")
    sid = oc.session("reply")
    n = oc.prompt(sid, "hello")
    check(oc.idle(sid, n), "no idle")
    s = wait_until(lambda: (ph_session(sid) or {}).get("caps", {}).get("reply") and ph_session(sid), 15, what="caps.reply")
    check(s["caps"].get("reply_via") == "plugin", f"caps {s['caps']}")
    n = oc.mark()
    rc, out = phone("reply", PHONE_STATE, sid, "PHONE-REPLY please summarize")
    check(rc == 0, "phone reply: " + out)
    check(oc.idle(sid, n), "the reply started no turn")
    check(mock_saw("PHONE-REPLY please summarize"), "the model never got the reply")
    s = wait_until(lambda: "PHONE-REPLY" in (ph_session(sid) or {}).get("last_message", "") and ph_session(sid), 10, what="last message")
    evs = sock(f"/v1/sessions/{s['id']}/events?limit=50") or []
    check(any(e["type"] == "session.reply" for e in evs), "no session.reply event")
    return f"reply acted on; last message {s['last_message']!r}"


def s_daemon_down(oc):
    sid = oc.session("down")
    stop("daemon")
    n = oc.prompt(sid, "RUNBASH echo forbidden-while-down")
    ev = oc.wait_event(lambda e: e["type"] == "permission.asked", n, 20)
    check(ev, "OpenCode stopped asking with the daemon down")
    oc.req("POST", f"/permission/{ev['properties']['id']}/reply", {"reply": "once"})
    check(oc.idle(sid, n, 30), "OpenCode hung with the daemon down")
    st, res = tool_result(oc.last_tool(sid, "bash"))
    check(st == "completed" and "forbidden-while-down" in res, f"tool {st}: {res} (fail open expected)")
    procs["daemon"] = start_daemon()
    # The plugin reconnects and announces its sessions: replies work again.
    n = oc.prompt(sid, "back")
    check(oc.idle(sid, n), "no idle after restart")
    s = wait_until(lambda: (ph_session(sid) or {}).get("caps", {}).get("reply") and ph_session(sid), 30, what="caps.reply after restart")
    return f"fail open; after restart session {s['id']} reply={s['caps'].get('reply')}"


SERVE = [s_policy_deny, s_desk_step_aside, s_away_allow, s_away_deny, s_question, s_ask_held_at_desk, s_reply, s_daemon_down]


# ---------- TUI (in-process client: no listening server) ----------

def s_tui():
    sys.path.insert(0, os.path.join(HERE, "..", "claude"))
    from ptyterm import Term  # needs pyte
    presence("away")
    before = {s["id"] for s in sock("/v1/sessions") or []}
    t = Term([OPENCODE, "--prompt", "RUNBASH echo tui-held"], env=oc_env(), cwd=PROJECT, log=os.path.join(ROOT, "tui.log"), cols=140, rows=40)
    try:
        rc, out = phone("respond", PHONE_STATE, "permission", "Run: echo tui-held", '{"behavior":"allow"}', "90")
        check(rc == 0, "phone: " + out)
        wait_until(lambda: mock_saw("tui-held") and any("RESULT: tui-held" in (json.loads(l).get("reply") or "")
                                                        for l in open(MOCK_LOG)), 30, what="tool ran in the TUI")
        new = wait_until(lambda: [s for s in sock("/v1/sessions") or [] if s["id"] not in before and s["agent"] == "opencode"], 10, what="TUI session")
        s = new[0]
        wait_until(lambda: (sock(f"/v1/sessions/{s['id']}") or {}).get("caps", {}).get("reply"), 30, what="caps.reply (TUI)")
        presence("here")
        rc, out = phone("reply", PHONE_STATE, s["agent_session_id"], "TUI-REPLY from the phone")
        check(rc == 0, "reply: " + out)
        wait_until(lambda: mock_saw("TUI-REPLY from the phone"), 30, what="reply reached the TUI session")
        return f"held+allowed and replied in the TUI (session {s['id']})"
    finally:
        t.close()
        try:
            t.wait_exit(10)
        except Exception:
            pass


def s_wrap():
    """`pilothouse run opencode`: the plugin's hooks join the wrapped session."""
    presence("away")
    env = oc_env()
    env["PATH"] = os.path.dirname(OPENCODE) + ":" + env.get("PATH", "/usr/bin:/bin")
    before = {s["id"] for s in sock("/v1/sessions") or []}
    r = subprocess.run([PH, "run", "-d", "-n", "wrapped-oc", "--", "opencode", "--prompt", "RUNBASH echo wrapped-1"],
                       env=env, cwd=PROJECT, capture_output=True, text=True, timeout=30)
    check(r.returncode == 0, "run: " + r.stdout + r.stderr)
    sid = r.stdout.strip().splitlines()[-1]
    oc_pid = 0
    try:
        rc, out = phone("respond", PHONE_STATE, "permission", "Run: echo wrapped-1", '{"behavior":"allow"}', "90")
        check(rc == 0, "phone: " + out)
        wait_until(lambda: any("RESULT: wrapped-1" in (json.loads(l).get("reply") or "") for l in open(MOCK_LOG)), 30, what="tool ran wrapped")
        s = wait_until(lambda: (lambda x: x if x and x.get("agent_session_id") and x["caps"].get("reply") else None)(sock(f"/v1/sessions/{sid}")), 30, what="wrapped session with caps.reply")
        oc_pid = s.get("pid") or 0
        check(s["mode"] == "wrapped" and s["agent"] == "opencode" and s["caps"].get("reply_via") == "plugin", f"session {s}")
        extra = [x for x in sock("/v1/sessions") or [] if x["id"] not in before and x["id"] != sid and x["agent"] == "opencode"]
        check(not extra, f"the wrapped OpenCode also showed up as another session: {extra}")
        return f"wrapped session {sid} (OpenCode pid {oc_pid}) caps={s['caps']}"
    finally:
        if oc_pid:
            try:
                os.kill(oc_pid, signal.SIGTERM)  # the OpenCode this scenario started; the holder exits with it
            except ProcessLookupError:
                pass


def main():
    args = [a for a in sys.argv[1:] if not a.startswith("--")]
    tui = "--tui" in sys.argv
    try:
        setup()
        oc = start_serve()
        for fn in SERVE:
            if args and fn.__name__ not in args:
                continue
            say("---", fn.__name__)
            try:
                results[fn.__name__] = "PASS: " + fn(oc)
            except Exception as ex:
                traceback.print_exc()
                results[fn.__name__] = f"FAIL: {ex}"
            say(results[fn.__name__])
        stop("serve")
        for fn in [s_tui, s_wrap] if tui else []:
            if args and fn.__name__ not in args:
                continue
            say("---", fn.__name__)
            try:
                results[fn.__name__] = "PASS: " + fn()
            except Exception as ex:
                traceback.print_exc()
                results[fn.__name__] = f"FAIL: {ex}"
            say(results[fn.__name__])
    finally:
        teardown()
    print("[e2e] RESULTS " + json.dumps(results, indent=1), flush=True)
    sys.exit(0 if results and all(v.startswith("PASS") for v in results.values()) else 1)


if __name__ == "__main__":
    main()
