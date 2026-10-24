package daemon

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/paths"
	"github.com/xasdta/pilothouse/internal/preview"
	"github.com/xasdta/pilothouse/internal/risk"
)

// enrichRequest adds what helps a person decide at a glance to a request
// about to be opened: risk labels, a preview of file changes and the
// agent's last words (F4; workstream D). It must be fast and never fail:
// on any problem it leaves the fields empty. Budget: well under a few
// milliseconds — risk is pure CPU, the preview reads at most a few
// bounded files, the context reads one transcript tail.
func (d *Daemon) enrichRequest(req *api.Request, h *hookCall) {
	defer func() {
		if p := recover(); p != nil {
			d.log.Error("enriching a request panicked; showing it without labels", "request", req.ID, "panic", p)
		}
	}()
	req.Risk = risk.Classify(risk.Call{
		Tool: req.Tool, Kind: req.ToolKind, Command: req.Command, Paths: req.Paths, Input: req.Input,
		Cwd: h.cwd, Repo: h.root, Branch: h.branch,
	})
	if req.ToolKind == api.ToolEdit || req.ToolKind == api.ToolShell {
		req.Preview = preview.Build(preview.Call{
			Tool: req.Tool, Kind: req.ToolKind, Command: req.Command, Input: req.Input, Cwd: h.cwd, Repo: h.root,
		})
	}
	req.Context = lastWords(h.ev.TranscriptPath)
}

const (
	// transcriptTail is how much of a transcript's end is read for the
	// agent's last words.
	transcriptTail = 64 << 10
	maxContext     = 300 // runes
)

// transcriptRoots are the only places transcripts are read from: the
// path comes from the hook payload, which the agent controls.
func transcriptRoots() []string {
	home := paths.UserHome()
	codex := os.Getenv("CODEX_HOME")
	if codex == "" {
		codex = filepath.Join(home, ".codex")
	}
	return []string{filepath.Join(home, ".claude", "projects"), filepath.Join(codex, "sessions")}
}

// confinedTranscript returns p if it is a regular .jsonl file inside a
// transcript root, symlinks resolved on both sides.
func confinedTranscript(p string) (string, bool) {
	if p == "" || !filepath.IsAbs(p) || filepath.Ext(p) != ".jsonl" {
		return "", false
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(p))
	if err != nil {
		return "", false
	}
	for _, root := range transcriptRoots() {
		if r, err := filepath.EvalSymlinks(root); err == nil {
			root = r
		}
		if strings.HasPrefix(real, root+string(filepath.Separator)) {
			if fi, err := os.Stat(real); err == nil && fi.Mode().IsRegular() {
				return real, true
			}
		}
	}
	return "", false
}

// lastWords is the last thing the agent wrote to the person in its
// current turn, from the end of its transcript (Claude Code projects
// JSONL or a Codex rollout), trimmed and with credentials redacted. It is
// "" when the agent said nothing since the person's last prompt.
func lastWords(transcript string) string {
	p, ok := confinedTranscript(transcript)
	if !ok {
		return ""
	}
	// Non-blocking: a FIFO swapped in after the check must not hang the hook.
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	off := max(fi.Size()-transcriptTail, 0)
	buf := make([]byte, fi.Size()-off)
	n, err := f.ReadAt(buf, off)
	if err != nil && err != io.EOF {
		return ""
	}
	buf = buf[:n]
	if off > 0 { // the first line is cut
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		}
	}
	lines := bytes.Split(buf, []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		text, human := transcriptLine(lines[i])
		if human {
			return ""
		}
		if text != "" {
			return trimContext(text)
		}
	}
	return ""
}

// transcriptLine reads one transcript record: assistant text, or whether
// it is a person's prompt (which ends the search).
func transcriptLine(line []byte) (text string, human bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return "", false
	}
	var rec struct {
		Type    string `json:"type"`
		Message *struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		Payload *struct {
			Type    string          `json:"type"`
			Role    string          `json:"role"`
			Message string          `json:"message"`
			Content json.RawMessage `json:"content"`
		} `json:"payload"`
		IsSidechain bool `json:"isSidechain"`
	}
	if json.Unmarshal(line, &rec) != nil || rec.IsSidechain {
		return "", false
	}
	switch {
	case rec.Message != nil && rec.Type == "assistant": // Claude Code
		return lastText(rec.Message.Content, "text"), false
	case rec.Message != nil && rec.Type == "user":
		// Tool results come back as user records; a person's prompt is a
		// string or has text blocks.
		var s string
		if json.Unmarshal(rec.Message.Content, &s) == nil {
			return "", strings.TrimSpace(s) != ""
		}
		return "", lastText(rec.Message.Content, "text") != ""
	case rec.Payload != nil && rec.Type == "response_item" && rec.Payload.Type == "message": // Codex
		switch rec.Payload.Role {
		case "assistant":
			return lastText(rec.Payload.Content, "output_text"), false
		case "user":
			return "", true
		}
	case rec.Payload != nil && rec.Type == "event_msg":
		switch rec.Payload.Type {
		case "agent_message":
			return rec.Payload.Message, false
		case "user_message":
			return "", true
		}
	}
	return "", false
}

// lastText is the last content block of the given type with text.
func lastText(content json.RawMessage, typ string) string {
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	for i := len(blocks) - 1; i >= 0; i-- {
		if blocks[i].Type == typ && strings.TrimSpace(blocks[i].Text) != "" {
			return blocks[i].Text
		}
	}
	return ""
}

// trimContext keeps the end of the agent's words — the part just before
// the tool call — on one line, without credentials.
func trimContext(s string) string {
	s = risk.Redact(strings.Join(strings.Fields(s), " "))
	if utf8.RuneCountInString(s) <= maxContext {
		return s
	}
	r := []rune(s)
	cut := string(r[len(r)-maxContext+1:])
	if i := strings.IndexByte(cut, ' '); i >= 0 && i < 40 {
		cut = cut[i+1:] // start at a word
	}
	return "…" + cut
}
