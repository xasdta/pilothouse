package wrap

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/paths"
)

// testEnv points every Pilothouse path at temp dirs (Invariant 1) and kills
// any holder and agent left behind when the test ends.
func testEnv(t *testing.T) {
	t.Helper()
	t.Setenv("PILOTHOUSE_HOME", t.TempDir())
	t.Setenv("PILOTHOUSE_USER_HOME", t.TempDir())
	run, err := os.MkdirTemp("/tmp", "ag")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PILOTHOUSE_RUN_DIR", run)
	t.Setenv("PILOTHOUSE_NO_UPDATE_CHECK", "1")
	t.Cleanup(func() {
		killHolders(t)
		os.RemoveAll(run)
	})
}

// killHolders SIGKILLs every holder (and its agent's process group) still
// answering in the run dir.
func killHolders(t *testing.T) {
	hs, _ := ListHolders(context.Background())
	for _, h := range hs {
		if h.PID > 0 {
			_ = syscall.Kill(-h.PID, syscall.SIGKILL)
		}
		if h.HolderPID > 0 && h.HolderPID != os.Getpid() {
			_ = syscall.Kill(h.HolderPID, syscall.SIGKILL)
		}
	}
}

var (
	binOnce sync.Once
	binPath string
	binErr  error
	binDir  string
)

// pilothouseBin builds the pilothouse binary once per test run.
func pilothouseBin(t *testing.T) string {
	t.Helper()
	binOnce.Do(func() {
		binDir, binErr = os.MkdirTemp("", "pilothouse-bin")
		if binErr != nil {
			return
		}
		_, file, _, _ := runtime.Caller(0)
		root := filepath.Join(filepath.Dir(file), "..", "..")
		binPath = filepath.Join(binDir, "pilothouse")
		cmd := exec.Command("go", "build", "-race", "-o", binPath, "./cmd/pilothouse")
		cmd.Dir = root
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			binErr = &buildError{out.String()}
		}
	})
	if binErr != nil {
		t.Fatalf("build pilothouse: %v", binErr)
	}
	return binPath
}

type buildError struct{ out string }

func (e *buildError) Error() string { return e.out }

func TestMain(m *testing.M) {
	code := m.Run()
	if binDir != "" {
		os.RemoveAll(binDir)
	}
	os.Exit(code)
}

// fakeDaemon serves the daemon routes a holder uses on paths.Socket().
type fakeDaemon struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []recorded
	// fail makes every request return 503 (daemon "busy"), to exercise retry.
	fail bool
	// pid is what /v1/health reports; health checks are counted, not
	// recorded in reqs.
	pid    int
	health int
}

type recorded struct {
	Method, Path string
	Body         []byte
}

func startFakeDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	fd := &fakeDaemon{pid: 1000}
	ln, err := net.Listen("unix", paths.Socket())
	if err != nil {
		t.Fatal(err)
	}
	fd.srv = httptest.NewUnstartedServer(http.HandlerFunc(fd.serve))
	fd.srv.Listener = ln
	fd.srv.Start()
	t.Cleanup(fd.srv.Close)
	return fd
}

func (fd *fakeDaemon) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if r.URL.Path == "/v1/health" {
		fd.mu.Lock()
		fd.health++
		pid := fd.pid
		fd.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.Health{Version: "test", Protocol: 1, PID: pid})
		return
	}
	fd.mu.Lock()
	fd.reqs = append(fd.reqs, recorded{r.Method, r.URL.Path, body})
	fail := fd.fail
	fd.mu.Unlock()
	if fail {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	switch {
	case r.URL.Path == "/v1/sessions/wrapped":
		var b api.WrappedSessionBody
		_ = json.Unmarshal(body, &b)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.Session{ID: b.SessionID, Mode: api.ModeWrapped, HolderSock: b.HolderSock})
	case strings.HasSuffix(r.URL.Path, "/osc"), strings.HasSuffix(r.URL.Path, "/exit"):
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(api.Error{Error: "not found", Code: "not_found"})
	}
}

func (fd *fakeDaemon) setPID(pid int) {
	fd.mu.Lock()
	fd.pid = pid
	fd.mu.Unlock()
}

func (fd *fakeDaemon) healthChecks() int {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	return fd.health
}

func (fd *fakeDaemon) requests() []recorded {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	return append([]recorded(nil), fd.reqs...)
}

// waitFor polls cond until it holds or the timeout passes.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// session drives an attached Conn: it collects output in the background.
type session struct {
	t      *testing.T
	c      *Conn
	mu     sync.Mutex
	out    bytes.Buffer
	frames []Frame // non-data frames
	done   chan struct{}
	err    error
}

func attachSession(t *testing.T, sock string, replay bool) *session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, sock, Hello{Mode: ModeAttach, Replay: replay, Cols: 100, Rows: 30})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	s := &session{t: t, c: c, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		for {
			f, err := c.Recv()
			s.mu.Lock()
			if err != nil {
				s.err = err
				s.mu.Unlock()
				return
			}
			if f.Type == FrameData {
				s.out.Write(f.Payload)
			} else {
				s.frames = append(s.frames, f)
			}
			s.mu.Unlock()
		}
	}()
	t.Cleanup(func() { c.Close(); <-s.done })
	return s
}

func (s *session) output() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.out.String()
}

func (s *session) waitOutput(substr string) {
	s.t.Helper()
	waitFor(s.t, 10*time.Second, "output "+strings.TrimSpace(substr), func() bool { return strings.Contains(s.output(), substr) })
}

// waitFrame waits for a control frame of type ft and returns it.
func (s *session) waitFrame(ft FrameType) Frame {
	s.t.Helper()
	var got Frame
	waitFor(s.t, 15*time.Second, ft.String()+" frame", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, f := range s.frames {
			if f.Type == ft {
				got = f
				return true
			}
		}
		return false
	})
	return got
}

func (s *session) send(in string) {
	s.t.Helper()
	if err := s.c.Send(FrameData, []byte(in)); err != nil {
		s.t.Fatalf("send: %v", err)
	}
}

// fakeAgent prints a status sequence and a prompt, echoes one line, reports
// idle and exits 3.
const fakeAgent = `printf 'ready\033]7501;state=working;app=fake\007\n'; ` +
	`read line; echo "got:$line"; printf '\033]7501;state=idle\033\\'; exit 3`
