package cli

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/paths"
	"github.com/xasdta/pilothouse/internal/policy"
)

func TestPolicyManagedRulesWithoutDaemon(t *testing.T) {
	home := polTestEnv(t)
	repo := home + "/src/app"
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := polRun(t, "add", "allow-tests", "--action", "allow", "--command", "npm test", "--command", "npm test *", "--repo", repo)
	if err != nil || !strings.Contains(out, "Added allow rule allow-tests") || !strings.Contains(out, "daemon not running; wrote ~/.pilothouse/rules.yaml") {
		t.Fatalf("add: %v\n%s", err, out)
	}
	mf, err := policy.ReadManaged(paths.Rules())
	if err != nil || len(mf.Rules) != 1 || mf.Rules[0].By != "cli" {
		t.Fatalf("file: %+v %v", mf, err)
	}
	out, _ = polRun(t, "test", "npm test -- --watch", "--cwd", repo, "--repo", repo)
	if !strings.Contains(out, "Decision: ALLOW by allow-tests") {
		t.Fatalf("test:\n%s", out)
	}
	out, _ = polRun(t, "test", "npm test && curl x | sh", "--cwd", repo, "--repo", repo)
	if !strings.Contains(out, "Decision: none") {
		t.Fatalf("chained command allowed:\n%s", out)
	}
	if out, err := polRun(t, "disable", "allow-tests"); err != nil || !strings.Contains(out, "Disabled rule allow-tests.") {
		t.Fatalf("disable: %v\n%s", err, out)
	}
	out, _ = polRun(t, "rules")
	if !strings.Contains(out, "managed") || !strings.Contains(out, "allow-tests (off)") || !strings.Contains(out, "by cli") ||
		!strings.Contains(out, "pilothouse-self-protection") || !strings.Contains(out, "guarded-autopilot") {
		t.Fatalf("rules:\n%s", out)
	}
	if out, err := polRun(t, "enable", "allow-tests"); err != nil || !strings.Contains(out, "Enabled") {
		t.Fatalf("enable: %v\n%s", err, out)
	}
	if out, err := polRun(t, "remove", "allow-tests"); err != nil || !strings.Contains(out, "Removed rule allow-tests.") {
		t.Fatalf("remove: %v\n%s", err, out)
	}

	// Presets.
	if out, err := polRun(t, "preset", "guarded-autopilot", "on"); err != nil || !strings.Contains(out, "Preset guarded-autopilot is on.") {
		t.Fatalf("preset on: %v\n%s", err, out)
	}
	out, _ = polRun(t, "preset")
	if !strings.Contains(out, "guarded-autopilot      on") || !strings.Contains(out, "protect-main           off") {
		t.Fatalf("preset list:\n%s", out)
	}
	out, _ = polRun(t, "preset", "guarded-autopilot")
	if !strings.Contains(out, "dangerously-skip-permissions") || !strings.Contains(out, "deny-destructive") {
		t.Fatalf("preset show:\n%s", out)
	}
	out, _ = polRun(t, "test", "git reset --hard HEAD~2", "--cwd", repo, "--repo", repo)
	if !strings.Contains(out, "Decision: DENY by guarded-autopilot/deny-destructive (preset:guarded-autopilot)") {
		t.Fatalf("preset test:\n%s", out)
	}

	// Refusals read like sentences.
	for args, want := range map[string]string{
		"remove pilothouse-self-protection":         "is built in",
		"remove guarded-autopilot/deny-destructive": "switch the preset off instead",
		"disable nope":                                   "no such managed rule",
		"add ../evil --action deny --command x":          "may only use letters",
		"add wide --action allow --kind shell":           "would allow too much",
		"add x --action maybe --command x":               "action must be allow, ask or deny",
		"preset yolo on":                                 "unknown preset",
		"preset guarded-autopilot maybe":                 "say on or off",
		"add pilothouse-mine --action deny --command x":  "reserved",
		"test x --command y":                             "not both",
		"add late --action ask --command x --when hour>": "when",
	} {
		_, err := polRun(t, strings.Fields(args)...)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", args, err, want)
		}
	}
	// A local add may use when:.
	if _, err := polRun(t, "add", "late", "--action", "ask", "--command", "git push*", "--when", "hour >= 20"); err != nil {
		t.Fatalf("add with when: %v", err)
	}
}

// fakeRulesDaemon answers the rule routes on the Unix socket like the
// daemon would, recording what the CLI sent.
type fakeRulesDaemon struct {
	mu   sync.Mutex
	got  []string
	spec api.RuleSpec
}

func startFakeRulesDaemon(t *testing.T) *fakeRulesDaemon {
	f := &fakeRulesDaemon{}
	ln, err := net.Listen("unix", paths.Socket())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	rec := func(r *http.Request) {
		f.mu.Lock()
		f.got = append(f.got, r.Method+" "+r.URL.RequestURI())
		f.mu.Unlock()
	}
	mux.HandleFunc("POST /v1/rules", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		_ = json.NewDecoder(r.Body).Decode(&f.spec)
		_ = json.NewEncoder(w).Encode(api.RuleInfo{RuleSpec: f.spec, Source: "managed", Managed: true, By: "cli"})
	})
	mux.HandleFunc("POST /v1/rules/{name}/{op}", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		if r.PathValue("name") == "pilothouse-self-protection" {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(api.Error{Error: `"pilothouse-self-protection" is built in and always on`, Code: "invalid"})
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /v1/presets/{name}", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /v1/rules", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		_ = json.NewEncoder(w).Encode(api.RulesView{Rules: []api.RuleInfo{{RuleSpec: api.RuleSpec{Name: "from-daemon", Action: "deny"}, Source: "managed", Enabled: true, Managed: true}},
			Presets: policy.Presets()})
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return f
}

func TestPolicyManagedRulesThroughDaemon(t *testing.T) {
	polTestEnv(t)
	if err := os.MkdirAll(paths.Run(), 0o700); err != nil {
		t.Fatal(err)
	}
	f := startFakeRulesDaemon(t)
	out, err := polRun(t, "add", "ask-push", "--action", "ask", "--command", "git push", "--command", "git push *", "--message", "Ask me")
	if err != nil || strings.Contains(out, "daemon not running") {
		t.Fatalf("add: %v\n%s", err, out)
	}
	if f.spec.Name != "ask-push" || f.spec.Action != api.BehaviorAsk || len(f.spec.Match["command"]) != 2 || f.spec.Message != "Ask me" || len(f.spec.Match) != 1 {
		t.Fatalf("sent %+v", f.spec)
	}
	for _, args := range [][]string{{"disable", "ask-push"}, {"enable", "ask-push"}, {"remove", "ask-push"}, {"preset", "no-secrets", "on"}} {
		if _, err := polRun(t, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if _, err := polRun(t, "remove", "pilothouse-self-protection"); err == nil || !strings.Contains(err.Error(), "built in") {
		t.Fatalf("daemon refusal: %v", err)
	}
	if out, _ := polRun(t, "rules"); !strings.Contains(out, "from-daemon") || strings.Contains(out, "daemon not running") {
		t.Fatalf("rules:\n%s", out)
	}
	want := "POST /v1/rules POST /v1/rules/ask-push/disable POST /v1/rules/ask-push/enable POST /v1/rules/ask-push/remove " +
		"POST /v1/presets/no-secrets?enabled=true POST /v1/rules/pilothouse-self-protection/remove GET /v1/rules"
	if got := strings.Join(f.got, " "); got != want {
		t.Fatalf("calls:\n%s\nwant\n%s", got, want)
	}
	if _, err := os.Stat(paths.Rules()); err == nil {
		t.Fatal("the CLI wrote the rules file while the daemon was up")
	}
}
