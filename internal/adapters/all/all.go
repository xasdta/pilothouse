// Package all lists every built-in adapter.
package all

import (
	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/adapters/claude"
	"github.com/xasdta/pilothouse/internal/adapters/codex"
	"github.com/xasdta/pilothouse/internal/adapters/opencode"
	"github.com/xasdta/pilothouse/internal/api"
)

// List returns every adapter, in display order.
func List() []adapters.Adapter {
	return []adapters.Adapter{claude.New(), codex.New(), opencode.New()}
}

// Get returns the adapter for kind, or nil.
func Get(kind api.AgentKind) adapters.Adapter {
	for _, a := range List() {
		if a.Kind() == kind {
			return a
		}
	}
	return nil
}
