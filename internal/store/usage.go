package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
)

// UsageRow is one session's token use on one local day with one model
// (table usage_daily, migration 2).
type UsageRow struct {
	SessionID  string
	Day        string // YYYY-MM-DD, local time
	Agent      api.AgentKind
	Model      string
	Repo       string
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
	Reasoning  int64
	CostUSD    float64
	Turns      int
}

// UpsertUsage records usage rows. Transcripts only grow, so a row only
// ever grows too: each column keeps the larger of the stored and the new
// value. That makes a re-count from a partly read transcript (a huge file
// read from its tail after a restart) harmless.
func (tx *Tx) UpsertUsage(ctx context.Context, rows []UsageRow) error {
	for _, u := range rows {
		if u.SessionID == "" || u.Day == "" {
			continue
		}
		_, err := tx.q.ExecContext(ctx, `
INSERT INTO usage_daily(session_id, day, agent, model, repo, input, output, cache_read, cache_write,
	reasoning, cost_usd, turns, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(session_id, day, model) DO UPDATE SET
	agent = excluded.agent,
	repo = CASE WHEN excluded.repo != '' THEN excluded.repo ELSE usage_daily.repo END,
	input = MAX(usage_daily.input, excluded.input),
	output = MAX(usage_daily.output, excluded.output),
	cache_read = MAX(usage_daily.cache_read, excluded.cache_read),
	cache_write = MAX(usage_daily.cache_write, excluded.cache_write),
	reasoning = MAX(usage_daily.reasoning, excluded.reasoning),
	cost_usd = MAX(usage_daily.cost_usd, excluded.cost_usd),
	turns = MAX(usage_daily.turns, excluded.turns),
	updated_at = excluded.updated_at`,
			u.SessionID, u.Day, string(u.Agent), u.Model, u.Repo, u.Input, u.Output, u.CacheRead,
			u.CacheWrite, u.Reasoning, u.CostUSD, u.Turns, nanos(tx.now))
		if err != nil {
			return fmt.Errorf("upsert usage: %w", err)
		}
	}
	return nil
}

// UsageDays returns usage rows for days from..to inclusive (YYYY-MM-DD).
func (r Reader) UsageDays(ctx context.Context, from, to string) ([]UsageRow, error) {
	rows, err := r.q.QueryContext(ctx, `
SELECT session_id, day, agent, model, repo, input, output, cache_read, cache_write, reasoning, cost_usd, turns
FROM usage_daily WHERE day >= ? AND day <= ? ORDER BY day, session_id, model`, from, to)
	if err != nil {
		return nil, fmt.Errorf("list usage: %w", err)
	}
	defer rows.Close()
	out := []UsageRow{}
	for rows.Next() {
		var u UsageRow
		var agent string
		if err := rows.Scan(&u.SessionID, &u.Day, &agent, &u.Model, &u.Repo, &u.Input, &u.Output,
			&u.CacheRead, &u.CacheWrite, &u.Reasoning, &u.CostUSD, &u.Turns); err != nil {
			return nil, fmt.Errorf("list usage: %w", err)
		}
		u.Agent = api.AgentKind(agent)
		out = append(out, u)
	}
	return out, rows.Err()
}

// SpentOn is the estimated cost of every session on day (YYYY-MM-DD).
func (r Reader) SpentOn(ctx context.Context, day string) (float64, error) {
	var sum sql.NullFloat64
	if err := r.q.QueryRowContext(ctx, `SELECT SUM(cost_usd) FROM usage_daily WHERE day = ?`, day).Scan(&sum); err != nil {
		return 0, fmt.Errorf("sum usage: %w", err)
	}
	return sum.Float64, nil
}

// SessionEventsOfType returns a session's latest events of one type,
// newest first (the activity feed uses it for session.reply).
func (r Reader) SessionEventsOfType(ctx context.Context, sessionID string, t api.EventType, limit int) ([]api.Event, error) {
	return queryEvents(ctx, r.q,
		`SELECT seq, at, type, session_id, request_id, data FROM events WHERE session_id = ? AND type = ?
		 ORDER BY seq DESC LIMIT ?`, sessionID, string(t), limitOr(limit, 100))
}

// DecisionRow is the part of a policy.decision, request.opened or
// request.closed event that insights count.
type DecisionRow struct {
	At        time.Time
	Type      api.EventType
	SessionID string
	// policy.decision (policy.DecisionEvent)
	Outcome api.Behavior
	Rule    string
	Grant   string
	Summary string
	// request.opened / request.closed (api.Request)
	Status     api.RequestStatus
	Checkpoint api.Checkpoint
	By         string
	CreatedAt  time.Time
	ClosedAt   time.Time
}

// MaxDecisionRows bounds one DecisionRows query.
const MaxDecisionRows = 500_000

// DecisionRows returns the decision and request events logged in
// [from, to), oldest first. Fields are extracted in SQL, so request
// payloads (tool input can be large) are never decoded.
func (r Reader) DecisionRows(ctx context.Context, from, to time.Time) ([]DecisionRow, error) {
	rows, err := r.q.QueryContext(ctx, `
SELECT at, type, session_id,
	COALESCE(json_extract(data, '$.outcome'), ''), COALESCE(json_extract(data, '$.rule'), ''),
	COALESCE(json_extract(data, '$.grant'), ''), COALESCE(json_extract(data, '$.summary'), ''),
	COALESCE(json_extract(data, '$.status'), ''), COALESCE(json_extract(data, '$.checkpoint'), ''),
	COALESCE(json_extract(data, '$.decision.by'), ''), COALESCE(json_extract(data, '$.created_at'), ''),
	COALESCE(json_extract(data, '$.closed_at'), '')
FROM events
WHERE at >= ? AND at < ? AND type IN (?, ?, ?) AND json_valid(data)
ORDER BY seq LIMIT ?`,
		nanos(from), nanos(to), string(api.EventPolicyDecision), string(api.EventRequestOpened),
		string(api.EventRequestClosed), MaxDecisionRows)
	if err != nil {
		return nil, fmt.Errorf("list decisions: %w", err)
	}
	defer rows.Close()
	out := []DecisionRow{}
	for rows.Next() {
		var (
			d                                     DecisionRow
			at                                    int64
			typ, outcome, status, cp, created, cl string
		)
		if err := rows.Scan(&at, &typ, &d.SessionID, &outcome, &d.Rule, &d.Grant, &d.Summary, &status, &cp,
			&d.By, &created, &cl); err != nil {
			return nil, fmt.Errorf("list decisions: %w", err)
		}
		d.At = time.Unix(0, at).UTC()
		d.Type, d.Outcome, d.Status, d.Checkpoint = api.EventType(typ), api.Behavior(outcome), api.RequestStatus(status), api.Checkpoint(cp)
		d.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		d.ClosedAt, _ = time.Parse(time.RFC3339Nano, cl)
		out = append(out, d)
	}
	return out, rows.Err()
}
