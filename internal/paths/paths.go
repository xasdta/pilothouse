// Package paths resolves every file location Pilothouse uses. Nothing else
// in the codebase may build these paths by hand: tests point PILOTHOUSE_HOME
// at a temp dir and must never touch the real home directory.
package paths

import (
	"os"
	"path/filepath"
)

// Home is the Pilothouse data directory: $PILOTHOUSE_HOME, else ~/.pilothouse.
func Home() string {
	if h := os.Getenv("PILOTHOUSE_HOME"); h != "" {
		return h
	}
	u, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "pilothouse")
	}
	return filepath.Join(u, ".pilothouse")
}

// UserHome is the user's home directory, used to find agent settings
// (~/.claude, ~/.codex). $PILOTHOUSE_USER_HOME overrides it for tests.
func UserHome() string {
	if h := os.Getenv("PILOTHOUSE_USER_HOME"); h != "" {
		return h
	}
	u, err := os.UserHomeDir()
	if err != nil {
		return os.TempDir()
	}
	return u
}

func Config() string       { return filepath.Join(Home(), "config.yaml") }
func Policy() string       { return filepath.Join(Home(), "policy.yaml") }
func Rules() string        { return filepath.Join(Home(), "rules.yaml") } // managed rules and presets (written by Pilothouse)
func DB() string           { return filepath.Join(Home(), "pilothouse.db") }
func Keys() string         { return filepath.Join(Home(), "keys") } // VAPID + daemon keys, 0700
func Logs() string         { return filepath.Join(Home(), "logs") }
func TrustedRepos() string { return filepath.Join(Home(), "trusted-repos.yaml") }

// Run is the directory for sockets. It is kept short because Unix socket
// paths are limited to ~104 bytes on macOS.
func Run() string {
	if d := os.Getenv("PILOTHOUSE_RUN_DIR"); d != "" {
		return d
	}
	return filepath.Join(Home(), "run")
}

// Socket is the daemon's Unix socket: the only path for local decisions.
func Socket() string { return filepath.Join(Run(), "pilothouse.sock") }

// HolderSocket is the socket of one `pilothouse run` holder.
func HolderSocket(sessionID string) string {
	return filepath.Join(Run(), "h-"+sessionID+".sock")
}

// Ensure creates the data directories with private permissions.
func Ensure() error {
	for _, d := range []string{Home(), Run(), Keys(), Logs()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}
