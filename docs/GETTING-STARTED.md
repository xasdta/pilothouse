# Getting started

```sh
pilothouse demo     # optional: try the phone app first, with pretend agents
pilothouse setup    # set it up for real
```

## Try it: `pilothouse demo`

The demo runs the real Pilothouse web app against an inbox of pretend agents
that keep asking for things: permission requests, questions, plans. Answer
them from this computer's browser or your phone.

- It lives in a temporary directory that is deleted when you press Ctrl-C.
  Your daemon, hooks, paired devices and settings are not touched; nothing
  reaches your real agents.
- It listens on a free port on `127.0.0.1` and opens the browser signed in
  (`--no-open` to just print the link).
- **On your phone** it needs HTTPS the phone can reach. When Tailscale is
  connected with HTTPS certificates on, the demo serves itself on your tailnet
  at `https://<this machine>:8443` for as long as it runs (a foreground
  `tailscale serve` that stops with the demo) and shows a QR code to pair.
  It never takes over a port Tailscale already serves; pick another with
  `--tailscale-port`, or stay on this computer with `--local`.

## Set up: `pilothouse setup`

Setup is safe to run again any time; it only changes what is missing or out
of date. It walks through:

1. **Agents.** It finds the agents you have installed (Claude Code, Codex,
   OpenCode and Grok Build) and shows the **exact diff** of each file it would
   change (`~/.claude/settings.json`, `~/.codex/hooks.json` with its hook trust
   entries, the OpenCode plugin and `~/.grok/hooks/pilothouse.json`) before
   asking. Your own hooks are kept, and a backup is written first. Long diffs
   are cut; `pilothouse hooks install --dry-run` shows all of it.
2. **Daemon.** It runs the daemon at login with launchd (macOS) or systemd
   (Linux), or starts it in the background.
3. **Phone.**
   - **Remote access.** It checks Tailscale — installed, signed in, MagicDNS
     and HTTPS certificates on — and says what's missing. When everything is
     ready it offers `pilothouse remote tailscale`: the web app is served on
     your tailnet only, never on the public internet.
   - **Pairing.** It shows the pairing QR code right in the terminal. On
     iPhone/iPad, add the web app to the Home Screen first, then enter the code
     in the installed app (the camera opens Safari, which has separate storage).
     On Android, scan the QR code. Finish on the phone, then press Enter.
   - **Telegram** (works without Tailscale) and **ntfy** are offered too; see
     [NOTIFICATIONS.md](NOTIFICATIONS.md).
4. **Test notification** to every channel that can reach you, and a short
   **checklist** of what was done and what's next.

Non-interactive: `pilothouse setup --yes` accepts the defaults (hooks, login
service) and skips the phone steps, which need you to scan or tap something.
`--no-phone` skips them in an interactive run. Afterwards:

```sh
pilothouse doctor          # checks hooks, daemon, service, presence, notifications, Telegram
pilothouse pair            # pair another device
pilothouse notify test     # send a test notification
```
