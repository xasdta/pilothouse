package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/xasdta/pilothouse/internal/api"
)

const minWidth = 60

// columns of a list row for a given terminal width. Narrow terminals drop
// the hint column (the detail pane and footer still say what to do) and
// shorten the repo column before the command, which matters most.
type columns struct {
	agent, where, age, hint int
}

func columnsFor(w int) columns {
	switch {
	case w >= 110:
		return columns{agent: 8, where: 26, age: 8, hint: 17}
	case w >= 90:
		return columns{agent: 8, where: 22, age: 8, hint: 16}
	case w >= 75:
		return columns{agent: 8, where: 18, age: 7, hint: 0}
	default:
		return columns{agent: 8, where: 14, age: 7, hint: 0}
	}
}

func (m *Model) size() (int, int) {
	w, h := m.width, m.height
	if w <= 0 {
		w = 100
	}
	if h <= 0 {
		h = 30
	}
	return w, h
}

// layout splits the body between the list and the detail pane.
func (m *Model) layout() (listH, detailH int) {
	_, h := m.size()
	body := h - 4 // header, blank, flash, footer
	if m.bannerText() != "" {
		body--
	}
	switch {
	case body < 8:
		detailH = 0
	case body < 22:
		detailH = 6
	case m.expanded:
		detailH = max(9, body/2)
	default:
		detailH = 8
	}
	return max(1, body-detailH), detailH
}

func (m *Model) listHeight() int { l, _ := m.layout(); return l }

// listLines renders the list; idx[i] is the row index of line i or -1.
func (m *Model) listLines(w int) (lines []string, idx []int) {
	add := func(s string, i int) {
		lines = append(lines, s)
		idx = append(idx, i)
	}
	ri := 0
	for sec := 0; sec < numSections; sec++ {
		n := m.counts[sec]
		if n == 0 && sec != sectionNeeds {
			continue
		}
		if len(lines) > 0 {
			add("", -1)
		}
		title := m.st.section.Render(sectionTitles[sec])
		if sec == sectionNeeds && n > 0 {
			title = m.st.needs.Render(sectionTitles[sec])
		}
		add(" "+title+" "+m.st.count.Render(fmt.Sprint(n)), -1)
		if n == 0 {
			add("   "+m.st.dim.Render("Nothing needs you right now."), -1)
			if len(m.rows) == 0 {
				add("", -1)
				add("   "+m.st.dim.Render("No agent sessions yet. Start Claude Code or Codex as usual and they"), -1)
				add("   "+m.st.dim.Render("show up here, or run one you can detach from: ")+m.st.bold.Render("pilothouse run claude"), -1)
			}
			continue
		}
		for ; ri < len(m.rows) && m.rows[ri].section == sec; ri++ {
			add(m.renderRow(m.rows[ri], w, ri == m.cursor), ri)
		}
	}
	return lines, idx
}

// selLines returns the line of the selected row and the line to keep
// visible above it (its section header, when it is the section's first row).
func selLines(idx []int, cursor int) (sel, top int) {
	sel, top = -1, -1
	for i, r := range idx {
		if r == cursor && r >= 0 {
			sel, top = i, i
			if i > 0 && idx[i-1] == -1 {
				top = i - 1
			}
			break
		}
	}
	return sel, top
}

// window picks the visible slice [start,end) of n list lines in h rows,
// reserving a row for "↑ more" / "↓ more" indicators when they're needed
// and keeping lines top..sel visible.
func window(n, h, scroll, sel, top int) (start, end int, up, down bool) {
	if n <= h {
		return 0, n, false, false
	}
	start = max(0, min(scroll, n-1))
	var avail int
	for i := 0; i < 8; i++ {
		up = start > 0 && h >= 3
		avail = h
		if up {
			avail--
		}
		down = start+avail < n && h >= 3
		if down {
			avail--
		}
		switch {
		case top >= 0 && top < start:
			start = top
		case sel >= 0 && sel >= start+avail:
			start = sel - avail + 1
		case start+avail > n:
			start = max(0, n-avail)
		default:
			return start, min(n, start+avail), up, down
		}
	}
	return start, min(n, start+avail), up, down
}

// clampScroll keeps the selected row visible.
func (m *Model) clampScroll() {
	w, _ := m.size()
	_, idx := m.listLines(w)
	sel, top := selLines(idx, m.cursor)
	m.scroll, _, _, _ = window(len(idx), m.listHeight(), m.scroll, sel, top)
}

func (m *Model) rowGlyph(r row) (string, lipgloss.Style) {
	if r.req != nil {
		if r.req.Status == api.RequestOpen {
			return "●", m.st.needs
		}
		return "○", m.st.needs
	}
	s := r.sess
	switch s.State {
	case api.StateBlocked:
		return "○", m.st.needs
	case api.StateWorking:
		return "●", m.st.working
	case api.StateStarting:
		return "◌", m.st.working
	case api.StateWaiting:
		return "●", m.st.waiting
	case api.StateErrored:
		return "✗", m.st.errored
	case api.StateExited:
		if s.ExitCode != nil && *s.ExitCode != 0 {
			return "✗", m.st.exited
		}
		return "✓", m.st.exited
	}
	return "?", m.st.dim
}

func (m *Model) rowWhat(r row) string {
	if r.req != nil {
		return requestWhat(*r.req)
	}
	s := r.sess
	switch s.State {
	case api.StateExited:
		code := ""
		if s.ExitCode != nil {
			code = fmt.Sprintf(" (code %d)", *s.ExitCode)
		}
		if s.LastMessage != "" {
			return "exited" + code + " · " + oneLine(s.LastMessage)
		}
		return "exited" + code
	case api.StateWaiting:
		if s.LastMessage != "" {
			return oneLine(s.LastMessage)
		}
		return "Finished its turn"
	case api.StateBlocked:
		if s.StateDetail != "" {
			return oneLine(s.StateDetail)
		}
		switch s.BlockKind {
		case api.BlockQuestion:
			return "Asked a question"
		case api.BlockAuth:
			return "Needs you to sign in"
		}
		return "Waiting for permission"
	case api.StateStarting:
		return "Starting…"
	case api.StateUnknown:
		return "No news for a while"
	}
	if s.StateDetail != "" {
		return oneLine(s.StateDetail)
	}
	if s.LastMessage != "" {
		return oneLine(s.LastMessage)
	}
	return string(s.State)
}

func (m *Model) rowAge(r row) time.Duration {
	if r.req != nil {
		return m.now.Sub(r.req.CreatedAt)
	}
	if r.sess.State == api.StateExited && r.sess.ExitedAt != nil {
		return m.now.Sub(*r.sess.ExitedAt)
	}
	return m.now.Sub(r.sess.UpdatedAt)
}

// rowHint is the short "what can I do" column.
func (m *Model) rowHint(r row) (string, lipgloss.Style) {
	if r.req != nil {
		if m.pending[r.req.ID] {
			return "sending…", m.st.dim
		}
		if r.req.Status == api.RequestOpen {
			return "held · y a n", m.st.key
		}
		if attachable(r.sess) {
			return "enter to attach", m.st.dim
		}
		return "in its terminal", m.st.dim
	}
	switch {
	case r.sess.State == api.StateBlocked && attachable(r.sess):
		return "enter to attach", m.st.dim
	case r.sess.State == api.StateBlocked:
		return "in its terminal", m.st.dim
	case attachable(r.sess):
		return "enter to attach", m.st.faint
	}
	return "", m.st.faint
}

func (m *Model) renderRow(r row, w int, sel bool) string {
	c := columnsFor(w)
	base := m.st.r.NewStyle()
	if sel {
		base = m.st.sel
	}
	seg := func(s string, st lipgloss.Style) string { return st.Inherit(base).Render(s) }

	glyph, gst := m.rowGlyph(r)
	bar := seg(" ", base)
	if sel {
		bar = m.st.selBar.Render("▌")
	}
	var sess *api.Session
	cwd := ""
	agent := api.AgentKind("")
	if r.req != nil {
		sess, cwd, agent = r.sess, r.req.Cwd, r.req.Agent
	} else {
		sess, agent = r.sess, r.sess.Agent
	}
	if agent == "" && sess != nil {
		agent = sess.Agent
	}
	whatW := w - 4 - c.agent - c.where - c.age - c.hint
	whatSt := m.st.text
	switch {
	case r.req != nil:
		whatSt = m.st.cmd
	case r.sess.State == api.StateExited:
		whatSt = m.st.dim
	case r.sess.Unseen:
		whatSt = m.st.bold
	}
	agentSt := m.st.dim
	if r.section == sectionExited {
		agentSt = m.st.faint
	}
	hint, hst := m.rowHint(r)

	var b strings.Builder
	b.WriteString(bar)
	b.WriteString(seg(" ", base))
	b.WriteString(seg(glyph, gst))
	b.WriteString(seg(" ", base))
	b.WriteString(seg(fit(agentName(agent), c.agent-1)+" ", agentSt))
	b.WriteString(seg(fit(whereFit(sess, cwd, c.where-1), c.where-1)+" ", m.st.text))
	b.WriteString(seg(fit(m.rowWhat(r), whatW-1)+" ", whatSt))
	b.WriteString(seg(fmt.Sprintf("%*s", c.age-2, Age(m.rowAge(r)))+"  ", m.st.dim))
	if c.hint > 0 {
		b.WriteString(seg(fit(hint, c.hint), hst))
	}
	return b.String()
}

// View implements tea.Model.
func (m *Model) View() string {
	w, h := m.size()
	if w < minWidth {
		return padLines(wrap(fmt.Sprintf("pilothouse needs at least %d columns (this terminal has %d). Widen the window, or use `pilothouse ls`. q quits.", minWidth, w), w), h)
	}
	if !m.loaded {
		return m.viewNotLoaded(w, h)
	}
	m.clampScroll()
	var out []string
	out = append(out, m.viewHeader(w), "")
	if b := m.bannerText(); b != "" {
		out = append(out, m.st.banner.Render(fit(" "+b, w)))
	}
	listH, detailH := m.layout()
	switch m.mode {
	case modeHelp:
		out = append(out, m.viewHelp(w, listH+detailH)...)
	case modeExplain:
		out = append(out, m.viewExplain(w, listH+detailH)...)
	default:
		lines, idx := m.listLines(w)
		sel, top := selLines(idx, m.cursor)
		start, end, up, down := window(len(lines), listH, m.scroll, sel, top)
		var vis []string
		if up {
			vis = append(vis, "   "+m.st.dim.Render(fmt.Sprintf("↑ %d more", countRows(idx[:start]))))
		}
		vis = append(vis, lines[start:end]...)
		if down {
			vis = append(vis, "   "+m.st.dim.Render(fmt.Sprintf("↓ %d more", countRows(idx[end:]))))
		}
		vis = fill(vis, listH)
		out = append(out, vis...)
		out = append(out, m.viewDetail(w, detailH)...)
	}
	out = append(out, m.viewFlash(w), m.viewFooter(w))
	// Safety net: never emit a line wider than the terminal, or it wraps
	// and shifts the whole screen.
	for i, l := range out {
		if lipgloss.Width(l) > w {
			out[i] = ansi.Truncate(l, w, "…")
		}
	}
	return padLines(out, h)
}

func countRows(idx []int) int {
	n := 0
	for _, i := range idx {
		if i >= 0 {
			n++
		}
	}
	return n
}

func padLines(lines []string, h int) string {
	for len(lines) < h {
		lines = append(lines, "")
	}
	if len(lines) > h {
		lines = lines[:h]
	}
	return strings.Join(lines, "\n")
}

func (m *Model) viewHeader(w int) string {
	left := " " + m.st.brand.Render("pilothouse")
	if m.machine != "" {
		left += m.st.dim.Render("  " + m.machine)
	}
	// Presence: where requests go right now.
	var pres string
	if m.presence.State == api.PresenceAway {
		pres = m.st.away.Render("● away")
	} else {
		pres = m.st.here.Render("● here")
	}
	mode := "auto"
	if m.presence.Mode == api.PresenceForceHere || m.presence.Mode == api.PresenceForceAway {
		mode = "set by you"
	}
	pres += m.st.dim.Render(" · " + mode)

	needs := m.counts[sectionNeeds]
	var counts []string
	if needs > 0 {
		counts = append(counts, m.st.needs.Render(fmt.Sprintf("%d need you", needs)))
	} else {
		counts = append(counts, m.st.dim.Render("0 need you"))
	}
	counts = append(counts, m.st.dim.Render(fmt.Sprintf("%d working", m.counts[sectionWorking])))
	counts = append(counts, m.st.dim.Render(fmt.Sprintf("%d waiting", m.counts[sectionWaiting])))
	countStr := strings.Join(counts, m.st.faint.Render(" · "))

	var conn string
	switch m.conn {
	case connLive:
		conn = m.st.working.Render("● live")
	default:
		conn = m.st.warn.Render("◌ offline")
	}
	gap := "   "
	candidates := []string{
		pres + gap + countStr + gap + conn + " ",
		pres + gap + counts[0] + gap + conn + " ",
		pres + gap + counts[0] + " ",
		pres + " ",
	}
	for _, right := range candidates {
		if lipgloss.Width(left)+lipgloss.Width(right)+2 <= w {
			return left + strings.Repeat(" ", w-lipgloss.Width(left)-lipgloss.Width(right)) + right
		}
	}
	return fit(left, w)
}

func (m *Model) bannerText() string {
	if !m.loaded || m.conn == connLive {
		return ""
	}
	wait := m.retryAt.Sub(m.now).Round(time.Second)
	when := "now"
	if wait > 0 {
		when = "in " + wait.String()
	}
	why := "lost connection to the daemon"
	if m.connErr != nil && strings.Contains(m.connErr.Error(), "not running") {
		why = "the daemon stopped"
	}
	return fmt.Sprintf("⚠ Offline: %s · retrying %s · showing the last known state · start it with `pilothouse start`", why, when)
}

func (m *Model) viewFlash(w int) string {
	if m.flash.text == "" {
		return ""
	}
	st := m.st.dim
	switch m.flash.kind {
	case flashOK:
		st = m.st.ok
	case flashWarn:
		st = m.st.warn
	case flashErr:
		st = m.st.err
	}
	return " " + st.Render(trunc(m.flash.text, w-2))
}

type keyHint struct{ key, desc string }

func (m *Model) denyInputView() string {
	if m.st.r.ColorProfile() != termenv.Ascii {
		return m.input.View()
	}
	val := []rune(m.input.Value())
	pos := m.input.Position()
	if pos < 0 {
		pos = 0
	}
	if pos > len(val) {
		pos = len(val)
	}
	v := "_"
	if len(val) > 0 {
		v = string(val[:pos]) + "_" + string(val[pos:])
	}
	if m.input.Width > 0 {
		vw := lipgloss.Width(v)
		if vw < m.input.Width {
			v += strings.Repeat(" ", m.input.Width-vw)
		}
	}
	return v
}

func (m *Model) viewFooter(w int) string {
	if m.mode == modeDeny {
		label := m.st.warn.Render(" Deny "+shortID(m.denyID)) + m.st.dim.Render(" reason: ")
		tail := m.st.dim.Render("  enter send · esc cancel")
		m.input.Width = max(10, w-lipgloss.Width(label)-lipgloss.Width(tail)-2)
		return fit(label+m.denyInputView()+tail, w)
	}
	var hints []keyHint
	switch m.mode {
	case modeHelp:
		hints = []keyHint{{"esc", "back"}, {"q", "quit"}}
	case modeExplain:
		hints = []keyHint{{"esc", "back"}, {"q", "quit"}}
	default:
		r := m.selected()
		switch {
		case r != nil && r.req != nil && r.req.Status == api.RequestOpen:
			hints = append(hints, keyHint{"y", "allow"}, keyHint{"a", "allow for session"}, keyHint{"n", "deny"})
		case r != nil && attachable(r.sess):
			hints = append(hints, keyHint{"enter", "attach"})
		case r != nil:
			hints = append(hints, keyHint{"enter", "details"})
		}
		hints = append(hints, keyHint{"e", "explain"}, keyHint{"p", "presence"}, keyHint{"?", "help"}, keyHint{"q", "quit"})
	}
	var parts []string
	used := 1
	for _, k := range hints {
		p := m.st.key.Render(k.key) + " " + m.st.dim.Render(k.desc)
		pw := lipgloss.Width(p) + 3
		if used+pw > w {
			break
		}
		parts = append(parts, p)
		used += pw
	}
	return " " + strings.Join(parts, m.st.faint.Render("   "))
}

// rule draws "── title ─────" across the width.
func (m *Model) rule(title string, w int) string {
	t := ""
	if title != "" {
		t = " " + title + " "
	}
	left := m.st.rule.Render("──")
	rest := w - 2 - lipgloss.Width(t)
	if rest < 0 {
		return fit(left+t, w)
	}
	return left + t + m.st.rule.Render(strings.Repeat("─", rest))
}

func (m *Model) labelLine(label, value string, w int) string {
	return "  " + m.st.label.Render(fmt.Sprintf("%-7s", label)) + trunc(value, w-10)
}

func (m *Model) viewDetail(w, h int) []string {
	if h <= 0 {
		return nil
	}
	r := m.selected()
	var lines []string
	if r == nil {
		return fill(nil, h)
	}
	inner := w - 4
	if r.req != nil {
		q := r.req
		status := "held for you"
		if q.Status == api.RequestSteppedAside {
			status = "in the agent's own dialog"
		}
		title := m.st.bold.Render(shortID(q.ID)) + m.st.dim.Render(" · "+agentName(q.Agent)+" · ") + m.st.needs.Render(status)
		if q.Status == api.RequestOpen && !q.ExpiresAt.IsZero() {
			left := q.ExpiresAt.Sub(m.now)
			if left > 0 {
				title += m.st.dim.Render(" · back to the terminal in " + Age(left))
			}
		}
		lines = append(lines, m.rule(title, w))
		// The summary usually restates the command; show it only when it
		// adds something (file edits, MCP calls).
		if q.Summary != "" && (q.Command == "" || !strings.Contains(q.Summary, oneLine(q.Command))) {
			lines = append(lines, "  "+m.st.bold.Render(trunc(oneLine(q.Summary), inner)))
		}
		var body []string
		switch {
		case q.Command != "":
			body = wrap("$ "+q.Command, inner)
		case len(q.Paths) > 0:
			for _, p := range q.Paths {
				body = append(body, ShortPath(p))
			}
		}
		maxBody := 2
		if m.expanded {
			maxBody = max(2, h-6)
		}
		if len(body) > maxBody {
			body = append(body[:maxBody-1], "… ("+fmt.Sprint(len(body)-maxBody+1)+" more lines, enter to expand)")
		}
		for _, l := range body {
			lines = append(lines, "  "+m.st.cmd.Render(trunc(l, inner)))
		}
		loc := where(r.sess, q.Cwd) + "  " + m.st.dim.Render(ShortPath(q.Cwd))
		lines = append(lines, m.labelLine("where", loc, w))
		if q.PolicyRule != "" {
			lines = append(lines, m.labelLine("rule", q.PolicyRule+m.st.dim.Render(" · your policy asks a person for this"), w))
		} else {
			lines = append(lines, m.labelLine("tool", q.Tool, w))
		}
		if r.sess != nil && r.sess.LastMessage != "" {
			lines = append(lines, m.labelLine("said", "“"+oneLine(r.sess.LastMessage)+"”", w))
		}
		if q.Status == api.RequestSteppedAside {
			lines = append(lines, "  "+m.st.warn.Render(trunc(m.answerElsewhere(r.sess), inner)))
		}
		return fill(lines, h)
	}

	s := r.sess
	state := string(s.State)
	if s.BlockKind != "" {
		state += " (" + string(s.BlockKind) + ")"
	}
	_, gst := m.rowGlyph(*r)
	title := m.st.bold.Render(shortID(s.ID)) + m.st.dim.Render(" · "+agentName(s.Agent)+" · "+string(s.Mode)+" · ") +
		gst.Render(state) + m.st.dim.Render(" for "+Age(m.rowAge(*r)))
	lines = append(lines, m.rule(title, w))
	lines = append(lines, "  "+m.st.bold.Render(trunc(m.rowWhat(*r), inner)))
	loc := where(s, "") + "  " + m.st.dim.Render(ShortPath(s.Cwd))
	lines = append(lines, m.labelLine("where", loc, w))
	if s.LastMessage != "" {
		said := wrap("“"+oneLine(s.LastMessage)+"”", inner-7)
		maxSaid := 2
		if m.expanded {
			maxSaid = max(2, h-6)
		}
		if len(said) > maxSaid {
			said = said[:maxSaid]
			said[maxSaid-1] = trunc(said[maxSaid-1]+" …", inner-7)
		}
		for i, l := range said {
			label := "said"
			if i > 0 {
				label = ""
			}
			lines = append(lines, m.labelLine(label, l, w))
		}
	}
	src := string(s.StateLayer)
	if src == "" {
		src = "unknown source"
	}
	lines = append(lines, m.labelLine("state", state+m.st.dim.Render(" · from "+src+" · e to explain"), w))
	switch {
	case s.State == api.StateBlocked && attachable(s):
		lines = append(lines, "  "+m.st.warn.Render("Answer it in the agent: press enter to attach"))
	case s.State == api.StateBlocked:
		lines = append(lines, "  "+m.st.warn.Render(trunc("Answer it in the terminal where the agent runs", inner)))
	case attachable(s):
		lines = append(lines, "  "+m.st.dim.Render("enter to attach · ctrl+\\ detaches"))
	}
	return fill(lines, h)
}

func fill(lines []string, h int) []string {
	if len(lines) > h {
		return lines[:h]
	}
	for len(lines) < h {
		lines = append(lines, "")
	}
	return lines
}

func (m *Model) viewHelp(w, h int) []string {
	k := func(key, desc string) string {
		return "   " + m.st.key.Render(fmt.Sprintf("%-14s", key)) + m.st.text.Render(desc)
	}
	lines := []string{
		m.rule(m.st.bold.Render("Keys"), w),
		k("j k  ↑ ↓", "move"),
		k("y", "allow once"),
		k("a", "allow for the rest of this session"),
		k("n", "deny (type an optional reason, enter sends)"),
		k("enter", "attach to a wrapped session, or show more detail"),
		k("e", "explain why a session is in its state"),
		k("p", "presence: auto → here → away"),
		k("r", "reload from the daemon"),
		k("?", "this help · q quits"),
		"",
		m.rule(m.st.bold.Render("Presence"), w),
		k("here", "requests show in the agent's own dialog, in its terminal"),
		k("away", "requests are held and sent to your paired phone"),
		k("auto", "here or away from keyboard and mouse idle time"),
		"",
		"   " + m.st.dim.Render(trunc("From any shell: pilothouse approve <id> · pilothouse deny <id> -m \"why\" · pilothouse ls", w-4)),
	}
	return fill(lines, h)
}

func (m *Model) viewExplain(w, h int) []string {
	lines := []string{}
	switch {
	case m.explainErr != nil:
		lines = append(lines, m.rule(m.st.bold.Render("Explain"), w), "  "+m.st.err.Render(describeErr(m.explainErr, "explain")))
	case m.explain == nil:
		lines = append(lines, m.rule(m.st.bold.Render("Explain"), w), "  "+m.st.dim.Render("Loading…"))
	default:
		ex := m.explain
		s := ex.Session
		lines = append(lines, m.rule(m.st.bold.Render("Explain "+shortID(s.ID))+m.st.dim.Render(" · "+agentName(s.Agent)+" · "+where(&s, "")), w))
		state := string(s.State)
		if s.BlockKind != "" {
			state += " (" + string(s.BlockKind) + ")"
		}
		lines = append(lines, m.labelLine("state", m.st.bold.Render(state)+m.st.dim.Render(" · picked by: ")+ex.Rule, w))
		if s.StateDetail != "" {
			lines = append(lines, m.labelLine("detail", s.StateDetail, w))
		}
		lines = append(lines, "")
		lines = append(lines, "  "+m.st.section.Render(fmt.Sprintf("%-10s %-10s %-20s %s", "WHEN", "LAYER", "STATE", "EVIDENCE")))
		for _, e := range ex.Evidence {
			st := string(e.State)
			if e.Kind != "" {
				st += "/" + string(e.Kind)
			}
			line := fmt.Sprintf("%-10s %-10s %-20s %s", Age(m.now.Sub(e.At))+" ago", e.Layer, st, oneLine(e.Detail))
			style := m.st.text
			if !e.Fresh {
				style = m.st.faint
			}
			lines = append(lines, "  "+style.Render(trunc(line, w-4)))
		}
		if len(ex.Evidence) == 0 {
			lines = append(lines, "  "+m.st.dim.Render("No evidence recorded yet."))
		}
		lines = append(lines, "", "  "+m.st.dim.Render("Faint lines are stale and no longer count."))
	}
	return fill(lines, h)
}

func (m *Model) viewNotLoaded(w, h int) string {
	var body []string
	if m.conn == connDown {
		k := func(desc, cmd string) string {
			return "   " + m.st.dim.Render(fmt.Sprintf("%-26s", desc)) + m.st.bold.Render(cmd)
		}
		body = []string{
			" " + m.st.brand.Render("pilothouse"),
			"",
			"   " + m.st.err.Render("✗") + " " + m.st.bold.Render("The Pilothouse daemon isn't running."),
			"",
			k("Start it now", "pilothouse start"),
			k("Start it at every login", "pilothouse service install"),
			k("First time here?", "pilothouse setup"),
			"",
			"   " + m.st.dim.Render("Retrying every 2s · q to quit"),
		}
	} else {
		body = []string{" " + m.st.brand.Render("pilothouse"), "", "   " + m.st.dim.Render("Connecting to the Pilothouse daemon…")}
		if m.connErr != nil {
			body = append(body, "", "   "+m.st.warn.Render(trunc(m.connErr.Error(), w-6)), "   "+m.st.dim.Render("Retrying · q to quit"))
		}
	}
	top := max(0, (h-len(body))/3)
	out := make([]string, top, h)
	out = append(out, body...)
	return padLines(out, h)
}
