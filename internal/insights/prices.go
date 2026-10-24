// Package insights estimates what agents cost and aggregates usage and
// policy decisions for the insights page and `pilothouse usage`
// (docs/PLAN-0.2.md F6).
package insights

import (
	"sort"
	"strings"

	"github.com/xasdta/pilothouse/internal/config"
	"github.com/xasdta/pilothouse/internal/transcript"
)

// Price is a list price in USD per million tokens.
type Price struct {
	Input      float64
	Output     float64
	CacheRead  float64
	CacheWrite float64 // 5-minute cache writes; 1-hour writes cost 2× Input
	// Long applies instead when a request's prompt (input + cache reads +
	// cache writes) is larger than LongAbove tokens.
	LongAbove int64
	Long      *Price
}

// Built-in list prices, standard speed, USD per million tokens.
//
// Claude: Anthropic first-party API prices from the claude-api skill's
// model table (cached 2026-10-06) and its prompt-caching notes: cache
// writes 1.25× input (5-minute) and 2× (1-hour); cache reads 0.1× input,
// except Fable 5.1 / Mythos 5.1 ($0.25) and Opus 5.5 ($0.20). Haiku 5.5
// costs $0.50 / $2.50 above 100K prompt tokens. Fast mode (usage.speed
// "fast") is billed at 2× (Opus 5.5: $8 / $40).
//
// OpenAI (Codex): the Codex token rate card (learn.chatgpt.com/docs/pricing,
// read 2026-10-23), which quotes credits per million tokens at standard
// speed, converted at $0.04 per credit. That card prices ChatGPT-plan
// credit usage; API-key usage is billed at OpenAI's API prices, which
// could not be checked here. Codex charges no separate cache write, so
// cache writes are priced as input. Override any of these with
// usage.prices in config.yaml.
//
// Models not listed cost 0; their tokens are still counted.
var builtin = map[string]Price{
	// Claude Fable / Mythos.
	"claude-fable-5-1":  {Input: 10, Output: 50, CacheRead: 0.25, CacheWrite: 12.5},
	"claude-mythos-5-1": {Input: 10, Output: 50, CacheRead: 0.25, CacheWrite: 12.5},
	"claude-fable-5":    {Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5},
	"claude-mythos-5":   {Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5},
	// Claude Opus.
	"claude-opus-5-5": {Input: 4, Output: 20, CacheRead: 0.20, CacheWrite: 5},
	"claude-opus-5":   {Input: 5, Output: 25, CacheRead: 0.50, CacheWrite: 6.25},
	"claude-opus-4-8": {Input: 5, Output: 25, CacheRead: 0.50, CacheWrite: 6.25},
	"claude-opus-4-7": {Input: 5, Output: 25, CacheRead: 0.50, CacheWrite: 6.25},
	"claude-opus-4-6": {Input: 5, Output: 25, CacheRead: 0.50, CacheWrite: 6.25},
	"claude-opus-4-5": {Input: 5, Output: 25, CacheRead: 0.50, CacheWrite: 6.25},
	"claude-opus-4-1": {Input: 15, Output: 75, CacheRead: 1.50, CacheWrite: 18.75},
	"claude-opus-4-0": {Input: 15, Output: 75, CacheRead: 1.50, CacheWrite: 18.75},
	"claude-opus-4":   {Input: 15, Output: 75, CacheRead: 1.50, CacheWrite: 18.75}, // claude-opus-4-20250514
	// Claude Sonnet.
	"claude-sonnet-5-5": {Input: 2, Output: 10, CacheRead: 0.20, CacheWrite: 2.5},
	"claude-sonnet-5":   {Input: 2, Output: 10, CacheRead: 0.20, CacheWrite: 2.5},
	"claude-sonnet-4-6": {Input: 3, Output: 15, CacheRead: 0.30, CacheWrite: 3.75},
	"claude-sonnet-4-5": {Input: 3, Output: 15, CacheRead: 0.30, CacheWrite: 3.75},
	"claude-sonnet-4-0": {Input: 3, Output: 15, CacheRead: 0.30, CacheWrite: 3.75},
	"claude-sonnet-4":   {Input: 3, Output: 15, CacheRead: 0.30, CacheWrite: 3.75}, // claude-sonnet-4-20250514
	// Claude Haiku.
	"claude-haiku-5-5": {Input: 0.10, Output: 0.50, CacheRead: 0.01, CacheWrite: 0.125,
		LongAbove: 100_000, Long: &Price{Input: 0.50, Output: 2.50, CacheRead: 0.05, CacheWrite: 0.625}},
	"claude-haiku-4-5": {Input: 1, Output: 5, CacheRead: 0.10, CacheWrite: 1.25},

	// OpenAI models Codex runs (credits × $0.04, see above).
	"gpt-6-astra":              {Input: 10, Output: 50, CacheRead: 1, CacheWrite: 10},
	"gpt-6.1-sol":              {Input: 2, Output: 10, CacheRead: 0.10, CacheWrite: 2},
	"gpt-6-sol":                {Input: 2, Output: 10, CacheRead: 0.20, CacheWrite: 2},
	"gpt-6-luna":               {Input: 0.10, Output: 0.50, CacheRead: 0.01, CacheWrite: 0.10},
	"gpt-5.6-sol":              {Input: 4, Output: 20, CacheRead: 0.40, CacheWrite: 4},
	"gpt-5.6-terra":            {Input: 2, Output: 12, CacheRead: 0.20, CacheWrite: 2},
	"gpt-5.6-luna":             {Input: 0.20, Output: 1.20, CacheRead: 0.02, CacheWrite: 0.20},
	"gpt-daybreak-blue-latest": {Input: 4, Output: 20, CacheRead: 0.40, CacheWrite: 4},
	"gpt-daybreak-red-latest":  {Input: 12.5, Output: 75, CacheRead: 1.25, CacheWrite: 12.5},
}

// fastMultiplier is what fast mode costs relative to standard speed.
const fastMultiplier = 2

type entry struct {
	key   string
	price Price
	loose bool // config override: any id the key is a prefix of, at a separator
}

// Prices looks up per-model prices: built-ins plus config overrides. It
// implements transcript.Pricer.
type Prices struct {
	entries []entry // longest key first; overrides before built-ins of equal length
}

var _ transcript.Pricer = (*Prices)(nil)

// NewPrices builds the table with usage.prices overrides (nil: none).
func NewPrices(overrides map[string]config.ModelPrice) *Prices {
	p := &Prices{}
	for k, v := range overrides {
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "" {
			continue
		}
		p.entries = append(p.entries, entry{key: strings.TrimSuffix(k, "*"), loose: true, price: Price{
			Input: v.Input, Output: v.Output, CacheRead: v.CacheRead, CacheWrite: v.CacheWrite}})
	}
	for k, v := range builtin {
		p.entries = append(p.entries, entry{key: k, price: v})
	}
	sort.SliceStable(p.entries, func(i, j int) bool {
		a, b := p.entries[i], p.entries[j]
		if len(a.key) != len(b.key) {
			return len(a.key) > len(b.key)
		}
		if a.loose != b.loose {
			return a.loose
		}
		return a.key < b.key
	})
	return p
}

// Lookup finds the price for a model id as agents report it: a plain id
// ("claude-opus-5-5"), with a date ("claude-haiku-4-5-20251001"), a
// context tag ("claude-opus-4-8[1m]"), a Bedrock or Vertex spelling
// ("us.anthropic.claude-opus-5-5-v1:0", "claude-opus-4-5@20251101"). A
// built-in only matches its own model, never a newer one that starts the
// same way ("claude-opus-5" is not "claude-opus-5-7"); a config key also
// matches ids it is a prefix of at a "-", "." or "@" boundary.
func (p *Prices) Lookup(model string) (Price, bool) {
	id := normalizeModel(model)
	if id == "" {
		return Price{}, false
	}
	for _, e := range p.entries {
		if !strings.HasPrefix(id, e.key) {
			continue
		}
		rest := id[len(e.key):]
		if rest == "" || (e.loose && strings.ContainsRune("-.@:_[", rune(rest[0]))) || suffixOnly(rest) {
			return e.price, true
		}
	}
	return Price{}, false
}

// normalizeModel lowercases and strips provider prefixes and tags.
func normalizeModel(m string) string {
	m = strings.ToLower(strings.TrimSpace(m))
	if i := strings.IndexByte(m, '['); i >= 0 {
		m = m[:i] // claude-opus-4-8[1m]
	}
	// Bedrock: [region.]anthropic.<id>[-v1:0]; Vertex/others: publisher/<id>.
	if i := strings.LastIndex(m, "anthropic."); i >= 0 {
		m = m[i+len("anthropic."):]
	}
	if i := strings.LastIndexByte(m, '/'); i >= 0 {
		m = m[i+1:]
	}
	return m
}

// suffixOnly reports whether rest (what follows a matched model id) only
// dates or versions it: "-20251001", "@20251101", "-v1:0", ":0", "-latest".
func suffixOnly(rest string) bool {
	switch {
	case strings.HasPrefix(rest, "@"):
		return true
	case strings.HasPrefix(rest, ":"):
		return true
	case rest == "-latest":
		return true
	case strings.HasPrefix(rest, "-v") && len(rest) > 2 && isDigit(rest[2]):
		return true
	case len(rest) >= 9 && rest[0] == '-' && allDigits(rest[1:9]) && (len(rest) == 9 || suffixOnly(rest[9:])):
		return true // -YYYYMMDD
	}
	return false
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return s != ""
}

// Cost is the estimated cost of one response.
func (p *Prices) Cost(model string, t transcript.Tokens, fast bool) float64 {
	pr, ok := p.Lookup(model)
	if !ok {
		return 0
	}
	if pr.Long != nil && pr.LongAbove > 0 && t.Prompt() > pr.LongAbove {
		pr = *pr.Long
	}
	write5m := t.CacheWrite - t.CacheWrite1h
	c := float64(t.Input)*pr.Input + float64(t.Output)*pr.Output + float64(t.CacheRead)*pr.CacheRead +
		float64(write5m)*pr.CacheWrite + float64(t.CacheWrite1h)*2*pr.Input
	c /= 1e6
	if fast {
		c *= fastMultiplier
	}
	return c
}
