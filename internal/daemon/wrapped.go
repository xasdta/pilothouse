package daemon

import (
	"context"
	"errors"
	"strconv"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/state"
	"github.com/xasdta/pilothouse/internal/store"
)

// RegisterWrapped records a session started by `pilothouse run`. It is
// idempotent on SessionID: holders re-register after a daemon restart, and
// a hook may have created the session first.
func (d *Daemon) RegisterWrapped(ctx context.Context, b api.WrappedSessionBody) (api.Session, error) {
	if !validSessionID(b.SessionID) {
		return api.Session{}, badRequest("invalid session id %q", b.SessionID)
	}
	if b.Agent == "" {
		b.Agent = api.AgentGeneric
	}
	d.learnHolder(ctx, b.SessionID) // terminal.go: what the holder supports, for the session's caps
	root, branch := d.git.lookup(ctx, b.Cwd)
	var out api.Session
	err := d.db.Write(ctx, func(tx *store.Tx) error {
		s, err := tx.Session(ctx, b.SessionID)
		var before *api.Session
		switch {
		case errors.Is(err, store.ErrNotFound):
			now := tx.Now()
			s = api.Session{ID: b.SessionID, Agent: b.Agent, Machine: d.machine, Cwd: b.Cwd,
				RepoRoot: root, Branch: branch, State: api.StateStarting, StartedAt: now, UpdatedAt: now}
		case err != nil:
			return err
		default:
			cp := s
			before = &cp
		}
		s.Mode = api.ModeWrapped
		if s.Agent == "" || s.Agent == api.AgentGeneric {
			s.Agent = b.Agent
		}
		s.Title = firstNonEmpty(b.Title, s.Title, titleFor(root, b.Cwd, b.Agent))
		if s.Cwd == "" {
			s.Cwd, s.RepoRoot, s.Branch = b.Cwd, root, branch
		}
		if b.PID > 0 {
			s.PID = b.PID
			if s.ExitedAt != nil && d.alive(b.PID) {
				s.ExitedAt, s.ExitCode = nil, nil // marked exited while we weren't watching
			}
		}
		s.HolderSock = firstNonEmpty(b.HolderSock, s.HolderSock)
		s.TaskID = firstNonEmpty(b.TaskID, s.TaskID)
		if err := linkTask(ctx, tx, s.TaskID, s.ID); err != nil {
			return err
		}
		out, err = d.saveSession(ctx, tx, before, s)
		return err
	})
	return out, err
}

// linkTask points a task at the session running it.
func linkTask(ctx context.Context, tx *store.Tx, taskID, sessionID string) error {
	if taskID == "" {
		return nil
	}
	t, err := tx.Task(ctx, taskID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil || t.SessionID == sessionID {
		return err
	}
	t.SessionID, t.UpdatedAt = sessionID, tx.Now()
	if err := tx.PutTask(ctx, t); err != nil {
		return err
	}
	_, err = tx.Emit(ctx, api.EventTaskUpdated, sessionID, "", t)
	return err
}

// ReportOSC records an OSC 7501 status a holder saw in the agent's output
// (layer L1).
func (d *Daemon) ReportOSC(ctx context.Context, sessionID string, b api.OSCBody) error {
	st, kind, ok := state.FromOSC(b.State, b.Kind)
	if !ok {
		return badRequest("unknown OSC state %q", b.State)
	}
	id, err := d.db.ResolveSessionID(ctx, sessionID)
	if err != nil {
		return err
	}
	detail := "Terminal reported " + b.State
	if b.App != "" {
		detail = b.App + " reported " + b.State
	}
	return d.db.Write(ctx, func(tx *store.Tx) error {
		s, err := tx.Session(ctx, id)
		if err != nil {
			return err
		}
		if err := tx.AddEvidence(ctx, id, api.StateEvidence{At: tx.Now(), Layer: api.LayerOSC,
			State: st, Kind: kind, Detail: detail}); err != nil {
			return err
		}
		_, err = d.saveSession(ctx, tx, &s, s)
		return err
	})
}

// ReportExit records a wrapped session's exit, as reported by its holder.
func (d *Daemon) ReportExit(ctx context.Context, sessionID string, code int) error {
	id, err := d.db.ResolveSessionID(ctx, sessionID)
	if err != nil {
		return err
	}
	return d.markExited(ctx, id, "Exited with code "+strconv.Itoa(code), &code)
}

// ---- tasks ----

// PutTask inserts or updates a task (POST /v1/tasks upserts).
func (d *Daemon) PutTask(ctx context.Context, t api.Task) (api.Task, error) {
	if t.ID == "" {
		t.ID = newID("t_")
	}
	if t.Status == "" {
		t.Status = api.TaskRunning
	}
	err := d.db.Write(ctx, func(tx *store.Tx) error {
		old, err := tx.Task(ctx, t.ID)
		switch {
		case err == nil:
			if t.CreatedAt.IsZero() {
				t.CreatedAt = old.CreatedAt
			}
		case !errors.Is(err, store.ErrNotFound):
			return err
		}
		if t.CreatedAt.IsZero() {
			t.CreatedAt = tx.Now()
		}
		t.UpdatedAt = tx.Now()
		if err := tx.PutTask(ctx, t); err != nil {
			return err
		}
		_, err = tx.Emit(ctx, api.EventTaskUpdated, t.SessionID, "", t)
		return err
	})
	return t, err
}

// Task returns a task by id or unique prefix.
func (d *Daemon) Task(ctx context.Context, id string) (api.Task, error) {
	full, err := d.db.ResolveTaskID(ctx, id)
	if err != nil {
		return api.Task{}, err
	}
	return d.db.Task(ctx, full)
}
