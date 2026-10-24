"""Scripted OpenAI-compatible (chat completions, streaming) model for
OpenCode e2e runs. The last user message picks what the "model" does:

  RUNBASH <command>        call the bash tool with <command>
  WRITEFILE <path> <text>  call the write tool
  ASKQ                     ask one question (Red / Blue)
  ASKMULTI                 ask a multi-select question and a yes/no one
  anything else            answer "ok: <the prompt>"

After a tool result it answers "RESULT: <what the tool returned>".
Every request is appended to the log (argv[2]) as one JSON line.
Usage: mock_openai.py PORT LOG"""
import itertools, json, sys, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

LOG = sys.argv[2]
ids = itertools.count(1)


def text_of(m):
    c = m.get("content")
    if isinstance(c, str):
        return c
    if isinstance(c, list):
        return " ".join(p.get("text", "") for p in c if isinstance(p, dict))
    return ""


def plan(last):
    if last.get("role") == "tool":
        return None, "RESULT: " + text_of(last)[:300]
    t = text_of(last) if last.get("role") == "user" else ""
    if "RUNBASH " in t:
        cmd = t.split("RUNBASH ", 1)[1].strip().splitlines()[0]
        return ("bash", {"command": cmd, "description": "e2e command"}), None
    if "WRITEFILE " in t:
        path, _, body = t.split("WRITEFILE ", 1)[1].strip().partition(" ")
        return ("write", {"filePath": path, "content": body or "hello"}), None
    if "ASKMULTI" in t:
        return ("question", {"questions": [
            {"question": "Which checks?", "header": "Checks", "multiple": True,
             "options": [{"label": "Lint", "description": "lint"}, {"label": "Unit", "description": "unit"}, {"label": "E2E", "description": "e2e"}]},
            {"question": "Ship it?", "header": "Ship", "options": [{"label": "Yes", "description": "y"}, {"label": "No", "description": "n"}]}]}), None
    if "ASKQ" in t:
        return ("question", {"questions": [{"question": "Which color?", "header": "Color",
                                            "options": [{"label": "Red", "description": "red"}, {"label": "Blue", "description": "blue"}]}]}), None
    return None, "ok: " + t.strip()[:80]


class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def do_GET(self):
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps({"object": "list", "data": [{"id": "m1", "object": "model"}]}).encode())

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))) or b"{}")
        msgs = body.get("messages", [])
        last = msgs[-1] if msgs else {}
        call, reply = plan(last)
        with open(LOG, "a") as f:
            f.write(json.dumps({"last_role": last.get("role"), "last_text": text_of(last)[:600],
                                "tools": len(body.get("tools") or []), "call": call and call[0], "reply": reply}) + "\n")
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.end_headers()

        def chunk(delta, finish=None):
            d = {"id": "c1", "object": "chat.completion.chunk", "created": int(time.time()), "model": "m1",
                 "choices": [{"index": 0, "delta": delta, "finish_reason": finish}]}
            self.wfile.write(("data: " + json.dumps(d) + "\n\n").encode())
            self.wfile.flush()

        if call:
            cid = "call_%d_%d" % (int(time.time() * 1000), next(ids))
            chunk({"role": "assistant", "tool_calls": [{"index": 0, "id": cid, "type": "function",
                                                        "function": {"name": call[0], "arguments": json.dumps(call[1])}}]})
            chunk({}, "tool_calls")
        else:
            chunk({"role": "assistant", "content": reply})
            chunk({}, "stop")
        u = {"id": "c1", "object": "chat.completion.chunk", "created": 0, "model": "m1", "choices": [],
             "usage": {"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18}}
        self.wfile.write(("data: " + json.dumps(u) + "\n\ndata: [DONE]\n\n").encode())
        self.wfile.flush()


ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
