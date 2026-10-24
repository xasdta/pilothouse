package telegram

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/xasdta/pilothouse/internal/config"
	"github.com/xasdta/pilothouse/internal/paths"
)

// PairTTL is how long a pairing link works.
const PairTTL = 15 * time.Minute

// Keys is ~/.pilothouse/keys/telegram.json (0600): the bot token and the
// pending pairing code. The token never goes into config.yaml, events,
// logs or the API; the pairing code is stored only as a hash.
type Keys struct {
	Token       string `json:"token"`
	BotID       int64  `json:"bot_id,omitempty"`
	BotUsername string `json:"bot_username,omitempty"`
	// PairHash is hex SHA-256 of the one-time code in the t.me/<bot>?start=
	// link; empty once a chat used it.
	PairHash    string    `json:"pair_hash,omitempty"`
	PairExpires time.Time `json:"pair_expires,omitempty"`
}

// KeysPath is where the Telegram keys live.
func KeysPath() string { return filepath.Join(paths.Keys(), "telegram.json") }

// LoadKeys reads the keys file; a missing file is (Keys{}, nil).
func LoadKeys(path string) (Keys, error) {
	var k Keys
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return k, nil
	}
	if err != nil {
		return k, err
	}
	if err := json.Unmarshal(b, &k); err != nil {
		return Keys{}, fmt.Errorf("%s: %w", path, err)
	}
	return k, nil
}

// SaveKeys writes the keys file atomically with mode 0600. Prefer
// UpdateKeys when the keys were read earlier: the CLI and the daemon both
// write this file.
func SaveKeys(path string, k Keys) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return config.WithFileLock(path, func() error { return saveKeys(path, k) })
}

// UpdateKeys changes the keys file as one locked read-modify-write (see
// config.Update). fn returning an error saves nothing.
func UpdateKeys(path string, fn func(*Keys) error) (Keys, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Keys{}, err
	}
	var out Keys
	err := config.WithFileLock(path, func() error {
		k, err := LoadKeys(path)
		if err != nil {
			return err
		}
		if err := fn(&k); err != nil {
			return err
		}
		out = k
		return saveKeys(path, k)
	})
	return out, err
}

// RemoveKeys deletes the keys file (under the same lock).
func RemoveKeys(path string) error {
	if _, err := os.Stat(filepath.Dir(path)); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return config.WithFileLock(path, func() error {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
}

// ErrCodeChanged: the pairing code was used, replaced or expired between
// checking it and spending it.
var ErrCodeChanged = errors.New("the pairing code is no longer pending")

// SpendCode clears the pending pairing code if it is still code: the check
// and the spend are one locked step, so a code is used at most once and a
// newer code (`pilothouse notify telegram` run again) is never cleared.
func SpendCode(path, code string, now time.Time) error {
	_, err := UpdateKeys(path, func(k *Keys) error {
		if !k.CheckCode(code, now) {
			return ErrCodeChanged
		}
		k.PairHash, k.PairExpires = "", time.Time{}
		return nil
	})
	return err
}

func saveKeys(path string, k Keys) error {
	b, _ := json.MarshalIndent(k, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// NewPairCode returns a one-time code for the ?start= link (22 base64url
// characters, 128 bits) and stores its hash and expiry in k.
func (k *Keys) NewPairCode(now time.Time) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	code := base64.RawURLEncoding.EncodeToString(b)
	k.PairHash = hashCode(code)
	k.PairExpires = now.Add(PairTTL).UTC()
	return code
}

// Pending reports whether a pairing code is waiting to be used.
func (k Keys) Pending(now time.Time) bool {
	return k.PairHash != "" && now.Before(k.PairExpires)
}

// CheckCode reports whether code is the pending, unexpired pairing code.
// The comparison is constant-time.
func (k Keys) CheckCode(code string, now time.Time) bool {
	if !k.Pending(now) || !codeRE.MatchString(code) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hashCode(code)), []byte(k.PairHash)) == 1
}

var codeRE = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

func hashCode(code string) string {
	h := sha256.Sum256([]byte(code))
	return hex.EncodeToString(h[:])
}

// StartLink is the deep link that opens the bot and sends /start <code>.
func StartLink(bot, code string) string {
	return "https://t.me/" + bot + "?start=" + code
}

// ValidToken does a cheap shape check of a bot token ("123456:ABC…")
// before it is sent anywhere.
func ValidToken(tok string) bool { return tokenRE.MatchString(tok) }

var tokenRE = regexp.MustCompile(`^[0-9]{3,20}:[A-Za-z0-9_-]{20,100}$`)

// startCode returns the code of a "/start <code>" (or "/start@bot <code>")
// message, if it is one.
func startCode(text string) (string, bool) {
	f := strings.Fields(text)
	if len(f) == 0 {
		return "", false
	}
	cmd, _, _ := strings.Cut(f[0], "@")
	if cmd != "/start" {
		return "", false
	}
	if len(f) < 2 {
		return "", true
	}
	return f[1], true
}
