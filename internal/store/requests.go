package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
)

// PendingStatuses are the statuses a request can still be answered from
// (BUILD-SPEC invariant 4).
var PendingStatuses = []api.RequestStatus{api.RequestOpen, api.RequestSteppedAside}

// RequestFilter selects requests. Zero fields don't filter.
type RequestFilter struct {
	SessionID  string
	Statuses   []api.RequestStatus
	Checkpoint api.Checkpoint
	Closed     bool // only requests no longer pending (decided, expired, answered locally)
	Limit      int  // 0 = 500
}

// Request returns the request with exactly this id.
func (r Reader) Request(ctx context.Context, id string) (api.Request, error) {
	return scanRequest(r.q.QueryRowContext(ctx, `SELECT data FROM requests WHERE id = ?`, id))
}

// ResolveRequestID expands an id or unique prefix.
func (r Reader) ResolveRequestID(ctx context.Context, prefix string) (string, error) {
	return r.resolveID(ctx, "requests", "r_", prefix)
}

// Requests lists requests matching f, newest first.
func (r Reader) Requests(ctx context.Context, f RequestFilter) ([]api.Request, error) {
	var where []string
	var args []any
	if f.SessionID != "" {
		where = append(where, "session_id = ?")
		args = append(args, f.SessionID)
	}
	if len(f.Statuses) > 0 {
		where = append(where, "status IN ("+placeholders(len(f.Statuses))+")")
		for _, s := range f.Statuses {
			args = append(args, string(s))
		}
	}
	if f.Closed {
		where = append(where, "status NOT IN ("+placeholders(len(PendingStatuses))+")")
		for _, s := range PendingStatuses {
			args = append(args, string(s))
		}
	}
	if f.Checkpoint != "" {
		where = append(where, "checkpoint = ?")
		args = append(args, string(f.Checkpoint))
	}
	q := `SELECT data FROM requests`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 500
	}
	q += " ORDER BY created_at DESC, id DESC LIMIT ?"
	args = append(args, limit)
	return queryRequests(ctx, r.q, q, args...)
}

// MatchPending finds the pending request (open — held for devices — or
// stepped aside) a post_tool hook completes: the one with the same
// tool_use_id, else the newest with the same tool and summary. A held
// request matches too: the agent's own dialog shows while devices are
// asked, and a post_tool means the person answered it there.
func (r Reader) MatchPending(ctx context.Context, sessionID, toolUseID, tool, summary string) (api.Request, error) {
	in := placeholders(len(PendingStatuses))
	args := func(more ...any) []any {
		a := []any{sessionID}
		for _, s := range PendingStatuses {
			a = append(a, string(s))
		}
		return append(a, more...)
	}
	if toolUseID != "" {
		req, err := scanRequest(r.q.QueryRowContext(ctx,
			`SELECT data FROM requests WHERE session_id = ? AND status IN (`+in+`) AND tool_use_id = ?
			 ORDER BY created_at DESC LIMIT 1`, args(toolUseID)...))
		if !errors.Is(err, ErrNotFound) {
			return req, err
		}
	}
	return scanRequest(r.q.QueryRowContext(ctx,
		`SELECT data FROM requests WHERE session_id = ? AND status IN (`+in+`) AND tool = ? AND summary = ?
		 ORDER BY created_at DESC LIMIT 1`, args(tool, summary)...))
}

// InsertRequest stores a new request. toolUseID is kept only for matching
// the post_tool hook that completes it.
func (tx *Tx) InsertRequest(ctx context.Context, req api.Request, toolUseID string) error {
	b, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("insert request %s: %w", req.ID, err)
	}
	_, err = tx.q.ExecContext(ctx, `
INSERT INTO requests(id, session_id, checkpoint, status, tool, summary, tool_use_id, created_at, closed_at, data)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		req.ID, req.SessionID, string(req.Checkpoint), string(req.Status), req.Tool, req.Summary,
		toolUseID, nanos(req.CreatedAt), nullNanos(req.ClosedAt), string(b))
	if err != nil {
		return fmt.Errorf("insert request %s: %w", req.ID, err)
	}
	return nil
}

// CloseRequest moves a pending request (open or stepped aside) to status
// `to`. It is a compare-and-set: if another closer got there first it
// returns ErrAlreadyClosed and changes nothing.
func (tx *Tx) CloseRequest(ctx context.Context, id string, to api.RequestStatus, d *api.Decision, at time.Time) (api.Request, error) {
	return tx.TransitionRequest(ctx, id, PendingStatuses, to, d, at)
}

// TransitionRequest moves a request from one of `from` to `to`, setting
// ClosedAt (and the decision, if any). The UPDATE's WHERE clause is the
// compare-and-set that makes the first answer win.
func (tx *Tx) TransitionRequest(ctx context.Context, id string, from []api.RequestStatus, to api.RequestStatus, d *api.Decision, at time.Time) (api.Request, error) {
	req, err := tx.Request(ctx, id)
	if err != nil {
		return api.Request{}, err
	}
	req.Status = to
	if d != nil {
		dd := *d
		req.Decision = &dd
	}
	closed := at
	req.ClosedAt = &closed
	b, err := json.Marshal(req)
	if err != nil {
		return api.Request{}, fmt.Errorf("close request %s: %w", id, err)
	}
	args := []any{string(to), nanos(at), string(b), id}
	for _, s := range from {
		args = append(args, string(s))
	}
	res, err := tx.q.ExecContext(ctx,
		`UPDATE requests SET status = ?, closed_at = ?, data = ? WHERE id = ? AND status IN (`+placeholders(len(from))+`)`,
		args...)
	if err != nil {
		return api.Request{}, fmt.Errorf("close request %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return api.Request{}, fmt.Errorf("close request %s: %w", id, err)
	}
	if n == 0 {
		return api.Request{}, ErrAlreadyClosed
	}
	return req, nil
}

func scanRequest(row *sql.Row) (api.Request, error) {
	var data string
	if err := row.Scan(&data); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return api.Request{}, ErrNotFound
		}
		return api.Request{}, fmt.Errorf("read request: %w", err)
	}
	var req api.Request
	if err := json.Unmarshal([]byte(data), &req); err != nil {
		return api.Request{}, fmt.Errorf("decode request: %w", err)
	}
	return req, nil
}

func queryRequests(ctx context.Context, q querier, query string, args ...any) ([]api.Request, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list requests: %w", err)
	}
	defer rows.Close()
	out := []api.Request{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("list requests: %w", err)
		}
		var req api.Request
		if err := json.Unmarshal([]byte(data), &req); err != nil {
			return nil, fmt.Errorf("decode request: %w", err)
		}
		out = append(out, req)
	}
	return out, rows.Err()
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?, ", n-1) + "?"
}
