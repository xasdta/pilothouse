"""Minimal PTY terminal for driving interactive TUIs (Claude Code, the
Pilothouse inbox, `pilothouse run`/`attach`) from test scripts.

Needs `pyte` (pip install pyte) to keep a virtual screen so assertions can
match what a person would see, not the raw escape stream."""
import codecs, os, pty, re, signal, struct, fcntl, termios, threading, time

import pyte


class Term:
    def __init__(self, argv, env=None, cwd=None, cols=150, rows=50, log=None):
        self.cols, self.rows = cols, rows
        self.screen = pyte.Screen(cols, rows)
        self.stream = pyte.Stream(self.screen)
        self.dec = codecs.getincrementaldecoder("utf-8")("replace")
        self.raw = bytearray()
        self.lock = threading.Lock()
        self.log = open(log, "ab") if log else None
        self.exitcode = None
        pid, fd = pty.fork()
        if pid == 0:
            if cwd:
                os.chdir(cwd)
            os.execvpe(argv[0], argv, env if env is not None else os.environ)
        self.pid, self.fd = pid, fd
        fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
        self.t = threading.Thread(target=self._read, daemon=True)
        self.t.start()

    def _read(self):
        while True:
            try:
                b = os.read(self.fd, 65536)
            except OSError:
                b = b""
            if not b:
                break
            with self.lock:
                self.raw += b
                if self.log:
                    self.log.write(b); self.log.flush()
                try:
                    self.stream.feed(self.dec.decode(b))
                except Exception:
                    pass
        try:
            _, st = os.waitpid(self.pid, 0)
            self.exitcode = os.waitstatus_to_exitcode(st)
        except ChildProcessError:
            pass

    def text(self):
        with self.lock:
            return "\n".join(l.rstrip() for l in self.screen.display)

    def rawtext(self):
        with self.lock:
            return bytes(self.raw)

    def wait(self, pattern, timeout=30, raw=False):
        rx = re.compile(pattern, re.S | re.I if isinstance(pattern, str) else 0)
        end = time.time() + timeout
        while time.time() < end:
            hay = self.rawtext() if raw else self.text()
            m = rx.search(hay)
            if m:
                return m
            if not self.t.is_alive() and not raw:
                break
            time.sleep(0.2)
        raise TimeoutError(f"timeout waiting for {pattern!r}\n----- screen -----\n{self.text()}\n------------------")

    def alive(self):
        return self.t.is_alive()

    def send(self, s, delay=0.05):
        if isinstance(s, str):
            s = s.encode()
        os.write(self.fd, s)
        time.sleep(delay)

    def type(self, s, delay=0.02):
        for ch in s:
            self.send(ch, delay)

    def resize(self, cols, rows):
        fcntl.ioctl(self.fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
        with self.lock:
            self.screen.resize(rows, cols)

    def wait_exit(self, timeout=30):
        self.t.join(timeout)
        return self.exitcode

    def close(self, sig=signal.SIGTERM):
        try:
            os.kill(self.pid, sig)
        except ProcessLookupError:
            pass
        self.t.join(5)
        if self.t.is_alive():
            try:
                os.kill(self.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
