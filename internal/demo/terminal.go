package demo

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
)

// The live terminal of wrapped sessions: an ANSI rendering of the
// session's activity in the style of the agent's own TUI, a spinner while
// it works, and an input line that turns into a reply on Enter.

// maxScrollback bounds what a terminal keeps for replay.
const maxScrollback = 128 << 10

// maxTerminalInput bounds one TerminalInput call.
const maxTerminalInput = 4096

type term struct {
	agent    api.AgentKind
	buf      []byte
	watchers map[chan []byte]struct{}
	line     []rune // what the person is typing
	frame    int
	cols     uint16
	rows     uint16
}

const (
	esc     = "\x1b["
	reset   = esc + "0m"
	bold    = esc + "1m"
	dim     = esc + "2m"
	red     = esc + "31m"
	green   = esc + "32m"
	yellow  = esc + "33m"
	cyan    = esc + "36m"
	gray    = esc + "90m"
	clay    = esc + "38;5;173m"
	magenta = esc + "35m"
	clearLn = "\r" + esc + "2K"
	crlf    = "\r\n"
)

// openTermLocked creates a session's terminal with the agent's banner.
func (c *Core) openTermLocked(s api.Session) *term {
	t := &term{agent: s.Agent, watchers: map[chan []byte]struct{}{}, cols: 100, rows: 32}
	c.terms[s.ID] = t
	switch s.Agent {
	case api.AgentCodex:
		c.writeLocked(s.ID, crlf+bold+">_ OpenAI Codex"+reset+dim+" (v0.48.0)"+reset+crlf+
			dim+"   model:     "+reset+"gpt-5-codex high"+crlf+
			dim+"   directory: "+reset+tilde(s.Cwd)+crlf+
			dim+"   approval:  "+reset+"on-request · Pilothouse is watching"+crlf)
	default:
		inner := 52
		line := func(s string) string {
			pad := inner - utf8.RuneCountInString(stripANSI(s))
			if pad < 0 {
				pad = 0
			}
			return clay + "│" + reset + " " + s + strings.Repeat(" ", pad) + clay + "│" + reset + crlf
		}
		c.writeLocked(s.ID, clay+"╭"+strings.Repeat("─", inner+1)+"╮"+reset+crlf+
			line(clay+"✻"+reset+" Welcome to "+bold+"Claude Code"+reset+"!")+
			line("")+
			line(dim+"  /help for help, /status for your current setup"+reset)+
			line("")+
			line(dim+"  cwd: "+tilde(s.Cwd)+reset)+
			clay+"╰"+strings.Repeat("─", inner+1)+"╯"+reset+crlf)
	}
	return t
}

// writeLocked appends to the scrollback and fans out to watchers.
func (c *Core) writeLocked(sid, s string) {
	t := c.terms[sid]
	if t == nil || s == "" {
		return
	}
	t.buf = append(t.buf, s...)
	if len(t.buf) > maxScrollback {
		cut := len(t.buf) - maxScrollback
		// Start the kept part at a line boundary so no escape is cut.
		if i := strings.Index(string(t.buf[cut:]), "\n"); i >= 0 {
			cut += i + 1
		}
		t.buf = append([]byte(nil), t.buf[cut:]...)
	}
	c.broadcastLocked(t, s)
}

func (c *Core) broadcastLocked(t *term, s string) {
	for ch := range t.watchers {
		select {
		case ch <- []byte(s):
		default: // a watcher that can't keep up misses a frame
		}
	}
}

// renderLocked writes one activity item the way the agent's TUI shows it.
func (c *Core) renderLocked(sid string, it api.ActivityItem) {
	t := c.terms[sid]
	if t == nil || it.Sidechain {
		return
	}
	codex := t.agent == api.AgentCodex
	var b strings.Builder
	b.WriteString(clearLn)
	switch it.Kind {
	case api.ActivityPrompt:
		b.WriteString(crlf + gray + "> " + reset + wrap(it.Text, 4))
		if it.Source == "pilothouse" {
			b.WriteString(crlf + dim + "  ⎿  sent from Pilothouse" + reset)
		}
		b.WriteString(crlf)
	case api.ActivityText:
		if codex {
			b.WriteString(crlf + magenta + "codex" + reset + crlf + wrap(it.Text, 0) + crlf)
		} else {
			b.WriteString(crlf + "⏺ " + wrap(it.Text, 2) + crlf)
		}
	case api.ActivityTool:
		if codex {
			verb, arg := codexVerb(it)
			b.WriteString(crlf + bold + "•" + reset + " " + bold + verb + reset + " " + arg + crlf)
		} else {
			name, arg := claudeTool(it)
			b.WriteString(crlf + green + "⏺" + reset + " " + bold + name + reset + "(" + arg + ")" + crlf)
		}
		if it.Pending {
			b.WriteString(c.pendingLine(sid, it, codex))
		} else {
			b.WriteString(resultLines(it, codex))
		}
	case api.ActivitySubagent:
		b.WriteString(crlf + cyan + "⏺" + reset + " " + bold + "Task" + reset + "(" + clip(it.Summary, 60) + ")" + crlf)
		if it.Text != "" {
			b.WriteString(dim + "  ⎿  " + reset + clip(it.Text, 90) + crlf)
		}
	case api.ActivityError:
		b.WriteString(crlf + red + "⏺ " + wrap(it.Text, 2) + reset + crlf)
	case api.ActivitySystem:
		b.WriteString(crlf + dim + "  " + it.Text + reset + crlf)
	}
	c.writeLocked(sid, b.String())
}

// renderResultLocked shows the result of a tool call that was pending.
func (c *Core) renderResultLocked(sid string, it api.ActivityItem) {
	t := c.terms[sid]
	if t == nil {
		return
	}
	c.writeLocked(sid, clearLn+resultLines(it, t.agent == api.AgentCodex))
}

// pendingLine is shown under a tool call without a result: held for a
// person (the session is blocked on it) or still running.
func (c *Core) pendingLine(sid string, it api.ActivityItem, codex bool) string {
	lead := "  " + dim + "⎿  " + reset
	if codex {
		lead = "  " + dim + "└ " + reset
	}
	if s, ok := c.session(sid); ok && s.State == api.StateBlocked {
		return lead + yellow + "Waiting for you — Pilothouse sent it to your devices" + reset + crlf
	}
	return lead + dim + "Running…" + reset + crlf
}

func resultLines(it api.ActivityItem, codex bool) string {
	lead := "  " + dim + "⎿  " + reset
	cont := "     "
	if codex {
		lead, cont = "  "+dim+"└ "+reset, "    "
	}
	res := strings.TrimRight(it.Result, "\n")
	if res == "" {
		res = "Done"
	}
	lines := strings.Split(res, "\n")
	more := 0
	if len(lines) > 5 {
		more = len(lines) - 4
		lines = lines[:4]
	}
	var b strings.Builder
	for i, l := range lines {
		if i == 0 {
			b.WriteString(lead)
		} else {
			b.WriteString(cont)
		}
		switch {
		case it.IsError:
			b.WriteString(red + l + reset)
		case it.ToolKind == api.ToolEdit && strings.HasPrefix(l, "+"):
			b.WriteString(green + l + reset)
		case it.ToolKind == api.ToolEdit && strings.HasPrefix(l, "-"):
			b.WriteString(red + l + reset)
		default:
			b.WriteString(l)
		}
		b.WriteString(crlf)
	}
	if more > 0 {
		b.WriteString(cont + dim + fmt.Sprintf("… +%d lines (ctrl+r to expand)", more) + reset + crlf)
	}
	return b.String()
}

func claudeTool(it api.ActivityItem) (string, string) {
	name := it.Tool
	arg := it.Summary
	for _, p := range []string{"Run: ", "Edit ", "Read ", "Write ", "Search ", "Fetch "} {
		arg = strings.TrimPrefix(arg, p)
	}
	if name == "Edit" || name == "MultiEdit" {
		name = "Update"
	}
	return name, clip(arg, 70)
}

func codexVerb(it api.ActivityItem) (string, string) {
	arg := it.Summary
	for _, p := range []string{"Run: ", "Edit ", "Read ", "Write "} {
		arg = strings.TrimPrefix(arg, p)
	}
	switch it.ToolKind {
	case api.ToolEdit:
		return "Edited", arg
	case api.ToolRead:
		return "Explored", arg
	default:
		if it.Pending {
			return "Running", arg
		}
		return "Ran", arg
	}
}

// wrap breaks text at ~88 columns, indenting continuation lines.
func wrap(s string, indent int) string {
	const width = 88
	var out []string
	for _, para := range strings.Split(s, "\n") {
		line := ""
		for _, w := range strings.Fields(para) {
			if line != "" && utf8.RuneCountInString(line)+1+utf8.RuneCountInString(w) > width-indent {
				out = append(out, line)
				line = w
				continue
			}
			if line != "" {
				line += " "
			}
			line += w
		}
		out = append(out, line)
	}
	return strings.Join(out, crlf+strings.Repeat(" ", indent))
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func tilde(p string) string {
	if strings.HasPrefix(p, Home) {
		return "~" + strings.TrimPrefix(p, Home)
	}
	return p
}

var spinFrames = []string{"·", "✢", "✳", "✶", "✻", "✽", "✻", "✶", "✳", "✢"}
var spinWords = []string{"Thinking", "Pondering", "Crunching", "Brewing", "Noodling"}

// tickTerminals animates the spinner of working sessions that someone is
// watching. Frames go to watchers only, never into the scrollback.
func (c *Core) tickTerminals() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for sid, t := range c.terms {
		if len(t.watchers) == 0 || len(t.line) > 0 {
			continue
		}
		s, ok := c.session(sid)
		if !ok || s.State != api.StateWorking {
			continue
		}
		t.frame++
		secs := int(c.now().Sub(s.UpdatedAt).Seconds())
		if secs < 0 {
			secs = 0
		}
		toks := 0.0
		if s.Usage != nil {
			toks = float64(s.Usage.OutputTokens) / 1000
		}
		if t.agent == api.AgentCodex {
			c.broadcastLocked(t, clearLn+bold+"•"+reset+" "+dim+fmt.Sprintf("Working (%ds · esc to interrupt)", secs)+reset)
			continue
		}
		word := spinWords[(t.frame/40)%len(spinWords)]
		c.broadcastLocked(t, clearLn+clay+spinFrames[t.frame%len(spinFrames)]+" "+word+"…"+reset+" "+
			dim+fmt.Sprintf("(%ds · ↓ %.1fk tokens · esc to interrupt)", secs, toks)+reset)
	}
}

// WatchTerminal streams a wrapped session's terminal, starting with the
// scrollback when replay is set, until ctx ends or the session exits.
func (c *Core) WatchTerminal(ctx context.Context, sessionID string, replay bool, out func([]byte) error) error {
	s, err := c.FakeCore.Session(ctx, sessionID)
	if err != nil {
		return err
	}
	c.mu.Lock()
	t := c.terms[s.ID]
	if t == nil || !s.Caps.Watch {
		c.mu.Unlock()
		return fmt.Errorf("%w: only sessions started with pilothouse run have a live terminal", core.ErrUnsupported)
	}
	ch := make(chan []byte, 256)
	t.watchers[ch] = struct{}{}
	var first []byte
	if replay {
		first = append([]byte(nil), t.buf...)
	}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(t.watchers, ch)
		c.mu.Unlock()
	}()
	if len(first) > 0 {
		if err := out(first); err != nil {
			return err
		}
	}
	poll := time.NewTicker(time.Second)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case b := <-ch:
			if err := out(b); err != nil {
				return err
			}
		case <-poll.C:
			if s, ok := c.session(s.ID); !ok || s.State == api.StateExited {
				return nil
			}
		}
	}
}

// TerminalInput types into the session: printable text echoes on the
// input line, Enter sends it as a reply, Esc or Ctrl-C interrupts.
func (c *Core) TerminalInput(ctx context.Context, sessionID string, data []byte, by string) error {
	s, err := c.FakeCore.Session(ctx, sessionID)
	if err != nil {
		return err
	}
	if len(data) > maxTerminalInput {
		return core.Invalid("too much input at once")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.terms[s.ID]
	if t == nil || !s.Caps.Watch || s.State == api.StateExited {
		return fmt.Errorf("%w: this session has no live terminal", core.ErrUnsupported)
	}
	in := string(data)
	for len(in) > 0 {
		r, size := utf8.DecodeRuneInString(in)
		in = in[size:]
		switch {
		case r == '\r' || r == '\n':
			text := strings.TrimSpace(string(t.line))
			t.line = nil
			c.broadcastLocked(t, clearLn)
			if text != "" {
				c.replyLocked(s, api.Reply{Text: text}, by)
			}
		case r == 0x1b || r == 0x03:
			// Esc alone or Ctrl-C; an escape sequence (arrow keys) is skipped.
			if r == 0x1b && strings.HasPrefix(in, "[") {
				if i := strings.IndexFunc(in[1:], func(r rune) bool { return r >= 0x40 && r <= 0x7e }); i >= 0 {
					in = in[i+2:]
				} else {
					in = ""
				}
				continue
			}
			t.line = nil
			c.replyLocked(s, api.Reply{Interrupt: true}, by)
		case r == 0x7f || r == '\b':
			if len(t.line) > 0 {
				t.line = t.line[:len(t.line)-1]
			}
			c.broadcastLocked(t, clearLn+gray+"> "+reset+string(t.line))
		case r >= 0x20 && r != utf8.RuneError:
			if len(t.line) < 2000 {
				t.line = append(t.line, r)
			}
			c.broadcastLocked(t, clearLn+gray+"> "+reset+string(t.line))
		}
	}
	return nil
}

// ResizeTerminal records the size; the demo's output doesn't reflow.
func (c *Core) ResizeTerminal(ctx context.Context, sessionID string, cols, rows uint16) error {
	s, err := c.FakeCore.Session(ctx, sessionID)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.terms[s.ID]
	if t == nil {
		return fmt.Errorf("%w: this session has no live terminal", core.ErrUnsupported)
	}
	if cols > 0 && rows > 0 {
		t.cols, t.rows = cols, rows
	}
	return nil
}
