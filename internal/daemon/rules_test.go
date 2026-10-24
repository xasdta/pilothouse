package daemon

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/client"
	"github.com/xasdta/pilothouse/internal/paths"
	"github.com/xasdta/pilothouse/internal/web"
)

// gitRepo makes a real repository (the daemon asks git for repo and
// branch) with one commit on branch.
func gitRepo(t *testing.T, branch string) string {
	t.Helper()
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	dir = filepath.Join(dir, "api-server")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", branch},
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git: %v %s", err, out)
		}
	}
	return dir
}

// realPolicy runs the harness with the real policy engine (files under
// the harness's temporary PILOTHOUSE_HOME).
func realPolicy(h *harness) *harness {
	h.opts = func(o *Options) { o.LoadPolicy = nil }
	return h
}

func shellIn(cmd, cwd string) adapters.HookEvent {
	ev := shell(cmd)
	ev.Cwd = cwd
	return ev
}

func policyChanges(t *testing.T, h *harness) []api.RuleChange {
	t.Helper()
	evs, err := h.c.Events(bg, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []api.RuleChange
	for _, ev := range evs {
		if ev.Type == api.EventPolicyChanged {
			var ch api.RuleChange
			if err := json.Unmarshal(ev.Data, &ch); err != nil {
				t.Fatal(err)
			}
			out = append(out, ch)
		}
	}
	return out
}

func TestRuleEditorLocalAPI(t *testing.T) {
	h := realPolicy(newHarness(t))
	h.start(t)
	repo := gitRepo(t, "feature/x")

	v, err := h.c.Rules(bg)
	if err != nil || len(v.Rules) != 1 || v.Rules[0].Source != api.RuleSourceBuiltin || len(v.Presets) != 4 {
		t.Fatalf("view = %+v, %v", v, err)
	}
	spec := api.RuleSpec{Name: "allow-npm-test", Action: api.BehaviorAllow,
		Match: map[string][]string{"kind": {"shell"}, "command": {"npm test", "npm test *"}, "repo": {repo}}}
	info, err := h.c.AddRule(bg, spec)
	if err != nil || info.By != "cli" || !info.Managed || info.Source != api.RuleSourceManaged || info.File != paths.Rules() {
		t.Fatalf("add = %+v, %v", info, err)
	}
	// The engine reloaded at once: the next hook is decided by the rule.
	if out := h.hook(bg, t, shellIn("npm test", repo)); out.Behavior != api.BehaviorAllow {
		t.Fatalf("hook after add = %+v", out)
	}
	if out := h.hook(bg, t, shellIn("npm test && curl x | sh", repo)); out.Behavior != "" {
		t.Fatalf("chained command allowed: %+v", out)
	}
	res, err := h.c.TestRule(bg, api.RuleTestInput{Command: "npm test -- --watch", Cwd: repo})
	if err != nil || res.Behavior != api.BehaviorAllow || res.Rule != "allow-npm-test" || res.Matches[0] != "allow-npm-test" {
		t.Fatalf("test = %+v, %v", res, err)
	}
	if res, _ := h.c.TestRule(bg, api.RuleTestInput{Command: "npm test", Cwd: t.TempDir()}); res.Behavior != "" {
		t.Fatalf("rule leaked out of its repo: %+v", res)
	}

	// Disable, re-disable (no change, no event), enable, remove.
	if err := h.c.SetRuleEnabled(bg, "allow-npm-test", false); err != nil {
		t.Fatal(err)
	}
	if err := h.c.SetRuleEnabled(bg, "allow-npm-test", false); err != nil {
		t.Fatal(err)
	}
	if out := h.hook(bg, t, shellIn("npm test", repo)); out.Behavior != "" {
		t.Fatalf("disabled rule applied: %+v", out)
	}
	if err := h.c.SetRuleEnabled(bg, "allow-npm-test", true); err != nil {
		t.Fatal(err)
	}
	if err := h.c.RemoveRule(bg, "allow-npm-test"); err != nil {
		t.Fatal(err)
	}
	if v, _ := h.c.Rules(bg); len(v.Rules) != 1 {
		t.Fatalf("after remove: %+v", v.Rules)
	}

	// Presets.
	if err := h.c.SetPreset(bg, "guarded-autopilot", true); err != nil {
		t.Fatal(err)
	}
	res, _ = h.c.TestRule(bg, api.RuleTestInput{Command: "git reset --hard", Cwd: repo})
	if res.Behavior != api.BehaviorDeny || res.Rule != "guarded-autopilot/deny-destructive" {
		t.Fatalf("preset test = %+v", res)
	}
	if res, _ := h.c.TestRule(bg, api.RuleTestInput{Path: filepath.Join(repo, "src/a.go")}); res.Behavior != api.BehaviorAllow {
		t.Fatalf("edit in repo under autopilot = %+v", res)
	}
	v, _ = h.c.Rules(bg)
	if !v.Presets[0].Enabled || v.Rules[len(v.Rules)-1].Preset != "guarded-autopilot" {
		t.Fatalf("view after preset = %+v", v.Presets[0])
	}
	if err := h.c.SetPreset(bg, "guarded-autopilot", false); err != nil {
		t.Fatal(err)
	}

	var ops []string
	for _, ch := range policyChanges(t, h) {
		ops = append(ops, ch.Op+":"+ch.Name+":"+ch.By)
		if ch.At.IsZero() {
			t.Fatalf("change without time: %+v", ch)
		}
	}
	want := "add:allow-npm-test:cli disable:allow-npm-test:cli enable:allow-npm-test:cli remove:allow-npm-test:cli preset_on:guarded-autopilot:cli preset_off:guarded-autopilot:cli"
	if strings.Join(ops, " ") != want {
		t.Fatalf("events = %s", strings.Join(ops, " "))
	}
}

func TestRuleEditorRefusals(t *testing.T) {
	h := realPolicy(newHarness(t))
	if err := os.MkdirAll(paths.Home(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Policy(), []byte("version: 1\nrules:\n  - {name: mine, match: {command: \"make\"}, action: ask}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.start(t)
	deny := api.RuleSpec{Name: "x", Action: api.BehaviorDeny, Match: map[string][]string{"command": {"x"}}}
	for name, err := range map[string]error{
		"builtin off":    h.c.SetRuleEnabled(bg, "pilothouse-self-protection", false),
		"builtin remove": h.c.RemoveRule(bg, "pilothouse-self-protection"),
		"user remove":    h.c.RemoveRule(bg, "mine"),
		"unknown preset": h.c.SetPreset(bg, "yolo", true),
		"shadow user": func() error {
			_, err := h.c.AddRule(bg, api.RuleSpec{Name: "mine", Action: api.BehaviorDeny, Match: deny.Match})
			return err
		}(),
		"shadow builtin": func() error {
			_, err := h.c.AddRule(bg, api.RuleSpec{Name: "pilothouse-self-protection", Action: api.BehaviorDeny, Match: deny.Match})
			return err
		}(),
		"traversal name": func() error {
			_, err := h.c.AddRule(bg, api.RuleSpec{Name: "../../policy", Action: api.BehaviorDeny, Match: deny.Match})
			return err
		}(),
		"allow everything": func() error {
			_, err := h.c.AddRule(bg, api.RuleSpec{Name: "yolo", Action: api.BehaviorAllow, Match: map[string][]string{"tool": {"*"}}})
			return err
		}(),
		"no command": func() error { _, err := h.c.TestRule(bg, api.RuleTestInput{}); return err }(),
	} {
		if !client.IsCode(err, "invalid") {
			t.Errorf("%s: %v, want 400 invalid", name, err)
		}
	}
	if err := h.c.RemoveRule(bg, "nope"); !client.IsCode(err, "not_found") {
		t.Errorf("unknown rule: %v", err)
	}
	// A local caller may use when:; it is compiled before it is written.
	if _, err := h.c.AddRule(bg, api.RuleSpec{Name: "late", Action: api.BehaviorAsk, Match: deny.Match, When: "hour >= 20"}); err != nil {
		t.Errorf("local when: %v", err)
	}
	if _, err := h.c.AddRule(bg, api.RuleSpec{Name: "broken", Action: api.BehaviorAsk, Match: deny.Match, When: "hour >"}); !client.IsCode(err, "invalid") {
		t.Errorf("bad when: %v", err)
	}
	// Changes are human-only on the socket; reads are not.
	h.rejectCallers(errors.New("called from inside an agent"))
	for name, err := range map[string]error{
		"add":     func() error { _, err := h.c.AddRule(bg, deny); return err }(),
		"disable": h.c.SetRuleEnabled(bg, "late", false),
		"remove":  h.c.RemoveRule(bg, "late"),
		"preset":  h.c.SetPreset(bg, "no-secrets", true),
	} {
		if !client.IsCode(err, "agent_caller") {
			t.Errorf("%s under the guard: %v", name, err)
		}
	}
	if _, err := h.c.Rules(bg); err != nil {
		t.Errorf("rules under the guard: %v", err)
	}
	if _, err := h.c.TestRule(bg, api.RuleTestInput{Command: "ls"}); err != nil {
		t.Errorf("test under the guard: %v", err)
	}
}

// webDevice is a paired device talking to the daemon's web handler.
type webDevice struct {
	t      *testing.T
	h      http.Handler
	key    *ecdsa.PrivateKey
	cookie string
	id     string
	n      int
}

const webHost = "127.0.0.1:0" // the harness listens on port 0

func pairDevice(t *testing.T, h *harness) *webDevice {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	ps, err := h.c.Pair(bg)
	if err != nil {
		t.Fatal(err)
	}
	d := &webDevice{t: t, h: h.d.web.Handler(), key: k}
	w := d.do("POST", "/api/pair", map[string]string{"code": ps.Code, "name": "Test phone", "public_key": base64.RawURLEncoding.EncodeToString(der)})
	if w.Code != 200 {
		t.Fatalf("pair: %d %s", w.Code, w.Body)
	}
	var me struct {
		Device api.Device `json:"device"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &me)
	d.id = me.Device.ID
	for _, c := range w.Result().Cookies() {
		if c.Value != "" {
			d.cookie = c.Name + "=" + c.Value
		}
	}
	if d.cookie == "" || d.id == "" {
		t.Fatalf("pairing gave no cookie or device: %s", w.Body)
	}
	return d
}

func (d *webDevice) do(method, path string, body any) *httptest.ResponseRecorder {
	d.t.Helper()
	var rd *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	} else {
		rd = strings.NewReader("")
	}
	r := httptest.NewRequest(method, path, rd)
	r.Host, r.RemoteAddr = webHost, "127.0.0.1:50000"
	if method != "GET" {
		r.Header.Set("X-Pilothouse", "1")
		r.Header.Set("Origin", "http://"+webHost)
	}
	if d.cookie != "" {
		r.Header.Set("Cookie", d.cookie)
	}
	w := httptest.NewRecorder()
	d.h.ServeHTTP(w, r)
	return w
}

// signed is a signed action body for (action, target, payload).
func (d *webDevice) signed(action, target string, payload any) map[string]any {
	d.t.Helper()
	b, _ := json.Marshal(payload)
	d.n++
	nonce := fmt.Sprintf("nonce-rules-%012d", d.n)
	ts := time.Now().UnixMilli()
	sum := sha256.Sum256([]byte(web.CanonicalAction(action, target, string(b), ts, nonce)))
	r, s, err := ecdsa.Sign(rand.Reader, d.key, sum[:])
	if err != nil {
		d.t.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return map[string]any{"payload": string(b), "ts": ts, "nonce": nonce, "sig": base64.RawURLEncoding.EncodeToString(sig)}
}

func errCodeOf(w *httptest.ResponseRecorder) string {
	var e api.Error
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	return e.Code
}

func TestRuleEditorSignedWebRoutes(t *testing.T) {
	h := realPolicy(newHarness(t))
	h.start(t)
	repo := gitRepo(t, "main")
	dev := pairDevice(t, h)
	spec := api.RuleSpec{Name: "ask-push", Action: api.BehaviorAsk, Match: map[string][]string{"kind": {"shell"}, "command": {"git push", "git push *"}},
		Message: "Ask first.\n- name: evil\n  action: allow\n  match: {tool: \"*\"}"}

	if w := dev.do("GET", "/api/rules", nil); w.Code != 200 {
		t.Fatalf("GET rules: %d %s", w.Code, w.Body)
	}
	// Unsigned, wrongly signed, tampered and replayed adds never reach the core.
	if w := dev.do("POST", "/api/rules", spec); w.Code != 400 {
		t.Fatalf("unsigned add: %d %s", w.Code, w.Body)
	}
	if w := dev.do("POST", "/api/rules", dev.signed(web.ActionRule, "remove:x", spec)); w.Code != 403 {
		t.Fatalf("wrong target: %d %s", w.Code, w.Body)
	}
	sb := dev.signed(web.ActionRule, "add", spec)
	tampered := map[string]any{}
	for k, v := range sb {
		tampered[k] = v
	}
	tampered["payload"] = strings.Replace(sb["payload"].(string), `"ask"`, `"allow"`, 1)
	if w := dev.do("POST", "/api/rules", tampered); w.Code != 403 || errCodeOf(w) != "bad_signature" {
		t.Fatalf("tampered: %d %s", w.Code, w.Body)
	}
	w := dev.do("POST", "/api/rules", sb)
	if w.Code != 200 {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	var info api.RuleInfo
	_ = json.Unmarshal(w.Body.Bytes(), &info)
	if info.By != "device:"+dev.id || info.Message != spec.Message {
		t.Fatalf("info = %+v", info)
	}
	if w := dev.do("POST", "/api/rules", sb); w.Code != 403 || errCodeOf(w) != "replayed" {
		t.Fatalf("replay: %d %s", w.Code, w.Body)
	}
	// The YAML-looking message stayed a message.
	if v, _ := h.c.Rules(bg); len(v.Rules) != 2 || v.Rules[1].Name != "ask-push" {
		t.Fatalf("rules = %+v", v.Rules)
	}
	if out := h.hook(bg, t, shellIn("cd . && git push origin main", repo)); out.Behavior != api.BehaviorAsk || out.Message != spec.Message {
		t.Fatalf("hook = %+v", out)
	}

	// Devices can't add when:, touch built-in or hand-written rules, or
	// use names that aren't plain.
	for name, c := range map[string]struct {
		path, target string
		payload      any
		code         int
	}{
		"when":          {"/api/rules", "add", api.RuleSpec{Name: "w", Action: api.BehaviorDeny, Match: spec.Match, When: "true"}, 400},
		"traversal":     {"/api/rules", "add", api.RuleSpec{Name: "../../../etc/x", Action: api.BehaviorDeny, Match: spec.Match}, 400},
		"allow all":     {"/api/rules", "add", api.RuleSpec{Name: "all", Action: api.BehaviorAllow, Match: map[string][]string{"kind": {"shell"}}}, 400},
		"builtin off":   {"/api/rules/pilothouse-self-protection/disable", "disable:pilothouse-self-protection", struct{}{}, 400},
		"builtin rm":    {"/api/rules/pilothouse-self-protection/remove", "remove:pilothouse-self-protection", struct{}{}, 400},
		"remove escape": {"/api/rules/..%2F..%2Fpolicy/remove", "remove:../../policy", struct{}{}, 404},
		"bad preset":    {"/api/presets/yolo", "preset:yolo", map[string]bool{"enabled": true}, 400},
		"extra fields":  {"/api/rules", "add", map[string]any{"name": "e", "action": "deny", "match": spec.Match, "source": "builtin"}, 400},
	} {
		if w := dev.do("POST", c.path, dev.signed(web.ActionRule, c.target, c.payload)); w.Code != c.code {
			t.Errorf("%s: %d %s, want %d", name, w.Code, w.Body, c.code)
		}
	}
	// A signed disable can't be replayed as an enable of the same rule.
	dis := dev.signed(web.ActionRule, "disable:ask-push", struct{}{})
	if w := dev.do("POST", "/api/rules/ask-push/enable", dis); w.Code != 403 {
		t.Fatalf("disable signature used for enable: %d %s", w.Code, w.Body)
	}
	if w := dev.do("POST", "/api/rules/ask-push/disable", dis); w.Code != 204 {
		t.Fatalf("disable: %d %s", w.Code, w.Body)
	}
	if w := dev.do("POST", "/api/presets/protect-main", dev.signed(web.ActionRule, "preset:protect-main", map[string]bool{"enabled": true})); w.Code != 204 {
		t.Fatalf("preset: %d %s", w.Code, w.Body)
	}
	w = dev.do("POST", "/api/rules/test", api.RuleTestInput{Command: "git push -f origin main", Cwd: repo})
	var res api.RuleTestResult
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if w.Code != 200 || res.Behavior != api.BehaviorDeny || res.Rule != "protect-main/deny-force-push" {
		t.Fatalf("test: %d %s", w.Code, w.Body)
	}
	if w := dev.do("POST", "/api/rules/ask-push/remove", dev.signed(web.ActionRule, "remove:ask-push", struct{}{})); w.Code != 204 {
		t.Fatalf("remove: %d %s", w.Code, w.Body)
	}
	var bys []string
	for _, ch := range policyChanges(t, h) {
		bys = append(bys, ch.Op+":"+ch.By)
	}
	d := "device:" + dev.id
	if strings.Join(bys, " ") != "add:"+d+" disable:"+d+" preset_on:"+d+" remove:"+d {
		t.Fatalf("changes = %v", bys)
	}
}

func TestSuggestRulesFromRepeatedApprovals(t *testing.T) {
	h := realPolicy(newHarness(t))
	h.presence.setAway(true)
	h.start(t)
	waitFor(t, "away", func() bool { return h.d.Presence().State == api.PresenceAway })
	repo := gitRepo(t, "feature/x")
	dev := pairDevice(t, h)

	ask := func(cmd string) api.Request {
		t.Helper()
		ev := permission(cmd, "")
		ev.Cwd = repo
		done := make(chan fakeOutcome, 1)
		go func() { done <- h.hook(bg, t, ev) }()
		var r api.Request
		waitFor(t, "request for "+cmd, func() bool {
			rs, _ := h.c.Requests(bg, api.RequestOpen)
			for _, x := range rs {
				if x.Command == cmd {
					r = x
					return true
				}
			}
			return false
		})
		t.Cleanup(func() {
			_, _ = h.c.Decide(bg, r.ID, api.DecideBody{Behavior: api.BehaviorDeny})
			<-done
		})
		return r
	}
	for i := 0; i < 3; i++ {
		r := ask(fmt.Sprintf("npm test -- --shard=%d", i))
		if _, err := h.c.Decide(bg, r.ID, api.DecideBody{Behavior: api.BehaviorAllow}); err != nil {
			t.Fatal(err)
		}
	}
	// Approvals answered by grants or rules don't count; a denial doesn't.
	r0 := ask("npm test -- --bail")
	if _, err := h.c.Decide(bg, r0.ID, api.DecideBody{Behavior: api.BehaviorDeny}); err != nil {
		t.Fatal(err)
	}
	r := ask("npm test")
	sg, err := h.c.Suggestions(bg, r.ID)
	if err != nil || len(sg) == 0 {
		t.Fatalf("suggestions = %+v, %v", sg, err)
	}
	if sg[0].Title != "Always allow `npm test` in api-server" || sg[0].Reason != "You allowed this 3 times this week" || sg[0].Scope != "repo" {
		t.Fatalf("first suggestion = %+v", sg[0])
	}
	// Through the device API, then accepted with one signed tap.
	w := dev.do("GET", "/api/requests/"+r.ID+"/suggestions", nil)
	var webSg []api.RuleSuggestion
	_ = json.Unmarshal(w.Body.Bytes(), &webSg)
	if w.Code != 200 || len(webSg) != len(sg) || webSg[0].ID != sg[0].ID {
		t.Fatalf("web suggestions: %d %s", w.Code, w.Body)
	}
	if w := dev.do("POST", "/api/rules", dev.signed(web.ActionRule, "add", webSg[0].Rule)); w.Code != 200 {
		t.Fatalf("accept suggestion: %d %s", w.Code, w.Body)
	}
	if out := h.hook(bg, t, shellIn("npm test", repo)); out.Behavior != api.BehaviorAllow {
		t.Fatalf("accepted suggestion not in force: %+v", out)
	}
	// Once added, it isn't suggested again.
	if sg, _ := h.c.Suggestions(bg, r.ID); len(sg) != 0 && sg[0].Title == webSg[0].Title {
		t.Fatalf("suggested again: %+v", sg)
	}
	// Risky calls get ask/deny suggestions, never an allow.
	push := ask("git push --force origin main")
	sg, _ = h.c.Suggestions(bg, push.ID)
	if titles(sg) != "Always ask before `git push` | Never allow force-pushes" || sg[0].Reason == "" {
		t.Fatalf("push suggestions = %s (%+v)", titles(sg), sg)
	}
	if _, err := h.c.Suggestions(bg, "r_nope"); !client.IsCode(err, "not_found") {
		t.Fatalf("unknown request: %v", err)
	}
}

func TestRuleEditorUnsupportedWithFakeEngine(t *testing.T) {
	h := newHarness(t) // fake policy: no managed file
	h.start(t)
	if _, err := h.c.Rules(bg); !client.IsCode(err, "unsupported") {
		t.Fatalf("rules with a fake engine: %v", err)
	}
	if _, err := h.c.AddRule(bg, api.RuleSpec{Name: "x"}); !client.IsCode(err, "unsupported") {
		t.Fatalf("add with a fake engine: %v", err)
	}
	// The tester only needs Evaluate.
	h.policy.set("ls", api.BehaviorAllow, "fake-allow")
	if res, err := h.c.TestRule(bg, api.RuleTestInput{Command: "ls"}); err != nil || res.Rule != "fake-allow" {
		t.Fatalf("test with a fake engine: %+v %v", res, err)
	}
}
