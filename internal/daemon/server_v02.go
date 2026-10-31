package daemon

import (
	"net/http"
	"strconv"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/client"
)

// Local (Unix socket) API added in 0.2 for the CLI and the TUI. Calls that
// change what an agent may or will do are wrapped with guarded (the
// agent-caller check), like decisions.
func (d *Daemon) routesV02(mux *http.ServeMux, guarded func(http.HandlerFunc) http.Handler) {
	mux.Handle("POST /v1/sessions/{id}/reply", guarded(d.handleReply))
	mux.HandleFunc("GET /v1/sessions/{id}/activity", d.handleActivity)
	mux.HandleFunc("GET /v1/insights", d.handleInsights)

	mux.HandleFunc("GET /v1/rules", d.handleRules)
	mux.Handle("POST /v1/rules", guarded(d.handleAddRule))
	mux.HandleFunc("POST /v1/rules/test", d.handleTestRule)
	mux.Handle("POST /v1/rules/{name}/enable", guarded(d.handleRuleEnable))
	mux.Handle("POST /v1/rules/{name}/disable", guarded(d.handleRuleDisable))
	mux.Handle("POST /v1/rules/{name}/remove", guarded(d.handleRemoveRule))
	mux.Handle("POST /v1/presets/{name}", guarded(d.handlePreset))
	mux.HandleFunc("GET /v1/requests/{id}/suggestions", d.handleSuggestions)

	mux.HandleFunc("GET /v1/repos", d.handleRepos)
	mux.Handle("POST /v1/tasks/start", guarded(d.handleStartTask))
	mux.HandleFunc("GET /v1/tasks/{id}/diff", d.handleTaskDiff)
	mux.Handle("POST /v1/tasks/{id}/finish", guarded(d.handleFinishTask))

	// Plugin agents (OpenCode): hooks and the reply channel. Unguarded like
	// /v1/hook — the plugin runs inside the agent; see plugin.go for what
	// keeps a session's replies to its own plugin.
	d.routesPlugin(mux)
}

// localBy names the local caller like handleDecide does.
func localBy(r *http.Request) string {
	if r.Header.Get(client.ClientHeader) == "tui" {
		return "tui"
	}
	return "cli"
}

func (d *Daemon) handleReply(w http.ResponseWriter, r *http.Request) {
	var b api.Reply
	if err := decodeJSON(w, r, &b, maxBody); err != nil {
		writeErr(w, err)
		return
	}
	res, err := d.Reply(r.Context(), r.PathValue("id"), b, localBy(r))
	reply(w, res, err)
}

func (d *Daemon) handleActivity(w http.ResponseWriter, r *http.Request) {
	limit, err := queryInt(r, "limit", 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	q := api.ActivityQuery{Cursor: r.URL.Query().Get("cursor"), Limit: limit, Peek: r.URL.Query().Get("peek") == "1"}
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeErr(w, badRequest("since must be RFC 3339"))
			return
		}
		q.Since = t
	}
	a, err := d.Activity(r.Context(), r.PathValue("id"), q)
	reply(w, a, err)
}

func (d *Daemon) handleInsights(w http.ResponseWriter, r *http.Request) {
	days, err := queryInt(r, "days", 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	in, err := d.Insights(r.Context(), api.InsightsQuery{Days: days})
	reply(w, in, err)
}

func (d *Daemon) handleRules(w http.ResponseWriter, r *http.Request) {
	v, err := d.Rules(r.Context())
	reply(w, v, err)
}

func (d *Daemon) handleAddRule(w http.ResponseWriter, r *http.Request) {
	var spec api.RuleSpec
	if err := decodeJSON(w, r, &spec, maxBody); err != nil {
		writeErr(w, err)
		return
	}
	info, err := d.AddRule(r.Context(), spec, localBy(r))
	reply(w, info, err)
}

func (d *Daemon) handleTestRule(w http.ResponseWriter, r *http.Request) {
	var in api.RuleTestInput
	if err := decodeJSON(w, r, &in, maxBody); err != nil {
		writeErr(w, err)
		return
	}
	res, err := d.TestRule(r.Context(), in)
	reply(w, res, err)
}

func (d *Daemon) handleRuleEnable(w http.ResponseWriter, r *http.Request) {
	replyNoContent(w, d.SetRuleEnabled(r.Context(), r.PathValue("name"), true, localBy(r)))
}

func (d *Daemon) handleRuleDisable(w http.ResponseWriter, r *http.Request) {
	replyNoContent(w, d.SetRuleEnabled(r.Context(), r.PathValue("name"), false, localBy(r)))
}

func (d *Daemon) handleRemoveRule(w http.ResponseWriter, r *http.Request) {
	replyNoContent(w, d.RemoveRule(r.Context(), r.PathValue("name"), localBy(r)))
}

func (d *Daemon) handlePreset(w http.ResponseWriter, r *http.Request) {
	on, err := strconv.ParseBool(r.URL.Query().Get("enabled"))
	if err != nil {
		writeErr(w, badRequest("enabled must be true or false"))
		return
	}
	replyNoContent(w, d.SetPreset(r.Context(), r.PathValue("name"), on, localBy(r)))
}

func (d *Daemon) handleSuggestions(w http.ResponseWriter, r *http.Request) {
	sg, err := d.SuggestRules(r.Context(), r.PathValue("id"))
	if sg == nil {
		sg = []api.RuleSuggestion{}
	}
	reply(w, sg, err)
}

func (d *Daemon) handleRepos(w http.ResponseWriter, r *http.Request) {
	repos, err := d.Repos(r.Context())
	if repos == nil {
		repos = []api.RepoInfo{}
	}
	reply(w, repos, err)
}

func (d *Daemon) handleStartTask(w http.ResponseWriter, r *http.Request) {
	var b api.TaskStart
	if err := decodeJSON(w, r, &b, maxBody); err != nil {
		writeErr(w, err)
		return
	}
	t, err := d.StartTask(r.Context(), b, localBy(r))
	reply(w, t, err)
}

func (d *Daemon) handleTaskDiff(w http.ResponseWriter, r *http.Request) {
	diff, err := d.TaskDiff(r.Context(), r.PathValue("id"))
	reply(w, diff, err)
}

func (d *Daemon) handleFinishTask(w http.ResponseWriter, r *http.Request) {
	var b api.TaskFinish
	if err := decodeJSON(w, r, &b, maxBody); err != nil {
		writeErr(w, err)
		return
	}
	t, err := d.FinishTask(r.Context(), r.PathValue("id"), b, localBy(r))
	reply(w, t, err)
}
