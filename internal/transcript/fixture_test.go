package transcript

// Synthetic transcript builders. Every fixture in these tests is made up
// here, in the shape Claude Code and Codex write; none is copied from a
// real session.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
)

var t0 = time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

// fakePricer: model "m-known" costs $1/M input, $2/M output, $0.10/M
// cache reads, $1.25/M cache writes; anything else is free.
type fakePricer struct{}

func (fakePricer) Cost(model string, t Tokens, fast bool) float64 {
	if model != "m-known" {
		return 0
	}
	c := (float64(t.Input) + 2*float64(t.Output) + 0.1*float64(t.CacheRead) + 1.25*float64(t.CacheWrite)) / 1e6
	if fast {
		c *= 2
	}
	return c
}

// testEnv is a fake home with both agents' transcript folders.
type testEnv struct {
	t      testing.TB
	claude string // …/.claude/projects
	codex  string // …/.codex/sessions
	grok   string // …/.grok/sessions
	opts   Options
}

func newEnv(t testing.TB) *testEnv {
	t.Helper()
	home := t.TempDir()
	e := &testEnv{t: t, claude: filepath.Join(home, ".claude", "projects"), codex: filepath.Join(home, ".codex", "sessions"),
		grok: filepath.Join(home, ".grok", "sessions")}
	for _, d := range []string{e.claude, e.codex, e.grok} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	e.opts = Options{Roots: Roots{Claude: []string{e.claude}, Codex: []string{e.codex}, Grok: []string{e.grok}}, Pricer: fakePricer{}, Location: time.UTC}
	return e
}

func (e *testEnv) reader() *Reader { return NewReader(e.opts) }

// claudePath is where Claude Code would keep session sid of project dir.
func (e *testEnv) claudePath(sid string) string {
	p := filepath.Join(e.claude, "-home-dev-src-app", sid+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func writeFile(t testing.TB, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t testing.TB, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

// ---- Claude ----

type claudeLog struct {
	at        time.Time
	n         int
	lines     []string
	sidechain bool
	cwd       string
}

func newClaudeLog() *claudeLog { return &claudeLog{at: t0, cwd: "/home/dev/src/app"} }

func (c *claudeLog) ts() string {
	c.at = c.at.Add(time.Second)
	return c.at.Format(time.RFC3339Nano)
}

func (c *claudeLog) uuid() string {
	c.n++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", c.n)
}

func (c *claudeLog) add(v map[string]any) {
	if _, ok := v["uuid"]; !ok && v["type"] != "queue-operation" {
		v["uuid"] = c.uuid()
	}
	if _, ok := v["timestamp"]; !ok {
		v["timestamp"] = c.ts()
	}
	v["isSidechain"] = c.sidechain
	v["cwd"] = c.cwd
	v["sessionId"] = "sess-1"
	v["version"] = "2.1.309"
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	c.lines = append(c.lines, string(b))
}

func (c *claudeLog) raw(s string) { c.lines = append(c.lines, s) }

func (c *claudeLog) prompt(text, origin string) {
	v := map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}}
	if origin != "" {
		v["origin"] = map[string]any{"kind": origin}
	}
	c.add(v)
}

func usage(in, out, cr, cw int) map[string]any {
	return map[string]any{"input_tokens": in, "output_tokens": out, "cache_read_input_tokens": cr,
		"cache_creation_input_tokens": cw, "service_tier": "standard"}
}

// block logs one content block of response msgID, as Claude Code does:
// one record per block, each repeating the response's usage.
func (c *claudeLog) block(msgID, model string, block map[string]any, u map[string]any) {
	c.add(map[string]any{"type": "assistant", "message": map[string]any{
		"id": msgID, "role": "assistant", "model": model, "type": "message",
		"content": []any{block}, "usage": u, "stop_reason": nil,
	}})
}

func (c *claudeLog) text(msgID, model, text string, u map[string]any) {
	c.block(msgID, model, map[string]any{"type": "text", "text": text}, u)
}

func (c *claudeLog) toolUse(msgID, model, id, name string, input any, u map[string]any) {
	c.block(msgID, model, map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}, u)
}

func (c *claudeLog) result(id string, content any, isErr bool, toolUseResult any) {
	v := map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": id, "content": content, "is_error": isErr},
	}}}
	if toolUseResult != nil {
		v["toolUseResult"] = toolUseResult
	}
	c.add(v)
}

func (c *claudeLog) system(subtype string, extra map[string]any) {
	v := map[string]any{"type": "system", "subtype": subtype}
	for k, x := range extra {
		v[k] = x
	}
	c.add(v)
}

func (c *claudeLog) String() string {
	if len(c.lines) == 0 {
		return ""
	}
	return strings.Join(c.lines, "\n") + "\n"
}

// take returns the lines logged since the last take.
func (c *claudeLog) take() string {
	s := c.String()
	c.lines = nil
	return s
}

// ---- Codex ----

type codexLog struct {
	at    time.Time
	lines []string
}

func newCodexLog() *codexLog { return &codexLog{at: t0} }

func (c *codexLog) add(typ string, payload any) {
	c.at = c.at.Add(time.Second)
	b, err := json.Marshal(map[string]any{"timestamp": c.at.Format("2006-01-02T15:04:05.000Z"), "type": typ, "payload": payload})
	if err != nil {
		panic(err)
	}
	c.lines = append(c.lines, string(b))
}

func (c *codexLog) message(role, text string) {
	kind := "input_text"
	if role == "assistant" {
		kind = "output_text"
	}
	c.add("response_item", map[string]any{"type": "message", "role": role,
		"content": []any{map[string]any{"type": kind, "text": text}}})
}

func (c *codexLog) call(name, callID string, args any) {
	b, _ := json.Marshal(args)
	c.add("response_item", map[string]any{"type": "function_call", "name": name, "arguments": string(b), "call_id": callID})
}

func (c *codexLog) output(callID, out string) {
	c.add("response_item", map[string]any{"type": "function_call_output", "call_id": callID, "output": out})
}

func (c *codexLog) tokens(totalIn, totalCached, totalOut, lastIn, lastCached, lastOut, reasoning int) {
	c.add("event_msg", map[string]any{"type": "token_count", "info": map[string]any{
		"total_token_usage": map[string]any{"input_tokens": totalIn, "cached_input_tokens": totalCached, "output_tokens": totalOut,
			"reasoning_output_tokens": reasoning, "total_tokens": totalIn + totalOut},
		"last_token_usage": map[string]any{"input_tokens": lastIn, "cached_input_tokens": lastCached, "output_tokens": lastOut,
			"reasoning_output_tokens": reasoning, "total_tokens": lastIn + lastOut},
		"model_context_window": 400000,
	}, "rate_limits": nil})
}

func (c *codexLog) take() string {
	s := strings.Join(c.lines, "\n") + "\n"
	c.lines = nil
	return s
}

// ---- assertions ----

func byKind(items []api.ActivityItem, k api.ActivityKind) []api.ActivityItem {
	var out []api.ActivityItem
	for _, it := range items {
		if it.Kind == k {
			out = append(out, it)
		}
	}
	return out
}

func findItem(t testing.TB, items []api.ActivityItem, id string) api.ActivityItem {
	t.Helper()
	for _, it := range items {
		if it.ID == id {
			return it
		}
	}
	t.Fatalf("no item %q in %d items", id, len(items))
	return api.ActivityItem{}
}

func mustRead(t testing.TB, r *Reader, key string, src Source, q Query) Result {
	t.Helper()
	res, err := r.Read(key, src, q)
	if err != nil {
		t.Fatalf("read %s: %v", src.Path, err)
	}
	return res
}
