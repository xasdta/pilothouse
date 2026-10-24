package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// staticJS lists the app's own scripts (vendored libraries excluded).
func staticJS(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && strings.HasPrefix(p, "static/vendor") {
			return fs.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".js") {
			return nil
		}
		b, err := staticFS.ReadFile(p)
		out["/"+strings.TrimPrefix(p, "static/")] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The service worker installs only if every precached URL loads, and
// works offline only if every module the app imports is precached.
func TestServiceWorkerPrecachesTheShell(t *testing.T) {
	sw, err := staticFS.ReadFile("static/sw.js")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)const SHELL = \[(.*?)\];`).FindSubmatch(sw)
	if m == nil {
		t.Fatal("sw.js has no SHELL list")
	}
	shell := map[string]bool{}
	for _, q := range regexp.MustCompile(`'([^']+)'`).FindAllSubmatch(m[1], -1) {
		shell[string(q[1])] = true
	}
	e := newEnv(t)
	for p := range shell {
		if w := e.do("GET", p, nil, reqOpt{}); w.Code != 200 {
			t.Errorf("precached %s answers %d: the service worker would fail to install", p, w.Code)
		}
	}
	for p := range staticJS(t) {
		if !shell[p] && p != "/sw.js" {
			t.Errorf("%s is not precached in sw.js SHELL (the app breaks offline)", p)
		}
	}
	err = fs.WalkDir(staticFS, "static/css", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !shell["/"+strings.TrimPrefix(p, "static/")] {
			t.Errorf("%s is not precached in sw.js SHELL", p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Agent text (commands, plans, questions, diffs) is untrusted: the app
// builds DOM nodes from text and never parses strings as HTML or code.
func TestStaticJSNeverInjectsHTML(t *testing.T) {
	bad := regexp.MustCompile(`\.innerHTML\b|\.outerHTML\b|insertAdjacentHTML|document\.write|\bnew Function\b|\beval\(|createContextualFragment|DOMParser`)
	for p, src := range staticJS(t) {
		for i, line := range strings.Split(src, "\n") {
			if bad.MatchString(line) {
				t.Errorf("%s:%d: %s", p, i+1, strings.TrimSpace(line))
			}
		}
	}
}

func TestClientRoutesServeTheShell(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/rules", "/insights", "/tasks", "/new", "/t/t_1", "/s/s_1/terminal", "/js/theme.js", "/js/diff.js", "/js/markdown.js", "/js/palette.js"} {
		if w := e.do("GET", p, nil, reqOpt{}); w.Code != 200 {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	w := e.do("GET", "/", nil, reqOpt{})
	// theme.js runs before first paint, so it is a classic, blocking script.
	if !strings.Contains(w.Body.String(), `<script src="/js/theme.js"></script>`) {
		t.Error("index.html must load /js/theme.js synchronously in <head>")
	}
}
