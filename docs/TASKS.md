# Tasks

A task runs an agent on one prompt in its own git worktree and branch
(`pilothouse/<slug>`), so several agents can work on a repository without
touching your checkout. You start tasks from the terminal or from the web
app on your phone, look over the diff, then ship it (push and open a pull
request) or discard it.

- [From the terminal](#from-the-terminal)
- [From your phone](#from-your-phone)
- [Statuses and notifications](#statuses-and-notifications)
- [`.pilothouse/task.yaml`](#pilothousetaskyaml)
- [Security](#security)
- [Limits](#limits)

## From the terminal

```sh
pilothouse task new "add a --json flag to the export command"
pilothouse task new --agent codex --base main -a "fix the flaky login test"
pilothouse task new --agent opencode "update the README"
pilothouse task ls                 # open tasks; --all includes finished ones
pilothouse task diff <task>        # committed, uncommitted and new files; --stat
pilothouse task done <task> --pr   # push and open a PR with gh, remove the worktree
pilothouse task abandon <task>     # remove the worktree and delete the branch
```

`<task>` is a task id, a unique prefix of one, or its slug. The agent runs
detached under `pilothouse run` (attach with `pilothouse attach <session>`).
Worktrees live in `~/.pilothouse-worktrees/<repo>/<slug>`
(`$PILOTHOUSE_WORKTREES` overrides it). Tasks are recorded in
`~/.pilothouse/tasks.json` and work without the daemon; the daemon mirrors
them for the web app and the inbox.

`task ls` shows who started each task in its `BY` column: `cli`, `tui`, or
`device:<id>` for a phone or browser (`pilothouse devices` names it).

The agent gets the prompt the way it expects it: `claude "<prompt>"`,
`codex "<prompt>"` (after `--` when the prompt starts with `-`, so it is
never read as an option), `opencode --prompt "<prompt>"` (OpenCode reads a
bare argument as a project path).

## From your phone

*Tasks → New task* in the web app: pick a repository, an agent (Claude
Code, Codex or OpenCode) and write the prompt. The repositories offered
are the ones Pilothouse has seen agents work in (a task's worktree counts
as its repository), plus any you list in `~/.pilothouse/config.yaml`:

```yaml
tasks:
  repos:
    - ~/src/api-server
    - /Users/me/src/site
```

The agent must be installed on the computer and connected to Pilothouse
(`pilothouse hooks install`), so its permission requests reach your phone.

Starting, shipping and discarding are signed by your device, like
approvals. The task page shows the agent's progress and the diff file by
file as it grows. Then:

- **Ship** stops the agent, commits whatever it left uncommitted (the
  commit subject is the first line of your prompt), pushes the branch to
  `origin` and opens a pull request with `gh` (GitHub CLI) when it is
  installed. The worktree is removed; the branch stays. Without `gh` the
  branch is pushed and the task tells you to open the pull request on
  GitHub; without an `origin` remote the work stays committed on the branch
  and the task tells you how to push it. Ship is refused while the agent
  is still working. Git and `gh` never prompt: a push that needs a
  password you haven't stored fails, and the task stays open.
- **Discard** stops the agent, removes the worktree and deletes the branch.
  The task's note says how to restore the branch
  (`git branch pilothouse/<slug> <sha>`).

## Statuses and notifications

| Status | Meaning |
|---|---|
| `running` | The agent is working (or waiting for an approval). |
| `review` | The agent finished its turn or exited: look at the diff. |
| `done` | Shipped or `task done`. |
| `abandoned` | Discarded. |
| `failed` | The agent never started: `task.yaml`'s setup failed or the agent could not be launched. The worktree is kept for a look; discard it. |

When a task's agent finishes and you are away, Pilothouse sends *Ready for
review* (instead of the usual *finished its turn*); it opens the task. A
task that fails to start, or a Claude task waiting at Claude's "Do you trust
this folder?" question, is notified too. Claude asks that once per
repository it has never run in; Pilothouse never answers it for you —
attach (`pilothouse attach <session>`) or use the live terminal in the web
app.

## `.pilothouse/task.yaml`

Worktrees start from a clean checkout: no `.env`, no `node_modules`. A
repository can say what a new task's worktree needs:

```yaml
version: 1
copy:                 # from your checkout into each new worktree
  - .env
  - .env.*
  - config/local/*.json
setup:                # run in the worktree before the agent starts
  - npm ci
  - make generate
setup_timeout: 10m    # for all setup commands together (1s–2h, default 10m)
env:                  # for the setup commands and the agent
  PORT: "3001"
```

- `copy` — repo-relative globs (`*`, `?`, `[…]` within one path segment;
  `*` also matches names starting with a dot; no `**`), matched in your
  main checkout. A matching directory is copied with its contents. Files
  keep their permissions. A file the worktree already has (a tracked file)
  is never overwritten. Copied files stay local: they are left out of the
  task's diff and out of the commit Ship makes, and Ship refuses a branch on
  which the agent committed one of them.
- `setup` — shell commands (`/bin/sh -c`), run one after another in the new
  worktree with no input. A failing command stops the setup and the task
  fails without starting the agent. When `setup_timeout` runs out, each
  command's whole process group is killed. Output goes to
  `~/.pilothouse/logs/task-<id>.log` (and to your terminal for `task new`).
  The commands see `PILOTHOUSE_TASK_ID`, `PILOTHOUSE_TASK_REPO` (your main
  checkout) and `PILOTHOUSE_TASK_WORKTREE`.
- `env` — extra environment variables for the setup commands and the
  agent. Names starting with `PILOTHOUSE` are refused.

**Setup commands and `env` only take effect in repositories you trust**,
the same trust list that lets a repo's policy add allow rules:

```sh
pilothouse policy trust ~/src/api-server
```

In an untrusted repository the files are still copied, but the setup
commands are not run: `task new` lists them, and the task says why (the
phone shows it). Run them yourself in the worktree, or trust the repo and
start a new task.

`task.yaml` is read once, from your main checkout, when the task starts. A
file that doesn't parse stops the task from starting, with the reason.

From the phone, a task with setup commands appears right away with setup
"running"; the agent starts when the setup has finished.

## Security

- **Who can start a task.** From the web app only a paired device, with a
  signed request (single use, ±2 minutes). The daemon's local API for
  starting and finishing tasks refuses callers running inside an agent (the
  same check as approvals). `pilothouse task new` itself runs in your
  terminal like any command — an agent could run it too, just as it could
  run `git worktree add`. A device can only pick a repository from the list
  above and an agent from Claude Code, Codex, OpenCode and Grok Build — never
  an arbitrary command. The base branch may not start with `-`.
- **Where the agent runs.** The daemon starts the agent in a detached
  holder, so it never runs as a descendant of another agent or of the web
  request; it gets your prompt as data, never as options.
- **task.yaml is untrusted input.** Anything that can write to the
  repository can change it (an agent, a pull request, a repo you just
  cloned). So `setup` and `env` need your trust; `copy` can only copy
  regular files from inside the repository: no absolute paths or `..`,
  symlinks only to files inside the repository, symlinked directories not
  followed, never `.git`, never through a symlink in the worktree, and FIFOs
  and devices are skipped. A symlinked `task.yaml` is refused.
- **Secrets stay on the computer.** Copied files are not shown in diffs or
  committed by Ship. The setup commands' text and output stay in the task
  log (readable only by you); the task and notifications only say which
  command failed, by number. Credentials in a remote URL or GitHub tokens
  are removed from push and `gh` errors before they are shown. An untracked
  symlink in the worktree is shown as its target path, never read through.
- **Diffs are read-only and bounded.** Pilothouse reads the diff with git's
  external diff drivers, textconv and fsmonitor hook turned off (a
  repository's config could otherwise run commands), without taking git's
  locks next to a working agent.

## Limits

- Diffs on the phone: at most 300 files, 64 KiB of diff per file and 1 MiB
  in all; untracked files over 1 MiB are counted from their first MiB.
  `pilothouse task diff` shows everything.
- `copy`: at most 200 files and 50 MB per task; `task.yaml` at most 64 KiB,
  50 copy patterns, 20 setup commands, 50 env variables.
- Prompts: at most 32 KiB.
- Ship gives `git commit` 2 minutes, `git push` 3 and `gh pr create` 2.
- There is no `cleanup:` step yet; discarding removes the worktree with
  everything in it.
