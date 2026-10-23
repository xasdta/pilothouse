#!/usr/bin/env python3
"""Mock OpenAI Responses API for driving the real Codex CLI without an account.

    python3 mock_responses.py PORT LOGDIR

Codex is pointed at it with a custom provider (see README.md):

    model_provider = "mock"
    [model_providers.mock]
    base_url = "http://127.0.0.1:PORT/v1"
    env_key  = "MOCK_API_KEY"
    wire_api = "responses"

Scenario selection: the user prompt contains `scenario=<name>`. The step is the
number of function_call_output items already in the request's input, so step 0
emits the scenario's tool call, and the next request (carrying the tool result)
emits the final assistant message, which echoes the tool output so the test can
read what Codex reported back. Every request is logged (method, path, headers
minus auth, body) to LOGDIR/requests.jsonl."""
import json, os, re, sys, threading, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 7597
LOGDIR = sys.argv[2] if len(sys.argv) > 2 else "/tmp/qa6/mock"
os.makedirs(LOGDIR, exist_ok=True)
LOCK = threading.Lock()
N = [0]


def shell(cmd, escalate=False, justification=""):
    """exec_command call (Codex >= 0.1xx unified exec). Escalation asks for
    approval under approval_policy=on-request."""
    args = {"cmd": cmd}
    if escalate:
        args["sandbox_permissions"] = "require_escalated"
        args["justification"] = justification or "e2e: needs to run outside the sandbox"
    return ("function_call", "exec_command", args)


PATCH = """*** Begin Patch
*** Add File: patched.txt
+patched by codex
*** End Patch
"""

# name -> list of tool calls (one per step); after the last, a final message.
SCENARIOS = {
    "hello": [],
    "echo": [shell("echo hi > hello.txt")],
    "push": [shell("git push --force origin main")],
    "push_esc": [shell("git push --force origin main", escalate=True)],
    "touch_esc": [shell("touch escalated.txt", escalate=True)],
    "touch_esc2": [shell("touch escalated2.txt", escalate=True)],
    "touch_twice": [shell("touch twice1.txt", escalate=True), shell("touch twice2.txt", escalate=True)],
    "ls_esc": [shell("ls -la /", escalate=True)],
    # allow-for-session: the grant prefix for `mkdir -p x` is `mkdir`
    "grant1": [shell("mkdir -p sess_a", escalate=True)],
    "grant2": [shell("mkdir -p sess_b", escalate=True)],
    "allowpol": [shell("touch allowpol.txt", escalate=True)],
    "askpol": [shell("echo asked > asked.txt")],
    # apply_patch through the shell tool (Codex intercepts it as apply_patch)
    "patch_sh": [shell("apply_patch <<'EOF'\n" + PATCH + "EOF\n")],
    "patch_env": [shell("apply_patch <<'EOF'\n" + PATCH.replace("patched.txt", ".env.local") + "EOF\n")],
    # built-in self-protection (ask) on a protected path: Pilothouse's home
    "selfprot": [shell("touch " + os.path.realpath(os.environ.get("E2E_ROOT", "/tmp/qa6")) + "/home/qa6-selfprot-probe")],
    "sleep": [shell("sleep 30")],
    "push_ask": [shell("git push origin main", escalate=True)],
    "patch": [("custom_tool_call", "apply_patch", PATCH)],
    "patch_fn": [("function_call", "apply_patch", {"input": PATCH})],
}


def sse(ev):
    return f"event: {ev['type']}\ndata: {json.dumps(ev)}\n\n".encode()


def completed(rid):
    return {"type": "response.completed", "response": {"id": rid, "usage": {
        "input_tokens": 10, "input_tokens_details": None, "output_tokens": 5,
        "output_tokens_details": None, "total_tokens": 15}}}


def texts(items):
    out = []
    for it in items or []:
        c = it.get("content")
        if isinstance(c, list):
            for p in c:
                if isinstance(p, dict) and isinstance(p.get("text"), str):
                    out.append(p["text"])
        elif isinstance(c, str):
            out.append(c)
    return out


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def _log(self, body):
        hdrs = {k: v for k, v in self.headers.items() if k.lower() != "authorization"}
        hdrs["has_auth"] = "authorization" in {k.lower() for k in self.headers.keys()}
        with LOCK:
            N[0] += 1
            with open(os.path.join(LOGDIR, "requests.jsonl"), "a") as f:
                f.write(json.dumps({"n": N[0], "t": time.time(), "method": self.command, "path": self.path,
                                    "headers": hdrs, "body": body}) + "\n")

    def do_GET(self):
        self._log(None)
        if self.path.rstrip("/").endswith("/models"):
            data = json.dumps({"object": "list", "data": [], "models": []}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            return
        self.send_response(404)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n) if n else b""
        if self.headers.get("Content-Encoding") == "zstd":
            try:
                import zstandard
                raw = zstandard.ZstdDecompressor().decompressobj().decompress(raw)
            except Exception:
                pass
        try:
            body = json.loads(raw)
        except Exception:
            body = {"_raw": raw[:2000].decode("latin1")}
        self._log(body)
        if not self.path.rstrip("/").endswith("/responses"):
            self.send_response(404)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        items = body.get("input") or []
        user = "\n".join(texts([i for i in items if i.get("role") == "user"]))
        m = re.findall(r"scenario=(\w+)", user)
        name = m[-1] if m else "hello"
        outputs = [i for i in items if i.get("type") in ("function_call_output", "custom_tool_call_output")]
        # Only count outputs after the last scenario prompt (multi-turn sessions).
        last_user = max([k for k, i in enumerate(items) if i.get("role") == "user" and "scenario=" in "".join(texts([i]))] or [0])
        outputs = [i for k, i in enumerate(items) if k > last_user and i.get("type") in ("function_call_output", "custom_tool_call_output")]
        steps = SCENARIOS.get(name, [])
        if not body.get("tools"):
            steps = []  # side request (e.g. TUI title generation): no tools offered
        step = len(outputs)
        with LOCK:
            rid = f"resp_{N[0]}"
        evs = [{"type": "response.created", "response": {"id": rid}}]
        if step < len(steps):
            kind, tool, args = steps[step]
            cid = f"call_{name}_{step}_{N[0]}"
            if kind == "function_call":
                item = {"type": "function_call", "name": tool, "arguments": json.dumps(args), "call_id": cid}
            else:
                item = {"type": "custom_tool_call", "name": tool, "input": args, "call_id": cid}
            evs.append({"type": "response.output_item.added", "item": item})
            evs.append({"type": "response.output_item.done", "item": item})
        else:
            last = outputs[-1] if outputs else None
            out = ""
            if last is not None:
                o = last.get("output")
                out = o if isinstance(o, str) else json.dumps(o)
            text = f"MOCK-DONE scenario={name} steps={step} last_output={out[:600]!r}"
            item = {"type": "message", "role": "assistant", "id": f"msg_{N[0]}",
                    "content": [{"type": "output_text", "text": text}]}
            evs.append({"type": "response.output_item.done", "item": item})
        evs.append(completed(rid))
        payload = b"".join(sse(e) for e in evs)
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


if __name__ == "__main__":
    srv = ThreadingHTTPServer(("127.0.0.1", PORT), H)
    print(f"mock responses on 127.0.0.1:{PORT}, log {LOGDIR}/requests.jsonl", flush=True)
    srv.serve_forever()
