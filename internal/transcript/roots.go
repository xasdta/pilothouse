package transcript

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/xasdta/pilothouse/internal/adapters/codex"
	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/paths"
)

// Roots are the only directories transcripts are read from. A transcript
// path arrives in hook payloads, which the agent controls, so it is used
// only once it resolves (symlinks included) to a regular .jsonl file
// inside one of the roots for that agent.
type Roots struct {
	Claude []string // ~/.claude/projects, $CLAUDE_CONFIG_DIR/projects
	Codex  []string // $CODEX_HOME/sessions (default ~/.codex/sessions)
}

// DefaultRoots are the agents' own transcript folders. Under
// $PILOTHOUSE_USER_HOME (tests), $CLAUDE_CONFIG_DIR counts only inside
// it, as Codex's home does (codex.Home), so a test never reads a real
// transcript through an inherited variable.
func DefaultRoots() Roots {
	r := Roots{
		Claude: []string{filepath.Join(paths.UserHome(), ".claude", "projects")},
		Codex:  []string{filepath.Join(codex.Home(), "sessions")},
	}
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" && filepath.IsAbs(d) {
		uh := os.Getenv("PILOTHOUSE_USER_HOME")
		if d, uh = filepath.Clean(d), filepath.Clean(uh); uh == "." || d == uh || within(d, uh) {
			if p := filepath.Join(d, "projects"); p != r.Claude[0] {
				r.Claude = append(r.Claude, p)
			}
		}
	}
	return r
}

// For lists the roots an agent's transcripts may live in.
func (r Roots) For(agent api.AgentKind) []string {
	switch agent {
	case api.AgentClaude:
		return r.Claude
	case api.AgentCodex:
		return r.Codex
	}
	return nil
}

// Errors from Open. ErrOutside is what a hostile or mistaken path gets;
// ErrMissing means the agent hasn't written the file yet.
var (
	ErrOutside = errors.New("transcript is outside the agent's transcript folder")
	ErrMissing = errors.New("transcript does not exist (yet)")
)

// Resolve checks path against roots and returns where it really is.
func Resolve(path string, roots []string) (string, error) {
	if path == "" || !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
		return "", ErrOutside
	}
	path = filepath.Clean(path)
	if filepath.Ext(path) != ".jsonl" {
		return "", ErrOutside
	}
	real, err := filepath.EvalSymlinks(path)
	if errors.Is(err, fs.ErrNotExist) {
		// Still refuse paths that could never be accepted, so a missing
		// file outside the roots isn't reported as merely missing.
		if !inAnyRoot(path, roots, false) {
			return "", ErrOutside
		}
		return "", ErrMissing
	}
	if err != nil {
		return "", fmt.Errorf("resolve transcript: %w", err)
	}
	if filepath.Ext(real) != ".jsonl" || !inAnyRoot(real, roots, true) {
		return "", ErrOutside
	}
	return real, nil
}

// inAnyRoot reports whether p lies strictly inside one of roots. Roots
// are compared both as configured and with symlinks resolved (on macOS
// /tmp is /private/tmp; a home directory may be a symlink).
func inAnyRoot(p string, roots []string, resolvedOnly bool) bool {
	for _, root := range roots {
		if root == "" || !filepath.IsAbs(root) {
			continue
		}
		if real, err := filepath.EvalSymlinks(root); err == nil && within(p, real) {
			return true
		}
		if !resolvedOnly && within(p, filepath.Clean(root)) {
			return true
		}
	}
	return false
}

// within reports whether p lies strictly inside root (a root itself is a
// directory, never a transcript).
func within(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, "../") && !filepath.IsAbs(rel)
}

// Open opens a confined transcript for reading. It refuses anything but a
// regular file: a FIFO would block the reader, a device would never end.
// The file is opened without following a final symlink and without
// blocking, then checked again by descriptor, so a path swapped after
// Resolve can't redirect the read.
func Open(path string, roots []string) (*os.File, os.FileInfo, error) {
	real, err := Resolve(path, roots)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(real, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, ErrMissing
		}
		return nil, nil, ErrOutside
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, nil, ErrOutside
	}
	// The directories above the file could have been swapped for symlinks
	// between Resolve and the open: resolve again and make sure it is still
	// this file, inside a root.
	again, err := Resolve(path, roots)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if st2, err := os.Stat(again); err != nil || !os.SameFile(st, st2) {
		f.Close()
		return nil, nil, ErrOutside
	}
	return f, st, nil
}

// sessionIDPattern is what agent session ids look like (UUIDs, possibly
// with a suffix); anything else is never used to build a path.
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{7,127}$`)

// maxProjectDirs bounds the directory scan in FindClaude.
const maxProjectDirs = 5000

// FindClaude looks for <root>/<project>/<sessionID>.jsonl when a session's
// hooks never said where its transcript is.
func FindClaude(roots []string, sessionID string) (string, bool) {
	if !sessionIDPattern.MatchString(sessionID) {
		return "", false
	}
	for _, root := range roots {
		ents, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		var best string
		var bestMod time.Time
		for i, e := range ents {
			if i >= maxProjectDirs {
				break
			}
			if !e.IsDir() {
				continue
			}
			p := filepath.Join(root, e.Name(), sessionID+".jsonl")
			st, err := os.Lstat(p)
			if err != nil || !st.Mode().IsRegular() {
				continue
			}
			if best == "" || st.ModTime().After(bestMod) {
				best, bestMod = p, st.ModTime()
			}
		}
		if best != "" {
			return best, true
		}
	}
	return "", false
}

// codexScanDays bounds how far back FindCodexRollout looks.
const codexScanDays = 31

// FindCodexRollout finds the rollout file of a Codex thread when its hooks
// carried no transcript_path (Codex sends null until the file exists):
// $CODEX_HOME/sessions/YYYY/MM/DD/rollout-<local time>-<thread id>.jsonl,
// or …-<thread id>_<rollout id>.jsonl for a thread that was rolled back
// (the newest wins). The days around started are tried first, then up to
// a month back from now.
func FindCodexRollout(roots []string, threadID string, started, now time.Time) (string, bool) {
	if !sessionIDPattern.MatchString(threadID) {
		return "", false
	}
	var days []time.Time
	seen := map[string]bool{}
	add := func(t time.Time) {
		k := t.Local().Format("2006/01/02")
		if !seen[k] {
			seen[k] = true
			days = append(days, t)
		}
	}
	if !started.IsZero() {
		for _, d := range []int{0, -1, 1} {
			add(started.AddDate(0, 0, d))
		}
	}
	for d := 0; d < codexScanDays; d++ {
		add(now.AddDate(0, 0, -d))
	}
	for _, root := range roots {
		for _, day := range days {
			if p, ok := findRolloutIn(filepath.Join(root, day.Local().Format("2006/01/02")), threadID); ok {
				return p, true
			}
		}
	}
	return "", false
}

func findRolloutIn(dir, threadID string) (string, bool) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	type cand struct {
		path string
		mod  time.Time
	}
	var cands []cand
	for _, e := range ents {
		n := e.Name()
		if !strings.HasPrefix(n, "rollout-") || !strings.HasSuffix(n, ".jsonl") {
			continue
		}
		core := strings.TrimSuffix(n, ".jsonl")
		if !strings.HasSuffix(core, "-"+threadID) && !strings.Contains(core, "-"+threadID+"_") {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		cands = append(cands, cand{filepath.Join(dir, n), info.ModTime()})
	}
	if len(cands) == 0 {
		return "", false
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].mod.After(cands[j].mod) })
	return cands[0].path, true
}
