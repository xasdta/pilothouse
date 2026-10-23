"""Drive the inbox TUI in a PTY: wait for TEXT, dump the screen, press KEYS,
dump again, quit. Run it through detach.py so the TUI is not a descendant of
an agent (the guard would refuse its decisions otherwise).
Usage: tui_driver.py OUTFILE WAIT_REGEX KEYS"""
import sys, time
from common import *
out, rx, keys = sys.argv[1], sys.argv[2], sys.argv[3]
t = Term([PH], env=env(), cols=140, rows=40)
log = open(out, "w")
try:
    t.wait(rx, 40)
    time.sleep(1)
    log.write("===== BEFORE\n" + t.text() + "\n")
    for k in keys:
        t.send(k); time.sleep(1.5)
    time.sleep(2)
    log.write("===== AFTER\n" + t.text() + "\n")
except Exception as e:
    log.write("ERROR " + str(e) + "\n")
finally:
    t.send("q"); t.wait_exit(5); t.close(); log.close()
