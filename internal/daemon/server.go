package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/client"
	"github.com/xasdta/pilothouse/internal/core"
	"github.com/xasdta/pilothouse/internal/store"
	"github.com/xasdta/pilothouse/internal/version"
)

const (
	maxBody     = 1 << 20  // JSON bodies on the local API
	maxHookBody = 72 << 20 // hook envelopes carry up to 64 MB of stdin, verbatim, plus process context
	pingEvery   = 15 * time.Second
)

// connKey stores the request's net.Conn in its context for the peer guard.
type connKey struct{}

// routes builds the Unix-socket API (BUILD-SPEC §4). Routes that change
// who can approve what are wrapped in requireHuman (invariant 5); the web
// package wraps its local routes (pair, login-link, devices) with the same
// guard through web.Deps.LocalGuard.
func (d *Daemon) routes() http.Handler {
	mux := http.NewServeMux()
	guarded := func(h http.HandlerFunc) http.Handler { return d.requireHuman(h) }
	mux.HandleFunc("GET /v1/health", d.handleHealth)
	mux.Handle("POST /v1/shutdown", guarded(d.handleShutdown))
	mux.HandleFunc("GET /v1/snapshot", d.handleSnapshot)
	mux.HandleFunc("GET /v1/events", d.handleEvents)
	mux.HandleFunc("POST /v1/hook", d.handleHook)

	mux.HandleFunc("GET /v1/sessions", d.handleSessions)
	mux.HandleFunc("POST /v1/sessions/wrapped", d.handleRegisterWrapped)
	mux.HandleFunc("GET /v1/sessions/{id}", d.handleSession)
	mux.HandleFunc("GET /v1/sessions/{id}/events", d.handleSessionEvents)
	mux.HandleFunc("POST /v1/sessions/{id}/seen", d.handleSeen)
	mux.HandleFunc("GET /v1/sessions/{id}/explain", d.handleExplain)
	mux.HandleFunc("POST /v1/sessions/{id}/osc", d.handleOSC)
	mux.HandleFunc("POST /v1/sessions/{id}/exit", d.handleExit)

	mux.HandleFunc("GET /v1/requests", d.handleRequests)
	mux.HandleFunc("GET /v1/requests/{id}", d.handleRequest)
	mux.Handle("POST /v1/requests/{id}/decide", guarded(d.handleDecide))

	mux.HandleFunc("GET /v1/presence", d.handleGetPresence)
	mux.Handle("PUT /v1/presence", guarded(d.handlePutPresence))

	mux.HandleFunc("GET /v1/tasks", d.handleTasks)
	mux.HandleFunc("GET /v1/tasks/{id}", d.handleTask)
	mux.HandleFunc("POST /v1/tasks", d.handlePutTask)

	mux.HandleFunc("GET /v1/policy/status", d.handlePolicyStatus)
	mux.Handle("POST /v1/policy/reload", guarded(d.handlePolicyReload))
	mux.HandleFunc("POST /v1/notify/test", d.handleNotifyTest)

	d.routesV02(mux, guarded)

	if d.web != nil {
		d.web.RegisterLocal(mux) // guarded by web.Deps.LocalGuard = d.requireHuman
	}
	return mux
}

// requireHuman rejects the call with 403 agent_caller unless the peer
// process passes the PeerGuard (no agent among it and its ancestors).
func (d *Daemon) requireHuman(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := r.Context().Value(connKey{}).(net.Conn)
		err := errors.New("cannot identify the calling process")
		if c != nil {
			err = d.guard(c)
		}
		if err != nil {
			d.log.Warn("rejected admin call", "method", r.Method, "path", r.URL.Path, "reason", err)
			writeError(w, http.StatusForbidden, "agent_caller", err.Error())
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- handlers ----

func (d *Daemon) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, api.Health{Version: version.Version, Protocol: api.ProtocolVersion,
		PID: os.Getpid(), Machine: d.machine, WebURL: d.webURL})
}

func (d *Daemon) handleShutdown(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
	d.log.Info("shutdown requested over the API")
	d.stop()
}

func (d *Daemon) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	snap, err := d.Snapshot(r.Context())
	reply(w, snap, err)
}

func (d *Daemon) handleEvents(w http.ResponseWriter, r *http.Request) {
	after, err := queryInt(r, "after", 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	if r.URL.Query().Get("follow") != "1" {
		evs, err := d.db.Events(r.Context(), int64(after), 1000)
		reply(w, evs, err)
		return
	}
	d.streamEvents(w, r, int64(after))
}

// streamEvents serves the SSE tail of the event log.
func (d *Daemon) streamEvents(w http.ResponseWriter, r *http.Request, after int64) {
	sub, err := d.Subscribe(r.Context(), after)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer sub.Close()
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if sub.Resync {
		writeControl(w)
		_ = rc.Flush()
		return
	}
	_ = rc.Flush()

	ping := time.NewTicker(pingEvery)
	defer ping.Stop()
	for {
		select {
		case ev, ok := <-sub.C:
			if !ok {
				// Lagged, or the daemon is stopping: either way the client
				// reloads a snapshot.
				if r.Context().Err() == nil {
					writeControl(w)
					_ = rc.Flush()
				}
				return
			}
			if err := writeEvent(w, ev); err != nil {
				return
			}
			if len(sub.C) == 0 { // batch flushes while events are queued
				if rc.Flush() != nil {
					return
				}
			}
		case <-ping.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

func writeEvent(w io.Writer, ev api.Event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: event\nid: %d\ndata: %s\n\n", ev.Seq, b)
	return err
}

func writeControl(w io.Writer) {
	b, _ := json.Marshal(api.StreamControl{ResyncRequired: true})
	_, _ = fmt.Fprintf(w, "event: control\ndata: %s\n\n", b)
}

func (d *Daemon) handleHook(w http.ResponseWriter, r *http.Request) {
	var env adapters.HookEnvelope
	if err := decodeJSON(w, r, &env, maxHookBody); err != nil {
		writeErr(w, err)
		return
	}
	if env.ReceivedAt.IsZero() {
		env.ReceivedAt = d.now()
	}
	writeJSON(w, http.StatusOK, d.Hook(r.Context(), env))
}

func (d *Daemon) handleSessions(w http.ResponseWriter, r *http.Request) {
	ss, err := d.db.Sessions(r.Context(), d.now().Add(-snapshotExitedWindow))
	reply(w, ss, err)
}

func (d *Daemon) handleSession(w http.ResponseWriter, r *http.Request) {
	s, err := d.Session(r.Context(), r.PathValue("id"))
	reply(w, s, err)
}

func (d *Daemon) handleSessionEvents(w http.ResponseWriter, r *http.Request) {
	limit, err := queryInt(r, "limit", 100)
	if err != nil {
		writeErr(w, err)
		return
	}
	evs, err := d.SessionEvents(r.Context(), r.PathValue("id"), limit)
	reply(w, evs, err)
}

func (d *Daemon) handleSeen(w http.ResponseWriter, r *http.Request) {
	replyNoContent(w, d.MarkSeen(r.Context(), r.PathValue("id")))
}

func (d *Daemon) handleExplain(w http.ResponseWriter, r *http.Request) {
	e, err := d.Explain(r.Context(), r.PathValue("id"))
	reply(w, e, err)
}

func (d *Daemon) handleRegisterWrapped(w http.ResponseWriter, r *http.Request) {
	var b api.WrappedSessionBody
	if err := decodeJSON(w, r, &b, maxBody); err != nil {
		writeErr(w, err)
		return
	}
	s, err := d.RegisterWrapped(r.Context(), b)
	reply(w, s, err)
}

func (d *Daemon) handleOSC(w http.ResponseWriter, r *http.Request) {
	var b api.OSCBody
	if err := decodeJSON(w, r, &b, maxBody); err != nil {
		writeErr(w, err)
		return
	}
	replyNoContent(w, d.ReportOSC(r.Context(), r.PathValue("id"), b))
}

func (d *Daemon) handleExit(w http.ResponseWriter, r *http.Request) {
	var b api.ExitBody
	if err := decodeJSON(w, r, &b, maxBody); err != nil {
		writeErr(w, err)
		return
	}
	replyNoContent(w, d.ReportExit(r.Context(), r.PathValue("id"), b.ExitCode))
}

func (d *Daemon) handleRequests(w http.ResponseWriter, r *http.Request) {
	f := store.RequestFilter{Limit: 200}
	switch st := r.URL.Query().Get("status"); st {
	case "":
	case "pending":
		f.Statuses = store.PendingStatuses
	default:
		f.Statuses = []api.RequestStatus{api.RequestStatus(st)}
	}
	reqs, err := d.db.Requests(r.Context(), f)
	reply(w, reqs, err)
}

func (d *Daemon) handleRequest(w http.ResponseWriter, r *http.Request) {
	req, err := d.Request(r.Context(), r.PathValue("id"))
	reply(w, req, err)
}

func (d *Daemon) handleDecide(w http.ResponseWriter, r *http.Request) {
	var b api.DecideBody
	if err := decodeJSON(w, r, &b, maxBody); err != nil {
		writeErr(w, err)
		return
	}
	by := "cli"
	if r.Header.Get(client.ClientHeader) == "tui" {
		by = "tui"
	}
	req, err := d.Decide(r.Context(), api.Decision{RequestID: r.PathValue("id"), Behavior: b.Behavior,
		Scope: b.Scope, Message: b.Message, Answers: b.Answers, By: by})
	reply(w, req, err)
}

func (d *Daemon) handleGetPresence(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, d.Presence())
}

func (d *Daemon) handlePutPresence(w http.ResponseWriter, r *http.Request) {
	var b api.PresenceBody
	if err := decodeJSON(w, r, &b, maxBody); err != nil {
		writeErr(w, err)
		return
	}
	if err := d.SetPresenceMode(r.Context(), b.Mode); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d.Presence())
}

func (d *Daemon) handleTasks(w http.ResponseWriter, r *http.Request) {
	ts, err := d.Tasks(r.Context()) // taskrun.go: tasks.json and the database, with live status
	reply(w, ts, err)
}

func (d *Daemon) handleTask(w http.ResponseWriter, r *http.Request) {
	t, err := d.Task(r.Context(), r.PathValue("id"))
	reply(w, t, err)
}

func (d *Daemon) handlePutTask(w http.ResponseWriter, r *http.Request) {
	var t api.Task
	if err := decodeJSON(w, r, &t, maxBody); err != nil {
		writeErr(w, err)
		return
	}
	out, err := d.PutTask(r.Context(), t)
	reply(w, out, err)
}

func (d *Daemon) handlePolicyStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, d.PolicyStatus())
}

func (d *Daemon) handlePolicyReload(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, d.ReloadPolicy())
}

func (d *Daemon) handleNotifyTest(w http.ResponseWriter, _ *http.Request) {
	// Test notifications go to every configured channel; notify supplies
	// the standard wording.
	d.notify(core.Notification{Kind: core.NotifyTest, Remote: true})
	w.WriteHeader(http.StatusNoContent)
}

// ---- helpers ----

func decodeJSON(w http.ResponseWriter, r *http.Request, v any, limit int64) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	if err := dec.Decode(v); err != nil {
		return badRequest("invalid JSON body: %v", err)
	}
	return nil
}

func queryInt(r *http.Request, key string, def int) (int, error) {
	s := r.URL.Query().Get(key)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, badRequest("%s must be a non-negative integer", key)
	}
	return n, nil
}

func reply(w http.ResponseWriter, v any, err error) {
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func replyNoContent(w http.ResponseWriter, err error) {
	if err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, api.Error{Error: msg, Code: code})
}

// writeErr maps an error to the API's status codes.
func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errBadRequest), errors.Is(err, store.ErrAmbiguous):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, core.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, core.ErrAlreadyClosed):
		writeError(w, http.StatusConflict, "already_decided", "this request was already answered or handed back")
	case errors.Is(err, core.ErrForbidden):
		writeError(w, http.StatusForbidden, "agent_caller", err.Error())
	case errors.Is(err, core.ErrUnsupported):
		writeError(w, http.StatusNotImplemented, "unsupported", strings.TrimPrefix(err.Error(), core.ErrUnsupported.Error()+": "))
	case errors.Is(err, core.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid", strings.TrimPrefix(err.Error(), core.ErrInvalid.Error()+": "))
	case errors.Is(err, context.Canceled):
		writeError(w, http.StatusServiceUnavailable, "internal", "request cancelled")
	default:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}
