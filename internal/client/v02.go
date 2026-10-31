package client

import (
	"context"
	"net/url"
	"strconv"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
)

// Local API added in 0.2 (docs/PLAN-0.2.md).

// Reply sends a follow-up prompt or an interrupt to a session.
func (c *Client) Reply(ctx context.Context, sessionID string, r api.Reply) (api.ReplyResult, error) {
	var res api.ReplyResult
	return res, c.do(ctx, "POST", "/v1/sessions/"+url.PathEscape(sessionID)+"/reply", r, &res)
}

// Activity reads a session's feed.
func (c *Client) Activity(ctx context.Context, sessionID string, q api.ActivityQuery) (api.Activity, error) {
	v := url.Values{}
	if q.Cursor != "" {
		v.Set("cursor", q.Cursor)
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	if !q.Since.IsZero() {
		v.Set("since", q.Since.UTC().Format(time.RFC3339))
	}
	if q.Peek {
		v.Set("peek", "1")
	}
	path := "/v1/sessions/" + url.PathEscape(sessionID) + "/activity"
	if len(v) > 0 {
		path += "?" + v.Encode()
	}
	var a api.Activity
	return a, c.do(ctx, "GET", path, nil, &a)
}

// Insights aggregates usage and decisions over the last q.Days days.
func (c *Client) Insights(ctx context.Context, q api.InsightsQuery) (api.Insights, error) {
	path := "/v1/insights"
	if q.Days > 0 {
		path += "?days=" + strconv.Itoa(q.Days)
	}
	var in api.Insights
	return in, c.do(ctx, "GET", path, nil, &in)
}

// Rules lists rules and presets.
func (c *Client) Rules(ctx context.Context) (api.RulesView, error) {
	var v api.RulesView
	return v, c.do(ctx, "GET", "/v1/rules", nil, &v)
}

// AddRule adds a managed rule.
func (c *Client) AddRule(ctx context.Context, spec api.RuleSpec) (api.RuleInfo, error) {
	var info api.RuleInfo
	return info, c.do(ctx, "POST", "/v1/rules", spec, &info)
}

// TestRule asks what the rules would decide for a tool call.
func (c *Client) TestRule(ctx context.Context, in api.RuleTestInput) (api.RuleTestResult, error) {
	var res api.RuleTestResult
	return res, c.do(ctx, "POST", "/v1/rules/test", in, &res)
}

// SetRuleEnabled turns a managed rule on or off.
func (c *Client) SetRuleEnabled(ctx context.Context, name string, enabled bool) error {
	op := "disable"
	if enabled {
		op = "enable"
	}
	return c.do(ctx, "POST", "/v1/rules/"+url.PathEscape(name)+"/"+op, nil, nil)
}

// RemoveRule deletes a managed rule.
func (c *Client) RemoveRule(ctx context.Context, name string) error {
	return c.do(ctx, "POST", "/v1/rules/"+url.PathEscape(name)+"/remove", nil, nil)
}

// SetPreset switches a preset on or off.
func (c *Client) SetPreset(ctx context.Context, name string, enabled bool) error {
	return c.do(ctx, "POST", "/v1/presets/"+url.PathEscape(name)+"?enabled="+strconv.FormatBool(enabled), nil, nil)
}

// Suggestions proposes one-tap rules for a request.
func (c *Client) Suggestions(ctx context.Context, requestID string) ([]api.RuleSuggestion, error) {
	var s []api.RuleSuggestion
	return s, c.do(ctx, "GET", "/v1/requests/"+url.PathEscape(requestID)+"/suggestions", nil, &s)
}

// Repos lists repositories tasks may start in.
func (c *Client) Repos(ctx context.Context) ([]api.RepoInfo, error) {
	var r []api.RepoInfo
	return r, c.do(ctx, "GET", "/v1/repos", nil, &r)
}

// StartTask starts a task through the daemon.
func (c *Client) StartTask(ctx context.Context, t api.TaskStart) (api.Task, error) {
	var out api.Task
	return out, c.do(ctx, "POST", "/v1/tasks/start", t, &out)
}

// TaskDiff is what a task changed.
func (c *Client) TaskDiff(ctx context.Context, taskID string) (api.TaskDiff, error) {
	var d api.TaskDiff
	return d, c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(taskID)+"/diff", nil, &d)
}

// FinishTask ships or discards a task.
func (c *Client) FinishTask(ctx context.Context, taskID string, f api.TaskFinish) (api.Task, error) {
	var t api.Task
	return t, c.do(ctx, "POST", "/v1/tasks/"+url.PathEscape(taskID)+"/finish", f, &t)
}
