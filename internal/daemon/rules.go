package daemon

import (
	"context"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
)

// Rules from the inbox (F5; workstream D).

var _ core.RuleEditor = (*Daemon)(nil)

// Rules lists every loaded rule and the presets.
func (d *Daemon) Rules(ctx context.Context) (api.RulesView, error) {
	return api.RulesView{}, core.ErrUnsupported
}

// SuggestRules proposes one-tap rules for a request.
func (d *Daemon) SuggestRules(ctx context.Context, requestID string) ([]api.RuleSuggestion, error) {
	return nil, core.ErrUnsupported
}

// AddRule adds a managed rule.
func (d *Daemon) AddRule(ctx context.Context, spec api.RuleSpec, by string) (api.RuleInfo, error) {
	return api.RuleInfo{}, core.ErrUnsupported
}

// SetRuleEnabled turns a managed rule on or off.
func (d *Daemon) SetRuleEnabled(ctx context.Context, name string, enabled bool, by string) error {
	return core.ErrUnsupported
}

// RemoveRule deletes a managed rule.
func (d *Daemon) RemoveRule(ctx context.Context, name string, by string) error {
	return core.ErrUnsupported
}

// SetPreset switches a preset on or off.
func (d *Daemon) SetPreset(ctx context.Context, name string, enabled bool, by string) error {
	return core.ErrUnsupported
}

// TestRule evaluates a hypothetical tool call against the rules.
func (d *Daemon) TestRule(ctx context.Context, in api.RuleTestInput) (api.RuleTestResult, error) {
	return api.RuleTestResult{}, core.ErrUnsupported
}
