"""Logging HTTP CONNECT proxy: records every tunnel target (and plain-HTTP
request line) to OUT, then tunnels it. Used to prove what the daemon sends.
Usage: python3 -I connect_proxy.py PORT OUT"""
import socket, sys, threading, time

PORT, OUT = int(sys.argv[1]), sys.argv[2]

def pipe(a, b):
    try:
        while True:
            d = a.recv(65536)
            if not d:
                break
            b.sendall(d)
    except OSError:
        pass
    finally:
        for s in (a, b):
            try: s.shutdown(socket.SHUT_RDWR)
            except OSError: pass

def handle(c):
    data = b""
    while b"\r\n\r\n" not in data:
        d = c.recv(4096)
        if not d:
            return
        data += d
    line = data.split(b"\r\n", 1)[0].decode("latin-1")
    with open(OUT, "a") as f:
        f.write(f"{time.time():.1f} {line}\n")
    parts = line.split()
    if parts[0] != "CONNECT":
        c.sendall(b"HTTP/1.1 502 no\r\n\r\n"); c.close(); return
    host, port = parts[1].rsplit(":", 1)
    try:
        u = socket.create_connection((host, int(port)), timeout=10)
    except OSError:
        c.sendall(b"HTTP/1.1 502 no\r\n\r\n"); c.close(); return
    c.sendall(b"HTTP/1.1 200 Connection established\r\n\r\n")
    threading.Thread(target=pipe, args=(c, u), daemon=True).start()
    pipe(u, c)

s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", PORT)); s.listen(50)
while True:
    c, _ = s.accept()
    threading.Thread(target=handle, args=(c,), daemon=True).start()
