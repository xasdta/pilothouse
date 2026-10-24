package transcript

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
)

// A line the agent is still writing waits for its newline; a cursor
// returns only what is new or changed.
func TestIncrementalRead(t *testing.T) {
	e := newEnv(t)
	path := e.claudePath("sess-inc")
	c := newClaudeLog()
	c.prompt("first", "human")
	c.toolUse("msg_1", "m-known", "toolu_1", "Bash", map[string]any{"command": "make"}, usage(1, 1, 0, 0))
	writeFile(t, path, c.take())
	r := e.reader()
	src := Source{Agent: api.AgentClaude, Path: path}
	res := mustRead(t, r, "k", src, Query{})
	if len(res.Items) != 2 {
		t.Fatalf("first read: %d items", len(res.Items))
	}
	cursor := res.Cursor

	// Nothing new: nothing returned, same cursor.
	res = mustRead(t, r, "k", src, Query{Cursor: cursor})
	if len(res.Items) != 0 || res.Cursor != cursor {
		t.Fatalf("idle read returned %d items, cursor %q→%q", len(res.Items), cursor, res.Cursor)
	}

	// Half a line: not parsed yet.
	c.result("toolu_1", "built", false, nil)
	line := c.take()
	appendFile(t, path, line[:len(line)/2])
	res = mustRead(t, r, "k", src, Query{Cursor: cursor})
	if len(res.Items) != 0 {
		t.Fatalf("a partial line was parsed: %+v", res.Items)
	}
	appendFile(t, path, line[len(line)/2:])
	res = mustRead(t, r, "k", src, Query{Cursor: cursor})
	if len(res.Items) != 1 || res.Items[0].ID != "toolu_1" || res.Items[0].Result != "built" {
		t.Fatalf("completed line: %+v", res.Items)
	}
	// A line ending in \r\n and blank lines are fine.
	c.text("msg_2", "m-known", "done", usage(1, 1, 0, 0))
	appendFile(t, path, "\n\n"+strings.TrimSuffix(c.take(), "\n")+"\r\n")
	res = mustRead(t, r, "k", src, Query{Cursor: res.Cursor})
	if len(res.Items) != 1 || res.Items[0].Text != "done" {
		t.Fatalf("CRLF line: %+v", res.Items)
	}
	// A cursor from elsewhere means "everything".
	res = mustRead(t, r, "k", src, Query{Cursor: "zzz.1"})
	if len(res.Items) != 3 {
		t.Fatalf("foreign cursor returned %d items, want all 3", len(res.Items))
	}
}

// Truncated, replaced or rewritten files start the feed over; the old
// cursor then returns everything.
func TestTruncateAndRotate(t *testing.T) {
	e := newEnv(t)
	path := e.claudePath("sess-rot")
	c := newClaudeLog()
	c.prompt("one", "human")
	c.text("msg_1", "m-known", "reply one", usage(10, 10, 0, 0))
	c.prompt("two", "human")
	writeFile(t, path, c.take())
	r := e.reader()
	src := Source{Agent: api.AgentClaude, Path: path}
	first := mustRead(t, r, "k", src, Query{})
	if first.Usage.InputTokens != 10 || len(first.Items) != 3 {
		t.Fatalf("first read: %+v", first)
	}

	t.Run("truncated", func(t *testing.T) {
		c2 := newClaudeLog()
		c2.prompt("fresh", "human")
		writeFile(t, path, c2.take()) // shorter than before
		res := mustRead(t, r, "k", src, Query{Cursor: first.Cursor})
		if len(res.Items) != 1 || res.Items[0].Text != "fresh" || res.Usage.InputTokens != 0 {
			t.Fatalf("after truncation: %+v", res)
		}
		if strings.SplitN(res.Cursor, ".", 2)[0] == strings.SplitN(first.Cursor, ".", 2)[0] {
			t.Error("a reset must change the cursor epoch")
		}
	})

	t.Run("replaced", func(t *testing.T) {
		before := mustRead(t, r, "k", src, Query{})
		c3 := newClaudeLog()
		c3.prompt("replacement file", "human")
		c3.prompt("much longer than the old one", "human")
		tmp := path + ".tmp"
		writeFile(t, tmp, c3.take())
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
		res := mustRead(t, r, "k", src, Query{Cursor: before.Cursor})
		if len(res.Items) != 2 || res.Items[0].Text != "replacement file" {
			t.Fatalf("after replacement: %+v", res.Items)
		}
	})

	t.Run("rewritten in place", func(t *testing.T) {
		mustRead(t, r, "k", src, Query{})
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		rewritten := strings.Replace(string(b), "replacement file", "REPLACEMENT FILE", 1)
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteAt([]byte(rewritten), 0); err != nil {
			t.Fatal(err)
		}
		f.Close()
		res := mustRead(t, r, "k", src, Query{})
		if len(res.Items) != 2 || res.Items[0].Text != "REPLACEMENT FILE" {
			t.Fatalf("after rewrite: %+v", res.Items)
		}
	})

	t.Run("removed", func(t *testing.T) {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Read("k", src, Query{}); !IsMissing(err) {
			t.Fatalf("read of a deleted transcript = %v, want missing", err)
		}
	})
}

// Lines longer than MaxLine are skipped without being buffered, also
// while they are still being written; the lines around them are read.
func TestOverlongLines(t *testing.T) {
	e := newEnv(t)
	path := e.claudePath("sess-long")
	e.opts.MaxLine = 4096
	r := e.reader()
	src := Source{Agent: api.AgentClaude, Path: path}
	c := newClaudeLog()
	c.prompt("before", "human")
	c.prompt(strings.Repeat("x", 10_000), "human") // over-long
	c.prompt("after", "human")
	writeFile(t, path, c.take())
	res := mustRead(t, r, "k", src, Query{})
	var got []string
	for _, it := range res.Items {
		got = append(got, it.Text)
	}
	if strings.Join(got, ",") != "before,after" {
		t.Fatalf("items = %q", got)
	}

	// An over-long line that is still growing: dropped across reads.
	c.prompt(strings.Repeat("y", 10_000), "human")
	huge := c.take()
	appendFile(t, path, huge[:6000])
	mustRead(t, r, "k", src, Query{})
	appendFile(t, path, huge[6000:])
	c.prompt("last", "human")
	appendFile(t, path, c.take())
	res = mustRead(t, r, "k", src, Query{Cursor: res.Cursor})
	if len(res.Items) != 1 || res.Items[0].Text != "last" {
		t.Fatalf("after a growing over-long line: %+v", res.Items)
	}
	r.mu.Lock()
	skipped := r.sessions["k"].main.tail.skipped
	r.mu.Unlock()
	if skipped != 2 {
		t.Errorf("skipped %d over-long lines, want 2", skipped)
	}
}

// A huge transcript is parsed in full only near its end; before that only
// usage and turns are counted, so totals stay complete.
func TestFirstScanOfHugeFile(t *testing.T) {
	e := newEnv(t)
	path := e.claudePath("sess-huge")
	c := newClaudeLog()
	for i := 0; i < 200; i++ {
		c.prompt("question", "human")
		c.toolUse(msgID(i), "m-known", "toolu_"+msgID(i), "Bash", map[string]any{"command": "ls"}, usage(1, 2, 3, 4))
		c.result("toolu_"+msgID(i), strings.Repeat("o", 500), false, nil)
	}
	writeFile(t, path, c.take())
	st, _ := os.Stat(path)

	e.opts.FirstScan = st.Size() / 10
	res := mustRead(t, e.reader(), "k", Source{Agent: api.AgentClaude, Path: path}, Query{Limit: 1000})
	if res.Partial {
		t.Error("the head pass covered the start: not partial")
	}
	if len(res.Items) == 0 || len(res.Items) > 3*200/5 {
		t.Errorf("%d items: only the tail should be parsed for the feed", len(res.Items))
	}
	want := api.Usage{InputTokens: 200, OutputTokens: 400, CacheReadTokens: 600, CacheWriteTokens: 800, Model: "m-known", Turns: 200}
	res.Usage.CostUSD = 0
	if res.Usage != want {
		t.Errorf("usage = %+v, want %+v", res.Usage, want)
	}
	last := res.Items[len(res.Items)-1]
	if last.Turn != 200 {
		t.Errorf("turn numbers count the head too: last item turn %d", last.Turn)
	}

	// With the head pass bounded too, the start is never read: partial.
	e.opts.MaxHead = st.Size() / 10
	res = mustRead(t, e.reader(), "k2", Source{Agent: api.AgentClaude, Path: path}, Query{})
	if !res.Partial || res.Usage.InputTokens >= 200 {
		t.Errorf("bounded head: partial=%v usage %+v", res.Partial, res.Usage)
	}
}

func msgID(i int) string { return "msg_" + strings.Repeat("0", 4-len(itoa(i))) + itoa(i) }

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}

// One refresh reads at most MaxRead bytes; the next continues.
func TestMaxReadPerRefresh(t *testing.T) {
	e := newEnv(t)
	path := e.claudePath("sess-chunk")
	c := newClaudeLog()
	for i := 0; i < 100; i++ {
		c.prompt("p", "human")
	}
	content := c.take()
	writeFile(t, path, content)
	e.opts.MaxRead = int64(len(content) / 4)
	r := e.reader()
	src := Source{Agent: api.AgentClaude, Path: path}
	var total int
	cursor := ""
	for i := 0; i < 10 && total < 100; i++ {
		res := mustRead(t, r, "k", src, Query{Cursor: cursor, Limit: 1000})
		total += len(res.Items)
		cursor = res.Cursor
	}
	if total != 100 {
		t.Errorf("read %d prompts over several refreshes, want 100", total)
	}
}

func TestTimestampsAndOrder(t *testing.T) {
	e := newEnv(t)
	path := e.claudePath("sess-ts")
	c := newClaudeLog()
	c.prompt("a", "human")
	c.add(map[string]any{"type": "user", "timestamp": "not a time", "message": map[string]any{"role": "user", "content": "b"}})
	writeFile(t, path, c.take())
	res := mustRead(t, e.reader(), "k", Source{Agent: api.AgentClaude, Path: path}, Query{})
	if len(res.Items) != 2 || !res.Items[1].At.Equal(res.Items[0].At) || res.Items[1].Text != "b" {
		t.Errorf("a bad timestamp takes the previous one: %+v", res.Items)
	}
	if res.Items[0].At.Location() != time.UTC {
		t.Errorf("times are UTC: %v", res.Items[0].At)
	}
}
