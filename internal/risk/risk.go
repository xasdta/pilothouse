// Package risk labels tool calls that deserve a careful look before a
// person approves them (docs/PLAN-0.2.md F4): destructive commands,
// force-pushes, pushes and commits to protected branches, deploys,
// secrets, network uploads and piped installers, package installs, sudo,
// and writes outside the session's repo.
//
// Labels are hints for a person reading an approval card, not policy:
// rules decide, labels explain. Classify reads shell commands with the
// policy package's shell reader (quotes, chains, pipes, substitutions,
// here-documents, sudo/env/time wrappers) and never touches the disk or
// the network, so it costs microseconds. Label texts never repeat a
// credential found in the call.
package risk

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/paths"
)

// Call is the tool call to classify.
type Call struct {
	Tool    string
	Kind    api.ToolKind
	Command string   // shell tools
	Paths   []string // files the tool touches (absolute or relative to Cwd)
	Input   json.RawMessage
	Cwd     string
	Repo    string // the session's repo root ("" outside git)
	Branch  string // its current branch
	// Home is the user's home directory; "" means paths.UserHome().
	Home string
}

const (
	maxDepth = 6        // nested shells, substitutions and ssh commands
	maxScan  = 64 << 10 // bytes of a command or tool input scanned for tokens
)

// Classify returns the call's risk labels, most severe first, at most one
// per code.
func Classify(c Call) []api.RiskLabel {
	f := newFinder(c)
	switch c.Kind {
	case api.ToolShell:
		if c.Command != "" {
			f.script(c.Command, f.cwd, 0, false)
			f.tokens(c.Command, "Passes what looks like a credential on the command line")
		}
		// A shell command's paths are the files an apply_patch heredoc in
		// it names: written.
		for _, p := range c.Paths {
			if abs, ok := f.resolve(p, f.cwd); ok {
				f.write(abs)
			}
		}
	case api.ToolEdit:
		for _, p := range c.Paths {
			if abs, ok := f.resolve(p, f.cwd); ok {
				f.write(abs)
			}
		}
		f.tokens(string(c.Input), "Writes what looks like a credential")
	case api.ToolRead:
		for _, p := range c.Paths {
			if abs, ok := f.resolve(p, f.cwd); ok {
				f.read(abs, false)
			}
		}
	default:
		f.tokens(string(c.Input), "Sends what looks like a credential")
	}
	return f.result()
}

// finder collects labels for one call.
type finder struct {
	c      Call
	home   string
	cwd    string // absolute, cleaned ("" when unknown)
	repo   string // absolute, cleaned ("" outside git)
	labels []api.RiskLabel
	// downloaded are files a fetch in this line wrote (curl -o x.sh), so
	// `… && sh x.sh` reads as running a downloaded script.
	downloaded map[string]string // path → host
}

func newFinder(c Call) *finder {
	home := c.Home
	if home == "" {
		home = paths.UserHome()
	}
	f := &finder{c: c, home: filepath.Clean(home)}
	if c.Cwd != "" {
		f.cwd = filepath.Clean(expandHome(c.Cwd, f.home))
	}
	if c.Repo != "" {
		f.repo = filepath.Clean(expandHome(c.Repo, f.home))
	}
	return f
}

var levelRank = map[api.RiskLevel]int{api.RiskHigh: 3, api.RiskMedium: 2, api.RiskLow: 1}

// codeOrder breaks ties between labels of the same level.
var codeOrder = map[string]int{
	api.RiskDestructive: 0, api.RiskForcePush: 1, api.RiskSecrets: 2, api.RiskSudo: 3, api.RiskDeploy: 4,
	api.RiskProtectedBranch: 5, api.RiskNetwork: 6, api.RiskOutsideRepo: 7, api.RiskInstall: 8,
}

// add records a label. A code keeps its most severe finding; at equal
// level the first one found wins.
func (f *finder) add(code string, level api.RiskLevel, text string) {
	for i := range f.labels {
		if f.labels[i].Code == code {
			if levelRank[level] > levelRank[f.labels[i].Level] {
				f.labels[i] = api.RiskLabel{Code: code, Level: level, Text: text}
			}
			return
		}
	}
	f.labels = append(f.labels, api.RiskLabel{Code: code, Level: level, Text: text})
}

func (f *finder) result() []api.RiskLabel {
	sort.SliceStable(f.labels, func(i, j int) bool {
		a, b := f.labels[i], f.labels[j]
		if levelRank[a.Level] != levelRank[b.Level] {
			return levelRank[a.Level] > levelRank[b.Level]
		}
		return codeOrder[a.Code] < codeOrder[b.Code]
	})
	return f.labels
}

// ---- paths ----------------------------------------------------------------

// resolve makes a command argument or tool path absolute: ~, $HOME and
// ${HOME} are expanded, relative paths joined to cwd. ok is false for
// paths computed at run time ($X, $(…)) and relative paths without a cwd.
func (f *finder) resolve(arg, cwd string) (string, bool) {
	if arg == "" || arg == "-" {
		return "", false
	}
	a := expandHome(arg, f.home)
	if strings.ContainsAny(a, "$`") {
		return "", false
	}
	if !filepath.IsAbs(a) {
		if cwd == "" {
			return "", false
		}
		a = filepath.Join(cwd, a)
	}
	return filepath.Clean(a), true
}

func expandHome(p, home string) string {
	for _, pre := range []string{"~", "${HOME}", "$HOME"} {
		if p == pre {
			return home
		}
		if strings.HasPrefix(p, pre+"/") {
			return home + p[len(pre):]
		}
	}
	return p
}

// under reports whether p is root or inside it (both absolute, cleaned).
func under(p, root string) bool {
	if root == "" {
		return false
	}
	return p == root || strings.HasPrefix(p, strings.TrimSuffix(root, "/")+"/")
}

// show names a path for a person: relative inside the repo, ~/… in the
// home directory, absolute otherwise.
func (f *finder) show(p string) string {
	if f.repo != "" && under(p, f.repo) && p != f.repo {
		return strings.TrimPrefix(p, f.repo+"/")
	}
	if under(p, f.home) {
		return "~" + strings.TrimPrefix(p, f.home)
	}
	return p
}

// tempRoots are where writes outside the repo are routine.
var tempRoots = []string{"/tmp", "/private/tmp", "/var/tmp", "/private/var/tmp", "/var/folders", "/private/var/folders", "/dev/shm"}

func (f *finder) isTemp(p string) bool {
	for _, r := range tempRoots {
		if under(p, r) {
			return true
		}
	}
	if t := strings.TrimSuffix(osTempDir(), "/"); t != "" && t != "/" && under(p, filepath.Clean(t)) {
		return true
	}
	return false
}

// pseudoFiles are writes that change nothing on disk.
func pseudoFile(p string) bool {
	switch p {
	case "/dev/null", "/dev/stdout", "/dev/stderr", "/dev/tty", "/dev/zero":
		return true
	}
	return strings.HasPrefix(p, "/dev/fd/")
}

// diskDevice reports whether p is a block device a write would clobber.
func diskDevice(p string) bool {
	if !strings.HasPrefix(p, "/dev/") {
		return false
	}
	name := strings.TrimPrefix(p, "/dev/")
	for _, pre := range []string{"sd", "hd", "vd", "xvd", "nvme", "disk", "rdisk", "mmcblk", "md", "dm-", "mapper/", "loop"} {
		if strings.HasPrefix(name, pre) {
			return true
		}
	}
	return false
}

// write records a write to p: secrets and outside-the-repo labels.
func (f *finder) write(p string) {
	if pseudoFile(p) {
		return
	}
	if diskDevice(p) {
		f.add(api.RiskDestructive, api.RiskHigh, "Writes directly to disk device "+p)
		return
	}
	if what, ok := f.secretPath(p, true); ok {
		f.add(api.RiskSecrets, api.RiskHigh, "Writes "+what)
	}
	// A directory that contains the repo (rm -rf ~, rm -rf /*) is a
	// destructive finding, not a write next to the repo.
	containsRepo := under(f.repo, p) || (hasGlobMeta(filepath.Base(p)) && under(f.repo, filepath.Dir(p)))
	if f.repo != "" && !under(p, f.repo) && !f.isTemp(p) && !containsRepo {
		f.add(api.RiskOutsideRepo, api.RiskMedium, "Writes outside the repo: "+f.show(p))
	}
}

// read records a read of p. listing is set for tools that only look at
// metadata (ls, stat, test).
func (f *finder) read(p string, listing bool) {
	what, ok := f.secretPath(p, false)
	if !ok {
		return
	}
	if listing {
		f.add(api.RiskSecrets, api.RiskMedium, "Looks at "+what)
		return
	}
	f.add(api.RiskSecrets, api.RiskHigh, "Reads "+what)
}

// secretFileNames are base names that hold credentials wherever they are.
var secretFileNames = map[string]bool{
	".env": true, ".envrc": true, ".netrc": true, "_netrc": true, ".pgpass": true, ".git-credentials": true,
	".htpasswd": true, "credentials.json": true, ".npmrc": false, ".pypirc": false,
}

// envTemplates are .env.<suffix> files meant to be committed.
var envTemplates = map[string]bool{"example": true, "sample": true, "template": true, "dist": true, "defaults": true, "schema": true, "tmpl": true}

// secretExts are key and keystore file extensions.
var secretExts = map[string]bool{".pem": true, ".key": true, ".p12": true, ".pfx": true, ".jks": true, ".keystore": true, ".ppk": true}

// homeSecretDirs hold credentials under the home directory; entries in
// homeSecretExcept are fine to read (public keys, host lists).
var homeSecretDirs = []string{
	".ssh", ".aws", ".gnupg", ".config/gcloud", ".kube", ".docker/config.json", ".config/gh/hosts.yml",
	".codex/auth.json", ".claude/.credentials.json", "Library/Keychains", ".password-store", ".config/op",
	".local/share/keyrings", ".npmrc", ".pypirc", ".azure", ".terraform.d/credentials.tfrc.json",
}

var homeSecretExcept = map[string]bool{
	".ssh/known_hosts": true, ".ssh/known_hosts.old": true, ".ssh/config": true, ".ssh/authorized_keys": true, ".aws/config": true,
}

var systemSecrets = []string{"/etc/shadow", "/etc/gshadow", "/etc/master.passwd", "/etc/sudoers", "/etc/ssh/ssh_host_"}

// secretPath reports whether p holds credentials, and how to name it.
// Writes count under the credential directories even for files that are
// fine to read (~/.ssh/authorized_keys).
func (f *finder) secretPath(p string, write bool) (string, bool) {
	base := filepath.Base(p)
	shown := f.show(p)
	if rel, ok := strings.CutPrefix(p, f.home+"/"); ok {
		for _, d := range homeSecretDirs {
			if rel == d || strings.HasPrefix(rel, d+"/") {
				if !write && (homeSecretExcept[rel] || strings.HasSuffix(rel, ".pub")) {
					return "", false
				}
				return shown, true
			}
		}
	}
	for _, s := range systemSecrets {
		if p == s || (strings.HasSuffix(s, "_") && strings.HasPrefix(p, s)) {
			return shown, true
		}
	}
	if strings.HasPrefix(p, "/proc/") && base == "environ" {
		return shown, true // another process's environment
	}
	if secretFileNames[base] {
		return shown, true
	}
	if rest, ok := strings.CutPrefix(base, ".env."); ok && rest != "" {
		last := rest[strings.LastIndexByte(rest, '.')+1:]
		if envTemplates[strings.ToLower(last)] {
			return "", false
		}
		return shown, true
	}
	if strings.HasSuffix(base, ".pub") {
		return "", false
	}
	for _, k := range []string{"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519"} {
		if strings.HasPrefix(base, k) {
			return shown, true
		}
	}
	if secretExts[strings.ToLower(filepath.Ext(base))] {
		return shown, true
	}
	if (strings.HasPrefix(base, "service-account") || strings.HasPrefix(base, "client_secret")) && strings.HasSuffix(base, ".json") {
		return shown, true
	}
	if base == "credentials" && strings.Contains(p, "/.aws/") {
		return shown, true
	}
	return "", false
}

// broadDelete reports whether a recursive delete of p (absolute) would
// take out a lot: the root, a top-level directory, the home directory or
// one of its direct children, a system directory, or the repo itself or
// its .git directory. glob is set when p's last element is a wildcard, in
// which case its directory is what gets emptied.
func (f *finder) broadDelete(p string, glob bool) (string, bool) {
	if glob {
		dir := filepath.Dir(p)
		if dir == "/" || f.repo == dir || f.broadDir(dir) {
			return "everything in " + f.show(dir), true
		}
		return "", false
	}
	switch {
	case p == f.repo:
		return "the whole repo", true
	case p == f.home:
		return "your home directory (~)", true
	case p == "/":
		return "/", true
	case f.repo != "" && under(f.repo, p):
		return f.show(p) + ", which contains the repo", true
	case f.repo != "" && under(p, filepath.Join(f.repo, ".git")):
		return "the repo's .git directory", true
	case f.broadDir(p):
		return f.show(p), true
	}
	return "", false
}

// broadDir: a directory whose recursive deletion is never routine.
func (f *finder) broadDir(p string) bool {
	if p == "/" || strings.Count(p, "/") == 1 {
		return true
	}
	if under(f.home, p) || filepath.Dir(p) == f.home {
		return true // the home directory, an ancestor, or a direct child
	}
	if f.isTemp(p) {
		return false
	}
	for _, r := range []string{"/usr", "/etc", "/bin", "/sbin", "/lib", "/lib64", "/opt", "/var", "/boot", "/System", "/Library", "/Applications", "/private/etc", "/private/var"} {
		if under(p, r) {
			return true
		}
	}
	return false
}

func hasGlobMeta(s string) bool { return strings.ContainsAny(s, "*?[") }
