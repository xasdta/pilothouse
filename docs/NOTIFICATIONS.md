# Notifications: how Pilothouse reaches you

When an agent needs a person and you're **at your desk**, the agent's own
prompt appears as usual and Pilothouse only shows a desktop notification if
it waits too long (`presence.notify_after`, 45 s). When you're **away**
(keyboard idle for `presence.idle_threshold`, or `pilothouse away`), the
request is held and sent to every remote channel you set up:

| Channel | Set up with | Answer from it | Works without Tailscale |
|---|---|---|---|
| Web app (Web Push) | `pilothouse remote tailscale` + `pilothouse pair` | yes, signed by the phone's key | no |
| Telegram | `pilothouse notify telegram` | yes: buttons, replies | yes |
| ntfy app | `pilothouse notify ntfy` | Allow / Deny on permission requests when remote access is on | notifications yes, buttons no |
| Slack / Discord | `notify.webhooks` in `config.yaml` | no | yes |
| Desktop | on by default (`notify.desktop`) | no | — |

`pilothouse setup` offers all of them; `pilothouse notify test` sends a test
to each, and `pilothouse doctor` checks them.

## What a notification says

Every channel words requests the same way: the agent and the repo (and
branch), then what is asked — the command, the file, the question and its
options, or the plan's title. When the request carries them, extra lines
follow: risk labels (`⚠ Force-pushes to main`), the size of a file change
(`✎ 2 files, +12 −3`) and the agent's last words before it asked.

## Withdrawal: no stale buttons

When a request closes anywhere — answered on another device, in the
terminal, by the CLI or the inbox, or handed back to the terminal — every
remote notification about it is taken back:

- **Web Push** sends one more push with the same tag (`req-<id>`) and
  `{"type": "resolved", "request_id", "status", "by"}` plus a ready-made
  title and body ("✓ Allowed", "Run: git push — from iPhone"). The service
  worker replaces the notification with that short one, quietly. It is never
  a silent push (iOS revokes subscriptions that receive pushes without
  showing a notification). The device that answered is skipped: it already
  knows. Requests that only timed out keep their "waiting in the terminal"
  notice.
- **ntfy** notifications carry a sequence ID (`X-Sequence-ID: req-<id>`)
  and are cleared from the phone when the request is answered
  (`PUT /<topic>/<id>/clear`). This needs ntfy server 2.16 or newer
  (ntfy.sh is); older servers ignore both and the notification stays. A
  timeout replaces the request with "waiting in the terminal" in place.
- **Telegram** edits the message: "✓ Allowed from iPhone", "✗ Denied from the
  CLI", "⏱ Timed out — answer in the terminal". Its buttons stop working.
- Slack/Discord webhooks and desktop notifications can't be updated and are
  left as they are.

## Telegram

Telegram is a two-way channel that needs no Tailscale: your machine polls
Telegram's Bot API, so nothing listens for inbound connections.

### Set it up

```sh
pilothouse notify telegram
```

1. In Telegram, open **@BotFather**, send `/newbot`, pick a name and a
   username.
2. Paste the token it gives you when asked. Input is hidden; the token can
   also come on stdin (`pilothouse notify telegram < token.txt`), but never as
   a command-line argument (arguments show up in `ps` and shell history).
   Pilothouse checks it with Telegram and stores it in
   `~/.pilothouse/keys/telegram.json` (mode 0600).
3. Open the `https://t.me/<bot>?start=<code>` link on your phone (or scan the
   QR code) and tap **Start**. The code works once, for 15 minutes, and only
   in a private chat.

The daemon finishes pairing if it is running; otherwise the command does it
and you start the daemon afterwards. Running the command again re-pairs (press
Enter to keep the same bot); the chat that opens the new link takes over.

Options: `--notify-only` (no buttons, no replies), `--no-wait` (show the link
and exit), `--no-qr`. `pilothouse notify telegram off` turns Telegram off and
deletes the token; delete the bot in @BotFather (`/deletebot`) if you don't need it.

### Using it

- **Permission requests** arrive with **Allow**, **Allow for session** and
  **Deny**.
- **Questions** show one question at a time with a button per option. For
  multi-select questions the buttons toggle (☐/☑) and **Done** moves on. The
  last answer answers the agent. **Answer in the app** opens the web app for
  free-text answers (needs remote access).
- **Plans** arrive with **Approve** and **Request changes**. Request changes
  asks you to reply with what should change; your reply goes to the agent,
  which revises the plan. Replying straight to the plan message works too.
- **"Finished" messages**: reply to one to send that agent a follow-up
  prompt. This works for sessions that can receive replies (sessions started
  with `pilothouse run`, Claude Code sessions with a messaging socket, OpenCode
  through its plugin). Otherwise the bot tells you why it couldn't send it.
  A reply that reaches the daemon more than 10 minutes late (typed while your
  machine was asleep) isn't delivered.
- **/status** lists what is waiting right now and re-sends held requests that
  have no buttons in the chat yet.
- Every message is edited when its request closes, wherever it was answered.

Answers from Telegram show up as `telegram:<chat id>` in `pilothouse log` and
`explain`.

### Configuration

```yaml
notify:
  telegram:
    enabled: true
    chat_id: 123456789     # written by pairing; 0 = not paired
    decisions: true        # false: notify only (no buttons, no replies)
    api_base: ""           # Bot API server; default https://api.telegram.org
```

The daemon follows changes to this section and to the key file without a
restart.

### Security

- The bot answers exactly one chat: the private chat that sent the one-time
  code. Messages and button presses from any other chat are ignored without
  a reply.
- Every button carries a random single-use token (stored hashed) bound to
  that chat, that message, the request and the action, valid until the
  request expires. Pressing a button spends its whole keyboard; a replayed or
  forged press does nothing.
- The token never appears in `config.yaml`, logs, events or error messages.
- Telegram's servers see your requests' text (commands, file names, questions,
  plans). If that's not acceptable for a repo, use the web app (end to end
  between your machine and your phone over Tailscale) or `--notify-only`.
- Unlike the web app, Telegram answers are not signed by a key on your phone:
  anyone who can use your Telegram account can answer. Telegram's own
  security (passcode, two-step verification) applies.

### Troubleshooting

- `pilothouse doctor` checks the key file, its permissions, the pairing and
  that Telegram accepts the token.
- *"another program is reading this bot's updates"* in the daemon log: only
  one program may poll a bot. Use one bot per machine, and don't share it
  with another bot framework. A webhook set on the bot is removed by
  `pilothouse notify telegram`.
- *"the bot token was rejected"*: the token was revoked in @BotFather; run
  `pilothouse notify telegram` with the new one.

## ntfy

```sh
pilothouse notify ntfy            # a random, unguessable topic on ntfy.sh
pilothouse notify ntfy my-topic --server https://ntfy.example.com
```

Topics on ntfy.sh are public to anyone who knows the name, so the default is
random. Subscribe to it in the ntfy app (iPhone or Android). With remote
access on, permission requests carry Allow / Deny buttons backed by
single-use action tokens; questions and plans open the web app. An access token goes in `notify.ntfy.token`
(`tk_…` or `user:pass`). The daemon reads ntfy settings at start:
`pilothouse service restart` after changing them (setup does this for you).

## Web app (Web Push)

Pair your phone with `pilothouse pair` (after `pilothouse remote tailscale`),
add the web app to the home screen, and turn on notifications in
Settings → Notifications. Permission requests offer Allow / Deny right on the
notification on Android and desktop, signed with the device's key like every
answer from the app; questions and plans open the app.
