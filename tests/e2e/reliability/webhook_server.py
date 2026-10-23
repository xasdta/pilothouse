"""Fake Slack/Discord incoming-webhook endpoint for the reliability suite.

Paths: /ok answers 200, /fail answers 500, /slow sleeps SLOW seconds then 200.
Every request is appended to OUT (JSON lines: path, headers, body, t).
Usage: python3 -I webhook_server.py PORT OUT [SLOW]
"""
import json
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT, OUT = int(sys.argv[1]), sys.argv[2]
SLOW = float(sys.argv[3]) if len(sys.argv) > 3 else 30.0
lock = threading.Lock()


class H(BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(n).decode("utf-8", "replace")
        rec = {"t": time.time(), "path": self.path, "headers": dict(self.headers), "body": body}
        with lock, open(OUT, "a") as f:
            f.write(json.dumps(rec) + "\n")
        if self.path.startswith("/slow"):
            time.sleep(SLOW)
        code = 500 if self.path.startswith("/fail") else 200
        self.send_response(code)
        self.send_header("Content-Type", "text/plain")
        self.end_headers()
        try:
            self.wfile.write(b"ok" if code == 200 else b"boom")
        except BrokenPipeError:
            pass

    def log_message(self, *a):
        pass


ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()
