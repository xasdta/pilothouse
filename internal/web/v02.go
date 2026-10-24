package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
)

// Device API added in 0.2 (docs/PLAN-0.2.md). Handlers here are thin:
// verify, call an optional core capability, write JSON. The capabilities
// themselves live in the daemon.

// clientRoutesV02 are app-shell routes for the 0.2 views.
var clientRoutesV02 = []string{
	"GET /rules", "GET /insights", "GET /tasks", "GET /t/{id}", "GET /new", "GET /s/{id}/terminal",
}

func (s *Server) routesV02(mux *http.ServeMux, a func(http.HandlerFunc) http.HandlerFunc) {
	for _, p := range clientRoutesV02 {
		mux.HandleFunc(p, s.serveIndex)
	}
	// Third-party code the app loads on demand (xterm.js for the terminal).
	mux.HandleFunc("GET /vendor/", s.serveStatic)
	// F1: answers, plans (and every decision from 0.2 clients).
	mux.HandleFunc("POST /api/requests/{id}/respond", a(csrf(s.handleRespond)))
	// F2/F3: replies, activity, terminal.
	mux.HandleFunc("POST /api/sessions/{id}/reply", a(csrf(s.handleReply)))
	mux.HandleFunc("GET /api/sessions/{id}/activity", a(s.handleActivity))
	mux.HandleFunc("POST /api/sessions/{id}/terminal/token", a(csrf(s.handleTerminalToken)))
	mux.HandleFunc("GET /api/sessions/{id}/terminal", a(s.handleTerminalStream))
	mux.HandleFunc("POST /api/sessions/{id}/terminal/input", a(csrf(s.handleTerminalInput)))
	// F6.
	mux.HandleFunc("GET /api/insights", a(s.handleInsights))
	// F5.
	mux.HandleFunc("GET /api/rules", a(s.handleRules))
	mux.HandleFunc("POST /api/rules", a(csrf(s.handleAddRule)))
	mux.HandleFunc("POST /api/rules/test", a(csrf(s.handleTestRule)))
	mux.HandleFunc("POST /api/rules/{name}/enable", a(csrf(s.handleRuleEnable)))
	mux.HandleFunc("POST /api/rules/{name}/disable", a(csrf(s.handleRuleDisable)))
	mux.HandleFunc("POST /api/rules/{name}/remove", a(csrf(s.handleRemoveRule)))
	mux.HandleFunc("POST /api/presets/{name}", a(csrf(s.handlePreset)))
	mux.HandleFunc("GET /api/requests/{id}/suggestions", a(s.handleSuggestions))
	// F7.
	mux.HandleFunc("GET /api/repos", a(s.handleRepos))
	mux.HandleFunc("GET /api/tasks", a(s.handleTasks))
	mux.HandleFunc("POST /api/tasks", a(csrf(s.handleStartTask)))
	mux.HandleFunc("GET /api/tasks/{id}/diff", a(s.handleTaskDiff))
	mux.HandleFunc("POST /api/tasks/{id}/finish", a(csrf(s.handleFinishTask)))
}

func by(r *http.Request) string { return "device:" + deviceOf(r).ID }

// ---------- F1: respond ----------

// respondBody is the signed payload of POST /api/requests/{id}/respond.
type respondBody struct {
	Behavior api.Behavior      `json:"behavior"`
	Scope    api.Scope         `json:"scope,omitempty"`
	Message  string            `json:"message,omitempty"`
	Answers  map[string]string `json:"answers,omitempty"`
}

const (
	maxAnswers     = 8
	maxAnswerBytes = 2000
)

func (s *Server) handleRespond(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var b respondBody
	if !s.readSigned(w, r, ActionDecide, id, &b) {
		return
	}
	if b.Scope == "" {
		b.Scope = api.ScopeOnce
	}
	switch {
	case b.Behavior != api.BehaviorAllow && b.Behavior != api.BehaviorDeny:
		writeErr(w, http.StatusBadRequest, "bad_request", "behavior must be allow or deny")
		return
	case b.Scope != api.ScopeOnce && b.Scope != api.ScopeSession:
		writeErr(w, http.StatusBadRequest, "bad_request", "scope must be once or session")
		return
	case b.Scope == api.ScopeSession && b.Behavior != api.BehaviorAllow:
		writeErr(w, http.StatusBadRequest, "bad_request", "only allow can apply to the whole session")
		return
	case len(b.Message) > maxMessage:
		writeErr(w, http.StatusBadRequest, "bad_request", "message is too long")
		return
	case len(b.Answers) > maxAnswers:
		writeErr(w, http.StatusBadRequest, "bad_request", "too many answers")
		return
	case len(b.Answers) > 0 && b.Behavior != api.BehaviorAllow:
		writeErr(w, http.StatusBadRequest, "bad_request", "answers go with allow")
		return
	}
	for k, v := range b.Answers {
		if k == "" || len(k) > maxAnswerBytes || len(v) > maxAnswerBytes || !utf8.ValidString(k) || !utf8.ValidString(v) {
			writeErr(w, http.StatusBadRequest, "bad_request", "invalid answer")
			return
		}
	}
	req, err := s.d.Core.Decide(r.Context(), api.Decision{
		RequestID: id,
		Behavior:  b.Behavior,
		Scope:     b.Scope,
		Message:   strings.TrimSpace(b.Message),
		Answers:   b.Answers,
		By:        by(r),
		At:        s.now().UTC(),
	})
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	s.log.Info("request answered", "request", id, "behavior", b.Behavior, "answers", len(b.Answers), "by", by(r))
	writeJSON(w, http.StatusOK, req)
}

// ---------- F2/F3: reply, activity ----------

// MaxReplyBytes bounds a follow-up prompt.
const MaxReplyBytes = 16 << 10

func (s *Server) handleReply(w http.ResponseWriter, r *http.Request) {
	rep, ok := s.d.Core.(core.Replier)
	if !ok {
		writeUnsupported(w, "")
		return
	}
	id := r.PathValue("id")
	var b api.Reply
	if !s.readSigned(w, r, ActionReply, id, &b) {
		return
	}
	b.Text = strings.TrimRight(b.Text, " \t\r\n")
	switch {
	case b.Text == "" && !b.Interrupt:
		writeErr(w, http.StatusBadRequest, "bad_request", "nothing to send")
		return
	case len(b.Text) > MaxReplyBytes || !utf8.ValidString(b.Text):
		writeErr(w, http.StatusBadRequest, "bad_request", "message is too long")
		return
	}
	res, err := rep.Reply(r.Context(), id, b, by(r))
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	ar, ok := s.d.Core.(core.ActivityReader)
	if !ok {
		writeUnsupported(w, "")
		return
	}
	q := api.ActivityQuery{Cursor: r.URL.Query().Get("cursor")}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeErr(w, http.StatusBadRequest, "bad_request", "limit must be a positive number")
			return
		}
		q.Limit = min(n, 1000)
	}
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "since must be RFC 3339")
			return
		}
		q.Since = t
	}
	act, err := ar.Activity(r.Context(), r.PathValue("id"), q)
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, act)
}

// ---------- F6: insights ----------

func (s *Server) handleInsights(w http.ResponseWriter, r *http.Request) {
	ir, ok := s.d.Core.(core.InsightsReader)
	if !ok {
		writeUnsupported(w, "")
		return
	}
	var q api.InsightsQuery
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 90 {
			writeErr(w, http.StatusBadRequest, "bad_request", "days must be 1-90")
			return
		}
		q.Days = n
	}
	in, err := ir.Insights(r.Context(), q)
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, in)
}

// ---------- F5: rules ----------

func (s *Server) ruleEditor(w http.ResponseWriter) (core.RuleEditor, bool) {
	re, ok := s.d.Core.(core.RuleEditor)
	if !ok {
		writeUnsupported(w, "")
	}
	return re, ok
}

func (s *Server) handleRules(w http.ResponseWriter, r *http.Request) {
	re, ok := s.ruleEditor(w)
	if !ok {
		return
	}
	v, err := re.Rules(r.Context())
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleAddRule(w http.ResponseWriter, r *http.Request) {
	re, ok := s.ruleEditor(w)
	if !ok {
		return
	}
	var spec api.RuleSpec
	if !s.readSigned(w, r, ActionRule, "add", &spec) {
		return
	}
	if spec.When != "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "rules with `when:` expressions can only be added in policy.yaml")
		return
	}
	info, err := re.AddRule(r.Context(), spec, by(r))
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

type enabledBody struct {
	Enabled bool `json:"enabled"`
}

func (s *Server) handleRuleEnable(w http.ResponseWriter, r *http.Request) { s.setRuleState(w, r, true) }
func (s *Server) handleRuleDisable(w http.ResponseWriter, r *http.Request) {
	s.setRuleState(w, r, false)
}

// setRuleState signs the new state into the target ("enable:<name>"), so
// a signed enable can't be replayed as a disable of the same rule.
func (s *Server) setRuleState(w http.ResponseWriter, r *http.Request, enabled bool) {
	re, ok := s.ruleEditor(w)
	if !ok {
		return
	}
	name := r.PathValue("name")
	target := "disable:" + name
	if enabled {
		target = "enable:" + name
	}
	var b struct{}
	if !s.readSigned(w, r, ActionRule, target, &b) {
		return
	}
	if err := re.SetRuleEnabled(r.Context(), name, enabled, by(r)); err != nil {
		s.writeCoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRemoveRule(w http.ResponseWriter, r *http.Request) {
	re, ok := s.ruleEditor(w)
	if !ok {
		return
	}
	name := r.PathValue("name")
	var b struct{}
	if !s.readSigned(w, r, ActionRule, "remove:"+name, &b) {
		return
	}
	if err := re.RemoveRule(r.Context(), name, by(r)); err != nil {
		s.writeCoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePreset(w http.ResponseWriter, r *http.Request) {
	re, ok := s.ruleEditor(w)
	if !ok {
		return
	}
	name := r.PathValue("name")
	var b enabledBody
	if !s.readSigned(w, r, ActionRule, "preset:"+name, &b) {
		return
	}
	if err := re.SetPreset(r.Context(), name, b.Enabled, by(r)); err != nil {
		s.writeCoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleTestRule(w http.ResponseWriter, r *http.Request) {
	re, ok := s.ruleEditor(w)
	if !ok {
		return
	}
	var in api.RuleTestInput
	if !decodeBody(w, r, &in) {
		return
	}
	res, err := re.TestRule(r.Context(), in)
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleSuggestions(w http.ResponseWriter, r *http.Request) {
	re, ok := s.ruleEditor(w)
	if !ok {
		return
	}
	sg, err := re.SuggestRules(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	if sg == nil {
		sg = []api.RuleSuggestion{}
	}
	writeJSON(w, http.StatusOK, sg)
}

// ---------- F7: tasks ----------

func (s *Server) taskRunner(w http.ResponseWriter) (core.TaskRunner, bool) {
	tr, ok := s.d.Core.(core.TaskRunner)
	if !ok {
		writeUnsupported(w, "")
	}
	return tr, ok
}

func (s *Server) handleRepos(w http.ResponseWriter, r *http.Request) {
	tr, ok := s.taskRunner(w)
	if !ok {
		return
	}
	repos, err := tr.Repos(r.Context())
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	if repos == nil {
		repos = []api.RepoInfo{}
	}
	writeJSON(w, http.StatusOK, repos)
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	tr, ok := s.taskRunner(w)
	if !ok {
		return
	}
	ts, err := tr.Tasks(r.Context())
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	if ts == nil {
		ts = []api.Task{}
	}
	writeJSON(w, http.StatusOK, ts)
}

// MaxTaskPrompt bounds a task prompt from a device.
const MaxTaskPrompt = 32 << 10

func (s *Server) handleStartTask(w http.ResponseWriter, r *http.Request) {
	tr, ok := s.taskRunner(w)
	if !ok {
		return
	}
	var b api.TaskStart
	if !s.readSigned(w, r, ActionTask, "new", &b) {
		return
	}
	b.Prompt = strings.TrimSpace(b.Prompt)
	if b.Prompt == "" || len(b.Prompt) > MaxTaskPrompt || !utf8.ValidString(b.Prompt) {
		writeErr(w, http.StatusBadRequest, "bad_request", "prompt must be 1-32768 bytes")
		return
	}
	t, err := tr.StartTask(r.Context(), b, by(r))
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleTaskDiff(w http.ResponseWriter, r *http.Request) {
	tr, ok := s.taskRunner(w)
	if !ok {
		return
	}
	d, err := tr.TaskDiff(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleFinishTask(w http.ResponseWriter, r *http.Request) {
	tr, ok := s.taskRunner(w)
	if !ok {
		return
	}
	id := r.PathValue("id")
	var b api.TaskFinish
	if !s.readSigned(w, r, ActionTask, id, &b) {
		return
	}
	if b.Action != "ship" && b.Action != "discard" {
		writeErr(w, http.StatusBadRequest, "bad_request", "action must be ship or discard")
		return
	}
	t, err := tr.FinishTask(r.Context(), id, b, by(r))
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}
