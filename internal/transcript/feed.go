package transcript

import (
	"hash/fnv"
	"sort"
	"strings"
	"time"

	"github.com/xasdta/pilothouse/internal/adapters/hookkit"
	"github.com/xasdta/pilothouse/internal/api"
)

// Caps on what a feed keeps. Everything a transcript contributes is
// bounded: items, text, the files list, the message ids used to dedupe
// usage and the cost samples behind the burn rate.
const (
	DefaultMaxItems = 1000
	maxText         = 2000 // prompt and reply text (head kept)
	maxResult       = 1000 // tool output (tail kept)
	maxSummary      = 200
	maxFiles        = 500
	recentMessages  = 512     // message ids whose usage is kept, for per-block dedupe
	maxSeenMessages = 200_000 // message ids remembered (hashed) so copies aren't counted
	costWindow      = 2 * time.Hour
	maxCostSamples  = 4096
)

// item is one feed entry plus what catch-up and file counts need.
type item struct {
	api.ActivityItem
	seq uint64 // ingest order: ties in At keep transcript order
	rev uint64 // last change; cursors return items with a newer rev

	command string     // shell command, for test detection
	edits   []fileEdit // files the tool call changes, as the input says
	exact   bool       // edits were replaced by the tool's own report
	asks    int        // questions put to the person (AskUserQuestion)
	plan    bool       // ExitPlanMode
	exit    *int       // exit code when the result says (Codex)
	denied  bool       // the error is a refusal (a person or a rule), not a failure
	fold    bool       // result folded in (no longer waiting)
	chain   string     // pending-tool scope: "" main, else the sidechain file
}

// fileEdit is what one tool call does to one file.
type fileEdit struct {
	path           string
	op             api.FileOp
	added, removed int
}

type usageKey struct{ day, model string }

type usageAgg struct {
	tokens Tokens
	cost   float64
	turns  int
}

type costSample struct {
	at   time.Time
	cost float64
}

// feed is everything one session's transcripts have said so far.
type feed struct {
	agent    api.AgentKind
	cwd      string
	pricer   Pricer
	loc      *time.Location
	maxItems int

	items []*item          // sorted by (At, seq)
	byID  map[string]*item // every retained item by ID
	seq   uint64
	rev   uint64

	turn        int
	model       string
	byDay       map[usageKey]*usageAgg
	total       Tokens
	cost        float64
	turns       int
	pendingTurn string            // local day of a prompt whose model isn't known yet
	msgUsage    map[string]Tokens // recent message id → usage counted
	msgOrder    []string
	seenMsgs    map[uint64]struct{} // hash of every message id counted
	costs       []costSample
	lastMsg     map[string]string // chain → last assistant message id

	files     map[string]*api.FileChange
	fileRefs  map[string]int // file → edits counted in it
	fileOrder []string       // least recently touched first

	partial bool // some transcript was only read from its tail
	skipped int  // over-long lines dropped
}

func newFeed(agent api.AgentKind, cwd string, p Pricer, loc *time.Location, maxItems int) *feed {
	if p == nil {
		p = NoPrices{}
	}
	if loc == nil {
		loc = time.Local
	}
	if maxItems <= 0 {
		maxItems = DefaultMaxItems
	}
	return &feed{
		agent: agent, cwd: cwd, pricer: p, loc: loc, maxItems: maxItems,
		byID: map[string]*item{}, byDay: map[usageKey]*usageAgg{},
		msgUsage: map[string]Tokens{}, seenMsgs: map[uint64]struct{}{}, lastMsg: map[string]string{},
		files: map[string]*api.FileChange{}, fileRefs: map[string]int{},
	}
}

func hashID(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}

// add inserts a new item, or updates the retained one with the same ID
// (a transcript re-read after an over-long line, a duplicate record).
func (f *feed) add(it *item) *item {
	if it.ID == "" {
		return nil
	}
	if old, ok := f.byID[it.ID]; ok {
		return old
	}
	f.seq++
	f.rev++
	it.seq, it.rev = f.seq, f.rev
	if it.Turn == 0 {
		it.Turn = f.turn
	}
	it.Text = capHead(it.Text, maxText)
	it.Summary = hookkit.Truncate(oneLine(it.Summary), maxSummary)
	// Usually newest: append. A sidechain read after the main file may
	// interleave, so insert in place.
	n := len(f.items)
	i := n
	if n > 0 && before(it, f.items[n-1]) {
		i = sort.Search(n, func(j int) bool { return before(it, f.items[j]) })
	}
	f.items = append(f.items, nil)
	copy(f.items[i+1:], f.items[i:])
	f.items[i] = it
	f.byID[it.ID] = it
	for len(f.items) > f.maxItems {
		delete(f.byID, f.items[0].ID)
		f.items[0] = nil
		f.items = f.items[1:]
	}
	return it
}

func before(a, b *item) bool {
	if !a.At.Equal(b.At) {
		return a.At.Before(b.At)
	}
	return a.seq < b.seq
}

// touch marks an item changed so cursors return it again.
func (f *feed) touch(it *item) {
	f.rev++
	it.rev = f.rev
}

// result folds a tool result into its call.
func (f *feed) result(id, text string, isError bool, exit *int) *item {
	it, ok := f.byID[id]
	if !ok || it.fold {
		return nil
	}
	it.fold, it.Pending = true, false
	it.Result = capTail(strings.TrimSpace(text), maxResult)
	it.IsError = isError || (exit != nil && *exit != 0)
	it.exit = exit
	if it.IsError && len(it.edits) > 0 {
		// The edit didn't happen: take it back out of the files list.
		f.applyEdits(it.edits, -1)
		it.edits = nil
	}
	f.touch(it)
	return it
}

// settle marks every tool call still waiting in chain as done: the agent
// moved on (a new response, the turn ended), so their results are lost to
// us (an over-long line) or never came (interrupted).
func (f *feed) settle(chain string) {
	for i := len(f.items) - 1; i >= 0; i-- {
		it := f.items[i]
		if it.Pending && it.chain == chain {
			it.Pending, it.fold = false, true
			f.touch(it)
		}
	}
}

// prompt starts a new turn.
func (f *feed) prompt(at time.Time) {
	f.turn++
	f.settle("")
	f.pendingTurn = f.day(at)
}

func (f *feed) day(t time.Time) string { return t.In(f.loc).Format("2006-01-02") }

// addUsage counts one response's tokens. msgID dedupes Claude's
// per-block records (the largest values win); "" means every call is a
// separate response (Codex's per-response deltas). main is false for
// subagents: their model is not the session's, and they answer no turn.
func (f *feed) addUsage(msgID, model string, at time.Time, t Tokens, fast, main bool) {
	if model == "" {
		model = f.model
	}
	if main && model != "" {
		f.model = model
	}
	if msgID != "" {
		prev, seen := f.msgUsage[msgID]
		if !seen {
			// Subagent transcripts can repeat responses logged long before
			// (forks copy their parent's context): count each id once.
			h := hashID(msgID)
			if _, dup := f.seenMsgs[h]; dup {
				return
			}
			if len(f.seenMsgs) < maxSeenMessages {
				f.seenMsgs[h] = struct{}{}
			}
		}
		next := maxTokens(prev, t)
		if seen && next == prev {
			return
		}
		f.msgUsage[msgID] = next
		if !seen {
			f.msgOrder = append(f.msgOrder, msgID)
			if len(f.msgOrder) > recentMessages {
				delete(f.msgUsage, f.msgOrder[0])
				f.msgOrder = f.msgOrder[1:]
			}
		}
		// Cost is priced on the response as a whole (Haiku's long-context
		// tier depends on the full prompt), counted as the difference.
		cost := f.pricer.Cost(model, next, fast)
		if seen {
			cost -= f.pricer.Cost(model, prev, fast)
		}
		f.count(model, at, subTokens(next, prev), cost, main)
		return
	}
	f.count(model, at, t, f.pricer.Cost(model, t, fast), main)
}

func (f *feed) count(model string, at time.Time, t Tokens, cost float64, main bool) {
	k := usageKey{f.day(at), model}
	a := f.byDay[k]
	if a == nil {
		a = &usageAgg{}
		f.byDay[k] = a
	}
	a.tokens.Add(t)
	a.cost += cost
	f.total.Add(t)
	f.cost += cost
	if main && f.pendingTurn != "" {
		// A turn counts for the model that answered it, on the day it was
		// asked.
		pk := usageKey{f.pendingTurn, model}
		if pa := f.byDay[pk]; pa != nil {
			pa.turns++
		} else {
			f.byDay[pk] = &usageAgg{turns: 1}
		}
		f.turns++
		f.pendingTurn = ""
	}
	if cost != 0 {
		f.costs = append(f.costs, costSample{at, cost})
		cut := 0
		for cut < len(f.costs) && (at.Sub(f.costs[cut].at) > costWindow || len(f.costs)-cut > maxCostSamples) {
			cut++
		}
		f.costs = f.costs[cut:]
	}
}

// applyEdits adds (sign 1) or takes back (sign -1) a call's edits in the
// files list. A file whose every edit was taken back leaves the list.
func (f *feed) applyEdits(edits []fileEdit, sign int) {
	for _, e := range edits {
		if e.path == "" {
			continue
		}
		p := hookkit.RelPath(f.cwd, e.path)
		fc := f.files[p]
		switch {
		case sign < 0 && fc == nil:
			continue
		case sign < 0:
			fc.Added = max(0, fc.Added-e.added)
			fc.Removed = max(0, fc.Removed-e.removed)
			if f.fileRefs[p]--; f.fileRefs[p] <= 0 {
				delete(f.files, p)
				delete(f.fileRefs, p)
				f.fileOrder = removeString(f.fileOrder, p)
			}
			continue
		case fc == nil:
			fc = &api.FileChange{Path: p, Op: e.op}
			f.files[p] = fc
		default:
			fc.Op = mergeOp(fc.Op, e.op)
			f.fileOrder = removeString(f.fileOrder, p)
		}
		f.fileRefs[p]++
		f.fileOrder = append(f.fileOrder, p)
		fc.Added += e.added
		fc.Removed += e.removed
		for len(f.fileOrder) > maxFiles {
			delete(f.files, f.fileOrder[0])
			delete(f.fileRefs, f.fileOrder[0])
			f.fileOrder = f.fileOrder[1:]
		}
	}
}

// mergeOp is a file's op after another change: deleting wins, a file the
// session created stays "created", otherwise the latest change says.
func mergeOp(prev, next api.FileOp) api.FileOp {
	switch {
	case next == api.FileDelete:
		return api.FileDelete
	case prev == api.FileCreate && next != api.FileRename:
		return api.FileCreate
	case prev == api.FileDelete && (next == api.FileCreate || next == api.FileOverwrite):
		return api.FileCreate
	}
	return next
}

// setEdits replaces what a call does to files (the tool's own report is
// better than the estimate from its input).
func (f *feed) setEdits(it *item, edits []fileEdit) {
	f.applyEdits(it.edits, -1)
	it.edits, it.exact = edits, true
	f.applyEdits(edits, 1)
	f.touch(it)
}

func removeString(ss []string, s string) []string {
	for i, x := range ss {
		if x == s {
			return append(ss[:i], ss[i+1:]...)
		}
	}
	return ss
}

// usage is the session total.
func (f *feed) usage() api.Usage {
	return apiUsage(f.total, f.cost, f.model, f.turns)
}

// DayUsage is one session's usage on one local day with one model.
type DayUsage struct {
	Day     string // YYYY-MM-DD, local time
	Model   string
	Tokens  Tokens
	CostUSD float64
	Turns   int
}

func (f *feed) days() []DayUsage {
	out := make([]DayUsage, 0, len(f.byDay))
	for k, a := range f.byDay {
		out = append(out, DayUsage{Day: k.day, Model: k.model, Tokens: a.tokens, CostUSD: roundCost(a.cost), Turns: a.turns})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Day != out[j].Day {
			return out[i].Day < out[j].Day
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// spentSince is the estimated cost of responses at or after t.
func (f *feed) spentSince(t time.Time) float64 {
	var sum float64
	for _, c := range f.costs {
		if !c.at.Before(t) {
			sum += c.cost
		}
	}
	return sum
}

func (f *feed) fileList() []api.FileChange {
	out := make([]api.FileChange, 0, len(f.fileOrder))
	for i := len(f.fileOrder) - 1; i >= 0; i-- { // most recently touched first
		out = append(out, *f.files[f.fileOrder[i]])
	}
	return out
}

// ---- text helpers ----

// capHead keeps the first n bytes of s.
func capHead(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return hookkit.CapBytes(s, n-len("…")) + "…"
}

// capTail keeps the last n bytes of s: the end of a command's output is
// where the verdict is.
func capTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := len(s) - (n - len("…"))
	for cut < len(s) && s[cut]&0xC0 == 0x80 {
		cut++
	}
	return "…" + s[cut:]
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
