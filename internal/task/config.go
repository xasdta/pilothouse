package task

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/xasdta/pilothouse/internal/config"
)

// ConfigFile is a repository's task settings, relative to its root. It is
// read from the main checkout (where untracked files such as .env live),
// once per task, before the worktree is set up.
const ConfigFile = ".pilothouse/task.yaml"

// Config is a repository's .pilothouse/task.yaml:
//
//	version: 1
//	copy:                 # from the main checkout into each new worktree
//	  - .env
//	  - .env.*
//	  - config/local/*.json
//	setup:                # run in the worktree before the agent starts
//	  - npm ci
//	setup_timeout: 10m    # for all setup commands together
//	env:                  # for the setup commands and the agent
//	  PORT: "3001"
//
// The file is in the repository, so anything that can write there (an
// agent, a pull request, a freshly cloned repo) can change it. Copying is
// confined to the repository and never overwrites a file the worktree
// already has; setup commands and env only take effect in repos the person
// trusts with `pilothouse policy trust`, the same list that lets a repo's
// policy add allow rules.
type Config struct {
	Version      int               `yaml:"version"`
	Copy         []string          `yaml:"copy"`
	Setup        []string          `yaml:"setup"`
	SetupTimeout config.Duration   `yaml:"setup_timeout"`
	Env          map[string]string `yaml:"env"`

	file string // where it was read from, for messages
}

// Bounds on task.yaml.
const (
	maxConfigBytes      = 64 << 10
	maxCopyPatterns     = 50
	maxPatternLen       = 256
	maxSetupCommands    = 20
	maxSetupCommandLen  = 4096
	maxEnvVars          = 50
	maxEnvValueLen      = 4096
	DefaultSetupTimeout = 10 * time.Minute
	maxSetupTimeout     = 2 * time.Hour
)

var envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// LoadConfig reads repo's task.yaml. A repo without one has a nil Config
// and no error. The file must be a regular file (not a symlink) of at most
// 64 KiB.
func LoadConfig(repo string) (*Config, error) {
	file := filepath.Join(repo, ConfigFile)
	// O_NONBLOCK: a FIFO planted at this path must not hang the caller.
	f, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("%s is a symlink; it must be a regular file", file)
		}
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", file)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxConfigBytes {
		return nil, fmt.Errorf("%s is larger than %d KiB", file, maxConfigBytes>>10)
	}
	return parseConfig(file, b)
}

func parseConfig(file string, b []byte) (*Config, error) {
	c := &Config{file: file}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %v", file, strings.TrimPrefix(err.Error(), "yaml: "))
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	if c.SetupTimeout.Duration == 0 {
		c.SetupTimeout.Duration = DefaultSetupTimeout
	}
	return c, nil
}

func (c *Config) validate() error {
	if c.Version != 0 && c.Version != 1 {
		return fmt.Errorf("unsupported version %d (this pilothouse reads version 1)", c.Version)
	}
	if len(c.Copy) > maxCopyPatterns {
		return fmt.Errorf("copy: at most %d patterns", maxCopyPatterns)
	}
	for i, p := range c.Copy {
		if err := checkCopyPattern(p); err != nil {
			return fmt.Errorf("copy[%d] %q: %w", i, p, err)
		}
	}
	if len(c.Setup) > maxSetupCommands {
		return fmt.Errorf("setup: at most %d commands", maxSetupCommands)
	}
	for i, s := range c.Setup {
		switch {
		case strings.TrimSpace(s) == "":
			return fmt.Errorf("setup[%d] is empty", i)
		case len(s) > maxSetupCommandLen:
			return fmt.Errorf("setup[%d] is longer than %d bytes", i, maxSetupCommandLen)
		case strings.ContainsRune(s, 0) || !utf8.ValidString(s):
			return fmt.Errorf("setup[%d] is not valid text", i)
		}
	}
	if d := c.SetupTimeout.Duration; d != 0 && (d < time.Second || d > maxSetupTimeout) {
		return fmt.Errorf("setup_timeout must be between 1s and %s", maxSetupTimeout)
	}
	if len(c.Env) > maxEnvVars {
		return fmt.Errorf("env: at most %d variables", maxEnvVars)
	}
	for k, v := range c.Env {
		switch {
		case !envKeyRE.MatchString(k):
			return fmt.Errorf("env: %q is not a valid variable name", k)
		case strings.HasPrefix(strings.ToUpper(k), "PILOTHOUSE"):
			// They tie the agent to its session and the daemon.
			return fmt.Errorf("env: %s is reserved for Pilothouse", k)
		case len(v) > maxEnvValueLen || strings.ContainsRune(v, 0) || !utf8.ValidString(v):
			return fmt.Errorf("env: the value of %s is too long or not valid text", k)
		}
	}
	return nil
}

// checkCopyPattern accepts repo-relative globs ("*", "?" and "[…]" within
// one path segment, slash-separated) that cannot leave the repository.
func checkCopyPattern(p string) error {
	switch {
	case p == "":
		return errors.New("is empty")
	case len(p) > maxPatternLen:
		return fmt.Errorf("is longer than %d bytes", maxPatternLen)
	case strings.ContainsRune(p, 0) || !utf8.ValidString(p):
		return errors.New("is not valid text")
	case strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") || filepath.IsAbs(p):
		return errors.New("must be relative to the repository root")
	case strings.Contains(p, "**"):
		return errors.New("** is not supported; use one * per directory level")
	}
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
			return errors.New("must be a clean relative path (no empty, . or trailing / segments)")
		case "..":
			return errors.New("must stay inside the repository (no ..)")
		case ".git":
			return errors.New("must not reach into .git")
		}
		if _, err := filepath.Match(seg, ""); err != nil {
			return errors.New("is not a valid glob")
		}
	}
	return nil
}

// envList is c.Env as KEY=value pairs, sorted for a stable order.
func (c *Config) envList() []string {
	if c == nil || len(c.Env) == 0 {
		return nil
	}
	out := make([]string, 0, len(c.Env))
	for k, v := range c.Env {
		out = append(out, k+"="+v)
	}
	slices.Sort(out)
	return out
}
