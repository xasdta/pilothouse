package web

import (
	"strings"
	"testing"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/demo"
)

// The device API over the demo core, end to end with signed actions:
// what the PWA, the devserver and `pilothouse demo` rely on.
func TestDeviceAPIOverDemoCore(t *testing.T) {
	dc := demo.New(demo.Options{Quiet: true})
	e := newEnv(t, func(d *Deps) { d.Core = dc })
	k, pub := genKey(t)
	ck, dev := e.pair(pub)
	o := reqOpt{cookie: ck}

	// Snapshot carries tasks and the 0.2 request fields.
	var snap api.Snapshot
	mustJSON(t, e.do("GET", "/api/snapshot", nil, o), &snap)
	if len(snap.Tasks) == 0 || len(snap.Sessions) < 8 {
		t.Fatalf("snapshot: %d sessions, %d tasks", len(snap.Sessions), len(snap.Tasks))
	}

	// A question answered with a bad option is refused with a readable message.
	bad := e.signAction(k, ActionDecide, "r_question05", respondBody{Behavior: api.BehaviorAllow, Answers: map[string]string{"q1": "SQLite FTS5", "q2": "Colour"}})
	if w := e.do("POST", "/api/requests/r_question05/respond", bad, o); w.Code != 400 || errCode(t, w) != "invalid" || !strings.Contains(w.Body.String(), "isn't one of the options") {
		t.Fatalf("bad answer: %d %s", w.Code, w.Body)
	}
	good := e.signAction(k, ActionDecide, "r_question05", respondBody{Behavior: api.BehaviorAllow, Answers: map[string]string{"q1": "SQLite FTS5", "q2": "Title, Body"}})
	if w := e.do("POST", "/api/requests/r_question05/respond", good, o); w.Code != 200 {
		t.Fatalf("answer: %d %s", w.Code, w.Body)
	}
	r, _ := dc.Request(t.Context(), "r_question05")
	if r.Decision == nil || r.Decision.By != "device:"+dev || r.Decision.Answers["q2"] != "Title, Body" {
		t.Fatalf("decision %+v", r.Decision)
	}

	// Suggestion → one tap → a managed rule.
	var sg []api.RuleSuggestion
	mustJSON(t, e.do("GET", "/api/requests/r_gitpush03/suggestions", nil, o), &sg)
	if len(sg) == 0 {
		t.Fatal("no suggestions")
	}
	spec := sg[0].Rule
	if w := e.do("POST", "/api/rules", e.signAction(k, ActionRule, "add", spec), o); w.Code != 200 {
		t.Fatalf("add rule: %d %s", w.Code, w.Body)
	}
	var rv api.RulesView
	mustJSON(t, e.do("GET", "/api/rules", nil, o), &rv)
	var added *api.RuleInfo
	for i := range rv.Rules {
		if rv.Rules[i].Name == spec.Name {
			added = &rv.Rules[i]
		}
	}
	if added == nil || !added.Managed || added.By != "device:"+dev {
		t.Fatalf("rule not listed as managed: %+v", added)
	}
	// A signed disable can't be replayed as an enable.
	dis := e.signAction(k, ActionRule, "disable:"+spec.Name, struct{}{})
	if w := e.do("POST", "/api/rules/"+spec.Name+"/enable", dis, o); w.Code != 403 {
		t.Fatalf("disable signature used for enable: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/rules/"+spec.Name+"/disable", e.signAction(k, ActionRule, "disable:"+spec.Name, struct{}{}), o); w.Code != 204 {
		t.Fatalf("disable: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/rules/self-protection/remove", e.signAction(k, ActionRule, "remove:self-protection", struct{}{}), o); w.Code != 400 || !strings.Contains(w.Body.String(), "built-in") {
		t.Fatalf("remove built-in: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/presets/protect-main", e.signAction(k, ActionRule, "preset:protect-main", enabledBody{Enabled: true}), o); w.Code != 204 {
		t.Fatalf("preset: %d %s", w.Code, w.Body)
	}
	var tr api.RuleTestResult
	mustJSON(t, e.do("POST", "/api/rules/test", api.RuleTestInput{Command: "git push --force origin main"}, o), &tr)
	if tr.Behavior != api.BehaviorDeny || tr.Rule != "protect-main-force" {
		t.Fatalf("tester: %+v", tr)
	}

	// Reply, activity, insights.
	if w := e.do("POST", "/api/sessions/s_infra0006/reply", e.signAction(k, ActionReply, "s_infra0006", api.Reply{Text: "Leave prod for Monday"}), o); w.Code != 200 || !strings.Contains(w.Body.String(), `"via":"message"`) {
		t.Fatalf("reply: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/sessions/s_site000002/reply", e.signAction(k, ActionReply, "s_site000002", api.Reply{Text: "hi"}), o); w.Code != 501 || !strings.Contains(w.Body.String(), "pilothouse run codex") {
		t.Fatalf("reply to observed codex: %d %s", w.Code, w.Body)
	}
	var act api.Activity
	mustJSON(t, e.do("GET", "/api/sessions/s_infra0006/activity?limit=1", nil, o), &act)
	if len(act.Items) != 1 || act.Items[0].Text != "Leave prod for Monday" || act.Items[0].Source != "pilothouse" {
		t.Fatalf("activity: %+v", act.Items)
	}
	var in api.Insights
	mustJSON(t, e.do("GET", "/api/insights?days=30", nil, o), &in)
	if len(in.Days) != 30 || in.Total.CostUSD <= 0 {
		t.Fatalf("insights: %d days, $%v", len(in.Days), in.Total.CostUSD)
	}

	// Tasks: start, then discard.
	var repos []api.RepoInfo
	mustJSON(t, e.do("GET", "/api/repos", nil, o), &repos)
	if len(repos) == 0 {
		t.Fatal("no repos")
	}
	w := e.do("POST", "/api/tasks", e.signAction(k, ActionTask, "new", api.TaskStart{Repo: repos[0].Root, Agent: api.AgentCodex, Prompt: "Add a health check"}), o)
	var task api.Task
	mustJSON(t, w, &task)
	if w.Code != 200 || task.Status != api.TaskRunning {
		t.Fatalf("start task: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/tasks/"+task.ID+"/finish", e.signAction(k, ActionTask, task.ID, api.TaskFinish{Action: "discard"}), o); w.Code != 200 || !strings.Contains(w.Body.String(), `"abandoned"`) {
		t.Fatalf("discard: %d %s", w.Code, w.Body)
	}
}
