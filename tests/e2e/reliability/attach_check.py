"""Attach to a wrapped session in a real PTY, expect output, detach with ctrl+\\.
Usage: python3 -I attach_check.py /path/to/pilothouse SESSION_ID
Exit 0 when "tick" was seen and the attach client exited after detach."""
import os, pty, select, sys, time

ph, sid = sys.argv[1], sys.argv[2]
pid, fd = pty.fork()
if pid == 0:
    os.execv(ph, [ph, "attach", sid])
buf, seen, deadline = b"", False, time.time() + 8
while time.time() < deadline:
    r, _, _ = select.select([fd], [], [], 0.2)
    if r:
        try:
            buf += os.read(fd, 4096)
        except OSError:
            break
    if not seen and buf.count(b"tick") >= 2:
        seen = True
        os.write(fd, b"\x1c")
        deadline = time.time() + 3
    if seen:
        wp, st = os.waitpid(pid, os.WNOHANG)
        if wp:
            print("attach: saw output, detached cleanly, exit", os.waitstatus_to_exitcode(st))
            sys.exit(0)
print("attach: FAILED; output tail:", buf[-300:])
try:
    os.kill(pid, 9)
except ProcessLookupError:
    pass
sys.exit(1)
