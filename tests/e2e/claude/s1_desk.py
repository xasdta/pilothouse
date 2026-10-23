"""Scenario 1 — desk path: presence 'here', interactive Claude asks for a
Write → Pilothouse steps aside, native dialog shows, answered via keystrokes →
answered_locally; then a Bash denied in the dialog → closed on next prompt."""
import os, shutil, sys, time
from common import *

W = f"{ROOT}/w_desk"
shutil.rmtree(W, ignore_errors=True); os.makedirs(W)
rc, out = ph_person("here"); say("here:", rc, out.strip())

def sess_for(cwd):
    for s in ls().get("sessions", []):
        if s.get("cwd", "").endswith(os.path.basename(cwd)) and s.get("state") != "exited":
            return s

t = claude(W, log=f"{ROOT}/s1.raw")
try:
    pass_startup(t)
    s = wait_until(lambda: sess_for(W), 20, what="session registered")
    sid = s["id"]; say("session", sid, s["state"], s.get("mode"))
    t.type("Use the Write tool to create the file hello.txt containing the single word hi. Do nothing else.")
    t.send("\r")
    t.wait(r"Do you want to (create|make this edit)", 90)
    say("native dialog visible")
    reqs = [r for r in requests() if r["session_id"] == sid]
    say("requests while dialog shown:", [(r["id"], r["tool"], r["status"], r.get("route")) for r in reqs])
    st = ph("ls", "--json"); s = [x for x in ls()["sessions"] if x["id"] == sid][0]
    say("session state while dialog:", s["state"], s.get("block_kind"), s.get("state_detail"))
    print(ph("explain", sid))
    time.sleep(7)  # let the ~6s Notification(permission_prompt) fire too
    t.send("1"); time.sleep(0.3)
    t.wait(r"hello\.txt", 5)
    wait_until(lambda: os.path.exists(f"{W}/hello.txt"), 60, what="hello.txt written")
    say("file written:", open(f"{W}/hello.txt").read().strip())
    r1 = wait_until(lambda: [r for r in requests() if r["session_id"] == sid and r["status"] != "stepped_aside"], 30, what="req closed")
    say("request after answer:", [(r["id"], r["status"], r.get("decision")) for r in r1])
    wait_until(lambda: [x for x in ls()["sessions"] if x["id"] == sid][0]["state"] == "waiting", 60, what="waiting")
    say("session reached waiting")
    print(ph("log", "--session", sid, "-n", "40"))

    # deny path
    t.type("Run this exact bash command with the Bash tool: touch denied_marker.txt")
    t.send("\r")
    t.wait(r"Do you want to proceed|Bash command", 90)
    t.wait(r"Do you want", 30)
    say("bash dialog visible")
    time.sleep(1)
    reqs = [r for r in requests() if r["session_id"] == sid and r["tool"] == "Bash"]
    say("bash req:", [(r["id"], r["status"]) for r in reqs])
    t.send("\x1b")  # Esc = No / reject
    time.sleep(4)
    say("screen after deny:\n" + t.text()[-1200:])
    reqs = [r for r in requests() if r["session_id"] == sid and r["tool"] == "Bash"]
    say("bash req after deny (before next prompt):", [(r["id"], r["status"]) for r in reqs])
    s = [x for x in ls()["sessions"] if x["id"] == sid][0]; say("state after deny:", s["state"])
    print(ph("explain", sid))
    t.type("Reply with just the word ok."); t.send("\r")
    wait_until(lambda: all(r["status"] != "stepped_aside" for r in requests() if r["session_id"] == sid), 60, what="bash req closed")
    reqs = [r for r in requests() if r["session_id"] == sid and r["tool"] == "Bash"]
    say("bash req after next prompt:", [(r["id"], r["status"]) for r in reqs])
    say("denied_marker exists?", os.path.exists(f"{W}/denied_marker.txt"))
    wait_until(lambda: [x for x in ls()["sessions"] if x["id"] == sid][0]["state"] == "waiting", 60, what="waiting2")
    print(ph("log", "--session", sid, "-n", "60"))
    print(ph("ls"))
finally:
    t.send("\x03"); time.sleep(0.5); t.send("\x03"); t.wait_exit(10); t.close()
    time.sleep(2)
    print(ph("ls", "-a"))
