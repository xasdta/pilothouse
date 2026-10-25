// Package config loads ~/.pilothouse/config.yaml. Every field has a working
// default so a missing file is valid.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"

	"github.com/xasdta/pilothouse/internal/paths"
)

type Config struct {
	// Web is the HTTP listener for the web app and the device API.
	// It binds to loopback only; remote access goes through Tailscale Serve.
	Web struct {
		Addr      string `yaml:"addr"`       // default 127.0.0.1:7517
		PublicURL string `yaml:"public_url"` // e.g. https://mac.tailnet.ts.net, set by `pilothouse remote tailscale`
	} `yaml:"web"`

	Presence struct {
		// IdleThreshold: OS idle longer than this means "away".
		IdleThreshold Duration `yaml:"idle_threshold"` // default 60s
		// NotifyAfter: when at the desk and a native dialog is waiting this
		// long, send a desktop notification anyway.
		NotifyAfter Duration `yaml:"notify_after"` // default 45s
	} `yaml:"presence"`

	Approvals struct {
		// HoldLimit is how long a request is held for remote devices before
		// it is handed back to the agent's own dialog. Clamped per agent
		// below the vendor's hook timeout.
		HoldLimit Duration `yaml:"hold_limit"` // default 9m
		// AlwaysHold routes every permission request to devices, even at the desk.
		AlwaysHold bool `yaml:"always_hold"`
		// FailClosed makes the hook shim deny pre_tool calls when the daemon is down.
		FailClosed bool `yaml:"fail_closed"`
	} `yaml:"approvals"`

	Notify struct {
		Desktop bool `yaml:"desktop"` // default true
		Ntfy    struct {
			Server string `yaml:"server"` // default https://ntfy.sh
			Topic  string `yaml:"topic"`  // empty = off
			Token  string `yaml:"token"`
		} `yaml:"ntfy"`
		Webhooks []string `yaml:"webhooks"` // Slack/Discord-compatible incoming webhook URLs
		// Telegram is a two-way channel: requests arrive with buttons and
		// a reply to a message prompts the agent. Set up with
		// `pilothouse notify telegram`; the bot token lives in
		// ~/.pilothouse/keys/telegram.json, not here.
		Telegram struct {
			Enabled bool  `yaml:"enabled"`
			ChatID  int64 `yaml:"chat_id"` // bound by the pairing code; 0 = not paired
			// Decisions lets the buttons answer requests (default true).
			// Off: Telegram only notifies.
			Decisions *bool `yaml:"decisions"`
			// APIBase overrides https://api.telegram.org (tests, proxies).
			APIBase string `yaml:"api_base"`
		} `yaml:"telegram"`
	} `yaml:"notify"`

	// Usage configures cost estimates and budget alerts.
	Usage struct {
		// DailyBudgetUSD sends a notification at 80 % and 100 % of this
		// estimated daily spend. 0 = no budget.
		DailyBudgetUSD float64 `yaml:"daily_budget_usd"`
		// Prices overrides or adds per-model list prices in USD per million
		// tokens, keyed by model id prefix (e.g. "claude-sonnet-5").
		Prices map[string]ModelPrice `yaml:"prices"`
	} `yaml:"usage"`

	// KeepAwake stops the computer from sleeping while agents work:
	// "working" (default) while any session is working or blocked,
	// "off" never.
	KeepAwake string `yaml:"keep_awake"`

	Tasks struct {
		// Repos are repository roots tasks may be started in from a device,
		// in addition to repos Pilothouse has seen sessions in.
		Repos []string `yaml:"repos"`
	} `yaml:"tasks"`

	// Retention is how long closed requests and raw hook events are kept.
	Retention Duration `yaml:"retention"` // default 30 days

	// SessionIdleExpiry: a session whose process can't be watched (no PID,
	// or one shared by several sessions such as Codex's app-server) and
	// that sends nothing for this long is marked exited and its open
	// requests expire. Codex sessions use at most CodexSessionIdleExpiry.
	// 0s turns expiry off.
	SessionIdleExpiry Duration `yaml:"session_idle_expiry"` // default 12h

	// Machine is the name shown in the inbox, the web app and notifications.
	// Empty means the hostname. Useful when several machines report to one
	// phone, or to keep the hostname out of screenshots.
	Machine string `yaml:"machine"`

	// UpdateCheck sends {version, os, arch, monthly-rotating id} once a day.
	UpdateCheck *bool `yaml:"update_check"` // default true
}

// ModelPrice is a list price in USD per million tokens.
type ModelPrice struct {
	Input      float64 `yaml:"input"`
	Output     float64 `yaml:"output"`
	CacheRead  float64 `yaml:"cache_read"`
	CacheWrite float64 `yaml:"cache_write"`
}

// Keep-awake modes.
const (
	KeepAwakeWorking = "working"
	KeepAwakeOff     = "off"
)

// TelegramDecisions reports whether Telegram buttons may answer requests.
func (c *Config) TelegramDecisions() bool {
	return c.Notify.Telegram.Decisions == nil || *c.Notify.Telegram.Decisions
}

// Duration is a time.Duration that reads "45s" / "9m" in YAML.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return d.Duration.String(), nil }

// Session idle expiry defaults (see Config.SessionIdleExpiry).
const (
	DefaultSessionIdleExpiry = 12 * time.Hour
	CodexSessionIdleExpiry   = 2 * time.Hour
)

// Default returns the configuration used when no file exists.
func Default() *Config {
	c := &Config{}
	c.Web.Addr = "127.0.0.1:7517"
	c.Presence.IdleThreshold = Duration{60 * time.Second}
	c.Presence.NotifyAfter = Duration{45 * time.Second}
	c.Approvals.HoldLimit = Duration{9 * time.Minute}
	c.Notify.Desktop = true
	c.Notify.Ntfy.Server = "https://ntfy.sh"
	c.Retention = Duration{30 * 24 * time.Hour}
	c.SessionIdleExpiry = Duration{DefaultSessionIdleExpiry}
	c.KeepAwake = KeepAwakeWorking
	return c
}

// Load reads paths.Config(), applying defaults for missing fields.
func Load() (*Config, error) {
	c := Default()
	b, err := os.ReadFile(paths.Config())
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("%s: %w", paths.Config(), err)
	}
	return c, c.Validate()
}

func (c *Config) Validate() error {
	if c.Presence.IdleThreshold.Duration < 5*time.Second {
		return errors.New("presence.idle_threshold must be at least 5s")
	}
	if c.Approvals.HoldLimit.Duration < 10*time.Second {
		return errors.New("approvals.hold_limit must be at least 10s")
	}
	if d := c.SessionIdleExpiry.Duration; d != 0 && d < time.Minute {
		return errors.New("session_idle_expiry must be at least 1m (or 0s to turn it off)")
	}
	switch c.KeepAwake {
	case "", KeepAwakeWorking, KeepAwakeOff:
	default:
		return fmt.Errorf("keep_awake must be %q or %q, got %q", KeepAwakeWorking, KeepAwakeOff, c.KeepAwake)
	}
	if c.Usage.DailyBudgetUSD < 0 {
		return errors.New("usage.daily_budget_usd must not be negative")
	}
	return nil
}

// UpdateCheckEnabled reports whether the anonymous update check is on.
func (c *Config) UpdateCheckEnabled() bool {
	if os.Getenv("PILOTHOUSE_NO_UPDATE_CHECK") != "" {
		return false
	}
	return c.UpdateCheck == nil || *c.UpdateCheck
}

// Save writes the config atomically with 0600 permissions. It replaces the
// whole file with c: when c was loaded a while ago, or another process
// (the daemon saves notify.telegram.chat_id when a chat pairs) may have
// written since, use Update instead so neither change is lost.
func (c *Config) Save() error {
	if err := paths.Ensure(); err != nil {
		return err
	}
	return WithFileLock(paths.Config(), c.save)
}

func (c *Config) save() error {
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return paths.WriteFileAtomic(paths.Config(), b, 0o600)
}

// Update changes config.yaml as one locked read-modify-write: under an
// exclusive lock it reads the file as it is now, applies fn and saves the
// result, so concurrent writers (CLI commands, setup, the daemon) each
// keep the others' changes. When the file doesn't exist yet, fn starts
// from a copy of base (nil: the defaults). A file that doesn't load is an
// error and is left alone. fn returning an error saves nothing. Update
// returns the saved config.
func Update(base *Config, fn func(*Config) error) (*Config, error) {
	if err := paths.Ensure(); err != nil {
		return nil, err
	}
	var out *Config
	err := WithFileLock(paths.Config(), func() error {
		c, err := current(base)
		if err != nil {
			return err
		}
		if err := fn(c); err != nil {
			return err
		}
		if err := c.save(); err != nil {
			return err
		}
		out = c
		return nil
	})
	return out, err
}

// current reads config.yaml, or copies base when there is no file.
func current(base *Config) (*Config, error) {
	if _, err := os.Stat(paths.Config()); err == nil || base == nil {
		return Load()
	}
	b, err := yaml.Marshal(base) // a deep copy: fn must not change base
	if err != nil {
		return nil, err
	}
	c := Default()
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, err
	}
	return c, nil
}

// WithFileLock runs fn holding an exclusive lock on path+".lock" (flock,
// so the kernel drops it if the process dies). Locks are per process and
// file: fn must not take the same lock again.
func WithFileLock(path string, fn func() error) error {
	f, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("lock %s: %w", path, err)
	}
	defer f.Close()
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("lock %s: %w", path, err)
	}
	defer func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }()
	return fn()
}

// MachineName is the name shown in the inbox, the web app and notifications:
// config `machine`, else $PILOTHOUSE_MACHINE, else the hostname without a
// trailing ".local". cfg may be nil.
func MachineName(cfg *Config) string {
	if cfg != nil && strings.TrimSpace(cfg.Machine) != "" {
		return strings.TrimSpace(cfg.Machine)
	}
	if m := strings.TrimSpace(os.Getenv("PILOTHOUSE_MACHINE")); m != "" {
		return m
	}
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "localhost"
	}
	return strings.TrimSuffix(h, ".local")
}
