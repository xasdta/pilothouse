package transcript

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
)

// writeBigClaude writes a synthetic Claude transcript of at least size
// bytes: turns of a prompt, a tool call with a large result and a reply,
// like a long session full of tool output.
func writeBigClaude(tb testing.TB, path string, size int64) (turns int) {
	tb.Helper()
	f, err := os.Create(path)
	if err != nil {
		tb.Fatal(err)
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 1<<20)
	output := strings.Repeat("build output line\\n", 2000) // ~38 KB per tool result
	var written int64
	at := t0
	for i := 0; written < size; i++ {
		at = at.Add(time.Second)
		ts := at.Format(time.RFC3339Nano)
		lines := []string{
			fmt.Sprintf(`{"parentUuid":null,"isSidechain":false,"type":"user","message":{"role":"user","content":"step %d"},"uuid":"p-%d","timestamp":"%s","origin":{"kind":"human"}}`, i, i, ts),
			fmt.Sprintf(`{"parentUuid":"p-%d","isSidechain":false,"type":"assistant","message":{"id":"msg_%d","role":"assistant","model":"m-known","content":[{"type":"tool_use","id":"toolu_%d","name":"Bash","input":{"command":"make build"}}],"usage":{"input_tokens":3,"output_tokens":20,"cache_read_input_tokens":5000,"cache_creation_input_tokens":100}},"uuid":"a-%d","timestamp":"%s"}`, i, i, i, i, ts),
			fmt.Sprintf(`{"parentUuid":"a-%d","isSidechain":false,"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_%d","content":"%s","is_error":false}]},"uuid":"r-%d","timestamp":"%s","toolUseResult":{"stdout":"%s","stderr":"","interrupted":false}}`, i, i, output, i, ts, output),
			fmt.Sprintf(`{"type":"api-request-blob","hash":"h%d","message":{"role":"user","content":"%s"}}`, i, output),
			fmt.Sprintf(`{"parentUuid":"r-%d","isSidechain":false,"type":"assistant","message":{"id":"msg_%db","role":"assistant","model":"m-known","content":[{"type":"text","text":"Built step %d."}],"usage":{"input_tokens":1,"output_tokens":10,"cache_read_input_tokens":6000,"cache_creation_input_tokens":0}},"uuid":"t-%d","timestamp":"%s"}`, i, i, i, i, ts),
		}
		for _, l := range lines {
			n, _ := w.WriteString(l + "\n")
			written += int64(n)
		}
		turns++
	}
	if err := w.Flush(); err != nil {
		tb.Fatal(err)
	}
	return turns
}

// A 128 MB transcript: the first read takes bounded time, memory holds
// only the newest items, usage covers the whole file, and the next read
// is incremental.
func TestLargeTranscriptPerformance(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a 128 MB file")
	}
	e := newEnv(t)
	var fs syscall.Statfs_t
	if err := syscall.Statfs(e.claude, &fs); err == nil {
		if free := int64(fs.Bavail) * int64(fs.Bsize); free < 512<<20 {
			t.Skipf("needs 512 MB of free disk for a 128 MB transcript, %d MB free", free>>20)
		}
	}
	path := e.claudePath("sess-big")
	turns := writeBigClaude(t, path, 128<<20)
	r := e.reader()
	src := Source{Agent: api.AgentClaude, Path: path}

	start := time.Now()
	res := mustRead(t, r, "big", src, Query{Limit: 1000, CatchUp: true, Now: t0})
	took := time.Since(start)
	t.Logf("first read of 128 MB (%d turns): %v", turns, took)
	limit := 20 * time.Second
	if raceEnabled {
		limit = 120 * time.Second
	}
	if took > limit {
		t.Errorf("first read took %v, want under %v", took, limit)
	}
	if len(res.Items) != 1000 || res.Partial {
		t.Errorf("items %d partial %v", len(res.Items), res.Partial)
	}
	if res.Usage.Turns != turns || res.Usage.InputTokens != int64(4*turns) || res.Usage.OutputTokens != int64(30*turns) {
		t.Errorf("usage = %+v for %d turns", res.Usage, turns)
	}
	r.mu.Lock()
	kept := len(r.sessions["big"].feed.items)
	r.mu.Unlock()
	if kept > DefaultMaxItems {
		t.Errorf("feed keeps %d items", kept)
	}

	start = time.Now()
	res2 := mustRead(t, r, "big", src, Query{Cursor: res.Cursor})
	if d := time.Since(start); d > time.Second || len(res2.Items) != 0 {
		t.Errorf("incremental read took %v and returned %d items", d, len(res2.Items))
	}
}

func BenchmarkFirstRead(b *testing.B) {
	e := newEnv(b)
	path := e.claudePath("sess-bench")
	writeBigClaude(b, path, 32<<20)
	st, _ := os.Stat(path)
	b.SetBytes(st.Size())
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := e.reader()
		if _, err := r.Read("b", Source{Agent: api.AgentClaude, Path: path}, Query{}); err != nil {
			b.Fatal(err)
		}
	}
}
