package telegram

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
	"github.com/xasdta/pilothouse/internal/notify"
)

// Messages use Telegram's HTML parse mode: everything that comes from an
// agent, a repo or a person goes through esc, and is capped before it is
// escaped so a cut never splits a tag or an entity.

// maxText keeps a message under Telegram's 4096-character limit (which
// counts text after entities are parsed, so tags give extra margin).
const maxText = 3900

func esc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// clip caps s at n runes (multi-line text keeps its lines).
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

func header(icon string, n core.Notification) string {
	agent := "An agent"
	switch {
	case n.Request != nil:
		agent = notify.AgentName(n.Request.Agent)
	case n.Session != nil:
		agent = notify.AgentName(n.Session.Agent)
	}
	s := icon + " <b>" + esc(agent) + "</b>"
	if w := notify.Where(n.Request, n.Session); w != "" {
		s += " · " + esc(notify.Short(w, 80))
	}
	return s
}

// answerState is a question request's progress, kept with its message:
// Telegram shows one question at a time.
type answerState struct {
	Q       int               `json:"q"`                 // index of the question being answered
	Answers map[string]string `json:"answers,omitempty"` // Question.Key() → answer
	Sel     []int             `json:"sel,omitempty"`     // chosen options of the current multi-select question
}

func parseState(s string) answerState {
	var st answerState
	_ = json.Unmarshal([]byte(s), &st)
	if st.Answers == nil {
		st.Answers = map[string]string{}
	}
	return st
}

func (st answerState) String() string {
	b, _ := json.Marshal(st)
	return string(b)
}

func (st answerState) selected(i int) bool {
	for _, s := range st.Sel {
		if s == i {
			return true
		}
	}
	return false
}

// requestText renders a request. compact drops option descriptions and
// diffs, for requests too large for one message.
func requestText(n core.Notification, st answerState, now time.Time, compact bool) string {
	r := n.Request
	var b strings.Builder
	switch r.Checkpoint {
	case api.CheckpointQuestion:
		b.WriteString(header("❓", n))
	case api.CheckpointPlan:
		b.WriteString(header("📋", n))
	default:
		b.WriteString(header("🔐", n))
	}
	b.WriteString("\n")
	switch {
	case r.Checkpoint == api.CheckpointQuestion && len(r.Questions) > 0:
		writeQuestions(&b, r.Questions, st, compact)
	case r.Checkpoint == api.CheckpointPlan:
		b.WriteString("wants to start on this plan:\n")
		md := ""
		if r.Plan != nil {
			md = r.Plan.Markdown
		}
		limit := 2800
		if compact {
			limit = 1500
		}
		b.WriteString("<blockquote expandable>" + esc(clip(md, limit)) + "</blockquote>")
	case r.Command != "":
		limit := 1500
		if compact {
			limit = 400
		}
		b.WriteString("wants to run:\n<pre>" + esc(clip(r.Command, limit)) + "</pre>")
	default:
		b.WriteString(esc(notify.Action(r)))
	}
	if line := notify.RiskLine(r.Risk); line != "" {
		b.WriteString("\n" + esc(line))
	}
	if p := r.Preview; p != nil && len(p.Files) > 0 {
		writePreview(&b, p, compact)
	}
	if c := strings.TrimSpace(r.Context); c != "" {
		b.WriteString("\n💬 <i>" + esc(notify.Short(c, 280)) + "</i>")
	}
	if r.PolicyRule != "" {
		b.WriteString("\nRule: <code>" + esc(notify.Short(r.PolicyRule, 60)) + "</code>")
	}
	if !r.ExpiresAt.IsZero() && r.Status == api.RequestOpen {
		if left := r.ExpiresAt.Sub(now); left > 0 {
			b.WriteString("\n<i>Waiting up to " + strconv.Itoa(int(left.Round(time.Minute).Minutes())) + " min, then it goes back to the terminal.</i>")
		}
	}
	return b.String()
}

func writeQuestions(b *strings.Builder, qs []api.Question, st answerState, compact bool) {
	if len(qs) == 1 {
		b.WriteString("asks:")
	} else {
		b.WriteString("asks " + strconv.Itoa(len(qs)) + " questions:")
	}
	for i, q := range qs {
		if i >= 4 {
			b.WriteString("\n…")
			break
		}
		b.WriteString("\n\n")
		if ans, ok := st.Answers[q.Key()]; ok {
			b.WriteString("✓ " + esc(clip(q.Text, 200)) + "\n→ <b>" + esc(clip(ans, 200)) + "</b>")
			continue
		}
		if i == st.Q && len(qs) > 1 {
			b.WriteString("👉 ")
		}
		if q.Header != "" {
			b.WriteString("<b>" + esc(clip(q.Header, 40)) + "</b> ")
		}
		b.WriteString(esc(clip(q.Text, 400)))
		if q.MultiSelect {
			b.WriteString(" <i>(pick any)</i>")
		}
		for _, o := range q.Options {
			b.WriteString("\n• " + esc(clip(o.Label, 80)))
			if d := strings.TrimSpace(o.Description); d != "" && !compact {
				b.WriteString(" — <i>" + esc(clip(d, 140)) + "</i>")
			}
		}
	}
}

func writePreview(b *strings.Builder, p *api.Preview, compact bool) {
	files := "1 file"
	if len(p.Files) > 1 {
		files = strconv.Itoa(len(p.Files)) + " files"
	}
	b.WriteString("\n✏️ " + files + ", +" + strconv.Itoa(p.Added) + " −" + strconv.Itoa(p.Removed) + ":")
	for i, f := range p.Files {
		if i == 3 {
			b.WriteString(" …")
			break
		}
		b.WriteString(" <code>" + esc(notify.Short(f.Path, 60)) + "</code>")
	}
	if compact || p.Files[0].Diff == "" {
		return
	}
	lines := strings.Split(strings.TrimRight(p.Files[0].Diff, "\n"), "\n")
	if len(lines) > 14 {
		lines = append(lines[:14], "…")
	}
	b.WriteString("\n<pre><code class=\"language-diff\">" + esc(clip(strings.Join(lines, "\n"), 900)) + "</code></pre>")
}

// fitRequest renders a request within the message limit.
func fitRequest(n core.Notification, st answerState, now time.Time) string {
	s := requestText(n, st, now, false)
	if utf8.RuneCountInString(s) <= maxText {
		return s
	}
	s = requestText(n, st, now, true)
	if utf8.RuneCountInString(s) <= maxText {
		return s
	}
	// Still too long (many long questions): drop the HTML and cut.
	return esc(clip(notify.Line(n), maxText-200))
}

// closedText is a request message after the request closed: the original
// text with how it ended, and without buttons.
func closedText(orig string, r api.Request, deviceName func(string) string) string {
	line := notify.ResolvedLine(r, deviceName)
	if line == "" {
		return orig
	}
	s := clip(orig, maxText-400) + "\n\n<b>" + esc(line) + "</b>"
	if d := r.Decision; d != nil {
		if r.Checkpoint == api.CheckpointQuestion && len(d.Answers) > 0 {
			var parts []string
			for _, q := range r.Questions {
				if a, ok := d.Answers[q.Key()]; ok {
					parts = append(parts, a)
				}
			}
			if len(parts) > 0 {
				s += "\n→ " + esc(clip(strings.Join(parts, " / "), 300))
			}
		}
		if m := strings.TrimSpace(d.Message); m != "" && d.Behavior == api.BehaviorDeny {
			s += "\n“" + esc(clip(m, 300)) + "”"
		}
	}
	return s
}

// sessionText is the "finished its turn" message.
func sessionText(n core.Notification, canReply bool) string {
	s := header("✅", n) + "\nfinished its turn"
	last := ""
	if n.Session != nil {
		last = strings.TrimSpace(n.Session.LastMessage)
	}
	if last != "" {
		s += ":\n<blockquote expandable>" + esc(clip(last, 2500)) + "</blockquote>"
	} else {
		s += " and is waiting for you."
	}
	if canReply {
		s += "\n<i>Reply to this message to send it a follow-up.</i>"
	}
	return s
}

// blockedText is a request waiting in the terminal (no buttons).
func blockedText(n core.Notification) string {
	return header("⏳", n) + "\n" + esc(notify.Action(n.Request)) + " — waiting in the terminal."
}

func testText(n core.Notification, machine string) string {
	body := n.Body
	if body == "" || body == "Notifications are working." {
		body = "Notifications from " + machine + " will appear here."
	}
	return "🔔 <b>" + esc(notify.Short(n.Title, 120)) + "</b>\n" + esc(clip(body, 500))
}

// ---- keyboards ----

// keyboardSpec is a keyboard before its buttons get tokens.
type keyboardSpec [][]buttonSpec

type buttonSpec struct {
	text   string
	action string // minted into a token
	url    string // or a link
}

// requestKeyboard lays out the buttons for a request. decisions=false
// leaves only the link to the web app.
func requestKeyboard(r api.Request, st answerState, decisions bool, appURL string) keyboardSpec {
	var kb keyboardSpec
	if decisions && r.Status == api.RequestOpen {
		switch {
		case r.Checkpoint == api.CheckpointQuestion && len(r.Questions) > 0:
			if st.Q < len(r.Questions) {
				q := r.Questions[st.Q]
				for i, o := range q.Options {
					if i >= 10 {
						break
					}
					label := clip(o.Label, 60)
					if q.MultiSelect {
						if st.selected(i) {
							label = "☑ " + label
						} else {
							label = "☐ " + label
						}
					}
					kb = append(kb, []buttonSpec{{text: label, action: "opt:" + strconv.Itoa(st.Q) + ":" + strconv.Itoa(i)}})
				}
				if q.MultiSelect {
					kb = append(kb, []buttonSpec{{text: "Done ✓", action: "done:" + strconv.Itoa(st.Q)}})
				}
			}
		case r.Checkpoint == api.CheckpointPlan:
			kb = append(kb, []buttonSpec{{text: "✅ Approve", action: "approve"}, {text: "✎ Request changes", action: "changes"}})
		default:
			kb = append(kb,
				[]buttonSpec{{text: "✅ Allow", action: "allow"}, {text: "Allow for session", action: "session"}},
				[]buttonSpec{{text: "❌ Deny", action: "deny"}})
		}
	}
	if appURL != "" {
		label := "Open in the app"
		if r.Checkpoint == api.CheckpointQuestion && decisions {
			label = "Answer in the app"
		}
		kb = append(kb, []buttonSpec{{text: label, url: appURL + "/r/" + r.ID}})
	}
	return kb
}

// appURL is the web app link for buttons: only https, which Telegram
// accepts and a phone can open (a tailnet name works; localhost doesn't).
func appURL(public string) string {
	if strings.HasPrefix(public, "https://") {
		return strings.TrimRight(public, "/")
	}
	return ""
}
