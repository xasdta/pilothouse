package insights

import (
	"math"
	"testing"

	"github.com/xasdta/pilothouse/internal/config"
	"github.com/xasdta/pilothouse/internal/transcript"
)

func TestLookup(t *testing.T) {
	p := NewPrices(nil)
	cases := []struct {
		model string
		input float64 // 0: not found
	}{
		{"claude-opus-5-5", 4},
		{"claude-opus-5", 5},
		{"claude-fable-5-1", 10},
		{"claude-sonnet-5-5", 2},
		{"claude-haiku-5-5", 0.10},
		{"claude-haiku-4-5-20251001", 1},
		{"claude-opus-4-8[1m]", 5},
		{"Claude-Opus-4-8", 5},
		{"us.anthropic.claude-opus-5-5-v1:0", 4},
		{"anthropic.claude-sonnet-4-5-20250929-v1:0", 3},
		{"claude-opus-4-5@20251101", 5},
		{"claude-opus-4-1-20250805", 15},
		{"claude-opus-4-20250514", 15},
		{"claude-sonnet-4-20250514", 3},
		{"gpt-6-sol", 2},
		{"gpt-6.1-sol", 2},
		{"gpt-5.6-sol", 4},
		{"gpt-daybreak-blue-latest", 4},
		// A newer model that merely starts like a known one is unknown.
		{"claude-opus-5-7", 0},
		{"claude-sonnet-5-5-mini", 0},
		{"gpt-6-sol-mini", 0},
		{"claude-3-5-haiku-20241022", 0},
		{"<synthetic>", 0},
		{"", 0},
	}
	for _, c := range cases {
		pr, ok := p.Lookup(c.model)
		if ok != (c.input != 0) || pr.Input != c.input {
			t.Errorf("Lookup(%q) = %+v %v, want input %v", c.model, pr, ok, c.input)
		}
	}
}

func TestOverrides(t *testing.T) {
	p := NewPrices(map[string]config.ModelPrice{
		"claude-opus-5-5": {Input: 1, Output: 2},   // replaces the built-in
		"my-local-model":  {Input: 0.5},            // a new model, and its variants
		"claude-sonnet-5": {Input: 9, Output: 9},   // must not swallow claude-sonnet-5-5
		"gpt-6*":          {Input: 7, Output: 7},   // explicit prefix
		"  ":              {Input: 100, Output: 1}, // ignored
	})
	cases := map[string]float64{
		"claude-opus-5-5":        1,
		"my-local-model":         0.5,
		"my-local-model-q4":      0.5,
		"my-local-modelx":        0,
		"claude-sonnet-5":        9,
		"claude-sonnet-5-5":      2, // the longer built-in wins
		"gpt-6-luna":             0.10,
		"gpt-6-something-new":    7,
		"claude-haiku-4-5":       1,
		"something-else-entirel": 0,
	}
	for model, want := range cases {
		pr, _ := p.Lookup(model)
		if pr.Input != want {
			t.Errorf("Lookup(%q).Input = %v, want %v", model, pr.Input, want)
		}
	}
}

func TestCost(t *testing.T) {
	p := NewPrices(nil)
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	// Opus 5.5: $4 in, $20 out, $0.20 cache read, $5 5-minute write, $8 1-hour write.
	tk := transcript.Tokens{Input: 1_000_000, Output: 100_000, CacheRead: 2_000_000, CacheWrite: 300_000, CacheWrite1h: 100_000}
	want := 4 + 2 + 0.4 + 0.2*5 + 0.1*8
	if got := p.Cost("claude-opus-5-5", tk, false); !near(got, want) {
		t.Errorf("opus 5.5 cost = %v, want %v", got, want)
	}
	if got := p.Cost("claude-opus-5-5", tk, true); !near(got, 2*want) {
		t.Errorf("fast mode = %v, want %v", got, 2*want)
	}
	// Haiku 5.5's long-context tier applies above 100K prompt tokens.
	short := transcript.Tokens{Input: 50_000, Output: 1_000}
	long := transcript.Tokens{Input: 150_000, Output: 1_000}
	if got := p.Cost("claude-haiku-5-5", short, false); !near(got, 0.05*0.10+0.001*0.50) {
		t.Errorf("haiku short = %v", got)
	}
	if got := p.Cost("claude-haiku-5-5", long, false); !near(got, 0.15*0.50+0.001*2.50) {
		t.Errorf("haiku long = %v", got)
	}
	if got := p.Cost("unknown-model", tk, false); got != 0 {
		t.Errorf("unknown model costs %v", got)
	}
	var _ transcript.Pricer = p
}
