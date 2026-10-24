package policy

import (
	"fmt"
	"strings"
	"sync"

	"github.com/xasdta/pilothouse/internal/api"
)

// Presets are named bundles of rules a person switches on from the inbox
// or with `pilothouse policy preset <name> on`. They are ordinary rules
// (match/when, as in policy.yaml) evaluated after the managed rules, with
// shell-aware command matching (shellmatch.go). Their rules are named
// "<preset>/<rule>".

// presetDef is one preset.
type presetDef struct {
	name, title, description string
	rules                    []api.RuleSpec
}

// Glob building blocks shared by several presets.
var (
	// recursiveRm are the spellings of a recursive rm.
	recursiveRm = []string{"-r", "-R", "-rf", "-fr", "-Rf", "-fR", "-rF", "-rfv", "-vrf", "-frv", "--recursive", "-r -f", "-f -r", "-R -f", "-rf --", "--recursive --force", "--force --recursive"}
	// broadTargets are paths whose recursive deletion is never routine. A
	// literal '*' is escaped: in a glob it would match any target.
	broadTargets = `{/,/\*,~,~/,~/\*,.,./,./\*,\*,..,../,../\*,$HOME,$HOME/,$HOME/\*,/Users,/home,/usr,/etc,/var,/opt}`

	deployCommands = []string{
		"kubectl {apply,create,delete,replace,patch,rollout,scale,set,edit,drain}{, *}",
		"kubectl * {apply,create,delete,replace,patch,rollout,scale,set,edit,drain}{, *}",
		"oc {apply,create,delete,replace,patch,rollout}{, *}",
		"helm {install,upgrade,uninstall,delete,rollback}{, *}",
		"helm * {install,upgrade,uninstall,rollback}{, *}",
		"{terraform,tofu,terragrunt} {apply,destroy,import}{, *}",
		"{terraform,tofu} -chdir=* {apply,destroy,import}{, *}",
		"terragrunt run-all {apply,destroy}{, *}",
		"pulumi {up,update,destroy}{, *}",
		"{fly,flyctl} {deploy,launch}{, *}",
		"vercel{, deploy} {--prod,--production}{, *}", "vercel * {--prod,--production}{, *}", "vercel promote{, *}",
		"netlify deploy{, *}",
		"gcloud * deploy{, *}",
		"aws * deploy{, *}",
		"aws cloudformation {create-stack,update-stack,delete-stack,execute-change-set}{, *}",
		"aws lambda {update-function-code,update-function-configuration,create-function,publish-version}{, *}",
		"aws ecs {update-service,create-service}{, *}",
		"{sam,cdk} {deploy,destroy}{, *}", "{serverless,sls} {deploy,remove}{, *}",
		"firebase deploy{, *}", "wrangler {deploy,publish}{, *}", "railway {up,deploy}{, *}",
		"eb deploy{, *}", "kamal deploy{, *}", "skaffold {run,deploy}{, *}", "argocd app sync{, *}", "nomad job run{, *}",
		"heroku {releases:rollback,ps:scale,container:release}{, *}",
		"git push {heroku,dokku,production}{, *}",
		"{docker,podman} push{, *}",
		"{npm,pnpm,yarn,bun} publish{, *}", "cargo publish{, *}", "twine upload{, *}", "gem push{, *}",
		"{poetry,uv} publish{, *}", "gh release create{, *}",
		"ansible-playbook{, *}",
		"{npm,pnpm,yarn,bun} run {deploy,release,publish}{, *}", "make {deploy,release,publish}{, *}",
	}

	// secretPaths are credential files for path rules.
	secretPaths = []string{
		".env", ".env.*", ".envrc", "*.pem", "*.key", "*.p12", "*.pfx", "id_rsa*", "id_dsa*", "id_ecdsa*", "id_ed25519*",
		"credentials.json", "service-account*.json", "~/.ssh/**", "~/.aws/**", "~/.gnupg/**", "~/.config/gcloud/**",
		"~/.kube/config", "~/.docker/config.json", "~/.netrc", "~/.npmrc", "~/.pypirc", "~/.git-credentials",
		"~/.config/gh/hosts.yml", "~/.codex/auth.json",
	}
	// notSecret: templates and public files that match secretPaths.
	notSecret = `paths.all(p, p.endsWith(".example") || p.endsWith(".sample") || p.endsWith(".template") || p.endsWith(".dist") ||` +
		` p.endsWith(".pub") || p.endsWith("/known_hosts") || p.endsWith("/.ssh/config") || p.endsWith("/.aws/config"))`
	// secretInShell: a shell command naming a credential file.
	secretInShell = `command.matches(r'''(^|[\s"'=/<])\.env(rc|\.[A-Za-z0-9_-]+)?($|[\s"';|&)<>])''') && !command.matches(r'\.env\.(example|sample|template|dist)\b')` +
		` || command.matches(r'\.ssh/id_|\.aws/credentials|\.netrc|\.git-credentials|\.config/gh/hosts\.yml|\.codex/auth\.json|\.docker/config\.json|\.pem\b')`
	credentialDumps = []string{
		"printenv{, *}", "env", "export -p",
		"security {find-generic-password,find-internet-password,dump-keychain}{, *}",
		"gh auth token{, *}", "gcloud auth {print-access-token,print-identity-token}{, *}",
		"gcloud auth application-default print-access-token{, *}",
		"aws configure {get,export-credentials}{, *}", "aws secretsmanager get-secret-value{, *}",
		"op {read,inject}{, *}", "op item get{, *}", "vault read{, *}", "vault kv get{, *}",
		"kubectl get {secret,secrets}{, *}", "cat /proc/*/environ",
	}
	protectedBranches = []string{"main", "master", "release*", "release/**"}
	protectedTargets  = "{main,master,release*}"
	forcePushWhen     = `command.matches(r'\s(-f|--force|--force-with-lease|--force-if-includes|--mirror)(\s|=|$)|\s\+[^\s]')`
)

func rmBroad() []string {
	var out []string
	for _, f := range recursiveRm {
		out = append(out, "rm "+f+" "+broadTargets)
	}
	return append(out, "rm * --no-preserve-root*")
}

var presetDefs = []presetDef{
	{
		name:  "guarded-autopilot",
		title: "Guarded autopilot",
		description: "A safe alternative to --dangerously-skip-permissions: routine work in a repo (reading, editing files " +
			"inside the repo, tests, builds, linters, read-only git and shell commands) runs without prompts; destructive " +
			"commands are blocked; pushes, deploys, secrets and sudo need a person. Anything else falls through to the " +
			"agent's own permission prompt. Codex honours deny only: its hooks can't skip Codex's own approvals, so the " +
			"allow rules do nothing there, and an ask blocks the call (the agent is told a person must run or allow it).",
		rules: []api.RuleSpec{
			{Name: "deny-destructive", Description: "Recursive deletes of broad paths, discarding work, rewriting shared history, wiping disks and databases",
				Action: api.BehaviorDeny,
				Match: map[string][]string{"kind": {"shell"}, "command": append(rmBroad(),
					"git reset{, *} --hard{, *}",
					"git clean {-f,-fd,-df,-fdx,-dfx,-xdf,-fxd,-xfd,-dxf,-fx,-xf,-ffdx,-ffd,--force}{, *}",
					"git clean -d {-f,-fx,-xf}{, *}", "git clean -x {-f,-fd,-df}{, *}",
					"git push {-f,--force,--mirror}{, *}", "git push * {-f,--force,--mirror}{, *}", "git push * +*",
					"git checkout{, *} {.,-- .}", "git restore .", "git restore -W .", "git restore --worktree .",
					"git filter-branch{, *}", "git filter-repo{, *}",
					"mkfs*", "dd * of=/dev/*", "dd of=/dev/*", "{fdisk,sfdisk,parted,wipefs} *",
					"diskutil {eraseDisk,eraseVolume,partitionDisk,zeroDisk,randomDisk,secureErase,reformat} *",
					"chmod {-R,--recursive} {777,0777,a+rwx,ugo+rwx,a+w,o+w} *", "chmod {777,a+rwx} {-R,--recursive} *",
					"find {/,~,~/,$HOME,..} *{-delete,-exec rm}*",
					"kill {-9 -1,-1,-KILL -1}", "crontab -r{, *}",
					"dropdb *", "{rails,rake,bin/rails} db:{drop,reset,purge}*", "{prisma,npx prisma} migrate reset*",
					"{prisma,npx prisma} db push *--force-reset*",
				)},
				Message: "Blocked by the guarded-autopilot preset: this command destroys data or history. If it is really intended, ask the user to run it."},
			{Name: "deny-destructive-sql", Description: "DROP, TRUNCATE and FLUSH through a database client",
				Action: api.BehaviorDeny,
				Match: map[string][]string{"kind": {"shell"}, "command": {
					"{psql,mysql,mariadb,sqlite3,sqlite,duckdb,sqlcmd,cockroach,clickhouse-client,mongo,mongosh,redis-cli}{, *}"}},
				When:    `command.lowerAscii().matches(r'\bdrop\s+(table|database|schema|collection)\b|\btruncate\s+(table\s+)?[a-z_"]|\bflush(all|db)\b|dropdatabase')`,
				Message: "Blocked by the guarded-autopilot preset: destructive SQL. Ask the user to run it if it is intended."},
			{Name: "ask-push", Description: "Every git push", Action: api.BehaviorAsk,
				Match:   map[string][]string{"kind": {"shell"}, "command": {"git push{, *}"}},
				Message: "The guarded-autopilot preset asks a person before every push."},
			{Name: "ask-deploys", Description: "Deploys, infrastructure changes and package publishing", Action: api.BehaviorAsk,
				Match:   map[string][]string{"kind": {"shell"}, "command": deployCommands},
				Message: "The guarded-autopilot preset asks a person before deploys and publishing."},
			{Name: "ask-sudo", Description: "Running as root", Action: api.BehaviorAsk,
				Match:   map[string][]string{"kind": {"shell"}, "command": {"{sudo,doas,su,pkexec,run0}{, *}"}},
				Message: "The guarded-autopilot preset asks a person before anything runs as root."},
			{Name: "ask-secret-files", Description: "Reading or writing .env files, keys and credentials", Action: api.BehaviorAsk,
				Match:   map[string][]string{"path": secretPaths},
				When:    "!" + notSecret,
				Message: "The guarded-autopilot preset asks a person before an agent touches credentials."},
			{Name: "ask-secrets-in-shell", Description: "Shell commands that name credential files or print credentials", Action: api.BehaviorAsk,
				Match: map[string][]string{"kind": {"shell"}},
				When:  secretInShell + ` || command.matches(r'(^|[;&|(]\s*)(printenv|env\s*($|[;&|])|security\s+find-|gh\s+auth\s+token)')`,
			},
			{Name: "ask-downloaded-scripts", Description: "Running a script straight from the network (curl … | sh)", Action: api.BehaviorAsk,
				Match: map[string][]string{"kind": {"shell"}},
				When:  `command.matches(r"(curl|wget)\s[^|]*\|\s*(sudo\s+)?(ba|z|da|k)?sh\b") || command.matches(r'''(ba|z)?sh\s+(-c\s+)?["']?(\$\(|<\()\s*(curl|wget)''')`,
			},
			{Name: "ask-risky-git", Description: "Deleting unmerged branches, dropping stashes, forced checkouts", Action: api.BehaviorAsk,
				Match: map[string][]string{"kind": {"shell"}, "command": {
					"git branch {-D,--delete --force} *", "git stash {drop,clear}{, *}", "git checkout {-f,--force}{, *}",
					"find *{-delete,-exec,-execdir}*"}}},
			{Name: "allow-read", Description: "Reading and searching files", Action: api.BehaviorAllow,
				Match: map[string][]string{"kind": {"read"}}},
			{Name: "allow-edits-in-repo", Description: "Editing files inside the session's repo", Action: api.BehaviorAllow,
				Match: map[string][]string{"kind": {"edit"}},
				When:  `repo != "" && size(paths) > 0 && paths.all(p, p.startsWith(repo + "/"))`},
			{Name: "allow-routine-shell", Description: "Tests, builds, linters, installs from lockfiles, read-only git and shell commands, in a repo",
				Action: api.BehaviorAllow,
				Match:  map[string][]string{"kind": {"shell"}, "command": routineCommands},
				When:   `repo != ""`},
		},
	},
	{
		name:  "protect-main",
		title: "Protect main",
		description: "Pushes to main, master and release branches need a person, force-pushes to them are blocked, and " +
			"committing or merging directly on them asks first. Codex honours deny only: there the asks block the call.",
		rules: []api.RuleSpec{
			{Name: "deny-force-push", Description: "Force-pushing to main, master or a release branch", Action: api.BehaviorDeny,
				Match: map[string][]string{"kind": {"shell"}, "command": {
					"git push * " + protectedTargets + "{, *}", "git push * *:" + protectedTargets + "{, *}",
					"git push * +" + protectedTargets + "{, *}", "git push * HEAD:" + protectedTargets + "{, *}"}},
				When:    forcePushWhen,
				Message: "Force-pushing to a protected branch is blocked by the protect-main preset. Push to a new branch and open a pull request."},
			{Name: "deny-force-push-from-main", Description: "Force-pushing while on main, master or a release branch", Action: api.BehaviorDeny,
				Match:   map[string][]string{"kind": {"shell"}, "command": {"git push{, *}"}, "branch": protectedBranches},
				When:    forcePushWhen,
				Message: "Force-pushing to a protected branch is blocked by the protect-main preset. Push to a new branch and open a pull request."},
			{Name: "ask-push", Description: "Pushing to main, master or a release branch", Action: api.BehaviorAsk,
				Match: map[string][]string{"kind": {"shell"}, "command": {
					"git push * " + protectedTargets + "{, *}", "git push * *:" + protectedTargets + "{, *}",
					"git push * +" + protectedTargets + "{, *}", "git push {--all,--branches,--mirror}{, *}", "git push * {--all,--branches}{, *}"}},
				Message: "Pushing to a protected branch needs a person's OK (protect-main preset)."},
			{Name: "ask-push-from-main", Description: "Pushing while on main, master or a release branch", Action: api.BehaviorAsk,
				Match:   map[string][]string{"kind": {"shell"}, "command": {"git push{, *}"}, "branch": protectedBranches},
				Message: "Pushing from a protected branch needs a person's OK (protect-main preset)."},
			{Name: "ask-commit-on-main", Description: "Committing, merging or rebasing directly on a protected branch", Action: api.BehaviorAsk,
				Match: map[string][]string{"kind": {"shell"}, "branch": protectedBranches,
					"command": {"git {commit,merge,cherry-pick,revert,am,rebase}{, *}"}},
				Message: "Changing a protected branch directly needs a person's OK (protect-main preset). Consider working on a branch."},
			{Name: "ask-pr-merge", Description: "Merging a pull request", Action: api.BehaviorAsk,
				Match: map[string][]string{"kind": {"shell"}, "command": {"gh pr merge{, *}"}}},
		},
	},
	{
		name:  "no-secrets",
		title: "No secrets",
		description: "Agents can't read or write .env files, private keys and cloud or SSH credentials, can't print " +
			"credentials from the keychain, password managers or the environment, and must ask before putting what " +
			"looks like a token on a command line. Templates (.env.example) and public keys are fine.",
		rules: []api.RuleSpec{
			{Name: "deny-secret-files", Description: ".env files, keys and credential files", Action: api.BehaviorDeny,
				Match:   map[string][]string{"path": secretPaths},
				When:    "!" + notSecret,
				Message: "This file holds secrets; the no-secrets preset keeps agents out of it. Ask the user for what you need."},
			{Name: "deny-secrets-in-shell", Description: "Shell commands that name credential files", Action: api.BehaviorDeny,
				Match:   map[string][]string{"kind": {"shell"}},
				When:    secretInShell,
				Message: "This command touches a file that holds secrets; the no-secrets preset blocks it. Ask the user for what you need."},
			{Name: "deny-credential-dumps", Description: "Printing credentials (environment, keychain, password managers, cloud CLIs)", Action: api.BehaviorDeny,
				Match:   map[string][]string{"kind": {"shell"}, "command": credentialDumps},
				Message: "Printing credentials is blocked by the no-secrets preset."},
			{Name: "ask-tokens-in-commands", Description: "Commands with what looks like a token or key in them", Action: api.BehaviorAsk,
				Match: map[string][]string{"kind": {"shell"}},
				When: `command.matches(r'\bgh[pousr]_[A-Za-z0-9]{36}|\bgithub_pat_|\bglpat-|\bxox[abposr]-|\bsk-ant-|\bsk-[A-Za-z0-9_-]{40}|\b(AKIA|ASIA)[0-9A-Z]{16}\b|\bAIza[0-9A-Za-z_-]{35}|\b(sk|rk)_live_|-----BEGIN [A-Z ]*PRIVATE KEY')` +
					` || command.matches(r'(?i)authorization:\s*(bearer|token|basic)\s+[A-Za-z0-9._~+/=-]{16,}')`,
				Message: "This command seems to contain a credential; a person must confirm it (no-secrets preset)."},
		},
	},
	{
		name:  "deploys-need-approval",
		title: "Deploys need approval",
		description: "Deploys, infrastructure changes (kubectl, terraform, helm, cloud CLIs, fly, vercel --prod, netlify), " +
			"image pushes and package publishing need a person. Codex honours deny only: there the asks block the call.",
		rules: []api.RuleSpec{
			{Name: "ask-deploys", Description: "Deploys, infrastructure changes and publishing", Action: api.BehaviorAsk,
				Match:   map[string][]string{"kind": {"shell"}, "command": deployCommands},
				Message: "Deploys need a person's OK (deploys-need-approval preset)."},
		},
	},
}

// routineCommands are what guarded-autopilot allows without a prompt.
// Shell-aware matching means every simple command of a line must be one
// of these (or harmless: cd within the repo, echo, a pipe into tail/grep…).
var routineCommands = []string{
	"{npm,pnpm,yarn,bun} {test,run test,run lint,run build,run typecheck,run check,run format,run fmt}{, *}",
	"{npm,pnpm,yarn,bun} run test:*", "{yarn,pnpm} {lint,build,typecheck}{, *}", "bun test{, *}",
	"npm ci{, *}", "{npm,bun} install", "{npm,bun} i", "pnpm install{, --frozen-lockfile}", "yarn{, install}{, --frozen-lockfile,--immutable}",
	"go {test,build,vet,fmt,list,version,env}{, *}", "go mod {tidy,download,verify,why,graph}{, *}", "gofmt *", "golangci-lint *",
	"cargo {test,build,check,clippy,fmt,doc,tree,metadata,fetch}{, *}",
	"{pytest,ruff,mypy,black,flake8,pylint,isort}{, *}", "{python,python3} -m {pytest,mypy,ruff,unittest}{, *}",
	"uv run {pytest,ruff,mypy}{, *}", "uv sync{, *}", "pip install -r *", "poetry install{, *}", "bundle install{, *}",
	"{tsc,eslint,prettier,jest,vitest,biome}{, *}", "npx {tsc,eslint,prettier,jest,vitest,biome}{, *}",
	"make", "make {test,build,lint,check,fmt,all,clean}{, *}",
	"git {status,diff,log,show,branch,add,commit,stash,switch,checkout,fetch,pull,rev-parse,ls-files,blame,grep,tag,describe,shortlog,reflog}{, *}",
	"git remote{, -v,show *,get-url *}", "git worktree list{, *}", "git config --get *",
	"ls{, *}", "pwd", "cat *", "head *", "tail *", "wc *", "grep *", "rg *", "find *", "fd *", "which *", "diff *",
	"tree{, *}", "stat *", "file *", "du *", "df{, *}", "sort *", "uniq *", "jq *", "date{, *}", "whoami", "uname{, *}",
	"mkdir *", "touch *", "realpath *", "basename *", "dirname *",
}

// PresetNames lists the presets in display order.
func PresetNames() []string {
	out := make([]string, len(presetDefs))
	for i, p := range presetDefs {
		out[i] = p.name
	}
	return out
}

func presetByName(name string) (presetDef, bool) {
	for _, p := range presetDefs {
		if p.name == name {
			return p, true
		}
	}
	return presetDef{}, false
}

// Presets describes every preset; Enabled is filled in by the engine.
func Presets() []api.Preset {
	out := make([]api.Preset, len(presetDefs))
	for i, p := range presetDefs {
		out[i] = api.Preset{Name: p.name, Title: p.title, Description: p.description, Rules: presetSpecs(p)}
	}
	return out
}

// presetSpecs are a preset's rules with their full names.
func presetSpecs(p presetDef) []api.RuleSpec {
	out := make([]api.RuleSpec, len(p.rules))
	for i, r := range p.rules {
		r.Name = p.name + "/" + r.Name
		m := make(map[string][]string, len(r.Match))
		for k, v := range r.Match {
			m[k] = append([]string(nil), v...)
		}
		r.Match = m
		out[i] = r
	}
	return out
}

// presetCache holds compiled presets per home directory (path globs
// expand ~ at compile time). Presets never change at run time.
var presetCache sync.Map // home + "\x00" + name → []*rule

func presetRules(name, home string) ([]*rule, error) {
	key := home + "\x00" + name
	if v, ok := presetCache.Load(key); ok {
		return v.([]*rule), nil
	}
	p, ok := presetByName(name)
	if !ok {
		return nil, FileErrors{{File: "preset:" + name, Msg: fmt.Sprintf("unknown preset %q (known: %s)", name, strings.Join(PresetNames(), ", "))}}
	}
	rules, err := compileSpecs("preset:"+name, presetSpecs(p), home)
	if err != nil {
		return nil, err
	}
	presetCache.Store(key, rules)
	return rules, nil
}
