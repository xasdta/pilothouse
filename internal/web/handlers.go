package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
)

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	snap, err := s.d.Core.Snapshot(r.Context())
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	if snap.Sessions == nil {
		snap.Sessions = []api.Session{}
	}
	if snap.Requests == nil {
		snap.Requests = []api.Request{}
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	sess, err := s.d.Core.Session(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) handleSessionEvents(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	evs, err := s.d.Core.SessionEvents(r.Context(), r.PathValue("id"), limit)
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	if evs == nil {
		evs = []api.Event{}
	}
	writeJSON(w, http.StatusOK, evs)
}

func (s *Server) handleSeen(w http.ResponseWriter, r *http.Request) {
	if err := s.d.Core.MarkSeen(r.Context(), r.PathValue("id")); err != nil {
		s.writeCoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) {
	req, err := s.d.Core.Request(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, req)
}

func (s *Server) handlePresence(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.d.Core.Presence())
}

func (s *Server) handleSetPresence(w http.ResponseWriter, r *http.Request) {
	var b api.PresenceBody
	if !decodeBody(w, r, &b) {
		return
	}
	switch b.Mode {
	case api.PresenceAuto, api.PresenceForceHere, api.PresenceForceAway:
	default:
		writeErr(w, http.StatusBadRequest, "bad_request", "mode must be auto, here or away")
		return
	}
	if err := s.d.Core.SetPresenceMode(r.Context(), b.Mode); err != nil {
		s.writeCoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.d.Core.Presence())
}

// streamEvent reports whether an event goes to devices. Raw hook events
// are high-volume (every tool call) and only matter for the session
// timeline, which is fetched on demand.
func streamEvent(e api.Event) bool { return e.Type != api.EventHook }

// handleEvents streams the event log as SSE, like /v1/events?follow=1:
// `event: event` with `id: <seq>`, `event: control` {"resync_required":
// true} when the client must reload /api/snapshot, and a ping every 15 s.
// The ping is an `event: ping` with data (EventSource only surfaces events
// with data, so the app's watchdog can see it) preceded by a `: ping`
// comment line, which older clients that look for the comment still get.
// EventSource's automatic Last-Event-ID takes precedence over ?after.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			after = n
		}
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	d := deviceOf(r)
	key := s.addStream(d.ID, cancel)
	defer s.removeStream(d.ID, key)

	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{}) // long-lived: undo any server WriteTimeout
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	sub, err := s.d.Core.Subscribe(ctx, after)
	if err != nil {
		s.log.Warn("subscribe", "err", err)
		writeControl(w)
		_ = rc.Flush()
		return
	}
	defer sub.Close()
	if sub.Resync {
		writeControl(w)
		_ = rc.Flush()
		return
	}
	fmt.Fprint(w, "retry: 3000\n\n")
	if rc.Flush() != nil {
		return
	}
	ping := time.NewTicker(s.d.PingEvery)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\nevent: ping\ndata: {}\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		case ev, ok := <-sub.C:
			if !ok {
				// The core dropped us (we fell behind) or is shutting down.
				writeControl(w)
				_ = rc.Flush()
				return
			}
			if !streamEvent(ev) {
				continue
			}
			b, _ := json.Marshal(ev)
			if _, err := fmt.Fprintf(w, "id: %d\nevent: event\ndata: %s\n\n", ev.Seq, b); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}

func writeControl(w http.ResponseWriter) {
	b, _ := json.Marshal(api.StreamControl{ResyncRequired: true})
	fmt.Fprintf(w, "event: control\ndata: %s\n\n", b)
}

func (s *Server) addStream(deviceID string, cancel context.CancelFunc) *int {
	key := new(int)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streams[deviceID] == nil {
		s.streams[deviceID] = map[*int]context.CancelFunc{}
	}
	s.streams[deviceID][key] = cancel
	return key
}

func (s *Server) removeStream(deviceID string, key *int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.streams[deviceID], key)
	if len(s.streams[deviceID]) == 0 {
		delete(s.streams, deviceID)
	}
}
