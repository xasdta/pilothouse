package daemon

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/xasdta/pilothouse/internal/store"
	"github.com/xasdta/pilothouse/internal/version"
)

// The anonymous update check (BUILD-SPEC invariant 7) sends only the
// version, OS, architecture and an id that changes every month, once a day.
// It reveals nothing about sessions, prompts, commands or paths.

const (
	updateSeedKey    = "update_seed"
	updateCheckedKey = "update_checked_at"
	updateEvery      = 24 * time.Hour
	updateFirstDelay = time.Minute
)

// updateURL is a variable so tests can point it at a local server.
var updateURL = "https://pilothouse.dev/api/version"

func (d *Daemon) updateCheckEnabled() bool {
	// Development builds ("dev") never phone home: that also keeps every
	// test binary off the network.
	return !d.opts.DisableUpdateCheck && d.cfg.UpdateCheckEnabled() && version.Version != "dev"
}

func (d *Daemon) startUpdateCheck() {
	if !d.updateCheckEnabled() {
		return
	}
	d.goBackground(func() {
		t := time.NewTimer(updateFirstDelay)
		defer t.Stop()
		for {
			select {
			case <-d.ctx.Done():
				return
			case <-t.C:
			}
			if d.updateDue(d.ctx) {
				d.checkForUpdate(d.ctx)
			}
			t.Reset(time.Hour)
		}
	})
}

func (d *Daemon) updateDue(ctx context.Context) bool {
	v, ok, err := d.db.Meta(ctx, updateCheckedKey)
	if err != nil {
		return false
	}
	if !ok {
		return true
	}
	last, err := time.Parse(time.RFC3339, v)
	return err != nil || d.now().Sub(last) >= updateEvery
}

// checkForUpdate asks for the latest version and logs if this one is
// older. Every failure is ignored: the check must never affect the daemon.
func (d *Daemon) checkForUpdate(ctx context.Context) {
	id, err := d.anonymousID(ctx)
	if err != nil {
		return
	}
	// Record the attempt first so a failing endpoint is retried tomorrow,
	// not every hour.
	_ = d.db.Write(ctx, func(tx *store.Tx) error {
		return tx.SetMeta(ctx, updateCheckedKey, d.now().Format(time.RFC3339))
	})
	q := url.Values{"v": {version.Version}, "os": {runtime.GOOS}, "arch": {runtime.GOARCH}, "id": {id}}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, updateURL+"?"+q.Encode(), nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", "pilothouse/"+version.Version)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	var body updateInfo
	if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body) != nil {
		return
	}
	latest := firstNonEmpty(body.Latest, body.Version)
	if newerVersion(latest, version.Version) {
		args := []any{"current", version.Version, "latest", latest, "upgrade", body.upgradeURL()}
		if body.Notes != "" {
			args = append(args, "notes", body.Notes)
		}
		d.log.Info("a newer pilothouse is available", args...)
	}
}

// installDocsURL is where upgrade instructions live when the update server
// doesn't name a page.
const installDocsURL = "https://pilothouse.dev/docs/install"

// updateInfo is the update server's answer.
type updateInfo struct {
	Latest  string `json:"latest"`
	Version string `json:"version"`
	URL     string `json:"url"`   // release / upgrade page
	Notes   string `json:"notes"` // release notes (a URL or short text)
}

// upgradeURL is the page to point people at: the server's url, else notes
// when they are a link, else the install docs.
func (u updateInfo) upgradeURL() string {
	for _, s := range []string{u.URL, u.Notes} {
		if strings.HasPrefix(s, "https://") {
			return s
		}
	}
	return installDocsURL
}

// anonymousID is sha256(random seed + YYYY-MM), first 16 hex chars: stable
// within a month so the server can count installs, unlinkable across months.
func (d *Daemon) anonymousID(ctx context.Context) (string, error) {
	seed, ok, err := d.db.Meta(ctx, updateSeedKey)
	if err != nil {
		return "", err
	}
	if !ok {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		seed = hex.EncodeToString(b[:])
		if err := d.db.Write(ctx, func(tx *store.Tx) error { return tx.SetMeta(ctx, updateSeedKey, seed) }); err != nil {
			return "", err
		}
	}
	return monthlyID(seed, d.now()), nil
}

func monthlyID(seed string, now time.Time) string {
	sum := sha256.Sum256([]byte(seed + now.UTC().Format("2006-01")))
	return hex.EncodeToString(sum[:])[:16]
}

// newerVersion reports whether semver-ish a ("v1.2.3", "1.2") is newer
// than b. Unparseable versions are never newer.
func newerVersion(a, b string) bool {
	pa, okA := parseVersion(a)
	pb, okB := parseVersion(b)
	if !okA || !okB {
		return false
	}
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func parseVersion(s string) ([3]int, bool) {
	var out [3]int
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if s == "" || len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
