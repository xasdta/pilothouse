<p align="center">
  <img src="docs/assets/logo.svg" width="72" height="72" alt="Pilothouse logo">
</p>

<h1 align="center">Pilothouse</h1>

<p align="center"><b>Steer every coding agent from one place.</b><br>
One inbox for Claude Code, Codex and OpenCode. Your rules run before every tool call,<br>and anything that needs a person reaches you at your desk or on your phone.</p>

<p align="center">
  <a href="https://pilothouse.dev">Website</a> ·
  <a href="https://pilothouse.dev/docs">Docs</a> ·
  <a href="docs/POLICY.md">Policy reference</a> ·
  <a href="docs/AGENTS.md">Agent support</a>
</p>

---

```sh
curl -fsSL https://pilothouse.dev/install.sh | sh     # or: brew install xasdta/tap/pilothouse
pilothouse demo      # optional: try the phone app with pretend agents (touches nothing)
pilothouse setup     # hooks (shows the diff first), daemon, phone, test notification
```

See [Getting started](docs/GETTING-STARTED.md) for what setup and the demo do.

<p align="center"><img src="docs/assets/demo.gif" width="900" alt="Pilothouse demo: a rule blocks a force-push, then the inbox holds a git push to main while you're away and you approve it with y"></p>

## Why

You start three agents, walk away, and come back to find two of them stuck on
a permission prompt since minute four. Or you approve everything to avoid that,
and one of them force-pushes. Pilothouse fixes both:

- **One inbox.** Every Claude Code, Codex and OpenCode session on the machine,
  in any terminal, sorted by what needs you. It uses the agents' official hooks
  (for OpenCode, a small plugin), not screen-scraping.
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
- **Questions and plans, too.** When Claude Code asks you something
  (`AskUserQuestion`) or wants a plan approved (`ExitPlanMode`), the question
  is held and pushed like a permission: pick an option or type your own
  answer, approve the plan or tell Claude what to change, from your phone,
  the inbox or the CLI. Claude carries on with your answer.
- **Yours.** A single open-source Go binary. No account, no cloud, no
  telemetry, no inbound ports. Your phone reaches your machine over your own
  Tailscale network.

## How it works

```
 Claude Code / Codex ──hook──▶ pilothouse daemon ──▶ policy (allow / ask / deny)
                                     │
                     needs a person? ├─ at desk ─▶ the agent's own prompt
                                     └─ away ────▶ phone (Web Push / ntfy / Telegram) ─▶ Allow / Deny
```

1. `pilothouse setup` installs hooks into Claude Code (`~/.claude/settings.json`)
   and Codex (`~/.codex/hooks.json`). It shows the exact diff, merges with your
   existing hooks and writes a backup first. For OpenCode it installs a plugin
   (`~/.config/opencode/plugins/pilothouse.js`) and offers to make OpenCode
   ask before shell commands and edits, which it otherwise allows. It then
   starts a small per-user daemon (launchd or systemd) and helps your phone
   reach you.
2. Every hook call goes to the daemon over a Unix socket. Policy checks take
   microseconds, and the whole round trip is about 1 ms.
3. Permission requests are routed by presence (OS idle time, or `pilothouse
   here` / `away` / `auto`). Questions and plans arrive at the same hook and
   are routed the same way; the answer goes back to the agent in its own
   format (for Claude Code, the answers or the approved plan; a plan you send
   back with feedback is revised).
4. Your phone runs a small web app served by your own machine. Pair it with a
   QR code. Each device keeps a non-extractable signing key, and every remote
   approval is signed.

## Everyday use

```sh
pilothouse                       # inbox (TUI): y allow · a allow for session · n deny · enter answer/attach · r reply
pilothouse ls                    # sessions and waiting requests (questions and plans too)
pilothouse approve r_7k2m        # or: deny r_7k2m -m "use a PR instead"
pilothouse answer r_9q4p 2 "Lint, Tests"   # answer a question: option number, label, or your own words
pilothouse deny r_3p1n -m "use PKCE"       # send a plan back with what to change (approve accepts it)
pilothouse away                  # hold requests for my phone (here / auto)
pilothouse explain s_3fq9        # why is this session "blocked"? show the evidence
pilothouse activity s_3fq9 -f    # what the agent is doing: prompts, tool calls, results, files changed
pilothouse usage --days 30       # tokens and estimated cost per day, agent, model and repo
pilothouse log -f                # live event log
```

### Usage and cost

Pilothouse reads Claude Code and Codex transcripts (read-only, on this machine)
to show each session's activity and token use, and estimates cost from list
prices. Set a daily budget to get a notification at 80 % and 100 % of it:

```yaml
# ~/.pilothouse/config.yaml
usage:
  daily_budget_usd: 20
  prices:                        # USD per million tokens; overrides the built-in list prices
    claude-opus-5-5: { input: 4, output: 20, cache_read: 0.2, cache_write: 5 }
```

Estimates use public list prices; plans and discounts aren't known to
Pilothouse. See [docs/USAGE.md](docs/USAGE.md) for what is read, how it is
counted and where the prices come from.

### Phone

```sh
pilothouse remote tailscale      # HTTPS on your tailnet via Tailscale Serve (nothing public)
pilothouse pair                  # pair a phone; on iOS add it to the Home Screen first, then enter the code
pilothouse notify telegram       # answer from Telegram: buttons, replies; no Tailscale needed
pilothouse notify ntfy           # or the ntfy app (Allow / Deny buttons with remote access)
pilothouse notify test           # check what reaches you
```

Notifications go out over Web Push from the paired web app, the
[ntfy](https://ntfy.sh) app, a Telegram bot you create, Slack or Discord
webhooks, or desktop notifications. They say what is asked (the command, the
question and its options, the plan), with risk labels and the agent's last
words when Pilothouse has them. When a request is answered anywhere, the
phone's notification is replaced ("✓ Allowed from iPhone") instead of
lingering with stale buttons. In Telegram, requests arrive with buttons
(Allow / Allow for session / Deny, question options, plan Approve / Request
changes), and replying to a "finished" message sends that agent a follow-up.
See [docs/NOTIFICATIONS.md](docs/NOTIFICATIONS.md).

Open a session in the app to see what the agent is doing: its activity turn by
turn (prompts, tool calls with their output, subagents, errors), the files it
changed, its tokens and estimated cost, and a *Catch me up* summary of what
happened since you last looked. Where the agent supports it you can reply,
interrupt, or open its live terminal (sessions started with `pilothouse run`),
with a key row for Esc, Ctrl-C, arrows, Enter, y and n. *Tasks* starts an agent
on a prompt in its own worktree; you review the diff and ship it as a pull
request or discard it. *Insights* shows usage and cost over 7, 30 or 90 days,
what your rules allowed, asked and blocked, and today's budget. Replies,
terminal typing (one signature unlocks it for 10 minutes), and starting or
finishing tasks are signed by the device like approvals.

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

`pilothouse policy test "git push -f origin main"` shows which rule decides,
and `pilothouse policy replay --since 7d` shows what your current rules would
have done to last week's tool calls. A repo can ship its own
`.pilothouse/policy.yaml`. Until you `pilothouse policy trust` that repo, its
rules can only make things stricter. See [docs/POLICY.md](docs/POLICY.md).

#### Rules without YAML

Every approval card shows what the call would do: a diff for file edits
(Claude's Edit/Write, Codex's apply_patch, OpenCode's edit/write), risk labels
such as *Force-pushes to main*, *Deploys*, *Reads .env* or *Runs a script
downloaded from get.example.com*, the rule that fired and the agent's last
words before it asked. Under it are one-tap rules: *Always allow `npm test` in
api-server*, *Always ask before `git push`*, *Never allow force-pushes*, *Allow
edits under `src/**` in api-server*. The ones that match what you approved
again and again this week come first ("You allowed this 5 times this week").

Presets bundle rules for a common need:

```sh
pilothouse policy preset guarded-autopilot on   # a safe alternative to --dangerously-skip-permissions
pilothouse policy preset protect-main on        # pushes/commits to main need you; force-pushes are blocked
pilothouse policy preset no-secrets on          # no .env, keys or credential dumps
pilothouse policy preset deploys-need-approval on
pilothouse policy add ask-push --action ask --command "git push" --command "git push *"
pilothouse policy rules                         # everything, in the order it is checked
```

Guarded autopilot lets routine work in a repo run without prompts (reading,
editing files inside the repo, tests, builds, linters, read-only git), blocks
destructive commands (`rm -rf ~`, `git reset --hard`, force-pushes, `DROP
TABLE`) and asks before pushes, deploys, secrets and sudo. On Codex only its
deny rules take effect (Codex's hooks can't skip its own approvals).

Rules from your phone, `policy add` and presets live in
`~/.pilothouse/rules.yaml`, which Pilothouse writes; your `policy.yaml` stays
yours. They match shell commands one command at a time, so allowing
`npm test` never allows `npm test && curl … | sh`. Adding a rule from a device
is a signed action, and a device can't add `when:` expressions or touch
built-in, hand-written or repo rules.

### Replies and the live terminal

```sh
pilothouse reply s_3fq9 "also add a test for the empty cart"
pilothouse reply s_3fq9 --interrupt "stop, use the staging database"
```

Sessions started with `pilothouse run` get replies typed into their terminal
(and can be interrupted); Claude Code sessions you started yourself get them
through Claude's own messaging socket. On the phone, wrapped sessions also have
a live terminal you can watch and type into, without disturbing the terminal
at your desk. While agents work, Pilothouse keeps the computer from idle
sleep (`keep_awake: off` to turn that off). See [docs/REPLIES.md](docs/REPLIES.md).

### Wrap mode and tasks

```sh
pilothouse run claude                     # detach with ctrl+\ — the agent keeps running
pilothouse attach s_3fq9                  # from any terminal, even after a daemon restart

pilothouse task new "fix the flaky checkout test"   # own worktree + branch + agent
pilothouse task diff t_8d1x
pilothouse task done t_8d1x --pr          # push and open a PR with gh
```

A repo's `.pilothouse/task.yaml` copies files such as `.env` into each new
worktree and runs setup commands (`npm ci`) before the agent starts — setup
only in repos you trust. Tasks started from the phone go through the same
steps. See [docs/TASKS.md](docs/TASKS.md).

## Security model

- The daemon listens on a Unix socket (mode 0600) and on `127.0.0.1` for the
  web app. Remote access goes through Tailscale Serve only.
- Remote decisions must carry an ECDSA P-256 signature from a paired device,
  with a timestamp and a single-use nonce.
- Telegram answers only the one private chat that opened the one-time pairing
  link; every other chat is ignored. Its buttons carry single-use tokens bound
  to the chat, message, request and action. The bot token stays in
  `~/.pilothouse/keys/telegram.json` (0600), never in config, logs or events.
  `notify.telegram.decisions: false` makes it notify-only.
- **Agents can't approve themselves.**
  - Local decisions are refused when the caller descends from an agent process.
  - A built-in rule forces a person to confirm any agent command that runs
    `pilothouse approve` or edits hook settings.
  - This is defense in depth, not a sandbox: policy sees what the agent's hook
    sees. A process that double-forks away from the agent escapes the ancestry
    check, and an obfuscated command can escape the rule. For hard isolation, run
    agents as another user or in a container.
- Replies from devices are signed like decisions. Typing into a live terminal
  uses a 10-minute token minted by one signed action and bound to that
  session and device. Claude's messaging-socket token is kept in a private
  table, never in events, logs or the API.
- Fail-open by default: if the daemon is down, agents behave as if Pilothouse
  weren't installed. Set `approvals.fail_closed: true` to deny instead.
- Transcripts are read only from the agents' own folders (`~/.claude/projects`,
  `$CODEX_HOME/sessions`), with symlinks resolved, and only as regular `.jsonl`
  files with bounded reads: a hook payload can't point Pilothouse at anything else.
- Telemetry: none. Apart from the notification services you configure (Web
  Push, ntfy, Telegram, webhooks), the only network call is a daily anonymous
  update check, sending the version, OS, arch and an ID that changes every
  month. Turn it off with `update_check: false` or
  `PILOTHOUSE_NO_UPDATE_CHECK=1`.

## Status

| | Claude Code | Codex | OpenCode |
|---|---|---|---|
| Integration | hooks | hooks | plugin |
| Session state, inbox | ✓ | ✓ | ✓ |
| Deny rules | ✓ | ✓ | ✓ |
| Ask rules | ✓ (its prompt) | denied with a note (no prompt from a hook) | held for you in Pilothouse, at the desk too |
| Remote approvals | ✓ | ✓ | ✓ |
| Questions from anywhere | ✓ (and plans) | shown, answered in the terminal | ✓ |

v0.1 supported **Claude Code** and **Codex**; 0.2 adds Claude Code questions
and plan approval from anywhere, and **OpenCode** (1.x) through a plugin:
rules before every tool call, its permission prompts and questions held while
you're away, and replies from your phone that it takes as your next prompt.
Codex's `request_user_input` questions show in the inbox but are answered in
its terminal (no hook can answer them). See [docs/AGENTS.md](docs/AGENTS.md)
for what each agent supports and why. It runs on **macOS and Linux** (amd64
and arm64).

Roadmap: an end-to-end-encrypted relay for people without Tailscale, more agents
(Copilot, Gemini), team policies, and Windows.

## Build from source

```sh
git clone https://github.com/xasdta/pilothouse && cd pilothouse
go build -o pilothouse ./cmd/pilothouse   # Go 1.27+, no cgo
go test -race ./...
```

## License

[Apache-2.0](LICENSE)
