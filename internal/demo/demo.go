// Package demo is a self-contained fake daemon core with a believable set
// of agents, used by `pilothouse demo` (try the phone app in 30 seconds
// without touching real agents) and by the web devserver.
//
// It implements core.Core and every optional 0.2 capability on canned,
// self-consistent data: sessions across Claude Code, Codex and OpenCode;
// permission requests with risk labels, diffs and the rule that fired; a
// question with options and a plan to approve; activity feeds that grow
// as agents work; usage that adds up to the insights; rules and presets
// that the "would this be allowed?" tester really evaluates; tasks with
// diffs; replies that show up in the feed; and an ANSI terminal mirror
// rendered from the same activity.
//
// Nothing here touches the file system, the network or real agents.
package demo

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
	"github.com/xasdta/pilothouse/internal/web/webtest"
)

// Options configures a demo core.
type Options struct {
	Machine string           // default "studio"
	Now     func() time.Time // default time.Now
	// Quiet makes Run only react to what people do (answers, replies,
	// tasks) instead of also raising new requests and keeping agents
	// busy. Screenshots and end-to-end tests use it for a stable inbox.
	Quiet bool
}

// Home is the made-up home directory of the demo machine.
const Home = "/Users/dev"

// agentOpenCode is OpenCode's agent kind (workstream E adds the constant).
const agentOpenCode api.AgentKind = "opencode"

// Core is the demo core. It embeds the in-memory FakeCore (sessions,
// requests, the event log) and adds the 0.2 capabilities on top.
type Core struct {
	*webtest.FakeCore

	opt Options

	mu       sync.Mutex
	feeds    map[string]*feed // session id → activity
	seen     map[string]time.Time
	reqItem  map[string]itemRef      // request id → its pending tool item
	reqMeta  map[string]reqScript    // request id → how the agent reacts
	terms    map[string]*term        // session id → terminal mirror
	rules    []api.RuleInfo          // managed, user, repo and builtin rules
	presets  []api.Preset            // switchable bundles
	tasks    map[string]*taskState   // task id → diff and progress
	queue    []scheduled             // reactions waiting for Run
	nextID   int                     // ids for generated requests, rules, tasks
	revision map[string]int          // plan request lineage → revision number
	added    map[string]api.RuleSpec // suggestion id → spec (to report "already added")
	script   int                     // position in the lively request script
	lastTick time.Time
}

// scheduled is a reaction that runs once its time has come.
type scheduled struct {
	at time.Time
	fn func()
}

var (
	_ core.Core           = (*Core)(nil)
	_ core.Replier        = (*Core)(nil)
	_ core.Terminal       = (*Core)(nil)
	_ core.ActivityReader = (*Core)(nil)
	_ core.InsightsReader = (*Core)(nil)
	_ core.RuleEditor     = (*Core)(nil)
	_ core.TaskRunner     = (*Core)(nil)
)

// New builds a seeded demo core.
func New(o Options) *Core {
	if o.Machine == "" {
		o.Machine = "studio"
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	fc := webtest.New()
	fc.Machine = o.Machine
	fc.Version = "0.2.0-demo"
	fc.Now = o.Now
	c := &Core{
		FakeCore: fc,
		opt:      o,
		feeds:    map[string]*feed{},
		seen:     map[string]time.Time{},
		reqItem:  map[string]itemRef{},
		reqMeta:  map[string]reqScript{},
		terms:    map[string]*term{},
		tasks:    map[string]*taskState{},
		revision: map[string]int{},
		added:    map[string]api.RuleSpec{},
		nextID:   100,
	}
	c.seed()
	return c
}

func (c *Core) now() time.Time { return c.opt.Now() }

// later schedules fn to run from Run after d.
func (c *Core) later(d time.Duration, fn func()) {
	c.mu.Lock()
	c.queue = append(c.queue, scheduled{at: time.Now().Add(d), fn: fn})
	c.mu.Unlock()
}

// runDue runs every scheduled reaction due at t, oldest first.
func (c *Core) runDue(t time.Time) {
	for {
		c.mu.Lock()
		sort.SliceStable(c.queue, func(i, j int) bool { return c.queue[i].at.Before(c.queue[j].at) })
		if len(c.queue) == 0 || c.queue[0].at.After(t) {
			c.mu.Unlock()
			return
		}
		next := c.queue[0]
		c.queue = c.queue[1:]
		c.mu.Unlock()
		next.fn()
	}
}

// Run keeps the demo alive — reactions to answers and replies, expiring
// requests and, unless Options.Quiet, new requests and busy agents —
// until ctx ends.
func (c *Core) Run(ctx context.Context) {
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	start := time.Now()
	lastRequest := start
	lastWork := start
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			c.runDue(now)
			for _, r := range c.ExpireRequests(c.now()) {
				c.expired(r)
			}
			c.tickTerminals()
			if c.opt.Quiet {
				continue
			}
			if now.Sub(lastWork) >= 7*time.Second {
				lastWork = now
				c.work()
			}
			if now.Sub(lastRequest) >= 30*time.Second {
				lastRequest = now
				c.raiseNext()
			}
		}
	}
}

// MarkSeen also remembers when a person last looked, for "catch me up".
func (c *Core) MarkSeen(ctx context.Context, sessionID string) error {
	if err := c.FakeCore.MarkSeen(ctx, sessionID); err != nil {
		return err
	}
	c.mu.Lock()
	c.seen[sessionID] = c.now()
	c.mu.Unlock()
	return nil
}

// session returns a session by exact id.
func (c *Core) session(id string) (api.Session, bool) {
	s, err := c.FakeCore.Session(context.Background(), id)
	if err != nil || s.ID != id {
		return api.Session{}, false
	}
	return s, true
}

func ptr[T any](v T) *T { return &v }
