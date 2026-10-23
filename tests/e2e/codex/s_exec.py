"""Non-interactive checks with `codex exec` (approval is always `never` in exec,
so no PermissionRequest here; see s_away.py for those).
  E1  SessionStart / UserPromptSubmit / Stop / SessionEnd tracked with cwd, repo, branch
  E2  deny rule on `git push*` blocks the command; the model gets the reason
  E3  a ~/.zshenv that prints to stdout (hooks run as `$SHELL -c`) must not
      disable the deny                                     [fails on v0.1: bug]
  E4  built-in self-protection on a write into ~/.pilothouse  [fails on v0.1: ask is a no-op on Codex]
  E5  path rule `**/.env*` vs apply_patch sent through exec_command
                                                           [fails on v0.1: paths not extracted]
  E6  fail-open: daemon stopped -> Codex runs normally, no hook errors
Needs: setup.sh done (daemon running, hooks installed, mock running)."""
import os, shutil, time
from common import *

results = {}
HOMEDIR = f"{ROOT}/uhome"
POL = f"{ROOT}/home/policy.yaml"


def check(name, ok, detail=""):
    results[name] = ok
    say(("PASS " if ok else "FAIL ") + name, detail)


def hook_lines(out):
    return [l for l in out.splitlines() if l.startswith("hook: ")]


shutil.copy(POL, POL + ".s_exec-bak") if os.path.exists(POL) else None
open(POL, "w").write("""version: 1
rules:
  - name: no-push
    match: { kind: shell, command: "git push*" }
    action: deny
    message: "QA6: pushing is blocked by Pilothouse policy."
  - name: no-secrets
    match: { path: "**/.env*" }
    action: deny
    message: "QA6: .env files are off limits."
""")
time.sleep(1.5)  # policy.yaml is re-read on change
try:
    before = {s["id"] for s in ls()["sessions"]}
    rc, out = codex_exec("scenario=hello")
    new = [s for s in ls()["sessions"] if s["id"] not in before]
    log = ph("log")
    s = new[-1] if new else {}
    check("E1 session tracked (start/prompt/stop/end, cwd, repo, branch)",
          len(new) == 1 and s.get("cwd") == REPO and s.get("repo_root") == REPO and s.get("branch") == "main"
          and s.get("state") == "exited" and all(k in log for k in ("SessionStart", "UserPromptSubmit", "Stop", "SessionEnd")),
          f"{s.get('id')} cwd={s.get('cwd')} repo={s.get('repo_root')} branch={s.get('branch')} state={s.get('state')}")

    rc, out = codex_exec("scenario=push")
    got = tool_output("call_push_0") or ""
    check("E2 deny rule blocks git push; model sees reason", "QA6: pushing is blocked" in got and "hook: PreToolUse Blocked" in out, got[:120])

    zshenv = os.path.join(HOMEDIR, ".zshenv")
    open(zshenv, "w").write('echo "QA6 ZSHENV NOISE"\n')
    try:
        rc, out = codex_exec("scenario=push")
    finally:
        os.unlink(zshenv)
    got = tool_output("call_push_0") or ""
    check("E3 noisy ~/.zshenv does not disable the deny", "QA6: pushing is blocked" in got,
          f"PreToolUse={[l for l in hook_lines(out) if 'PreToolUse' in l]} tool_output={got[:100]!r}")

    probe = f"{ROOT}/home/qa6-selfprot-probe"
    if os.path.exists(probe):
        os.unlink(probe)
    rc, out = codex_exec("scenario=selfprot")
    made = os.path.exists(probe)
    if made:
        os.unlink(probe)
    check("E4 built-in self-protection stops a write into the Pilothouse home", not made,
          f"file created={made}; policy log: {[l for l in ph('log').splitlines() if 'self-protection' in l][-1:]}")

    envf = os.path.join(REPO, ".env.local")
    if os.path.exists(envf):
        os.unlink(envf)
    rc, out = codex_exec("scenario=patch_env")
    made = os.path.exists(envf)
    if made:
        os.unlink(envf)
    check("E5 path rule **/.env* stops apply_patch sent via exec_command", not made,
          f".env.local created={made}; tool_output={(tool_output('call_patch_env_0') or '')[-60:]!r}")

    rc_stop, msg = ph_person("stop")
    try:
        t0 = time.time()
        rc, out = codex_exec("scenario=push")
        dt = time.time() - t0
        got = tool_output("call_push_0") or ""
        bad = [l for l in hook_lines(out) if "Failed" in l or "Blocked" in l]
        check("E6 fail-open: daemon stopped, Codex runs normally", rc == 0 and not bad and "blocked by PreToolUse" not in got,
              f"{dt:.1f}s, hook failures={bad}")
    finally:
        ph("start")
        time.sleep(1)
finally:
    if os.path.exists(POL + ".s_exec-bak"):
        shutil.move(POL + ".s_exec-bak", POL)
print("SUMMARY", results)
