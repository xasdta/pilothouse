<p align="center">
  <img src="docs/assets/logo.svg" width="72" height="72" alt="Pilothouse logo">
</p>

<h1 align="center">Pilothouse</h1>

<p align="center"><b>Steer every coding agent from one place.</b><br>
One inbox for Claude Code and Codex. Your rules run before every tool call,<br>and anything that needs a person reaches you at your desk or on your phone.</p>

<p align="center">
  <a href="https://pilothouse.dev">Website</a> ·
  <a href="https://pilothouse.dev/docs">Docs</a> ·
  <a href="docs/POLICY.md">Policy reference</a> ·
  <a href="docs/AGENTS.md">Agent support</a>
</p>

---

```sh
curl -fsSL https://pilothouse.dev/install.sh | sh     # or: brew install xasdta/tap/pilothouse
pilothouse setup
```

<p align="center"><img src="docs/assets/demo.gif" width="900" alt="Pilothouse demo: a rule blocks a force-push, then the inbox holds a git push to main while you're away and you approve it with y"></p>

## Why

You start three agents, walk away, and come back to find two of them stuck on
a permission prompt since minute four. Or you approve everything to avoid that,
and one of them force-pushes. Pilothouse fixes both:

- **One inbox.** Every Claude Code and Codex session on the machine, in any
  terminal, sorted by what needs you. It uses the agents' official hooks, not
  screen-scraping.
- **Rules before every tool call.** Policies in YAML, with optional CEL
  expressions, run at the agent's pre-tool hook: block force-push, ask before
  `git push` to main, keep agents out of `.env`. Any `deny` wins.
- **Approvals that follow you.**
  - **At your desk:** the agent's own prompt appears as usual, so nothing
    freezes.
  - **Away** (keyboard idle): the request is held and pushed to your paired
    phone. Tap Allow, Allow for session or Deny. Claude Code also keeps its
    own prompt open in the terminal; whichever you answer first wins, and the
    other one closes.
  - **When you come back:** any request still held is handed back to your
    terminal.
- **Yours.** A single open-source Go binary. No account, no cloud, no
  telemetry, no inbound ports. Your phone reaches your machine over your own
  Tailscale network.

## How it works

```
 Claude Code / Codex ──hook──▶ pilothouse daemon ──▶ policy (allow / ask / deny)
                                     │
                     needs a person? ├─ at desk ─▶ the agent's own prompt
                                     └─ away ────▶ phone (Web Push / ntfy) ─▶ signed Allow / Deny
```

1. `pilothouse setup` installs hooks into Claude Code (`~/.claude/settings.json`)
   and Codex (`~/.codex/hooks.json`). It merges them with your existing hooks
   and writes a backup first. It then starts a small per-user daemon (launchd or
   systemd).
2. Every hook call goes to the daemon over a Unix socket. Policy checks take
   microseconds, and the whole round trip is about 1 ms.
3. Permission requests are routed by presence (OS idle time, or `pilothouse
   here` / `away` / `auto`).
4. Your phone runs a small web app served by your own machine. Pair it with a
   QR code. Each device keeps a non-extractable signing key, and every remote
   approval is signed.

## Everyday use

```sh
pilothouse                       # inbox (TUI): y allow · a allow for session · n deny · enter attach
pilothouse ls                    # sessions and waiting requests
pilothouse approve r_7k2m        # or: deny r_7k2m -m "use a PR instead"
pilothouse away                  # hold requests for my phone (here / auto)
pilothouse explain s_3fq9        # why is this session "blocked"? show the evidence
pilothouse log -f                # live event log
```

### Phone

```sh
pilothouse remote tailscale      # HTTPS on your tailnet via Tailscale Serve (nothing public)
pilothouse pair                  # scan the QR code with your phone, add it to the home screen
```

Notifications go out over Web Push. You can also use [ntfy](https://ntfy.sh),
whose notifications have Allow and Deny buttons, Slack or Discord webhooks, or
desktop notifications.

The inbox shows each request with what the agent last said, the command
highlighted, a diff for file edits, risk labels and the rule that asked. You
can answer agents' questions and approve or send back their plans. **Make it
a rule** turns an answer into a rule with one tap. On a computer, the web app
has three panes (sessions · current view · needs you) and keyboard shortcuts:
`j`/`k` to move, `a` to allow, `d` to deny, `⌘K` to search, `?` to list them all.
Light and dark mode follow your device unless you pick one in Settings.

### Rules

```yaml
# ~/.pilothouse/policy.yaml   (pilothouse policy init writes a commented starter)
version: 1
rules:
  - name: no-force-push
    match: { kind: shell, command: "git push*" }
    when: 'command.matches("(--force|-f)\\b")'
    action: deny
    message: Force-push is blocked. Open a PR instead.

  - name: ask-before-pushing-main
    match: { kind: shell, command: "git push*{main,master}*" }
    action: ask

  - name: no-secrets
    match: { path: "**/.env*" }
    action: deny
```

`pilothouse policy test --tool Bash --command "git push -f origin main"` shows
which rule decides, and `pilothouse policy replay --since 7d` shows what your
current rules would have done to last week's tool calls. A repo can ship its own
`.pilothouse/policy.yaml`. Until you `pilothouse policy trust` that repo, its
rules can only make things stricter. See [docs/POLICY.md](docs/POLICY.md).

### Wrap mode and tasks

```sh
pilothouse run claude                     # detach with ctrl+\ — the agent keeps running
pilothouse attach s_3fq9                  # from any terminal, even after a daemon restart

pilothouse task new "fix the flaky checkout test"   # own worktree + branch + agent
pilothouse task diff t_8d1x
pilothouse task done t_8d1x --pr          # push and open a PR with gh
```

## Security model

- The daemon listens on a Unix socket (mode 0600) and on `127.0.0.1` for the
  web app. Remote access goes through Tailscale Serve only.
- Remote decisions must carry an ECDSA P-256 signature from a paired device,
  with a timestamp and a single-use nonce.
- **Agents can't approve themselves.**
  - Local decisions are refused when the caller descends from an agent process.
  - A built-in rule forces a person to confirm any agent command that runs
    `pilothouse approve` or edits hook settings.
  - This is defense in depth, not a sandbox: policy sees what the agent's hook
    sees. A process that double-forks away from the agent escapes the ancestry
    check, and an obfuscated command can escape the rule. For hard isolation, run
    agents as another user or in a container.
- Fail-open by default: if the daemon is down, agents behave as if Pilothouse
  weren't installed. Set `approvals.fail_closed: true` to deny instead.
- Telemetry: none. The only network call is a daily anonymous update check,
  sending the version, OS, arch and an ID that changes every month. Turn it off
  with `update_check: false` or `PILOTHOUSE_NO_UPDATE_CHECK=1`.

## Status

v0.1 supports **Claude Code** (rules, remote approvals and state) and **Codex**
(remote approvals, deny rules and state; Codex's pre-tool hook can't allow or
ask). See [docs/AGENTS.md](docs/AGENTS.md) for what each agent supports and
why. It runs on **macOS and Linux** (amd64 and arm64).

Roadmap: an end-to-end-encrypted relay for people without Tailscale, more agents
(OpenCode, Copilot, Gemini), team policies, and Windows.

## Build from source

```sh
git clone https://github.com/xasdta/pilothouse && cd pilothouse
go build -o pilothouse ./cmd/pilothouse   # Go 1.27+, no cgo
go test -race ./...
```

## License

[Apache-2.0](LICENSE)
