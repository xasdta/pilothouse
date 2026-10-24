package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/xasdta/pilothouse/internal/api"
)

// Views for questions, plans and activity (ask.go).

func (m *Model) bodyHeight() int { l, d := m.layout(); return l + d }

// askTitle is the rule title of a question or plan panel.
func (m *Model) askTitle(kind string, q api.Request) string {
	status := m.st.needs.Render("held for you")
	if q.Status != api.RequestOpen {
		status = m.st.dim.Render("in the agent's own dialog")
	}
	t := m.st.bold.Render(kind+" "+shortID(q.ID)) + m.st.dim.Render(" · "+agentName(q.Agent)+" · "+where(m.sessionByID(q.SessionID), q.Cwd)+" · ") + status
	if q.Status == api.RequestOpen && !q.ExpiresAt.IsZero() {
		if left := q.ExpiresAt.Sub(m.now); left > 0 {
			t += m.st.dim.Render(" · back to the terminal in " + Age(left))
		}
	}
	return t
}

// viewQuestion is the answer panel: the current question and its options.
func (m *Model) viewQuestion(w, h int) []string {
	req, q, ok := m.currentQuestion()
	if !ok {
		return fill([]string{m.rule(m.st.bold.Render("Question"), w), "  " + m.st.dim.Render("That question is gone.")}, h)
	}
	inner := w - 4
	lines := []string{m.rule(m.askTitle("Question", req), w)}
	if req.Context != "" {
		// The agent's last words before asking: why it asks.
		said := wrap("“"+oneLine(req.Context)+"”", inner)
		for _, l := range said[:min(2, len(said))] {
			lines = append(lines, "  "+m.st.dim.Render(l))
		}
	}
	lines = append(lines, "")
	head := ""
	if n := len(req.Questions); n > 1 {
		head = fmt.Sprintf("Question %d of %d", m.ask.qi+1, n)
	}
	if q.Header != "" {
		if head != "" {
			head += " · "
		}
		head += q.Header
	}
	if q.MultiSelect {
		if head != "" {
			head += " · "
		}
		head += "choose any"
	}
	if head != "" {
		lines = append(lines, "  "+m.st.label.Render(trunc(head, inner)))
	}
	for _, l := range wrap(q.Text, inner) {
		lines = append(lines, "  "+m.st.bold.Render(l))
	}
	lines = append(lines, "")

	labelW := 0
	for _, o := range q.Options {
		labelW = max(labelW, lipgloss.Width(o.Label))
	}
	labelW = min(max(labelW, 8), max(10, inner/2))
	for i := 0; i < choices(q); i++ {
		cur := i == m.ask.cursor
		mark := "  "
		if cur {
			mark = m.st.key.Render("❯ ")
		}
		box := ""
		switch {
		case q.MultiSelect && i < len(q.Options):
			box = "[ ] "
			if m.ask.picked[i] {
				box = "[x] "
			}
		case q.MultiSelect:
			box = "    "
		}
		num := fmt.Sprintf("%d ", i+1)
		label, desc := "Other…", "type your own answer (o)"
		if i < len(q.Options) {
			label, desc = q.Options[i].Label, q.Options[i].Description
		} else {
			num = "  "
		}
		if i >= 9 {
			num = "  "
		}
		lst := m.st.text
		if cur {
			lst = m.st.bold
		}
		left := "  " + mark + m.st.key.Render(num) + box + lst.Render(fit(label, labelW))
		used := lipgloss.Width(left) + 2
		if desc != "" && w-used > 8 {
			left += "  " + m.st.dim.Render(trunc(oneLine(desc), w-used-1))
		}
		lines = append(lines, left)
	}
	if prev := m.previousAnswers(req); prev != "" {
		lines = append(lines, "", "  "+m.st.dim.Render(trunc(prev, inner)))
	}
	return fill(lines, h)
}

// previousAnswers recaps the answers given so far in this panel.
func (m *Model) previousAnswers(req api.Request) string {
	var parts []string
	for _, q := range req.Questions[:min(m.ask.qi, len(req.Questions))] {
		if a, ok := m.ask.answers[q.Key()]; ok {
			parts = append(parts, oneLine(q.Text)+" → "+a)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "So far: " + strings.Join(parts, " · ")
}

// viewPlan is the scrollable plan view.
func (m *Model) viewPlan(w, h int) []string {
	req, ok := m.askRequest()
	if !ok || req.Plan == nil {
		return fill([]string{m.rule(m.st.bold.Render("Plan"), w), "  " + m.st.dim.Render("That plan is gone.")}, h)
	}
	head := []string{m.rule(m.askTitle("Plan", req), w)}
	if req.Status != api.RequestOpen {
		head = append(head, "  "+m.st.warn.Render(trunc(m.answerElsewhere(m.sessionByID(req.SessionID)), w-4)))
	}
	if req.Plan.Path != "" {
		head = append(head, "  "+m.st.dim.Render(trunc(ShortPath(req.Plan.Path), w-4)))
	}
	body := m.renderMarkdown(req.Plan.Markdown, w-4)
	if len(body) == 0 {
		body = []string{m.st.dim.Render("(the plan is empty)")}
	}
	avail := max(1, h-len(head))
	maxScroll := max(0, len(body)-avail)
	m.ask.scroll = min(m.ask.scroll, maxScroll)
	start := m.ask.scroll
	up := start > 0
	if up {
		avail--
	}
	end := min(len(body), start+avail)
	down := end < len(body)
	if down {
		end--
	}
	lines := head
	if up {
		lines = append(lines, "  "+m.st.dim.Render(fmt.Sprintf("↑ %d more lines", start)))
	}
	for _, l := range body[start:max(start, end)] {
		lines = append(lines, "  "+l)
	}
	if down {
		lines = append(lines, "  "+m.st.dim.Render(fmt.Sprintf("↓ %d more lines", len(body)-end)))
	}
	return fill(lines, h)
}

// renderMarkdown styles the markdown a plan is written in, line by line:
// headings, lists, quotes, rules and fenced code. Inline emphasis markers
// are dropped; everything else is shown as written.
func (m *Model) renderMarkdown(md string, w int) []string {
	var out []string
	code, blank := false, true
	for _, raw := range strings.Split(strings.ReplaceAll(md, "\r", ""), "\n") {
		line := strings.TrimRight(strings.ReplaceAll(raw, "\t", "    "), " ")
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") {
			code = !code
			continue
		}
		if code {
			out = append(out, m.st.cmd.Render(trunc("  "+line, w)))
			blank = false
			continue
		}
		if t == "" {
			if !blank {
				out = append(out, "")
			}
			blank = true
			continue
		}
		blank = false
		switch {
		case strings.HasPrefix(t, "#"):
			level := len(t) - len(strings.TrimLeft(t, "#"))
			text := inlinePlain(strings.TrimSpace(strings.TrimLeft(t, "#")))
			st := m.st.bold
			if level <= 2 {
				st = m.st.brand
			}
			for _, l := range wrap(text, w) {
				out = append(out, st.Render(l))
			}
		case strings.Trim(t, "-*_ ") == "" && len(t) >= 3:
			out = append(out, m.st.rule.Render(strings.Repeat("─", min(w, 40))))
		case strings.HasPrefix(t, ">"):
			for _, l := range wrap(inlinePlain(strings.TrimSpace(strings.TrimLeft(t, "> "))), w-2) {
				out = append(out, m.st.dim.Render("│ "+l))
			}
		default:
			indent := (len(line) - len(strings.TrimLeft(line, " "))) / 2 * 2
			bullet, rest := listItem(t)
			text := inlinePlain(rest)
			pad := strings.Repeat(" ", min(indent, w/3))
			if bullet == "" {
				for _, l := range wrap(text, w-len(pad)) {
					out = append(out, pad+m.st.text.Render(l))
				}
				continue
			}
			hang := strings.Repeat(" ", lipgloss.Width(bullet))
			for i, l := range wrap(text, max(10, w-len(pad)-len(hang))) {
				lead := hang
				if i == 0 {
					lead = m.st.key.Render(bullet)
				}
				out = append(out, pad+lead+m.st.text.Render(l))
			}
		}
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

// listItem splits a list marker ("- ", "1. ", "- [ ] ") from its text;
// the marker comes back as it should be drawn.
func listItem(t string) (bullet, rest string) {
	for _, p := range []string{"- [ ] ", "- [x] ", "* [ ] ", "* [x] "} {
		if strings.HasPrefix(t, p) {
			return "☐ ", t[len(p):]
		}
	}
	if len(t) > 2 && (t[0] == '-' || t[0] == '*' || t[0] == '+') && t[1] == ' ' {
		return "• ", t[2:]
	}
	i := 0
	for i < len(t) && i < 3 && t[i] >= '0' && t[i] <= '9' {
		i++
	}
	if i > 0 && i+1 < len(t) && (t[i] == '.' || t[i] == ')') && t[i+1] == ' ' {
		return t[:i+1] + " ", t[i+2:]
	}
	return "", t
}

func inlinePlain(s string) string {
	for _, mk := range []string{"**", "__"} {
		s = strings.ReplaceAll(s, mk, "")
	}
	return s
}

// detailAsk is the detail pane of a question or plan row in the list.
func (m *Model) detailAsk(r *row, w, h int) []string {
	q := r.req
	inner := w - 4
	kind := "Question"
	if q.Checkpoint == api.CheckpointPlan {
		kind = "Plan"
	}
	lines := []string{m.rule(m.askTitle(kind, *q), w)}
	switch q.Checkpoint {
	case api.CheckpointQuestion:
		if len(q.Questions) > 0 {
			qq := q.Questions[0]
			text := oneLine(qq.Text)
			if n := len(q.Questions); n > 1 {
				text += fmt.Sprintf("  (+%d more)", n-1)
			}
			lines = append(lines, "  "+m.st.bold.Render(trunc(text, inner)))
			var opts []string
			for i, o := range qq.Options {
				opts = append(opts, m.st.key.Render(fmt.Sprint(i+1))+" "+o.Label)
			}
			if qq.AllowOther {
				opts = append(opts, m.st.dim.Render("or your own words"))
			}
			lines = append(lines, "  "+fit(strings.Join(opts, m.st.faint.Render("  ·  ")), inner))
		}
	case api.CheckpointPlan:
		lines = append(lines, "  "+m.st.bold.Render(trunc(planTitle(*q), inner)))
		if q.Plan != nil {
			body := m.renderMarkdown(q.Plan.Markdown, inner)
			n := 2
			if m.expanded {
				n = max(2, h-6)
			}
			shown, title := 0, planTitle(*q)
			for _, l := range body {
				if shown == n {
					break
				}
				if t := strings.TrimSpace(ansi.Strip(l)); t == "" || t == title {
					continue // the title is the line above
				}
				lines = append(lines, "  "+l)
				shown++
			}
		}
	}
	lines = append(lines, m.labelLine("where", where(r.sess, q.Cwd)+"  "+m.st.dim.Render(ShortPath(q.Cwd)), w))
	said := q.Context
	if said == "" && r.sess != nil {
		said = r.sess.LastMessage
	}
	if said != "" {
		lines = append(lines, m.labelLine("said", "“"+oneLine(said)+"”", w))
	}
	switch {
	case q.Status == api.RequestOpen && q.Checkpoint == api.CheckpointQuestion:
		lines = append(lines, "  "+m.st.dim.Render(trunc("enter or a number answers · n declines", inner)))
	case q.Status == api.RequestOpen:
		lines = append(lines, "  "+m.st.dim.Render(trunc("enter reads the plan · y approves · n asks for changes", inner)))
	default:
		lines = append(lines, "  "+m.st.warn.Render(trunc(m.answerElsewhere(r.sess), inner)))
	}
	return fill(lines, h)
}

// activityLines renders up to n of a session's latest activity items,
// newest last; nil when there is nothing to show.
func (m *Model) activityLines(sessionID string, w, n int) []string {
	items := m.activity[sessionID].items
	if n <= 0 || len(items) == 0 {
		return nil
	}
	items = items[max(0, len(items)-n):]
	var out []string
	for i, it := range items {
		label := ""
		if i == 0 {
			label = "recent"
		}
		out = append(out, m.labelLine(label, m.activityText(it), w))
	}
	return out
}

func (m *Model) activityText(it api.ActivityItem) string {
	at := m.st.faint.Render(fmt.Sprintf("%5s ", Age(m.now.Sub(it.At))))
	switch it.Kind {
	case api.ActivityPrompt:
		who := "you"
		switch it.Source {
		case "pilothouse":
			who = "reply"
		case "peer":
			who = "peer"
		}
		return at + m.st.dim.Render(who+" › ") + oneLine(it.Text)
	case api.ActivityText:
		return at + "“" + oneLine(it.Text) + "”"
	case api.ActivityTool:
		s := firstNonEmpty(it.Summary, it.Tool)
		switch {
		case it.IsError:
			return at + m.st.err.Render("✗ ") + oneLine(s)
		case it.Pending:
			return at + m.st.working.Render("… ") + oneLine(s)
		}
		return at + m.st.dim.Render("· ") + oneLine(s)
	case api.ActivityError:
		return at + m.st.err.Render("! "+oneLine(firstNonEmpty(it.Text, it.Summary)))
	case api.ActivitySubagent:
		return at + m.st.dim.Render("⤷ ") + oneLine(firstNonEmpty(it.Summary, it.Text))
	}
	return at + m.st.dim.Render(oneLine(firstNonEmpty(it.Summary, it.Text)))
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// inputFooter is the footer while typing: a label, the input, the keys.
func (m *Model) inputFooter(w int) string {
	var label, tail string
	switch m.mode {
	case modeReply:
		name := "session"
		if s := m.sessionByID(m.replyID); s != nil {
			name = where(s, "")
		}
		label = m.st.key.Render(" Reply") + m.st.dim.Render(" to "+trunc(name, 24)+": ")
		tail = "  enter send · esc cancel"
	case modeChanges:
		label = m.st.warn.Render(" Changes to the plan") + m.st.dim.Render(": ")
		tail = "  enter send · esc back"
	case modeOther:
		label = m.st.key.Render(" Your answer") + m.st.dim.Render(": ")
		tail = "  enter use it · esc back"
	default: // modeDeny
		verb := "Deny"
		if r, ok := m.requests[m.denyID]; ok && r.Checkpoint == api.CheckpointQuestion {
			verb = "Decline"
		}
		label = m.st.warn.Render(" "+verb+" "+shortID(m.denyID)) + m.st.dim.Render(" reason: ")
		tail = "  enter send · esc cancel"
	}
	t := m.st.dim.Render(tail)
	m.input.Width = max(10, w-lipgloss.Width(label)-lipgloss.Width(t)-2)
	return fit(label+m.input.View()+t, w)
}

// askHints are the footer keys of the answer panel and plan view.
func (m *Model) askHints() []keyHint {
	if m.mode == modePlan {
		if r, ok := m.askRequest(); ok && r.Status == api.RequestOpen {
			return []keyHint{{"a", "approve"}, {"c", "request changes"}, {"↑↓", "scroll"}, {"esc", "back"}}
		}
		return []keyHint{{"↑↓", "scroll"}, {"esc", "back"}}
	}
	_, q, ok := m.currentQuestion()
	if !ok {
		return []keyHint{{"esc", "back"}}
	}
	var h []keyHint
	if n := len(q.Options); n > 0 {
		h = append(h, keyHint{fmt.Sprintf("1-%d", min(n, 9)), map[bool]string{true: "toggle", false: "choose"}[q.MultiSelect]})
	}
	h = append(h, keyHint{"↑↓", "move"})
	if q.MultiSelect {
		h = append(h, keyHint{"space", "toggle"}, keyHint{"enter", "send"})
	} else {
		h = append(h, keyHint{"enter", "choose"})
	}
	if q.AllowOther {
		h = append(h, keyHint{"o", "own answer"})
	}
	return append(h, keyHint{"n", "decline"}, keyHint{"esc", "back"})
}
