// Package demo is a self-contained fake daemon core with a believable set
// of agents, used by `pilothouse demo` (try the phone app in 30 seconds
// without touching real agents) and by the web devserver. It implements
// core.Core and, as workstream H fills it in, the optional 0.2
// capabilities with canned, self-consistent data.
package demo

import (
	"context"
	"time"

	"github.com/xasdta/pilothouse/internal/web/webtest"
)

// Options configures a demo core.
type Options struct {
	Machine string           // default "studio"
	Now     func() time.Time // default time.Now
}

// Core is the demo core.
type Core struct {
	*webtest.FakeCore
}

// New builds a seeded demo core.
func New(o Options) *Core {
	fc := webtest.New()
	fc.Machine = o.Machine
	if fc.Machine == "" {
		fc.Machine = "studio"
	}
	if o.Now != nil {
		fc.Now = o.Now
	}
	return &Core{FakeCore: fc}
}

// Run keeps the demo alive — new requests, questions, replies and
// activity — until ctx ends.
func (c *Core) Run(ctx context.Context) { <-ctx.Done() }
