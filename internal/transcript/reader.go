// Package transcript reads agent transcripts — Claude Code's session
// JSONL and Codex's rollout files — into activity feeds and token usage
// (docs/PLAN-0.2.md F3, F6).
//
// Reads are incremental: each file's offset is remembered, so a refresh
// parses only what the agent appended. They are also bounded everywhere:
// paths are confined to the agents' own transcript folders (Roots), a
// huge file is parsed in full only near its end (DefaultFirstScan; the
// part before that only counts usage, up to DefaultMaxHead), over-long
// lines are skipped, and a feed keeps only its newest items. Records this
// package doesn't know are skipped, never fatal: both formats are
// undocumented and change between agent versions.
package transcript

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
)

// Options configure a Reader. Zero values mean the defaults.
type Options struct {
	Roots       Roots
	Pricer      Pricer
	Location    *time.Location // days for usage; default time.Local
	MaxItems    int            // per session; default DefaultMaxItems
	MaxSessions int            // feeds kept in memory; default 24
	MaxLine     int
	FirstScan   int64
	MaxHead     int64
	MaxRead     int64
	// MaxSidechains bounds the subagent files read per Claude session
	// (the most recently written ones win); default 64.
	MaxSidechains int
}

// Source says where a session's transcript is.
type Source struct {
	Agent api.AgentKind
	Path  string // as reported by the agent's hooks, or found by FindClaude / FindCodexRollout
	Cwd   string // for relative paths in summaries
}

// Reader keeps one feed per session. It is safe for concurrent use;
// reads of different sessions run in parallel.
type Reader struct {
	opts Options

	mu       sync.Mutex
	sessions map[string]*session
}

type session struct {
	mu       sync.Mutex
	src      Source
	feed     *feed
	epoch    string
	main     *file
	sides    map[string]*file // subagent transcript path → state
	lastUsed time.Time        // guarded by Reader.mu
}

type file struct {
	path  string
	tail  tail
	parse lineFunc
	head  lineFunc // usage-only pass over a huge file's start (nil: none)
}

// NewReader returns a Reader.
func NewReader(o Options) *Reader {
	if o.MaxItems <= 0 {
		o.MaxItems = DefaultMaxItems
	}
	if o.MaxSessions <= 0 {
		o.MaxSessions = 24
	}
	if o.MaxLine <= 0 {
		o.MaxLine = DefaultMaxLine
	}
	if o.FirstScan <= 0 {
		o.FirstScan = DefaultFirstScan
	}
	if o.MaxHead <= 0 {
		o.MaxHead = DefaultMaxHead
	}
	if o.MaxRead <= 0 {
		o.MaxRead = DefaultMaxRead
	}
	if o.MaxSidechains <= 0 {
		o.MaxSidechains = 64
	}
	if o.Location == nil {
		o.Location = time.Local
	}
	if o.Pricer == nil {
		o.Pricer = NoPrices{}
	}
	return &Reader{opts: o, sessions: map[string]*session{}}
}

// Query selects what Read returns.
type Query struct {
	Cursor string    // from a previous Result: only items added or changed since
	Limit  int       // default 200, max 1000 (newest kept)
	Since  time.Time // catch-up start; with CatchUp false, no catch-up
	Now    time.Time
	// CatchUp asks for a summary of the feed after Since (all of it when
	// Since is zero).
	CatchUp bool
	Extra   CatchUpExtra
}

// Result is a session's feed and what derives from it.
type Result struct {
	Items   []api.ActivityItem // oldest first
	Cursor  string
	Usage   api.Usage
	Files   []api.FileChange
	CatchUp *api.CatchUp
	Receipt *api.Receipt
	// Partial: the transcript was too big to read whole, so the feed and
	// usage start part way through it.
	Partial bool
}

// UsageResult is a session's usage, without the feed.
type UsageResult struct {
	Usage   api.Usage
	Days    []DayUsage
	Partial bool
}

const (
	defaultLimit = 200
	maxLimit     = 1000
)

// Read refreshes the session's transcript and returns its feed.
func (r *Reader) Read(key string, src Source, q Query) (Result, error) {
	s := r.session(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := r.refresh(s, src); err != nil {
		return Result{}, err
	}
	f := s.feed
	limit := q.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	limit = min(limit, maxLimit)
	after := s.parseCursor(q.Cursor)
	var picked []*item
	for i := len(f.items) - 1; i >= 0 && len(picked) < limit; i-- {
		if it := f.items[i]; it.rev > after {
			picked = append(picked, it)
		}
	}
	res := Result{Items: make([]api.ActivityItem, len(picked)), Cursor: s.cursor(), Usage: f.usage(),
		Files: f.fileList(), Partial: f.partial}
	for i, it := range picked {
		res.Items[len(picked)-1-i] = it.ActivityItem
	}
	if q.CatchUp {
		res.CatchUp = f.catchUp(q.Since, q.Now, q.Extra)
	}
	res.Receipt = f.receipt()
	return res, nil
}

// Usage refreshes the session's transcript and returns its token use.
func (r *Reader) Usage(key string, src Source) (UsageResult, error) {
	s := r.session(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := r.refresh(s, src); err != nil {
		return UsageResult{}, err
	}
	return UsageResult{Usage: s.feed.usage(), Days: s.feed.days(), Partial: s.feed.partial}, nil
}

// SpentSince sums the estimated cost of every response at or after t
// across the sessions in memory (the burn rate). Only the last two hours
// are kept.
func (r *Reader) SpentSince(t time.Time) float64 {
	r.mu.Lock()
	ss := make([]*session, 0, len(r.sessions))
	for _, s := range r.sessions {
		ss = append(ss, s)
	}
	r.mu.Unlock()
	var sum float64
	for _, s := range ss {
		s.mu.Lock()
		if s.feed != nil {
			sum += s.feed.spentSince(t)
		}
		s.mu.Unlock()
	}
	return sum
}

// Forget drops a session's feed (it is rebuilt on the next read).
func (r *Reader) Forget(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, key)
}

// session returns the session's state, evicting the least recently used
// one when the cache is full.
func (r *Reader) session(key string) *session {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.sessions[key]
	if s == nil {
		for len(r.sessions) >= r.opts.MaxSessions {
			var oldK string
			var oldT time.Time
			for k, x := range r.sessions {
				if oldK == "" || x.lastUsed.Before(oldT) {
					oldK, oldT = k, x.lastUsed
				}
			}
			delete(r.sessions, oldK)
		}
		s = &session{}
		r.sessions[key] = s
	}
	s.lastUsed = time.Now()
	return s
}

// refresh reads whatever the session's transcripts gained. A changed
// source, or a file that was replaced or rewritten, starts the feed over
// under a new epoch, so clients holding an old cursor get everything.
func (r *Reader) refresh(s *session, src Source) error {
	if s.feed == nil || s.src.Agent != src.Agent || s.src.Path != src.Path {
		s.reset(src, r.opts)
	}
	roots := r.opts.Roots.For(src.Agent)
	if roots == nil {
		return ErrOutside
	}
	for attempt := 0; ; attempt++ {
		err := r.readFile(s.main, roots)
		if err == nil && src.Agent == api.AgentClaude {
			err = r.readSidechains(s, roots)
		}
		if errors.Is(err, errReset) && attempt == 0 {
			// Usage can't be taken back line by line: count it all again.
			s.reset(src, r.opts)
			continue
		}
		if err != nil {
			return err
		}
		break
	}
	partial := s.main.tail.partial
	for _, fl := range s.sides {
		partial = partial || fl.tail.partial
	}
	s.feed.partial = partial
	return nil
}

func (r *Reader) readFile(fl *file, roots []string) error {
	f, st, err := Open(fl.path, roots)
	if err != nil {
		return err
	}
	defer f.Close()
	lim := tailLimits{maxLine: r.opts.MaxLine, firstScan: r.opts.FirstScan, maxHead: r.opts.MaxHead, maxRead: r.opts.MaxRead}
	return fl.tail.read(f, st, lim, fl.parse, fl.head)
}

func (s *session) reset(src Source, o Options) {
	s.src = src
	s.feed = newFeed(src.Agent, src.Cwd, o.Pricer, o.Location, o.MaxItems)
	s.epoch = newEpoch()
	s.main = &file{path: src.Path}
	s.sides = map[string]*file{}
	switch src.Agent {
	case api.AgentClaude:
		p := &claudeParser{f: s.feed}
		s.main.parse, s.main.head = p.line, p.headLine
	case api.AgentCodex:
		// Rollout lines are small and few: the head pass is a full parse.
		p := &codexParser{f: s.feed}
		s.main.parse, s.main.head = p.line, p.line
	case api.AgentGrok:
		p := &grokParser{f: s.feed}
		s.main.parse, s.main.head = p.line, p.line
	default:
		s.main.parse = func([]byte, int64) {}
	}
}

// readSidechains reads Claude's subagent transcripts:
// <dir>/<session id>/subagents/agent-<id>.jsonl, newest MaxSidechains.
// Only a rewritten file is an error (errReset); an unreadable one is
// skipped.
func (r *Reader) readSidechains(s *session, roots []string) error {
	real, err := Resolve(s.main.path, roots)
	if err != nil {
		return nil
	}
	dir := filepath.Join(strings.TrimSuffix(real, ".jsonl"), "subagents")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type cand struct {
		path string
		mod  time.Time
	}
	var cands []cand
	for _, e := range ents {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		if info, err := e.Info(); err == nil {
			cands = append(cands, cand{filepath.Join(dir, e.Name()), info.ModTime()})
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if !cands[i].mod.Equal(cands[j].mod) {
			return cands[i].mod.After(cands[j].mod)
		}
		return cands[i].path < cands[j].path
	})
	if len(cands) > r.opts.MaxSidechains {
		cands = cands[:r.opts.MaxSidechains]
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].path < cands[j].path })
	for _, c := range cands {
		fl := s.sides[c.path]
		if fl == nil {
			p := &claudeParser{f: s.feed, sidechain: true, chain: filepath.Base(c.path)}
			p.turn = s.feed.turnOfCall(sidechainCall(c.path))
			fl = &file{path: c.path, parse: p.line, head: p.headLine}
			s.sides[c.path] = fl
		}
		if err := r.readFile(fl, roots); errors.Is(err, errReset) {
			return err
		}
	}
	return nil
}

// sidechainCall reads the id of the Agent tool call that started a
// subagent from its agent-<id>.meta.json, "" when unknown.
func sidechainCall(path string) string {
	meta := strings.TrimSuffix(path, ".jsonl") + ".meta.json"
	st, err := os.Lstat(meta)
	if err != nil || !st.Mode().IsRegular() || st.Size() > 64<<10 {
		return ""
	}
	b, err := os.ReadFile(meta)
	if err != nil {
		return ""
	}
	var m struct {
		ToolUseID string `json:"toolUseId"`
	}
	if json.Unmarshal(b, &m) != nil {
		return ""
	}
	return m.ToolUseID
}

// turnOfCall is the turn of the retained item with this id (0 when not
// retained: the sidechain then takes the current turn).
func (f *feed) turnOfCall(id string) int {
	if it := f.byID[id]; it != nil {
		return it.Turn
	}
	return 0
}

func newEpoch() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b)
}

func (s *session) cursor() string {
	return s.epoch + "." + strconv.FormatUint(s.feed.rev, 36)
}

// parseCursor returns the revision a cursor stands for; 0 (everything)
// for a cursor from another epoch or none.
func (s *session) parseCursor(c string) uint64 {
	epoch, rev, ok := strings.Cut(c, ".")
	if !ok || epoch != s.epoch {
		return 0
	}
	n, err := strconv.ParseUint(rev, 36, 64)
	if err != nil || n > s.feed.rev {
		return 0
	}
	return n
}

// IsMissing reports whether err means the transcript isn't there (yet).
func IsMissing(err error) bool { return errors.Is(err, ErrMissing) || errors.Is(err, fs.ErrNotExist) }
