package tui

import (
	"context"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/client"
)

// Questions, plans, replies and activity (docs/PLAN-0.2.md F1–F3).
//
// A held question opens an answer panel (enter, or a number key on its
// row): number keys and arrows pick, space toggles a multi-select option,
// enter sends, o types your own answer, n declines. A held plan opens a
// scrollable view: a approves, c asks for changes. r replies to a session
// whose caps say it takes replies. The session detail shows the last few
// activity items when the daemon can read the agent's transcript.

// askState is the question or plan being answered.
type askState struct {
	id      string // request id
	qi      int    // question being answered
	cursor  int    // option under the cursor; len(options) is "Other…"
	picked  map[int]bool
	answers map[string]string
	scroll  int // plan view
	// fromList: the plan-feedback input was opened from the list (n), so
	// esc goes back there rather than to the plan view.
	fromList bool
}

// activity is the last few feed items of one session.
type activity struct {
	items   []api.ActivityItem
	at      time.Time
	loading bool
}

const (
	activityLimit   = 6
	activityMaxAge  = 5 * time.Second  // refresh the selected session's feed this often
	activityRetry   = time.Minute      // after "not supported", try again this much later
	activityOnEvent = 2 * time.Second  // a session update refreshes an older feed
	maxReplyRunes   = 4000             // the daemon accepts 16 KiB
	maxAnswerRunes  = 1000             // devices may send 2000 bytes
	maxChangesRunes = 2000             // plan feedback (decision message)
	replyTimeout    = 15 * time.Second // a reply may wait on the agent's socket
)

type replyMsg struct {
	sessionID string
	res       api.ReplyResult
	err       error
}

type activityMsg struct {
	sessionID string
	act       api.Activity
	err       error
}

// asks reports whether r is answered rather than approved.
func asks(r *api.Request) bool {
	return r != nil && (r.Checkpoint == api.CheckpointQuestion || r.Checkpoint == api.CheckpointPlan)
}

// typing reports whether keys go to the text input.
func (m *Model) typing() bool {
	switch m.mode {
	case modeDeny, modeReply, modeChanges, modeOther:
		return true
	}
	return false
}

// askRequest is the request the answer panel or plan view is showing.
func (m *Model) askRequest() (api.Request, bool) {
	r, ok := m.requests[m.ask.id]
	return r, ok
}

// openAsk opens the answer panel or plan view for the selected request.
// digit > 0 puts the cursor on that option.
func (m *Model) openAsk(req *api.Request, digit int) tea.Cmd {
	if req.Status != api.RequestOpen && req.Checkpoint == api.CheckpointQuestion {
		m.setFlash(flashWarn, m.answerElsewhere(m.selectedSession()))
		return nil
	}
	m.ask = askState{id: req.ID, picked: map[int]bool{}, answers: map[string]string{}}
	if req.Checkpoint == api.CheckpointPlan {
		m.mode = modePlan
		return nil
	}
	if len(req.Questions) == 0 {
		m.setFlash(flashWarn, "This question came without its questions: answer it in the agent's terminal")
		return nil
	}
	m.mode = modeQuestion
	if digit > 0 {
		m.ask.cursor = min(digit-1, len(req.Questions[0].Options)-1)
	}
	return nil
}

// closeAsk leaves the panel; why (if set) is flashed.
func (m *Model) closeAsk(k flashKind, why string) {
	m.mode = modeList
	m.ask = askState{}
	m.input.Blur()
	if why != "" {
		m.setFlash(k, why)
	}
}

// checkAskGone leaves the panel when its request was closed elsewhere.
func (m *Model) checkAskGone() {
	switch m.mode {
	case modeQuestion, modePlan, modeChanges, modeOther:
	default:
		return
	}
	if _, ok := m.askRequest(); !ok && !m.pending[m.ask.id] {
		m.closeAsk(flashWarn, "Answered elsewhere: the first answer wins")
	}
}

func (m *Model) currentQuestion() (api.Request, api.Question, bool) {
	req, ok := m.askRequest()
	if !ok || m.ask.qi >= len(req.Questions) {
		return req, api.Question{}, false
	}
	return req, req.Questions[m.ask.qi], true
}

// choices is the number of cursor positions: the options, plus "Other…"
// when the question takes the person's own words.
func choices(q api.Question) int {
	n := len(q.Options)
	if q.AllowOther {
		n++
	}
	return n
}

func (m *Model) onQuestionKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	req, q, ok := m.currentQuestion()
	if !ok {
		m.closeAsk(flashWarn, "That question is gone (answered or expired)")
		return m, nil
	}
	n := choices(q)
	switch s := k.String(); s {
	case "esc":
		m.closeAsk(flashInfo, "Not answered")
		return m, nil
	case "up", "k", "ctrl+p":
		m.ask.cursor = (m.ask.cursor + n - 1) % max(n, 1)
	case "down", "j", "ctrl+n", "tab":
		m.ask.cursor = (m.ask.cursor + 1) % max(n, 1)
	case " ":
		if !q.MultiSelect {
			return m, m.chooseCursor(req, q)
		}
		if m.ask.cursor < len(q.Options) {
			m.ask.picked[m.ask.cursor] = !m.ask.picked[m.ask.cursor]
		} else {
			return m, m.startOther(q)
		}
	case "enter":
		return m, m.chooseCursor(req, q)
	case "o":
		return m, m.startOther(q)
	case "n":
		m.mode = modeDeny
		m.denyID = req.ID
		m.input.Reset()
		m.input.Placeholder = "why (optional, shown to the agent)"
		m.input.CharLimit = 500
		return m, m.input.Focus()
	default:
		if d, err := strconv.Atoi(s); err == nil && len(s) == 1 && d >= 1 && d <= len(q.Options) {
			m.ask.cursor = d - 1
			if q.MultiSelect {
				m.ask.picked[d-1] = !m.ask.picked[d-1]
				return m, nil
			}
			return m, m.answerCurrent(req, q, q.Options[d-1].Label)
		}
	}
	return m, nil
}

// chooseCursor answers with what the cursor is on: an option, the
// toggled options of a multi-select question, or "Other…".
func (m *Model) chooseCursor(req api.Request, q api.Question) tea.Cmd {
	if m.ask.cursor >= len(q.Options) {
		return m.startOther(q)
	}
	if !q.MultiSelect {
		return m.answerCurrent(req, q, q.Options[m.ask.cursor].Label)
	}
	var labels []string
	for i, o := range q.Options {
		if m.ask.picked[i] {
			labels = append(labels, o.Label)
		}
	}
	if len(labels) == 0 {
		labels = []string{q.Options[m.ask.cursor].Label}
	}
	return m.answerCurrent(req, q, api.JoinAnswers(labels))
}

func (m *Model) startOther(q api.Question) tea.Cmd {
	if !q.AllowOther {
		m.setFlash(flashWarn, "This question takes one of its options only")
		return nil
	}
	m.mode = modeOther
	m.ask.cursor = len(q.Options) // "Other…"
	m.input.Reset()
	m.input.Placeholder = "your answer"
	if q.MultiSelect && len(m.ask.picked) > 0 {
		m.input.Placeholder = "your answer (sent with the options you ticked)"
	}
	m.input.CharLimit = maxAnswerRunes
	return m.input.Focus()
}

// answerCurrent records the answer to the current question and moves on;
// after the last one it sends them all.
func (m *Model) answerCurrent(req api.Request, q api.Question, answer string) tea.Cmd {
	answer = strings.TrimSpace(answer)
	if answer == "" {
		m.setFlash(flashWarn, "An answer can't be empty")
		return nil
	}
	m.ask.answers[q.Key()] = answer
	if m.ask.qi+1 < len(req.Questions) {
		m.mode = modeQuestion
		m.ask.qi++
		m.ask.cursor, m.ask.picked = 0, map[int]bool{}
		return nil
	}
	answers := m.ask.answers
	var parts []string
	for _, x := range req.Questions {
		parts = append(parts, oneLine(x.Text)+" → "+answers[x.Key()])
	}
	m.closeAsk(flashInfo, "")
	return m.decideText(req.ID, api.DecideBody{Behavior: api.BehaviorAllow, Answers: answers}, "✓ Answered: "+strings.Join(parts, " · "))
}

func (m *Model) onPlanKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	req, ok := m.askRequest()
	if !ok {
		m.closeAsk(flashWarn, "That plan is gone (answered or expired)")
		return m, nil
	}
	page := max(1, m.bodyHeight()-4)
	switch k.String() {
	case "esc", "enter", "backspace", "q":
		m.closeAsk(flashInfo, "")
	case "down", "j", "ctrl+n":
		m.ask.scroll++
	case "up", "k", "ctrl+p":
		m.ask.scroll--
	case "pgdown", " ", "ctrl+d", "f":
		m.ask.scroll += page
	case "pgup", "ctrl+u", "b":
		m.ask.scroll -= page
	case "g", "home":
		m.ask.scroll = 0
	case "G", "end":
		m.ask.scroll = 1 << 20
	case "a", "y":
		if req.Status != api.RequestOpen {
			m.setFlash(flashWarn, m.answerElsewhere(m.sessionByID(req.SessionID)))
			return m, nil
		}
		m.closeAsk(flashInfo, "")
		return m, m.decideText(req.ID, api.DecideBody{Behavior: api.BehaviorAllow}, "✓ Approved the plan: "+planTitle(req))
	case "c", "n":
		if req.Status != api.RequestOpen {
			m.setFlash(flashWarn, m.answerElsewhere(m.sessionByID(req.SessionID)))
			return m, nil
		}
		return m, m.startChanges()
	}
	m.ask.scroll = max(0, m.ask.scroll) // the view clamps the bottom
	return m, nil
}

func (m *Model) startChanges() tea.Cmd {
	m.mode = modeChanges
	m.input.Reset()
	m.input.Placeholder = "what should change (shown to the agent)"
	m.input.CharLimit = maxChangesRunes
	return m.input.Focus()
}

// onInputKey handles enter and esc in the text inputs; other keys type.
func (m *Model) onInputKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.Type {
	case tea.KeyEsc:
		switch m.mode {
		case modeOther:
			m.mode = modeQuestion // back to the options
		case modeChanges:
			if _, ok := m.askRequest(); ok && !m.ask.fromList {
				m.mode = modePlan
			} else {
				m.closeAsk(flashInfo, "")
			}
		case modeReply:
			m.mode = modeList
			m.setFlash(flashInfo, "Reply not sent")
		default: // modeDeny
			m.mode = modeList
			m.setFlash(flashInfo, "Deny cancelled")
			if m.ask.id != "" {
				m.mode = modeQuestion
			}
		}
		m.input.Blur()
		return m, nil
	case tea.KeyEnter:
		text := strings.TrimSpace(m.input.Value())
		switch m.mode {
		case modeOther:
			req, q, ok := m.currentQuestion()
			if !ok {
				m.closeAsk(flashWarn, "That question is gone (answered or expired)")
				return m, nil
			}
			if text == "" {
				m.setFlash(flashWarn, "Type your answer, or esc to go back to the options")
				return m, nil
			}
			m.input.Blur()
			if q.MultiSelect {
				// Ticked options go along with the person's own words.
				var parts []string
				for i, o := range q.Options {
					if m.ask.picked[i] {
						parts = append(parts, o.Label)
					}
				}
				text = api.JoinAnswers(append(parts, text))
			}
			return m, m.answerCurrent(req, q, text)
		case modeChanges:
			req, ok := m.askRequest()
			m.closeAsk(flashInfo, "")
			if !ok {
				m.setFlash(flashWarn, "That plan is gone (answered or expired)")
				return m, nil
			}
			label := "✎ Asked for changes to the plan: " + planTitle(req)
			if text == "" {
				label = "✗ Rejected the plan: " + planTitle(req)
			}
			return m, m.decideText(req.ID, api.DecideBody{Behavior: api.BehaviorDeny, Message: text}, label)
		case modeReply:
			id := m.replyID
			m.mode = modeList
			m.input.Blur()
			if text == "" {
				m.setFlash(flashWarn, "Nothing to send")
				return m, nil
			}
			return m, m.sendReply(id, text)
		default: // modeDeny
			id := m.denyID
			req, isReq := m.requests[id]
			m.closeAsk(flashInfo, "")
			label := "Denied"
			if isReq && req.Checkpoint == api.CheckpointQuestion {
				return m, m.decideText(id, api.DecideBody{Behavior: api.BehaviorDeny, Message: text}, "✗ Declined to answer: "+requestWhat(req))
			}
			return m, m.decide(id, api.DecideBody{Behavior: api.BehaviorDeny, Message: text}, label)
		}
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	return m, cmd
}

// startReply opens the reply box for the selected session.
func (m *Model) startReply() tea.Cmd {
	s := m.selectedSession()
	switch {
	case s == nil:
		m.setFlash(flashWarn, "Select a session to reply to")
		return nil
	case s.State == api.StateExited:
		m.setFlash(flashWarn, "That session has exited")
		return nil
	case !s.Caps.Reply:
		m.setFlash(flashWarn, "This session can't take replies: run the agent with `pilothouse run`, or use Claude Code 2.1.224 or newer")
		return nil
	}
	m.mode = modeReply
	m.replyID = s.ID
	m.flash = flash{}
	m.input.Reset()
	m.input.Placeholder = "message for the agent"
	m.input.CharLimit = maxReplyRunes
	return m.input.Focus()
}

func (m *Model) sendReply(sessionID, text string) tea.Cmd {
	m.setFlash(flashInfo, "Sending…")
	be := m.opts.Backend
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), replyTimeout)
		defer cancel()
		res, err := be.Reply(ctx, sessionID, api.Reply{Text: text})
		return replyMsg{sessionID: sessionID, res: res, err: err}
	}
}

func (m *Model) onReplied(msg replyMsg) tea.Cmd {
	name := "the session"
	if s, ok := m.sessions[msg.sessionID]; ok {
		name = where(&s, "")
	}
	if msg.err != nil {
		m.setFlash(flashErr, describeErr(msg.err, "send the reply"))
		return nil
	}
	text := "✓ Sent to " + name
	if msg.res.Note != "" {
		text += " · " + msg.res.Note
	}
	m.setFlash(flashOK, text)
	return m.fetchActivity(msg.sessionID, true)
}

// fetchActivity loads the last few activity items of a session, unless
// they are fresh (force skips that), already loading, or the daemon said
// it can't read activity (it is asked again after activityRetry).
func (m *Model) fetchActivity(sessionID string, force bool) tea.Cmd {
	if sessionID == "" || m.opts.Now().Before(m.activityOff) {
		return nil
	}
	a := m.activity[sessionID]
	if a.loading || (!force && !a.at.IsZero() && m.opts.Now().Sub(a.at) < activityMaxAge) {
		return nil
	}
	a.loading = true
	m.activity[sessionID] = a
	be := m.opts.Backend
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		act, err := be.Activity(ctx, sessionID, api.ActivityQuery{Limit: activityLimit})
		return activityMsg{sessionID: sessionID, act: act, err: err}
	}
}

func (m *Model) onActivity(msg activityMsg) {
	a := m.activity[msg.sessionID]
	a.loading = false
	a.at = m.opts.Now()
	switch {
	case msg.err == nil:
		a.items = msg.act.Items
		if !msg.act.Available {
			a.items = nil
		}
	case client.IsCode(msg.err, "unsupported"):
		// Not in this daemon yet (or not for this agent): stay quiet.
		m.activityOff = m.opts.Now().Add(activityRetry)
		a.items = nil
	}
	// Other errors keep what was shown; the next refresh retries.
	m.activity[msg.sessionID] = a
}

// selectedActivity fetches the selected session's feed if it is stale.
func (m *Model) selectedActivity() tea.Cmd {
	if s := m.selectedSession(); s != nil {
		return m.fetchActivity(s.ID, false)
	}
	return nil
}

// decideText is decide with a flash worded by the caller.
func (m *Model) decideText(id string, body api.DecideBody, text string) tea.Cmd {
	cmd := m.decide(id, body, "")
	m.decideTexts[id] = text
	return cmd
}

// planTitle is a plan request's title for flashes.
func planTitle(r api.Request) string {
	t := strings.TrimSpace(strings.TrimPrefix(r.Summary, "Plan ready"))
	t = strings.TrimSpace(strings.TrimPrefix(t, ":"))
	if t == "" {
		return "plan"
	}
	return t
}

// sessionByID is a copy of a known session, or nil.
func (m *Model) sessionByID(id string) *api.Session {
	if s, ok := m.sessions[id]; ok {
		return &s
	}
	return nil
}
