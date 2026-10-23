// Package service installs the Pilothouse daemon as a per-user login service:
// a launchd user agent on macOS, a systemd user unit on Linux.
//
// The daemon runs as the user (never root) because it needs the user's
// agent settings, sockets and desktop session. Every call to launchctl or
// systemctl goes through an injectable Runner so tests can verify the
// exact commands without touching the real service manager.
package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xasdta/pilothouse/internal/paths"
)

const (
	// Label is the launchd job label.
	Label = "dev.pilothouse.daemon"
	// Unit is the systemd user unit name.
	Unit = "pilothouse.service"
)

// ErrUnsupported is returned on systems without a supported user service
// manager; callers fall back to `pilothouse start`.
var ErrUnsupported = errors.New("no supported service manager on this system (launchd or systemd --user)")

// Runner runs a service-manager command and returns combined output.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner runs a real command.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Manager installs and controls the service. The zero value is not
// usable; build one with New or fill every field (tests).
type Manager struct {
	GOOS string // "darwin" or "linux"
	Exe  string // absolute path of the pilothouse binary to run
	UID  int
	Run  Runner
	// Env is copied into the service environment: PILOTHOUSE_HOME and
	// PILOTHOUSE_RUN_DIR when set (so the service uses the same data dir as
	// the CLI that installed it) and PATH (service managers start jobs
	// with a minimal PATH, but the daemon needs git, tailscale and the
	// notification helpers the user has installed).
	Env map[string]string
	// Sleep waits between launchctl retries; nil means time.Sleep.
	Sleep func(time.Duration)
}

// New returns a Manager for this machine running exe.
func New(exe string) *Manager {
	env := map[string]string{}
	for _, k := range []string{"PILOTHOUSE_HOME", "PILOTHOUSE_RUN_DIR", "PATH"} {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}
	return &Manager{GOOS: runtime.GOOS, Exe: exe, UID: os.Getuid(), Run: ExecRunner, Env: env}
}

// Kind is "launchd", "systemd" or "" when unsupported.
func (m *Manager) Kind() string {
	switch m.GOOS {
	case "darwin":
		return "launchd"
	case "linux":
		return "systemd"
	}
	return ""
}

// Path is the service definition file.
func (m *Manager) Path() string {
	switch m.GOOS {
	case "darwin":
		return filepath.Join(paths.UserHome(), "Library", "LaunchAgents", Label+".plist")
	case "linux":
		return filepath.Join(paths.UserHome(), ".config", "systemd", "user", Unit)
	}
	return ""
}

// LogFile is where the daemon writes its log when run as a service.
func LogFile() string { return filepath.Join(paths.Logs(), "daemon.log") }

func (m *Manager) args() []string {
	return []string{m.Exe, "daemon", "--log-file", LogFile()}
}

// Render returns the service definition file contents.
func (m *Manager) Render() ([]byte, error) {
	if m.Exe == "" || !filepath.IsAbs(m.Exe) {
		return nil, fmt.Errorf("service needs the absolute path of pilothouse, got %q", m.Exe)
	}
	switch m.GOOS {
	case "darwin":
		return m.renderPlist(), nil
	case "linux":
		return m.renderUnit(), nil
	}
	return nil, ErrUnsupported
}

func (m *Manager) envKeys() []string {
	keys := make([]string, 0, len(m.Env))
	for k := range m.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (m *Manager) renderPlist() []byte {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Written by pilothouse service install. Changes are overwritten; edit ~/.pilothouse/config.yaml instead. -->
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + Label + `</string>
	<key>ProgramArguments</key>
	<array>
`)
	for _, a := range m.args() {
		b.WriteString("\t\t<string>" + xmlEscape(a) + "</string>\n")
	}
	b.WriteString("\t</array>\n")
	if len(m.Env) > 0 {
		b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
		for _, k := range m.envKeys() {
			b.WriteString("\t\t<key>" + xmlEscape(k) + "</key>\n\t\t<string>" + xmlEscape(m.Env[k]) + "</string>\n")
		}
		b.WriteString("\t</dict>\n")
	}
	// KeepAlive{SuccessfulExit:false}: restart after a crash, but let
	// `pilothouse stop` (a clean exit) stay stopped. AbandonProcessGroup:
	// `pilothouse run` holders must survive a daemon restart. Interactive:
	// approvals are latency-sensitive, so don't let launchd throttle us.
	b.WriteString(`	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>AbandonProcessGroup</key>
	<true/>
	<key>ProcessType</key>
	<string>Interactive</string>
	<key>StandardErrorPath</key>
	<string>` + xmlEscape(filepath.Join(paths.Logs(), "daemon.stderr.log")) + `</string>
</dict>
</plist>
`)
	return b.Bytes()
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}

func (m *Manager) renderUnit() []byte {
	var b bytes.Buffer
	b.WriteString(`# Written by pilothouse service install. Changes are overwritten.
[Unit]
Description=Pilothouse daemon: one inbox for every coding agent
Documentation=https://github.com/xasdta/pilothouse

[Service]
Type=simple
`)
	q := make([]string, 0, 4)
	for _, a := range m.args() {
		q = append(q, systemdQuote(a))
	}
	b.WriteString("ExecStart=" + strings.Join(q, " ") + "\n")
	for _, k := range m.envKeys() {
		b.WriteString("Environment=" + systemdQuote(k+"="+m.Env[k]) + "\n")
	}
	// KillMode=process: stopping the daemon must not kill `pilothouse run`
	// holders that happen to be in its cgroup.
	b.WriteString(`Restart=on-failure
RestartSec=2
KillMode=process

[Install]
WantedBy=default.target
`)
	return b.Bytes()
}

// systemdQuote quotes one word for ExecStart/Environment. systemd expands
// '%' specifiers and '$' variables, so both are escaped; words with spaces
// or quotes are double-quoted with C-style escapes.
func systemdQuote(s string) string {
	s = strings.ReplaceAll(s, "%", "%%")
	s = strings.ReplaceAll(s, "$", "$$")
	if s != "" && !strings.ContainsAny(s, " \t\"'\\;") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

// Result describes what Install/Uninstall did.
type Result struct {
	Path    string   `json:"path"`
	Changed bool     `json:"changed"` // definition file written or removed
	Actions []string `json:"actions"` // human lines: "wrote …", "loaded …"
	Notes   []string `json:"notes,omitempty"`
}

// Status is the state of the service.
type Status struct {
	Kind      string `json:"kind"`
	Path      string `json:"path"`
	Installed bool   `json:"installed"` // definition file exists
	Current   bool   `json:"current"`   // file matches what Install would write
	Loaded    bool   `json:"loaded"`    // known to the service manager
	Running   bool   `json:"running"`
	PID       int    `json:"pid,omitempty"`
	Detail    string `json:"detail,omitempty"`
	// Linger is systemd only: whether user services keep running after
	// logout. nil = unknown or not applicable.
	Linger *bool `json:"linger,omitempty"`
}

func (m *Manager) gui() string     { return "gui/" + strconv.Itoa(m.UID) }
func (m *Manager) service() string { return m.gui() + "/" + Label }

func (m *Manager) run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := m.Run(ctx, name, args...)
	s := strings.TrimSpace(string(out))
	if err != nil {
		if s != "" {
			return s, fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), s)
		}
		return s, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return s, nil
}

func (m *Manager) systemctl(ctx context.Context, args ...string) (string, error) {
	return m.run(ctx, "systemctl", append([]string{"--user"}, args...)...)
}

// writeFile writes the definition if it differs. It reports whether it
// changed anything.
func (m *Manager) writeFile() (bool, error) {
	want, err := m.Render()
	if err != nil {
		return false, err
	}
	p := m.Path()
	if have, err := os.ReadFile(p); err == nil && bytes.Equal(have, want) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return false, err
	}
	if err := os.MkdirAll(paths.Logs(), 0o700); err != nil {
		return false, err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, want, 0o644); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, p)
}

// Install writes the definition and starts the service. It is idempotent:
// an unchanged, running service is left alone.
func (m *Manager) Install(ctx context.Context) (Result, error) {
	if m.Kind() == "" {
		return Result{}, ErrUnsupported
	}
	res := Result{Path: m.Path()}
	changed, err := m.writeFile()
	if err != nil {
		return res, err
	}
	res.Changed = changed
	if changed {
		res.Actions = append(res.Actions, "wrote "+res.Path)
	}
	switch m.GOOS {
	case "darwin":
		loaded := m.launchdLoaded(ctx)
		if loaded && !changed {
			res.Actions = append(res.Actions, "already loaded "+Label)
			return res, nil
		}
		if loaded {
			// launchd caches the plist at bootstrap time; reload it.
			if err := m.bootout(ctx); err != nil {
				return res, err
			}
			res.Actions = append(res.Actions, "unloaded the old "+Label)
		}
		if err := m.bootstrap(ctx); err != nil {
			return res, err
		}
		res.Actions = append(res.Actions, "loaded "+Label+" (starts at login)")
	case "linux":
		if _, err := m.systemctl(ctx, "daemon-reload"); err != nil {
			return res, err
		}
		if changed {
			// enable --now would leave a unit that is already running on
			// the old definition alone; restart (which also starts) picks
			// up the new one.
			if _, err := m.systemctl(ctx, "enable", Unit); err != nil {
				return res, err
			}
			if _, err := m.systemctl(ctx, "restart", Unit); err != nil {
				return res, err
			}
		} else if _, err := m.systemctl(ctx, "enable", "--now", Unit); err != nil {
			return res, err
		}
		res.Actions = append(res.Actions, "enabled and started "+Unit)
		if st := m.linger(ctx); st != nil && !*st {
			res.Notes = append(res.Notes, "On a headless server, run `loginctl enable-linger $USER` so the daemon keeps running after you log out.")
		}
	}
	return res, nil
}

// Uninstall stops the service and removes its definition.
func (m *Manager) Uninstall(ctx context.Context) (Result, error) {
	if m.Kind() == "" {
		return Result{}, ErrUnsupported
	}
	res := Result{Path: m.Path()}
	_, statErr := os.Stat(res.Path)
	exists := statErr == nil
	switch m.GOOS {
	case "darwin":
		if m.launchdLoaded(ctx) {
			if _, err := m.run(ctx, "launchctl", "bootout", m.service()); err != nil {
				return res, err
			}
			res.Actions = append(res.Actions, "unloaded "+Label)
		}
	case "linux":
		if exists {
			// Ignore errors: the unit may already be stopped/disabled.
			_, _ = m.systemctl(ctx, "disable", "--now", Unit)
			res.Actions = append(res.Actions, "stopped and disabled "+Unit)
		}
	}
	if exists {
		if err := os.Remove(res.Path); err != nil {
			return res, err
		}
		res.Changed = true
		res.Actions = append(res.Actions, "removed "+res.Path)
		if m.GOOS == "linux" {
			_, _ = m.systemctl(ctx, "daemon-reload")
		}
	}
	if len(res.Actions) == 0 {
		res.Actions = append(res.Actions, "not installed")
	}
	return res, nil
}

func (m *Manager) launchdLoaded(ctx context.Context) bool {
	_, err := m.run(ctx, "launchctl", "print", m.service())
	return err == nil
}

// launchdSettle bounds how long bootout/bootstrap wait for launchd.
const launchdSettle = 5 * time.Second

func (m *Manager) sleep(d time.Duration) {
	if m.Sleep != nil {
		m.Sleep(d)
		return
	}
	time.Sleep(d)
}

// bootout unloads the job and waits until launchd has finished: bootout
// returns while the old process is still exiting, and a bootstrap in that
// window fails with "Bootstrap failed: 5: Input/output error".
func (m *Manager) bootout(ctx context.Context) error {
	if _, err := m.run(ctx, "launchctl", "bootout", m.service()); err != nil {
		return err
	}
	const step = 100 * time.Millisecond
	for waited := time.Duration(0); m.launchdLoaded(ctx) && waited < launchdSettle; waited += step {
		m.sleep(step)
	}
	return nil
}

// bootstrap loads the job, retrying while launchd is still tearing down a
// previous instance (the same error 5 as above).
func (m *Manager) bootstrap(ctx context.Context) error {
	const step = 250 * time.Millisecond
	var err error
	for waited := time.Duration(0); ; waited += step {
		if _, err = m.run(ctx, "launchctl", "bootstrap", m.gui(), m.Path()); err == nil {
			return nil
		}
		if waited >= launchdSettle || ctx.Err() != nil {
			return err
		}
		m.sleep(step)
		if m.launchdLoaded(ctx) {
			return nil // a retry raced with launchd finishing the load
		}
	}
}

var pathValue = regexp.MustCompile(`(<key>PATH</key>\s*<string>)[^<]*(</string>)|(?m)^(Environment="?PATH=).*$`)

// withoutPath blanks the PATH value of a service definition. PATH is copied
// from whichever shell ran the install, so a different terminal is not a
// stale service; the binary, arguments and Pilothouse settings are.
func withoutPath(def []byte) []byte {
	return pathValue.ReplaceAll(def, []byte("$1$3$2"))
}

// Status reports the state of the service.
func (m *Manager) Status(ctx context.Context) (Status, error) {
	st := Status{Kind: m.Kind(), Path: m.Path()}
	if st.Kind == "" {
		return st, ErrUnsupported
	}
	if have, err := os.ReadFile(st.Path); err == nil {
		st.Installed = true
		if want, err := m.Render(); err == nil {
			st.Current = bytes.Equal(withoutPath(have), withoutPath(want))
		}
	}
	switch m.GOOS {
	case "darwin":
		out, err := m.run(ctx, "launchctl", "print", m.service())
		if err != nil {
			st.Detail = "not loaded"
			return st, nil
		}
		st.Loaded = true
		st.Running, st.PID, st.Detail = ParseLaunchctlPrint(out)
	case "linux":
		out, err := m.systemctl(ctx, "show", Unit, "--property=LoadState,ActiveState,SubState,MainPID,UnitFileState")
		if err == nil {
			p := ParseSystemctlShow(out)
			st.Loaded = p["LoadState"] == "loaded"
			st.Running = p["ActiveState"] == "active"
			st.PID, _ = strconv.Atoi(p["MainPID"])
			st.Detail = strings.Trim(p["ActiveState"]+" ("+p["SubState"]+"), "+p["UnitFileState"], " (),")
		}
		st.Linger = m.linger(ctx)
	}
	return st, nil
}

func (m *Manager) linger(ctx context.Context) *bool {
	user := os.Getenv("USER")
	if user == "" {
		return nil
	}
	out, err := m.run(ctx, "loginctl", "show-user", user, "--property=Linger")
	if err != nil {
		return nil
	}
	v := ParseSystemctlShow(out)["Linger"] == "yes"
	return &v
}

// Start starts the service (loading it first if needed).
func (m *Manager) Start(ctx context.Context) error {
	switch m.GOOS {
	case "darwin":
		if !m.launchdLoaded(ctx) {
			if _, err := os.Stat(m.Path()); err != nil {
				return errors.New("service not installed — run `pilothouse service install`")
			}
			return m.bootstrap(ctx)
		}
		_, err := m.run(ctx, "launchctl", "kickstart", m.service())
		return err
	case "linux":
		_, err := m.systemctl(ctx, "start", Unit)
		return err
	}
	return ErrUnsupported
}

// Stop stops the service until the next Start or login.
func (m *Manager) Stop(ctx context.Context) error {
	switch m.GOOS {
	case "darwin":
		// bootout, not kill: with KeepAlive a killed job may be restarted.
		if !m.launchdLoaded(ctx) {
			return nil
		}
		return m.bootout(ctx)
	case "linux":
		_, err := m.systemctl(ctx, "stop", Unit)
		return err
	}
	return ErrUnsupported
}

// Restart restarts the service (starting it if stopped).
func (m *Manager) Restart(ctx context.Context) error {
	switch m.GOOS {
	case "darwin":
		if !m.launchdLoaded(ctx) {
			return m.Start(ctx)
		}
		_, err := m.run(ctx, "launchctl", "kickstart", "-k", m.service())
		return err
	case "linux":
		_, err := m.systemctl(ctx, "restart", Unit)
		return err
	}
	return ErrUnsupported
}

// ParseLaunchctlPrint extracts "state = running" and "pid = N" from
// `launchctl print gui/<uid>/<label>` output.
func ParseLaunchctlPrint(out string) (running bool, pid int, detail string) {
	state := ""
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		k, v, ok := strings.Cut(line, " = ")
		if !ok {
			continue
		}
		switch k {
		case "state":
			if state == "" {
				state = v
			}
		case "pid":
			if pid == 0 {
				pid, _ = strconv.Atoi(v)
			}
		case "last exit code":
			if detail == "" {
				detail = "last exit: " + v
			}
		}
	}
	running = state == "running"
	if state != "" {
		if detail != "" {
			detail = state + ", " + detail
		} else {
			detail = state
		}
	}
	return running, pid, detail
}

// ParseSystemctlShow parses `systemctl show` Key=Value lines.
func ParseSystemctlShow(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			m[k] = v
		}
	}
	return m
}
