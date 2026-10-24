package risk

import (
	"regexp"
	"strings"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/policy"
)

// ---- deploys -------------------------------------------------------------

// deploySubs maps a program and its subcommand to a deploy label level.
var deploySubs = map[string]map[string]api.RiskLevel{
	"kubectl": {"apply": high, "create": high, "delete": high, "replace": high, "patch": high, "rollout": high,
		"set": high, "edit": high, "drain": high, "scale": medium, "autoscale": medium, "cordon": medium,
		"uncordon": medium, "taint": medium, "label": medium, "annotate": medium, "expose": medium, "run": medium},
	"helm":       {"install": high, "upgrade": high, "uninstall": high, "delete": high, "rollback": high},
	"terraform":  {"apply": high, "destroy": high, "import": high, "taint": medium, "untaint": medium, "state": medium, "force-unlock": medium},
	"pulumi":     {"up": high, "update": high, "destroy": high, "import": high, "refresh": medium, "cancel": medium},
	"fly":        {"deploy": high, "launch": high, "destroy": high, "scale": medium, "secrets": medium},
	"firebase":   {"deploy": high},
	"wrangler":   {"deploy": high, "publish": high, "rollback": high, "delete": high},
	"railway":    {"up": high, "deploy": high, "redeploy": high, "down": high},
	"sam":        {"deploy": high, "delete": high, "sync": high},
	"cdk":        {"deploy": high, "destroy": high},
	"serverless": {"deploy": high, "remove": high},
	"eb":         {"deploy": high, "terminate": high, "swap": high},
	"kamal":      {"deploy": high, "redeploy": high, "rollback": high, "remove": high},
	"skaffold":   {"deploy": high, "run": medium, "delete": medium},
	"argocd":     {"app": medium},
	"nomad":      {"job": medium, "run": high},
	"heroku": {"releases:rollback": high, "apps:destroy": high, "pg:reset": high, "container:push": high,
		"container:release": high, "ps:scale": medium, "config:set": medium, "config:unset": medium},
	"supabase": {"db": medium, "functions": medium},
	"docker":   {"push": high},
	"npm":      {"publish": high, "unpublish": high, "deprecate": medium},
	"pnpm":     {"publish": high},
	"yarn":     {"publish": high},
	"bun":      {"publish": high},
	"cargo":    {"publish": high, "yank": medium},
	"twine":    {"upload": high},
	"gem":      {"push": high, "yank": medium},
	"poetry":   {"publish": high},
	"uv":       {"publish": high},
	"flit":     {"publish": high},
	"hatch":    {"publish": high},
}

// deployAliases share another program's table.
var deployAliases = map[string]string{"oc": "kubectl", "tofu": "terraform", "terragrunt": "terraform", "flyctl": "fly", "sls": "serverless", "podman": "docker"}

// destroys are deploy subcommands that also delete what they deployed.
var destroys = map[string]bool{
	"terraform destroy": true, "pulumi destroy": true, "cdk destroy": true, "serverless remove": true, "sam delete": true,
	"fly destroy": true, "heroku apps:destroy": true, "heroku pg:reset": true, "eb terminate": true, "railway down": true,
	"npm unpublish": true, "kamal remove": true,
}

// deployValueFlags are global options that take a value, per program.
var deployValueFlags = map[string]map[string]bool{
	"kubectl": flagSet("-n", "--namespace", "--context", "--kubeconfig", "--cluster", "--user", "-s", "--server", "--as", "-v", "--token", "-f", "--filename", "-l", "--selector"),
	"helm":    flagSet("-n", "--namespace", "--kube-context", "--kubeconfig"),
	"heroku":  flagSet("-a", "--app", "-r", "--remote"),
	"npm":     flagSet("--prefix", "-w", "--workspace", "--registry", "--userconfig"),
	"docker":  flagSet("-c", "--context", "-H", "--host", "--config", "-l", "--log-level"),
}

func deployNoun(prog string) string {
	switch prog {
	case "kubectl", "helm":
		return "Changes a Kubernetes cluster"
	case "terraform", "pulumi", "cdk", "sam", "serverless":
		return "Changes cloud infrastructure"
	case "docker":
		return "Pushes a container image"
	case "npm", "pnpm", "yarn", "bun", "cargo", "twine", "gem", "poetry", "uv", "flit", "hatch":
		return "Publishes a package"
	}
	return "Deploys"
}

func dryRun(args []string) bool {
	for _, a := range args {
		if a == "--dry-run" || (strings.HasPrefix(a, "--dry-run=") && a != "--dry-run=none") {
			return true
		}
	}
	return false
}

func (x *run) deploy(prog string, args []string) {
	f := x.f
	shown := prog
	if a, ok := deployAliases[prog]; ok {
		prog = a
	}
	if subs, ok := deploySubs[prog]; ok && !dryRun(args) {
		sub, rest := subcommand(args, deployValueFlags[prog])
		if prog == "terraform" && sub == "run-all" { // terragrunt run-all apply
			sub, rest = subcommand(rest, nil)
		}
		if level, ok := subs[sub]; ok {
			f.add(api.RiskDeploy, level, deployNoun(prog)+" ("+shown+" "+sub+")")
			if destroys[prog+" "+sub] || (sub == "apply" && (longFlag(rest, "-destroy") || longFlag(rest, "--destroy"))) {
				f.add(api.RiskDestructive, high, "Destroys what was deployed ("+shown+" "+sub+")")
			}
			if prog == "fly" && containsWord(rest, "destroy") {
				f.add(api.RiskDestructive, high, "Destroys a Fly app or machine")
			}
		}
	}
	switch prog {
	case "ansible-playbook":
		f.add(api.RiskDeploy, medium, "Runs an Ansible playbook")
	case "vercel":
		sub, _ := subcommand(args, flagSet("--scope", "--token", "-t", "--cwd", "-A", "--local-config"))
		switch {
		case longFlag(args, "--prod", "--production") || sub == "promote":
			f.add(api.RiskDeploy, high, "Deploys to production (vercel --prod)")
		case sub == "" || sub == "deploy":
			f.add(api.RiskDeploy, medium, "Deploys a preview (vercel)")
		case sub == "remove" || sub == "rm":
			f.add(api.RiskDestructive, medium, "Removes Vercel deployments")
		}
	case "netlify":
		if sub, _ := subcommand(args, nil); sub == "deploy" {
			if longFlag(args, "--prod", "--production") {
				f.add(api.RiskDeploy, high, "Deploys to production (netlify deploy --prod)")
			} else {
				f.add(api.RiskDeploy, medium, "Deploys a preview (netlify deploy)")
			}
		}
	case "gcloud", "az":
		pos := positionals(args, flagSet("--project", "--account", "--configuration", "--region", "--zone", "-g", "--resource-group", "--subscription"))
		switch {
		case containsWord(pos, "deploy") || (prog == "az" && len(pos) > 1 && pos[0] == "deployment" && pos[1] != "show" && pos[1] != "list"):
			f.add(api.RiskDeploy, high, "Deploys to the cloud ("+prog+" "+clip(strings.Join(pos, " "), 50)+")")
		case containsWord(pos, "delete"):
			f.add(api.RiskDestructive, medium, "Deletes cloud resources ("+prog+" "+clip(strings.Join(pos, " "), 50)+")")
		}
	case "aws":
		x.aws(args)
	case "gsutil":
		pos := positionals(args, nil)
		if len(pos) > 0 && (pos[0] == "rm" || pos[0] == "rb") && (shortFlag(args, "r") || pos[0] == "rb") {
			f.add(api.RiskDestructive, high, "Deletes Cloud Storage objects recursively (gsutil "+pos[0]+")")
		}
		if len(pos) > 2 && (pos[0] == "cp" || pos[0] == "rsync" || pos[0] == "mv") && strings.HasPrefix(pos[len(pos)-1], "gs://") {
			f.add(api.RiskNetwork, medium, "Uploads to Cloud Storage")
		}
	case "make":
		for _, t := range positionals(args, flagSet("-C", "--directory", "-f", "--file", "-j", "--jobs", "-I", "-o", "-W")) {
			if deployScript(t) {
				f.add(api.RiskDeploy, medium, "Runs make "+t)
				break
			}
		}
	case "npm", "pnpm", "yarn", "bun":
		sub, rest := subcommand(args, deployValueFlags["npm"])
		script := ""
		switch {
		case sub == "run" || sub == "run-script":
			script, _ = subcommand(rest, nil)
		case prog != "npm" && deployScript(sub):
			script = sub // yarn deploy
		}
		if deployScript(script) {
			f.add(api.RiskDeploy, medium, "Runs the "+clip(script, 30)+" script")
		}
	case "gh":
		pos := positionals(args, flagSet("-R", "--repo", "-t", "--title", "-b", "--body", "-F", "--body-file", "-n", "--notes", "-m"))
		if len(pos) >= 2 {
			switch pos[0] + " " + pos[1] {
			case "release create":
				f.add(api.RiskDeploy, medium, "Publishes a GitHub release")
			case "workflow run":
				f.add(api.RiskDeploy, medium, "Starts a GitHub Actions workflow")
			case "pr merge":
				f.add(api.RiskProtectedBranch, medium, "Merges a pull request")
			case "repo delete":
				f.add(api.RiskDestructive, high, "Deletes a GitHub repository")
			case "secret set", "secret delete":
				f.add(api.RiskSecrets, medium, "Changes repository secrets")
			case "auth token":
				f.add(api.RiskSecrets, high, "Prints credentials (gh auth token)")
			}
		}
	case "docker":
		x.docker(args)
	}
}

// deployScript: package scripts and make targets named like deploys.
func deployScript(s string) bool {
	for _, p := range []string{"deploy", "release", "publish", "ship"} {
		if s == p || strings.HasPrefix(s, p+":") || strings.HasPrefix(s, p+"-") {
			return true
		}
	}
	return s == "prod" || s == "production"
}

func containsWord(words []string, w string) bool {
	for _, x := range words {
		if x == w {
			return true
		}
	}
	return false
}

func (x *run) aws(args []string) {
	f := x.f
	pos := positionals(args, flagSet("--profile", "--region", "--output", "--endpoint-url", "--query", "--cli-input-json",
		"--stack-name", "--template-file", "--function-name", "--zip-file", "--cluster", "--service", "--bucket", "--key",
		"--secret-id", "--name", "--names", "--instance-ids", "--db-instance-identifier", "--table-name", "--exclude", "--include"))
	if len(pos) < 2 {
		return
	}
	svc, op := pos[0], pos[1]
	what := "aws " + svc + " " + op
	switch {
	case svc == "s3" && (op == "rb" || (op == "rm" && longFlag(args, "--recursive"))):
		f.add(api.RiskDestructive, high, "Deletes S3 data ("+what+")")
	case svc == "s3" && (op == "cp" || op == "sync" || op == "mv") && strings.HasPrefix(pos[len(pos)-1], "s3://"):
		f.add(api.RiskNetwork, medium, "Uploads to S3 ("+what+")")
	case op == "deploy" || svc == "deploy",
		svc == "cloudformation" && (op == "create-stack" || op == "update-stack" || op == "delete-stack" || op == "execute-change-set"),
		svc == "lambda" && (strings.HasPrefix(op, "update-function") || op == "create-function" || op == "publish-version" || op == "delete-function"),
		svc == "ecs" && (op == "update-service" || op == "create-service" || op == "delete-service" || op == "run-task"):
		f.add(api.RiskDeploy, high, "Deploys to AWS ("+what+")")
		if strings.HasPrefix(op, "delete-") {
			f.add(api.RiskDestructive, high, "Deletes AWS resources ("+what+")")
		}
	case strings.HasPrefix(op, "delete-") || strings.HasPrefix(op, "terminate-") || op == "rb":
		f.add(api.RiskDestructive, medium, "Deletes AWS resources ("+what+")")
	case svc == "secretsmanager" && op == "get-secret-value",
		svc == "ssm" && strings.HasPrefix(op, "get-parameter") && longFlag(args, "--with-decryption"),
		svc == "configure" && (op == "get" || op == "export-credentials"),
		svc == "sts" && (op == "get-session-token" || op == "assume-role"):
		f.add(api.RiskSecrets, high, "Reads credentials ("+what+")")
	}
}

func (x *run) docker(args []string) {
	f := x.f
	sub, rest := subcommand(args, deployValueFlags["docker"])
	switch sub {
	case "system", "volume", "image", "container", "builder", "network", "buildx":
		if op, _ := subcommand(rest, nil); op == "prune" || (sub == "volume" && (op == "rm" || op == "remove")) {
			f.add(api.RiskDestructive, medium, "Deletes Docker data (docker "+sub+" "+op+")")
		}
	case "compose":
		if op, _ := subcommand(rest, flagSet("-f", "--file", "-p", "--project-name", "--env-file", "--profile")); op == "down" && (shortFlag(rest, "v") || longFlag(rest, "--volumes")) {
			f.add(api.RiskDestructive, medium, "Deletes the project's Docker volumes (docker compose down -v)")
		}
	case "run", "create":
		if longFlag(rest, "--privileged") {
			f.add(api.RiskSudo, high, "Runs a privileged container")
		}
		for i, a := range rest {
			v := ""
			switch {
			case (a == "-v" || a == "--volume") && i+1 < len(rest):
				v = rest[i+1]
			case strings.HasPrefix(a, "--volume="):
				v = strings.TrimPrefix(a, "--volume=")
			}
			if strings.HasPrefix(v, "/:") {
				f.add(api.RiskSudo, medium, "Mounts the whole filesystem into a container")
			}
		}
	}
}

// ---- installs ------------------------------------------------------------

var (
	npmInstall = map[string]bool{"install": true, "i": true, "in": true, "ins": true, "inst": true, "insta": true, "instal": true, "isnt": true, "isnta": true, "isntal": true, "add": true, "ci": true, "clean-install": true}
	pipValue   = flagSet("-r", "--requirement", "-c", "--constraint", "-e", "--editable", "-i", "--index-url", "--extra-index-url", "-t", "--target", "--prefix", "--root", "-f", "--find-links", "--python")
	npmValue   = flagSet("--prefix", "-w", "--workspace", "--registry", "--userconfig", "--cache", "--loglevel", "--tag", "-C", "--dir", "--filter", "-F", "--cwd")
	sysInstall = map[string]map[string]bool{
		"apt": {"install": true, "reinstall": true}, "apt-get": {"install": true, "reinstall": true}, "aptitude": {"install": true},
		"yum": {"install": true, "reinstall": true}, "dnf": {"install": true, "reinstall": true}, "zypper": {"install": true, "in": true},
		"apk": {"add": true}, "port": {"install": true}, "snap": {"install": true}, "flatpak": {"install": true},
		"choco": {"install": true}, "winget": {"install": true}, "scoop": {"install": true}, "emerge": {"": true},
		"pkg": {"install": true}, "pkgin": {"install": true}, "xbps-install": {"": true},
		"brew":    {"install": true, "reinstall": true, "upgrade": true, "tap": true},
		"nix-env": {"": true}, "nix": {"profile": true},
	}
)

// pkgs labels an install: global, new packages, or the project's own
// dependencies.
func (x *run) pkgs(mgr string, global bool, names []string) {
	f := x.f
	switch {
	case global:
		if len(names) > 0 {
			f.add(api.RiskInstall, medium, "Installs packages globally ("+mgr+": "+listNames(names)+")")
		} else {
			f.add(api.RiskInstall, medium, "Installs packages globally ("+mgr+")")
		}
	case len(names) > 0:
		f.add(api.RiskInstall, medium, "Adds packages ("+mgr+": "+listNames(names)+")")
	default:
		f.add(api.RiskInstall, low, "Installs the project's dependencies ("+mgr+")")
	}
}

// localPkg: a path or archive, not a registry package.
func localPkg(s string) bool {
	return strings.HasPrefix(s, ".") || strings.HasPrefix(s, "/") || strings.HasPrefix(s, "~") || strings.HasPrefix(s, "file:")
}

func registryNames(pos []string) []string {
	var out []string
	for _, p := range pos {
		if !localPkg(p) {
			out = append(out, clip(p, 40))
		}
	}
	return out
}

func (x *run) install(prog string, args []string) {
	switch prog {
	case "npm", "pnpm", "bun":
		sub, rest := subcommand(args, npmValue)
		if npmInstall[sub] {
			global := shortFlag(args, "g") || longFlag(args, "--global", "--location=global")
			names := []string(nil)
			if sub != "ci" && sub != "clean-install" {
				names = registryNames(positionals(rest, npmValue))
			}
			x.pkgs(prog, global, names)
		}
	case "yarn":
		sub, rest := subcommand(args, npmValue)
		switch sub {
		case "", "install":
			if sub == "install" || len(positionals(args, npmValue)) == 0 {
				x.pkgs(prog, false, nil)
			}
		case "add":
			x.pkgs(prog, false, registryNames(positionals(rest, npmValue)))
		case "global":
			if op, more := subcommand(rest, nil); op == "add" {
				x.pkgs(prog, true, registryNames(positionals(more, nil)))
			}
		}
	case "pip", "pip3", "pip2":
		x.pip(prog, args)
	case "uv":
		sub, rest := subcommand(args, flagSet("--directory", "--project", "-p", "--python"))
		switch sub {
		case "pip":
			x.pip("uv pip", rest)
		case "add":
			x.pkgs("uv", false, registryNames(positionals(rest, flagSet("--group", "--optional", "-r", "--requirements"))))
		case "sync":
			x.pkgs("uv", false, nil)
		case "tool":
			if op, more := subcommand(rest, nil); op == "install" {
				x.pkgs("uv tool", true, registryNames(positionals(more, nil)))
			}
		}
	case "poetry", "bundle", "bundler", "composer":
		sub, rest := subcommand(args, flagSet("-C", "--directory", "--gemfile"))
		switch sub {
		case "add", "require":
			x.pkgs(prog, false, registryNames(positionals(rest, flagSet("-G", "--group"))))
		case "install":
			x.pkgs(prog, false, nil)
		}
	case "pipx", "gem", "cargo", "go", "deno":
		sub, rest := subcommand(args, flagSet("-C", "--config", "--color"))
		names := registryNames(positionals(rest, flagSet("--version", "--git", "--branch", "--tag", "--rev", "--root", "--path", "-n", "--name")))
		switch {
		case sub == "install":
			x.pkgs(prog, true, names)
		case sub == "add" || (prog == "go" && sub == "get"):
			x.pkgs(prog, false, names)
		}
	case "conda", "mamba", "micromamba":
		if sub, rest := subcommand(args, nil); sub == "install" || sub == "create" {
			x.pkgs(prog, false, registryNames(positionals(rest, flagSet("-n", "--name", "-p", "--prefix", "-c", "--channel", "--file"))))
		}
	case "gh":
		if pos := positionals(args, nil); len(pos) >= 3 && pos[0] == "extension" && pos[1] == "install" {
			x.pkgs("gh extension", true, pos[2:3])
		}
	case "pacman":
		for _, a := range args {
			if len(a) > 1 && a[0] == '-' && a[1] == 'S' && !strings.ContainsAny(a, "sicgl") {
				x.pkgs(prog, true, registryNames(positionals(args, nil)))
				return
			}
		}
	default:
		subs, ok := sysInstall[prog]
		if !ok {
			return
		}
		sub, rest := subcommand(args, nil)
		switch {
		case subs[""] && (prog != "nix-env" || shortFlag(args, "i") || longFlag(args, "--install")):
			x.pkgs(prog, true, registryNames(positionals(args, nil)))
		case prog == "nix" && sub == "profile":
			if op, more := subcommand(rest, nil); op == "install" {
				x.pkgs("nix profile", true, registryNames(positionals(more, nil)))
			}
		case subs[sub] && prog != "nix":
			x.pkgs(prog, true, registryNames(positionals(rest, nil)))
		}
	}
}

func (x *run) pip(mgr string, args []string) {
	sub, rest := subcommand(args, flagSet("--python"))
	if sub != "install" {
		return
	}
	global := longFlag(rest, "--user", "--break-system-packages", "--system")
	x.pkgs(mgr, global, registryNames(positionals(rest, pipValue)))
}

// ---- network -------------------------------------------------------------

func (x *run) network(prog string, args []string) {
	f := x.f
	switch prog {
	case "curl", "wget", "http", "https", "xh", "xhs", "curlie", "aria2c":
		x.fetch(prog, args)
	case "scp", "rsync", "rclone":
		pos := positionals(args, flagSet("-P", "-i", "-o", "-F", "-J", "-S", "-e", "--rsh", "--exclude", "--include", "--filter", "-f", "--config", "--port"))
		if prog == "rclone" {
			if len(pos) < 3 {
				return
			}
			pos = pos[1:]
		}
		if len(pos) < 2 {
			return
		}
		dst, srcs := pos[len(pos)-1], pos[:len(pos)-1]
		if host, ok := remotePath(dst); ok {
			f.add(api.RiskNetwork, medium, "Copies files to "+host)
			for _, s := range srcs {
				if _, remote := remotePath(s); !remote && !x.remote {
					if p, ok := f.resolve(s, *x.cwd); ok {
						f.read(p, false)
					}
				}
			}
			return
		}
		for _, s := range srcs {
			if host, ok := remotePath(s); ok {
				f.add(api.RiskNetwork, low, "Copies files from "+host)
				if p, ok := f.resolve(dst, *x.cwd); ok && !x.remote {
					f.write(p)
				}
				return
			}
		}
	case "sftp", "ftp", "lftp":
		f.add(api.RiskNetwork, medium, "Transfers files over "+prog)
	case "nc", "ncat", "netcat", "socat", "telnet":
		sends := x.cmd.PipedIn
		for _, r := range x.cmd.Redirects {
			if r.Op == "<" || r.Op == "<<<" || r.Op == "<<" || r.Op == "<<-" {
				sends = true
			}
		}
		if sends {
			f.add(api.RiskNetwork, medium, "Sends data over a raw network connection ("+prog+")")
		} else {
			f.add(api.RiskNetwork, low, "Opens a raw network connection ("+prog+")")
		}
	}
}

var sendMethods = map[string]bool{"POST": true, "PUT": true, "PATCH": true, "DELETE": true}

func (x *run) fetch(prog string, args []string) {
	f := x.f
	host := urlHost(args)
	var uploads []string
	sends := false
	var output []string
	value := func(i int, a, short, long string) (string, int, bool) {
		switch {
		case a == short || a == long:
			if i+1 < len(args) {
				return args[i+1], i + 1, true
			}
		case long != "" && strings.HasPrefix(a, long+"="):
			return a[len(long)+1:], i, true
		case short != "" && len(short) == 2 && strings.HasPrefix(a, short) && len(a) > 2 && !strings.HasPrefix(a, "--"):
			return a[2:], i, true
		}
		return "", i, false
	}
	switch prog {
	case "curl":
		for i := 0; i < len(args); i++ {
			a := args[i]
			if v, j, ok := value(i, a, "-T", "--upload-file"); ok {
				uploads, i = append(uploads, v), j
				continue
			}
			if v, j, ok := value(i, a, "-F", "--form"); ok {
				if k := strings.IndexAny(v, "@<"); k >= 0 {
					uploads = append(uploads, strings.SplitN(v[k+1:], ";", 2)[0])
				}
				sends, i = true, j
				continue
			}
			matched := false
			for _, fl := range [][2]string{{"-d", "--data"}, {"", "--data-binary"}, {"", "--data-raw"}, {"", "--data-ascii"}, {"", "--data-urlencode"}, {"", "--json"}} {
				if v, j, ok := value(i, a, fl[0], fl[1]); ok {
					if strings.HasPrefix(v, "@") && v != "@-" && fl[1] != "--data-raw" {
						uploads = append(uploads, v[1:])
					}
					sends, i, matched = true, j, true
					break
				}
			}
			if matched {
				continue
			}
			if v, j, ok := value(i, a, "-X", "--request"); ok {
				sends, i = sends || sendMethods[strings.ToUpper(v)], j
				continue
			}
			if v, j, ok := value(i, a, "-o", "--output"); ok {
				output, i = append(output, v), j
				continue
			}
		}
	case "wget":
		for i := 0; i < len(args); i++ {
			a := args[i]
			if v, j, ok := value(i, a, "-O", "--output-document"); ok {
				output, i = append(output, v), j
				continue
			}
			if v, j, ok := value(i, a, "", "--post-file"); ok {
				uploads, sends, i = append(uploads, v), true, j
				continue
			}
			if v, j, ok := value(i, a, "", "--body-file"); ok {
				uploads, sends, i = append(uploads, v), true, j
				continue
			}
			if strings.HasPrefix(a, "--post-data") || strings.HasPrefix(a, "--body-data") {
				sends = true
			}
			if v, j, ok := value(i, a, "", "--method"); ok {
				sends, i = sends || sendMethods[strings.ToUpper(v)], j
			}
		}
	default: // httpie and friends: `http POST url field=value @file`
		for _, a := range args {
			if sendMethods[a] {
				sends = true
			}
			if strings.HasPrefix(a, "@") || strings.Contains(a, "=@") || strings.Contains(a, ":=@") {
				sends = true
				uploads = append(uploads, a[strings.IndexByte(a, '@')+1:])
			}
		}
	}
	for _, o := range output {
		if o == "-" || x.remote {
			continue
		}
		if p, ok := f.resolve(o, *x.cwd); ok {
			f.write(p)
			if f.downloaded == nil {
				f.downloaded = map[string]string{}
			}
			f.downloaded[p] = host
		}
	}
	switch {
	case len(uploads) > 0:
		names := make([]string, 0, len(uploads))
		for _, u := range uploads {
			if p, ok := f.resolve(u, *x.cwd); ok && !x.remote {
				f.read(p, false)
				names = append(names, f.show(p))
			}
		}
		text := "Uploads files to " + hostOr(host)
		if len(names) > 0 {
			text = "Uploads " + listNames(names) + " to " + hostOr(host)
		}
		f.add(api.RiskNetwork, medium, text)
	case sends:
		f.add(api.RiskNetwork, medium, "Sends data to "+hostOr(host))
	default:
		f.add(api.RiskNetwork, low, "Downloads from "+hostOr(host))
	}
}

// ---- databases -----------------------------------------------------------

var dbClients = map[string]bool{
	"psql": true, "mysql": true, "mariadb": true, "sqlite3": true, "sqlite": true, "duckdb": true, "sqlcmd": true,
	"cockroach": true, "clickhouse-client": true, "clickhouse": true, "mongo": true, "mongosh": true, "redis-cli": true,
	"cqlsh": true, "snowsql": true, "bq": true, "usql": true, "pgcli": true, "mycli": true, "litecli": true, "turso": true,
}

var (
	sqlDropRE   = regexp.MustCompile(`(?i)\b(drop\s+(?:table|database|schema|index|view|collection|keyspace|materialized\s+view)|truncate\s+(?:table\s+)?[a-z_"\x60\[]|flushall|flushdb|dropdatabase\s*\(|\.drop\s*\(\s*\))`)
	sqlDeleteRE = regexp.MustCompile(`(?i)\bdelete\s+from\s+\S`)
	sqlUpdateRE = regexp.MustCompile(`(?i)\bupdate\s+\S+\s+set\b`)
	sqlWhereRE  = regexp.MustCompile(`(?i)\bwhere\b`)
	// dbWipeRE: framework commands that drop or reset a database.
	dbWipeRE = regexp.MustCompile(`^(?:db:(?:drop|reset|purge|wipe|schema:load|migrate:reset|truncate_all)(?::\S*)?|migrate:(?:fresh|reset)|doctrine:(?:database|schema):drop|reset_db|flush)$`)
)

// sqlRisk names the destructive statement in text, if any.
func sqlRisk(text string) (string, api.RiskLevel, bool) {
	if m := sqlDropRE.FindString(text); m != "" {
		return strings.ToUpper(strings.Join(strings.Fields(m), " ")), high, true
	}
	for _, stmt := range strings.Split(text, ";") {
		if sqlDeleteRE.MatchString(stmt) && !sqlWhereRE.MatchString(stmt) {
			return "DELETE without WHERE", high, true
		}
		if sqlUpdateRE.MatchString(stmt) && !sqlWhereRE.MatchString(stmt) {
			return "UPDATE without WHERE", medium, true
		}
	}
	return "", "", false
}

func (x *run) database(prog string, args []string) {
	f := x.f
	if dbClients[prog] {
		var b strings.Builder
		b.WriteString(strings.Join(args, " "))
		for _, r := range x.cmd.Redirects {
			b.WriteString("\n" + r.Body)
			if r.Op == "<<<" {
				b.WriteString("\n" + r.Target)
			}
		}
		// `echo "DROP TABLE x" | psql`: the statements come down the pipe.
		for j := x.i; j > 0 && x.line.Commands[j].PipedIn; j-- {
			prev := x.line.Commands[j-1]
			b.WriteString("\n" + strings.Join(prev.Words, " "))
			for _, r := range prev.Redirects {
				b.WriteString("\n" + r.Body)
			}
		}
		if what, level, ok := sqlRisk(b.String()); ok {
			f.add(api.RiskDestructive, level, "Runs destructive SQL ("+what+")")
		}
		if prog == "bq" {
			if sub, _ := subcommand(args, nil); sub == "rm" {
				f.add(api.RiskDestructive, high, "Deletes a BigQuery table or dataset (bq rm)")
			}
		}
		return
	}
	switch prog {
	case "dropdb", "dropuser":
		f.add(api.RiskDestructive, high, "Drops a database ("+prog+")")
		return
	case "npx", "bunx", "pnpx":
		if len(args) > 0 {
			x.database(policy.CommandName(args[0]), args[1:])
		}
		return
	}
	pos := positionals(args, nil)
	switch prog {
	case "rails", "rake", "bundle", "php", "artisan", "console", "symfony", "sequelize", "python", "python3", "manage.py", "django-admin":
		for _, p := range pos {
			if dbWipeRE.MatchString(p) && (p != "flush" || prog == "manage.py" || prog == "django-admin" || containsWord(pos, "manage.py")) {
				f.add(api.RiskDestructive, high, "Wipes a database ("+p+")")
				return
			}
		}
	case "prisma":
		if len(pos) >= 2 && pos[0] == "migrate" && pos[1] == "reset" {
			f.add(api.RiskDestructive, high, "Wipes a database (prisma migrate reset)")
		}
		if longFlag(args, "--force-reset", "--accept-data-loss") {
			f.add(api.RiskDestructive, high, "May delete database data (prisma db push --force-reset)")
		}
		if len(pos) >= 2 && pos[0] == "migrate" && pos[1] == "deploy" {
			f.add(api.RiskDeploy, medium, "Applies migrations to a database (prisma migrate deploy)")
		}
	}
}

// ---- disks, processes, permissions --------------------------------------

func (x *run) system(prog string, args []string) {
	f := x.f
	switch {
	case strings.HasPrefix(prog, "mkfs") || prog == "newfs" || prog == "mke2fs" || prog == "mkswap":
		f.add(api.RiskDestructive, high, "Formats a disk ("+prog+")")
	case prog == "fdisk" || prog == "sfdisk" || prog == "cfdisk" || prog == "parted" || prog == "gdisk" || prog == "sgdisk" || prog == "wipefs" || prog == "blkdiscard":
		f.add(api.RiskDestructive, high, "Changes disk partitions ("+prog+")")
	case prog == "diskutil":
		if sub, rest := subcommand(args, nil); strings.HasPrefix(sub, "erase") || sub == "partitionDisk" || sub == "zeroDisk" ||
			sub == "randomDisk" || sub == "secureErase" || sub == "reformat" || (sub == "apfs" && len(rest) > 0 && strings.HasPrefix(rest[0], "delete")) {
			f.add(api.RiskDestructive, high, "Erases a disk (diskutil "+sub+")")
		}
	case prog == "dd":
		for _, a := range args {
			if v, ok := strings.CutPrefix(a, "of="); ok && diskDevice(v) {
				f.add(api.RiskDestructive, high, "Writes directly to disk device "+v)
			}
		}
	case prog == "shred":
		f.add(api.RiskDestructive, medium, "Overwrites files beyond recovery (shred)")
	case prog == "crontab":
		if shortFlag(args, "r") {
			f.add(api.RiskDestructive, medium, "Removes every cron job (crontab -r)")
		}
	case prog == "kill" || prog == "pkill":
		if containsWord(args, "-1") && prog == "kill" && len(positionals(args, nil)) == 0 {
			f.add(api.RiskDestructive, high, "Kills every process you own (kill -1)")
		}
	case prog == "mv":
		if pos := positionals(args, nil); len(pos) >= 2 && pos[len(pos)-1] == "/dev/null" {
			f.add(api.RiskDestructive, medium, "Moves files to /dev/null")
		}
	}
}

var worldWritableRE = regexp.MustCompile(`^(?:0?[0-7]?[0-7][0-7][2367]|(?:.*,)?(?:[ugo]*o[ugo]*|a)[+=][rwxXst]*w.*)$`)

func (x *run) perms(prog string, args []string) {
	f := x.f
	if prog != "chmod" && prog != "chown" && prog != "chgrp" {
		return
	}
	recursive := shortFlag(args, "R") || longFlag(args, "--recursive")
	pos := positionals(args, flagSet("--reference"))
	if len(pos) < 2 {
		return
	}
	world := prog == "chmod" && worldWritableRE.MatchString(pos[0])
	for _, t := range pos[1:] {
		shown := t
		broad := false
		if p, ok := f.resolve(t, *x.cwd); ok && !x.remote {
			shown = f.show(p)
			broad = f.broadDir(p) || (f.repo != "" && under(f.repo, p))
		}
		switch {
		case recursive && world:
			f.add(api.RiskDestructive, high, "Makes "+shown+" writable by everyone, recursively ("+prog+" -R "+pos[0]+")")
		case recursive && broad:
			f.add(api.RiskDestructive, high, "Recursively changes "+map[bool]string{true: "permissions", false: "ownership"}[prog == "chmod"]+" of "+shown)
		case world:
			f.add(api.RiskDestructive, medium, "Makes "+shown+" writable by everyone ("+prog+" "+pos[0]+")")
		}
	}
}

// ---- credential dumps ------------------------------------------------------

func (x *run) credentials(prog string, args []string) {
	f := x.f
	pos := positionals(args, nil)
	first := func(words ...string) bool {
		if len(pos) < len(words) {
			return false
		}
		for i, w := range words {
			if pos[i] != w {
				return false
			}
		}
		return true
	}
	switch {
	case prog == "printenv":
		f.add(api.RiskSecrets, medium, "Prints environment variables, which may hold credentials")
	case prog == "security" && len(pos) > 0 && (strings.HasPrefix(pos[0], "find-") && strings.HasSuffix(pos[0], "-password") || pos[0] == "dump-keychain"):
		f.add(api.RiskSecrets, high, "Reads the macOS keychain (security "+pos[0]+")")
	case prog == "gcloud" && (containsWord(pos, "print-access-token") || containsWord(pos, "print-identity-token")):
		f.add(api.RiskSecrets, high, "Prints credentials (gcloud auth)")
	case prog == "op" && (first("read") || first("item", "get") || first("inject")),
		prog == "bw" && first("get"),
		prog == "pass" && (len(pos) == 0 || first("show")),
		prog == "vault" && (first("read") || first("kv", "get")),
		prog == "doppler" && first("secrets"):
		f.add(api.RiskSecrets, high, "Reads secrets from a password manager ("+prog+")")
	case prog == "kubectl" && len(pos) >= 2 && pos[0] == "get" && (pos[1] == "secret" || pos[1] == "secrets" || strings.HasPrefix(pos[1], "secret/")):
		f.add(api.RiskSecrets, high, "Reads Kubernetes secrets")
	case prog == "heroku" && (first("config") || first("config:get")):
		f.add(api.RiskSecrets, medium, "Prints app configuration, which may hold credentials")
	case prog == "gh" && first("auth", "status") && (shortFlag(args, "t") || longFlag(args, "--show-token")):
		f.add(api.RiskSecrets, high, "Prints credentials (gh auth status --show-token)")
	}
}
