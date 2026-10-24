package web

import (
	"strings"
	"testing"
)

// The live terminal loads xterm.js from static/vendor (the CSP allows
// only this origin). Module scripts are refused unless they are served
// with a JavaScript type (nosniff), so the types matter.
func TestVendoredXtermServed(t *testing.T) {
	e := newEnv(t)
	for path, ct := range map[string]string{
		"/vendor/xterm/xterm.mjs":         "text/javascript",
		"/vendor/xterm/addon-fit.mjs":     "text/javascript",
		"/vendor/xterm/xterm.css":         "text/css",
		"/vendor/xterm/LICENSE":           "text/plain",
		"/vendor/xterm/LICENSE-addon-fit": "text/plain",
		"/js/views/terminal.js":           "text/javascript",
		"/js/views/session.js":            "text/javascript",
		"/js/views/tasks.js":              "text/javascript",
		"/js/views/insights.js":           "text/javascript",
		"/css/views.css":                  "text/css",
	} {
		w := e.do("GET", path, nil, reqOpt{})
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), ct) {
			t.Errorf("%s: %d %q, want %s", path, w.Code, w.Header().Get("Content-Type"), ct)
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: no nosniff", path)
		}
	}
	w := e.do("GET", "/vendor/xterm/xterm.mjs", nil, reqOpt{})
	if !strings.Contains(w.Body.String(), "export{") || !strings.Contains(w.Body.String(), " as Terminal}") {
		t.Error("xterm.mjs is not the ES module build")
	}
	if !strings.Contains(e.do("GET", "/vendor/xterm/LICENSE", nil, reqOpt{}).Body.String(), "Permission is hereby granted") {
		t.Error("xterm LICENSE missing its text")
	}
}

// Every script the app loads comes from this origin: no CDN URLs.
func TestViewsLoadNothingCrossOrigin(t *testing.T) {
	for _, p := range []string{"static/js/views/terminal.js", "static/js/views/session.js", "static/js/views/tasks.js", "static/js/views/insights.js"} {
		b, err := staticFS.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		src := strings.ReplaceAll(string(b), "http://www.w3.org/2000/svg", "") // the SVG namespace, not a URL to load
		for _, bad := range []string{"https://", "http://", "//cdn", "unpkg", "jsdelivr"} {
			if strings.Contains(src, bad) {
				t.Errorf("%s mentions %q", p, bad)
			}
		}
	}
}
