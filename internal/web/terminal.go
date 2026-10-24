package web

import "net/http"

// Live terminal for wrapped sessions (F3, workstream B). A device mints a
// short-lived, session-bound input token with one signed ActionTerminal
// (so keystrokes need no per-key signature), streams output over SSE and
// posts input with the token.

func (s *Server) handleTerminalToken(w http.ResponseWriter, r *http.Request) {
	writeUnsupported(w, "live terminal is not available yet")
}

func (s *Server) handleTerminalStream(w http.ResponseWriter, r *http.Request) {
	writeUnsupported(w, "live terminal is not available yet")
}

func (s *Server) handleTerminalInput(w http.ResponseWriter, r *http.Request) {
	writeUnsupported(w, "live terminal is not available yet")
}
