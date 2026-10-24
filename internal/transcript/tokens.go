package transcript

import (
	"math"

	"github.com/xasdta/pilothouse/internal/api"
)

// Tokens is the token use of one model response (or a sum of them), in
// one shape for both agents: Input never includes cached tokens, and
// Output includes reasoning (Reasoning is the part of Output spent on it).
type Tokens struct {
	Input        int64
	Output       int64
	CacheRead    int64
	CacheWrite   int64 // every cache write, CacheWrite1h included
	CacheWrite1h int64 // Claude's 1-hour cache writes, priced higher
	Reasoning    int64
}

// Add accumulates o into t.
func (t *Tokens) Add(o Tokens) {
	t.Input += o.Input
	t.Output += o.Output
	t.CacheRead += o.CacheRead
	t.CacheWrite += o.CacheWrite
	t.CacheWrite1h += o.CacheWrite1h
	t.Reasoning += o.Reasoning
}

// Zero reports whether no tokens were counted.
func (t Tokens) Zero() bool { return t == Tokens{} }

// Prompt is the size of the prompt a response was generated from.
func (t Tokens) Prompt() int64 { return t.Input + t.CacheRead + t.CacheWrite }

// maxTokens is the larger of a and b in every field. A Claude response is
// logged once per content block, each block repeating the usage seen so
// far; the largest values are the response's own.
func maxTokens(a, b Tokens) Tokens {
	return Tokens{
		Input: max(a.Input, b.Input), Output: max(a.Output, b.Output),
		CacheRead: max(a.CacheRead, b.CacheRead), CacheWrite: max(a.CacheWrite, b.CacheWrite),
		CacheWrite1h: max(a.CacheWrite1h, b.CacheWrite1h), Reasoning: max(a.Reasoning, b.Reasoning),
	}
}

func subTokens(a, b Tokens) Tokens {
	return Tokens{
		Input: a.Input - b.Input, Output: a.Output - b.Output,
		CacheRead: a.CacheRead - b.CacheRead, CacheWrite: a.CacheWrite - b.CacheWrite,
		CacheWrite1h: a.CacheWrite1h - b.CacheWrite1h, Reasoning: a.Reasoning - b.Reasoning,
	}
}

// Pricer estimates what a response cost. fast is Claude's fast mode
// (usage.speed "fast"). A model it doesn't know costs 0.
type Pricer interface {
	Cost(model string, t Tokens, fast bool) float64
}

// NoPrices prices everything at 0.
type NoPrices struct{}

func (NoPrices) Cost(string, Tokens, bool) float64 { return 0 }

// apiUsage converts a total to the wire type.
func apiUsage(t Tokens, cost float64, model string, turns int) api.Usage {
	return api.Usage{
		InputTokens: t.Input, OutputTokens: t.Output, CacheReadTokens: t.CacheRead,
		CacheWriteTokens: t.CacheWrite, ReasoningTokens: t.Reasoning,
		CostUSD: roundCost(cost), Model: model, Turns: turns,
	}
}

// roundCost keeps an estimate to a millionth of a dollar, so sums of
// per-response costs don't show float noise.
func roundCost(c float64) float64 { return math.Round(c*1e6) / 1e6 }
