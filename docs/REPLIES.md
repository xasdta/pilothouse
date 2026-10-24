# Replies, the live terminal and keep-awake

Pilothouse 0.2 lets you steer a running agent from your phone, the inbox or
the CLI: send it a follow-up prompt, interrupt it, or watch and type into its
terminal. While agents work it also keeps the computer from falling asleep.

## Replies

```sh
pilothouse reply s_k3m "also update the changelog"
pilothouse reply s_k3m --interrupt "stop — use the staging database instead"
pilothouse reply s_k3m --interrupt          # just stop
git diff | pilothouse reply s_k3m -         # text from stdin (up to 16 KB)
```

On the phone, a session's page shows a reply box when the session can take
replies (`caps.reply` in the API). How a reply arrives depends on how the
agent was started:

| Session | How the reply arrives | Interrupt |
|---|---|---|
| Started with `pilothouse run` (any agent) | Typed into the agent's terminal: the text as one paste, then Enter | Yes: Esc first |
| Claude Code started on its own (2.1.224+) | Claude's session messaging socket | No |
| OpenCode | Its Pilothouse plugin | — |
| Codex started on its own | Not possible: start it with `pilothouse run codex` | — |

**Wrapped sessions.** The holder types the text as a *bracketed paste* when
the agent asked for one (Claude Code does), so a multi-line reply
reaches the agent as one prompt instead of being submitted at its first line.
Control characters in the text are dropped first, so a reply can never end
the paste early or press keys by itself. A program without bracketed paste
gets the text typed as is. An interrupt presses Esc, waits a moment, then
sends the text. A terminal attached with `pilothouse attach` stays attached
and sees the reply being typed.

**Claude Code sessions you started yourself.** Every hook of a Claude
session carries the path and token of the session's messaging socket; the
hook shim passes them to the daemon, which keeps them in a private table and
never in events, logs, `pilothouse explain` or any API response. A reply is
written to that socket. Claude starts a turn right away if it was idle (a busy
session queues it) and tells the model the message came from "another Claude
session", adding that such a message cannot grant permissions or approve
prompts. Pilothouse prefixes the text with `[Pilothouse] Message from the
person who started this session, sent from their phone with Pilothouse…`, so
the model treats it as your next prompt. Tool calls it makes still go through
Claude's permission settings and Pilothouse's rules as usual. If Claude has
exited or restarted, the reply fails with a clear message and the session
offers replies again after its next hook.

Every reply is recorded as a `session.reply` event (text, channel, who sent
it). Replies from devices are signed by the device key like decisions;
`pilothouse reply` is refused when it runs inside an agent's process tree.

## Live terminal

Sessions started with `pilothouse run` (by Pilothouse 0.2 or later) have a
live terminal on the phone: you see what the agent's terminal shows and can
type into it.

- Watching is read-only and never detaches a terminal attached at your desk.
  Several devices can watch at once; one that falls behind is dropped and
  reconnects.
- Typing needs a short-lived token: opening the terminal signs one action
  with the device key, which gives a token for that session and that device
  for 10 minutes. Keystrokes then need no signature of their own.
- While a terminal is attached at the desk, its size wins; with none
  attached, the phone may resize the agent's terminal to fit its screen.

Holders keep running across Pilothouse upgrades. A session started by an
older Pilothouse keeps working as before, but has no live terminal and no
replies through its terminal (a Claude session still gets replies through
its messaging socket); start it again to get them. Pilothouse only ever
probes such a holder, so it can never take over the terminal attached to it.

The device API is documented in `internal/web/terminal.go`:
`POST /api/sessions/{id}/terminal/token` (signed `terminal` action),
`GET /api/sessions/{id}/terminal?replay=1` (SSE: `size`, `data` as base64,
`exit`, `error`, `ping`) and `POST /api/sessions/{id}/terminal/input`
(`X-Terminal-Token`; `{"data": "<base64>"}` or `{"resize": {"cols", "rows"}}`).

## Keep-awake

```yaml
# ~/.pilothouse/config.yaml
keep_awake: working   # default; "off" never keeps the computer awake
```

While any session is working or waiting for you, the daemon runs
`caffeinate -i` (macOS) or `systemd-inhibit --what=idle:sleep` (Linux, when
installed) so the computer doesn't idle-sleep in the middle of a run. It
lets go a minute after the last session stops working, and when the daemon
stops. The inhibitor watches the daemon's process itself, so it ends even if
the daemon is killed. This prevents idle sleep only: closing a laptop's lid
still puts it to sleep.
