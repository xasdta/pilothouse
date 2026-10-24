// Package tui is the inbox: a live, keyboard-driven view of every agent
// session on this machine and every request that needs a person.
//
// The model loads a snapshot, then follows the daemon's event stream; on
// a resync request it reloads, and on disconnect it shows a banner and
// reconnects with backoff. All I/O goes through Backend so tests can
// drive Update with messages and assert on View.
package tui

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/client"
)

// Backend is the subset of *client.Client the inbox uses.
type Backend interface {
	Snapshot(ctx context.Context) (api.Snapshot, error)
	Stream(ctx context.Context, after int64, fn func(api.Event)) error
	Request(ctx context.Context, id string) (api.Request, error)
	Decide(ctx context.Context, id string, b api.DecideBody) (api.Request, error)
	SetPresence(ctx context.Context, m api.PresenceMode) (api.Presence, error)
	Explain(ctx context.Context, id string) (api.Explain, error)
	MarkSeen(ctx context.Context, id string) error
	// 0.2: replies and activity answer 501 "unsupported" until the daemon
	// (or the session's agent) can do them; the inbox degrades quietly.
	Reply(ctx context.Context, sessionID string, r api.Reply) (api.ReplyResult, error)
	Activity(ctx context.Context, sessionID string, q api.ActivityQuery) (api.Activity, error)
}

// Options configures the inbox.
type Options struct {
	Backend Backend
	// Exe is the pilothouse binary used for `pilothouse attach`; default os.Executable.
	Exe string
	// Now is the clock; tests fix it.
	Now func() time.Time
	// Renderer styles output; nil means stdout (which honours NO_COLOR).
	Renderer *lipgloss.Renderer
	// NoTick disables the one-second clock (tests).
	NoTick bool
}

type connState int

const (
	connConnecting   connState = iota // first snapshot not loaded yet
	connLive                          // streaming
	connReconnecting                  // lost the stream; retrying
	connDown                          // daemon not running and nothing loaded
)

type mode int

const (
	modeList mode = iota
	modeDeny
	modeHelp
	modeExplain
	modeQuestion // answer panel of a held question (ask.go)
	modePlan     // plan view
	modeReply    // reply box
	modeChanges  // plan feedback input
	modeOther    // own-words answer input
)

type flashKind int

const (
	flashInfo flashKind = iota
	flashOK
	flashWarn
	flashErr
)

type flash struct {
	text  string
	kind  flashKind
	until time.Time
}

const (
	sectionNeeds = iota
	sectionWorking
	sectionWaiting
	sectionExited
	numSections
)

var sectionTitles = [numSections]string{"NEEDS YOU", "WORKING", "WAITING", "RECENTLY EXITED"}

// row is one selectable line: a request (with its session, if known) or
// a session.
type row struct {
	section int
	key     string
	req     *api.Request
	sess    *api.Session
}

// Model is the Bubble Tea model of the inbox.
type Model struct {
	opts Options
	st   styles

	// data
	loaded   bool
	machine  string
	version  string
	seq      int64
	sessions map[string]api.Session
	requests map[string]api.Request // open and stepped-aside only
	presence api.Presence

	// connection
	conn      connState
	connErr   error
	attempt   int
	retryAt   time.Time
	gen       int
	cancel    context.CancelFunc
	streaming bool
	// nextListen waits for the next message of the current stream; it is
	// re-armed after every event.
	nextListen tea.Cmd

	// ui
	width, height int
	now           time.Time
	rows          []row
	counts        [numSections]int
	cursor        int
	selKey        string
	scroll        int
	expanded      bool
	mode          mode
	input         textinput.Model
	denyID        string
	explain       *api.Explain
	explainErr    error
	flash         flash
	pending       map[string]bool
	// questions, plans, replies, activity (ask.go)
	ask         askState
	replyID     string
	decideTexts map[string]string // flash text of a pending decision, by request id
	activity    map[string]activity
	activityOff time.Time // activity unsupported until then
}

// New returns an inbox model.
func New(opts Options) *Model {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Renderer == nil {
		opts.Renderer = lipgloss.NewRenderer(os.Stdout)
	}
	if opts.Exe == "" {
		opts.Exe, _ = os.Executable()
	}
	ti := textinput.New()
	ti.Placeholder = "reason shown to the agent (optional)"
	ti.CharLimit = 500
	ti.Prompt = ""
	m := &Model{
		opts:        opts,
		st:          newStyles(opts.Renderer),
		sessions:    map[string]api.Session{},
		requests:    map[string]api.Request{},
		pending:     map[string]bool{},
		input:       ti,
		activity:    map[string]activity{},
		decideTexts: map[string]string{},
		now:         opts.Now(),
	}
	return m
}

// Run shows the inbox until the user quits or ctx ends.
func Run(ctx context.Context, opts Options) error {
	m := New(opts)
	// Settle the colour profile (from the environment) and background
	// (one bounded query) before Bubble Tea starts reading stdin; left to
	// lipgloss, the query would happen lazily mid-program, race Bubble Tea
	// for the reply and wait up to 5 s on terminals that never answer.
	// This is the only place Pilothouse queries the terminal.
	r := m.opts.Renderer
	profile := r.ColorProfile()
	dark := darkBackground(os.Stdin, os.Stdout, queryTimeout)
	r.SetHasDarkBackground(dark)
	// Bubbles (the deny prompt) render through lipgloss's default
	// renderer; give it the same answers so it never asks either.
	lipgloss.SetColorProfile(profile)
	lipgloss.SetHasDarkBackground(dark)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	_, err := p.Run()
	m.stopStream()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return nil
	}
	return err
}

// messages

type snapshotMsg struct {
	snap api.Snapshot
	err  error
}

type eventMsg struct {
	gen int
	ev  api.Event
}

type streamEndMsg struct {
	gen int
	err error
}

type retryMsg struct{ gen int }

type tickMsg time.Time

type decideMsg struct {
	id    string
	body  api.DecideBody
	req   api.Request
	err   error
	label string
}

type requestMsg struct {
	req api.Request
	err error
}

type presenceMsg struct {
	p   api.Presence
	err error
}

type explainMsg struct {
	sessionID string
	ex        api.Explain
	err       error
}

type attachDoneMsg struct{ err error }

const callTimeout = 10 * time.Second

// Init loads the first snapshot and starts the clock.
func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.loadSnapshot()}
	if !m.opts.NoTick {
		cmds = append(cmds, tick())
	}
	return tea.Batch(cmds...)
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *Model) loadSnapshot() tea.Cmd {
	be := m.opts.Backend
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		s, err := be.Snapshot(ctx)
		return snapshotMsg{snap: s, err: err}
	}
}

// startStream follows the event log from m.seq in a goroutine and feeds
// events back through a channel. A generation number lets the model
// ignore messages from streams it has already replaced.
func (m *Model) startStream() tea.Cmd {
	m.stopStream()
	m.gen++
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.streaming = true
	ch := make(chan tea.Msg, 256)
	gen, after, be := m.gen, m.seq, m.opts.Backend
	go func() {
		err := be.Stream(ctx, after, func(ev api.Event) {
			select {
			case ch <- eventMsg{gen: gen, ev: ev}:
			case <-ctx.Done():
			}
		})
		select {
		case ch <- streamEndMsg{gen: gen, err: err}:
		case <-ctx.Done():
		}
	}()
	return listen(ctx, ch)
}

func listen(ctx context.Context, ch <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		select {
		case msg := <-ch:
			return msg
		case <-ctx.Done():
			return nil
		}
	}
}

func (m *Model) stopStream() {
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.streaming = false
}

func backoff(attempt int) time.Duration {
	d := 500 * time.Millisecond << min(attempt, 5)
	return min(d, 10*time.Second)
}

// Update implements tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.input.Width = max(10, m.width-30)
		m.clampScroll()
		return m, nil

	case tickMsg:
		m.now = m.opts.Now()
		if m.flash.text != "" && !m.flash.until.IsZero() && m.now.After(m.flash.until) {
			m.flash = flash{}
		}
		m.pruneExited()
		return m, tea.Batch(tick(), m.selectedActivity())

	case snapshotMsg:
		return m, m.onSnapshot(msg)

	case eventMsg:
		if msg.gen != m.gen {
			return m, nil
		}
		m.apply(msg.ev)
		m.rebuild()
		m.checkAskGone()
		cmd := m.relisten()
		// A session update for the one on screen refreshes its activity.
		if s := m.selectedSession(); s != nil && msg.ev.SessionID == s.ID && msg.ev.Type == api.EventSessionUpdated &&
			m.opts.Now().Sub(m.activity[s.ID].at) > activityOnEvent {
			cmd = tea.Batch(cmd, m.fetchActivity(s.ID, true))
		}
		return m, cmd

	case streamEndMsg:
		if msg.gen != m.gen {
			return m, nil
		}
		m.streaming = false
		if errors.Is(msg.err, client.ErrResync) {
			return m, m.loadSnapshot()
		}
		return m, m.scheduleRetry(msg.err)

	case retryMsg:
		if msg.gen != m.gen {
			return m, nil
		}
		return m, m.loadSnapshot()

	case decideMsg:
		return m, m.onDecided(msg)

	case replyMsg:
		return m, m.onReplied(msg)

	case activityMsg:
		m.onActivity(msg)
		return m, nil

	case requestMsg:
		if msg.err == nil {
			m.upsertRequest(msg.req)
			m.rebuild()
		}
		return m, nil

	case presenceMsg:
		if msg.err != nil {
			m.setFlash(flashErr, describeErr(msg.err, "change presence"))
			return m, nil
		}
		m.presence = msg.p
		m.setFlash(flashOK, "Presence: "+presenceWords(msg.p))
		return m, nil

	case explainMsg:
		if m.mode != modeExplain {
			return m, nil
		}
		if msg.err != nil {
			m.explainErr = msg.err
		} else {
			ex := msg.ex
			m.explain = &ex
		}
		return m, nil

	case attachDoneMsg:
		if msg.err != nil {
			m.setFlash(flashErr, "attach: "+msg.err.Error())
		}
		return m, m.loadSnapshot()

	case tea.KeyMsg:
		return m.onKey(msg)
	}
	if m.typing() {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	return m, nil
}

// relisten returns the command that waits for the next stream message.
func (m *Model) relisten() tea.Cmd { return m.nextListen }

func (m *Model) onSnapshot(msg snapshotMsg) tea.Cmd {
	if msg.err != nil {
		return m.scheduleRetry(msg.err)
	}
	s := msg.snap
	m.loaded = true
	m.machine, m.version, m.seq = s.Machine, s.Version, s.Seq
	m.presence = s.Presence
	m.sessions = make(map[string]api.Session, len(s.Sessions))
	for _, x := range s.Sessions {
		m.sessions[x.ID] = x
	}
	m.requests = map[string]api.Request{}
	for _, r := range s.Requests {
		m.upsertRequest(r)
	}
	wasDown := m.conn != connLive && m.conn != connConnecting
	m.conn, m.connErr, m.attempt = connLive, nil, 0
	if wasDown {
		m.setFlash(flashOK, "Reconnected to the daemon")
	}
	m.pruneExited()
	m.rebuild()
	return m.startStreamCmd()
}

// startStreamCmd starts a stream and remembers its listener so the model
// can re-arm it after each event.
func (m *Model) startStreamCmd() tea.Cmd {
	l := m.startStream()
	m.nextListen = l
	return l
}

func (m *Model) scheduleRetry(err error) tea.Cmd {
	m.stopStream()
	m.connErr = err
	var wait time.Duration
	if !m.loaded {
		m.conn = connDown
		if !errors.Is(err, client.ErrDaemonDown) {
			m.conn = connReconnecting
		}
		wait = 2 * time.Second
	} else {
		m.conn = connReconnecting
		wait = backoff(m.attempt)
	}
	m.attempt++
	m.retryAt = m.opts.Now().Add(wait)
	m.gen++
	gen := m.gen
	return tea.Tick(wait, func(time.Time) tea.Msg { return retryMsg{gen: gen} })
}

func (m *Model) apply(ev api.Event) {
	if ev.Seq != 0 && ev.Seq <= m.seq {
		return // already in the snapshot
	}
	if ev.Seq > m.seq {
		m.seq = ev.Seq
	}
	switch ev.Type {
	case api.EventSessionStarted, api.EventSessionUpdated, api.EventSessionExited:
		var s api.Session
		if json.Unmarshal(ev.Data, &s) == nil && s.ID != "" {
			m.sessions[s.ID] = s
		}
	case api.EventRequestOpened, api.EventRequestClosed:
		var r api.Request
		if json.Unmarshal(ev.Data, &r) == nil && r.ID != "" {
			if ev.Type == api.EventRequestClosed && m.pending[r.ID] {
				// Our own decision is reported by onDecided.
				break
			}
			if ev.Type == api.EventRequestClosed && r.Status == api.RequestDecided && r.Decision != nil && m.selectedRequestID() == r.ID {
				m.setFlash(flashInfo, "Answered by "+r.Decision.By+": "+string(r.Decision.Behavior))
			}
			m.upsertRequest(r)
		}
	case api.EventPresenceChanged:
		var p api.Presence
		if json.Unmarshal(ev.Data, &p) == nil && p.State != "" {
			m.presence = p
		}
	}
}

func (m *Model) upsertRequest(r api.Request) {
	if r.Status == api.RequestOpen || r.Status == api.RequestSteppedAside {
		m.requests[r.ID] = r
	} else {
		delete(m.requests, r.ID)
	}
}

// pruneExited drops sessions that exited more than a day ago; the
// snapshot does the same, so a long-running inbox matches a fresh one.
func (m *Model) pruneExited() {
	changed := false
	for id, s := range m.sessions {
		if s.State == api.StateExited && s.ExitedAt != nil && m.now.Sub(*s.ExitedAt) > 24*time.Hour {
			delete(m.sessions, id)
			changed = true
		}
	}
	if changed {
		m.rebuild()
	}
}

// rebuild recomputes the rows and keeps the selection on the same item.
func (m *Model) rebuild() {
	held := map[string]bool{} // sessions represented by a request row
	var reqs []api.Request
	for _, r := range m.requests {
		reqs = append(reqs, r)
		held[r.SessionID] = true
	}
	sort.Slice(reqs, func(i, j int) bool {
		// Held requests first (the agent is waiting on Pilothouse), then
		// oldest first: the longest wait is the most urgent.
		hi, hj := reqs[i].Status == api.RequestOpen, reqs[j].Status == api.RequestOpen
		if hi != hj {
			return hi
		}
		if !reqs[i].CreatedAt.Equal(reqs[j].CreatedAt) {
			return reqs[i].CreatedAt.Before(reqs[j].CreatedAt)
		}
		return reqs[i].ID < reqs[j].ID
	})
	var rows []row
	for i := range reqs {
		r := reqs[i]
		var sp *api.Session
		if s, ok := m.sessions[r.SessionID]; ok {
			sp = &s
		}
		rows = append(rows, row{section: sectionNeeds, key: "r:" + r.ID, req: &r, sess: sp})
	}
	buckets := [numSections][]api.Session{}
	for _, s := range m.sessions {
		if held[s.ID] {
			continue
		}
		buckets[sessionSection(s)] = append(buckets[sessionSection(s)], s)
	}
	sort.Slice(buckets[sectionNeeds], func(i, j int) bool { return older(buckets[sectionNeeds][i], buckets[sectionNeeds][j]) })
	sort.Slice(buckets[sectionWorking], func(i, j int) bool {
		a, b := buckets[sectionWorking][i], buckets[sectionWorking][j]
		// Stable by start time so working rows don't jump on every tool call.
		if !a.StartedAt.Equal(b.StartedAt) {
			return a.StartedAt.Before(b.StartedAt)
		}
		return a.ID < b.ID
	})
	sort.Slice(buckets[sectionWaiting], func(i, j int) bool { return newer(buckets[sectionWaiting][i], buckets[sectionWaiting][j]) })
	sort.Slice(buckets[sectionExited], func(i, j int) bool {
		a, b := buckets[sectionExited][i], buckets[sectionExited][j]
		ta, tb := a.UpdatedAt, b.UpdatedAt
		if a.ExitedAt != nil {
			ta = *a.ExitedAt
		}
		if b.ExitedAt != nil {
			tb = *b.ExitedAt
		}
		if !ta.Equal(tb) {
			return ta.After(tb)
		}
		return a.ID < b.ID
	})
	for sec := 0; sec < numSections; sec++ {
		for i := range buckets[sec] {
			s := buckets[sec][i]
			rows = append(rows, row{section: sec, key: "s:" + s.ID, sess: &s})
		}
	}
	m.rows = rows
	m.counts = [numSections]int{}
	for _, r := range rows {
		m.counts[r.section]++
	}
	// Restore the selection by key; if the item left (answered, exited
	// elsewhere), stay at the same position so the next item is selected.
	found := false
	for i, r := range rows {
		if r.key == m.selKey {
			m.cursor, found = i, true
			break
		}
	}
	if !found {
		m.cursor = min(m.cursor, len(rows)-1)
		m.cursor = max(m.cursor, 0)
		if len(rows) > 0 {
			m.selKey = rows[m.cursor].key
		} else {
			m.selKey = ""
		}
	}
	m.clampScroll()
}

func sessionSection(s api.Session) int {
	switch s.State {
	case api.StateBlocked:
		return sectionNeeds
	case api.StateWaiting, api.StateErrored:
		return sectionWaiting
	case api.StateExited:
		return sectionExited
	default: // starting, working, unknown
		return sectionWorking
	}
}

func older(a, b api.Session) bool {
	if !a.UpdatedAt.Equal(b.UpdatedAt) {
		return a.UpdatedAt.Before(b.UpdatedAt)
	}
	return a.ID < b.ID
}

func newer(a, b api.Session) bool {
	if !a.UpdatedAt.Equal(b.UpdatedAt) {
		return a.UpdatedAt.After(b.UpdatedAt)
	}
	return a.ID < b.ID
}

func (m *Model) selected() *row {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return nil
	}
	return &m.rows[m.cursor]
}

func (m *Model) selectedRequestID() string {
	if r := m.selected(); r != nil && r.req != nil {
		return r.req.ID
	}
	return ""
}

// selectedSession returns the session of the selected row, if known.
func (m *Model) selectedSession() *api.Session {
	if r := m.selected(); r != nil {
		return r.sess
	}
	return nil
}

func (m *Model) setFlash(k flashKind, text string) {
	m.flash = flash{text: text, kind: k, until: m.opts.Now().Add(6 * time.Second)}
}

func (m *Model) move(delta int) tea.Cmd {
	if len(m.rows) == 0 {
		return nil
	}
	m.cursor = max(0, min(len(m.rows)-1, m.cursor+delta))
	m.selKey = m.rows[m.cursor].key
	m.expanded = false
	m.clampScroll()
	return tea.Batch(m.markSeen(), m.selectedActivity())
}

// markSeen clears the unseen dot of the session under the cursor: the
// person has now looked at it.
func (m *Model) markSeen() tea.Cmd {
	s := m.selectedSession()
	if s == nil || !s.Unseen {
		return nil
	}
	id := s.ID
	if cur, ok := m.sessions[id]; ok {
		cur.Unseen = false
		m.sessions[id] = cur
	}
	s.Unseen = false
	be := m.opts.Backend
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		_ = be.MarkSeen(ctx, id) // best effort: the dot is cosmetic
		return nil
	}
}

func (m *Model) onKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Keys typed faster than the terminal is read arrive as one message
	// ("jj"); outside text entry each rune is its own command.
	if k.Type == tea.KeyRunes && len(k.Runes) > 1 && !k.Paste && !m.typing() {
		var cmds []tea.Cmd
		for _, r := range k.Runes {
			_, cmd := m.onKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}, Alt: k.Alt})
			cmds = append(cmds, cmd)
		}
		return m, tea.Sequence(cmds...)
	}
	if k.Type == tea.KeyCtrlC {
		m.stopStream()
		return m, tea.Quit
	}
	switch m.mode {
	case modeDeny, modeReply, modeChanges, modeOther:
		return m.onInputKey(k)
	case modeQuestion:
		return m.onQuestionKey(k)
	case modePlan:
		return m.onPlanKey(k)
	case modeHelp, modeExplain:
		switch k.String() {
		case "q":
			m.stopStream()
			return m, tea.Quit
		case "esc", "?", "e", "enter", "backspace":
			m.mode = modeList
			m.explain, m.explainErr = nil, nil
		}
		return m, nil
	}

	switch k.String() {
	case "q":
		m.stopStream()
		return m, tea.Quit
	case "j", "down", "ctrl+n":
		return m, m.move(1)
	case "k", "up", "ctrl+p":
		return m, m.move(-1)
	case "g", "home":
		return m, m.move(-len(m.rows))
	case "G", "end":
		return m, m.move(len(m.rows))
	case "pgdown", "ctrl+d":
		return m, m.move(max(1, m.listHeight()/2))
	case "pgup", "ctrl+u":
		return m, m.move(-max(1, m.listHeight()/2))
	case "?":
		m.mode = modeHelp
		return m, nil
	case "r":
		return m, m.startReply()
	case "R", "ctrl+r":
		m.setFlash(flashInfo, "Refreshing…")
		return m, m.loadSnapshot()
	case "p":
		return m, m.cyclePresence()
	case "y":
		return m, m.answer(api.BehaviorAllow, api.ScopeOnce)
	case "a":
		return m, m.answer(api.BehaviorAllow, api.ScopeSession)
	case "n":
		return m, m.answer(api.BehaviorDeny, "")
	case "e":
		return m, m.startExplain()
	case "enter":
		return m, m.enter()
	case "esc":
		m.expanded = false
		return m, nil
	}
	if d, err := strconv.Atoi(k.String()); err == nil && d >= 1 && d <= 9 {
		if r := m.selected(); r != nil && r.req != nil && r.req.Checkpoint == api.CheckpointQuestion {
			return m, m.openAsk(r.req, d)
		}
	}
	return m, nil
}

// answerable explains why the selected row can't be answered here, or
// returns the open request.
func (m *Model) answerable() (*api.Request, string) {
	r := m.selected()
	if r == nil {
		return nil, "Nothing selected"
	}
	if r.req == nil {
		if r.sess != nil && r.sess.State == api.StateBlocked {
			return nil, m.answerElsewhere(r.sess)
		}
		return nil, "Nothing to answer here: this session isn't waiting on a request"
	}
	if r.req.Status == api.RequestSteppedAside {
		return nil, m.answerElsewhere(r.sess)
	}
	if m.pending[r.req.ID] {
		return nil, "Already sending an answer…"
	}
	return r.req, ""
}

func (m *Model) answerElsewhere(s *api.Session) string {
	if attachable(s) {
		return "This one is showing in the agent's own dialog: press enter to attach and answer it there"
	}
	return "This one is showing in the agent's own dialog: answer it in that terminal"
}

func attachable(s *api.Session) bool {
	return s != nil && s.Mode == api.ModeWrapped && s.State != api.StateExited
}

func (m *Model) answer(b api.Behavior, scope api.Scope) tea.Cmd {
	req, why := m.answerable()
	if req == nil {
		m.setFlash(flashWarn, why)
		return nil
	}
	switch req.Checkpoint {
	case api.CheckpointQuestion:
		if b == api.BehaviorAllow {
			m.setFlash(flashWarn, "Pick an answer: enter opens the question (or press its number)")
			return nil
		}
	case api.CheckpointPlan:
		switch {
		case b == api.BehaviorDeny:
			m.ask = askState{id: req.ID, fromList: true}
			return m.startChanges()
		case scope == api.ScopeSession:
			m.setFlash(flashWarn, "A plan is approved once: y approves it (enter reads it first)")
			return nil
		}
		return m.decideText(req.ID, api.DecideBody{Behavior: api.BehaviorAllow}, "✓ Approved the plan: "+planTitle(*req))
	}
	if b == api.BehaviorDeny {
		m.mode = modeDeny
		m.denyID = req.ID
		m.input.Reset()
		m.input.Placeholder = "reason shown to the agent (optional)"
		m.input.CharLimit = 500
		return m.input.Focus()
	}
	label := "Allowed once"
	if scope == api.ScopeSession {
		label = "Allowed for this session"
	}
	return m.decide(req.ID, api.DecideBody{Behavior: b, Scope: scope}, label)
}

func (m *Model) decide(id string, body api.DecideBody, label string) tea.Cmd {
	m.pending[id] = true
	m.setFlash(flashInfo, "Sending…")
	be := m.opts.Backend
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		r, err := be.Decide(ctx, id, body)
		return decideMsg{id: id, body: body, req: r, err: err, label: label}
	}
}

func (m *Model) onDecided(msg decideMsg) tea.Cmd {
	delete(m.pending, msg.id)
	text := m.decideTexts[msg.id]
	delete(m.decideTexts, msg.id)
	if msg.err == nil && text != "" {
		if msg.req.ID != "" {
			m.upsertRequest(msg.req)
		} else {
			delete(m.requests, msg.id)
		}
		m.setFlash(flashOK, text)
		m.rebuild()
		return nil
	}
	if msg.err == nil {
		if msg.req.ID != "" {
			m.upsertRequest(msg.req)
		} else {
			delete(m.requests, msg.id)
		}
		what := ""
		if r, ok := m.findRequest(msg.id); ok {
			what = ": " + requestWhat(r)
		} else if msg.req.ID != "" {
			what = ": " + requestWhat(msg.req)
		}
		if msg.body.Behavior == api.BehaviorDeny {
			m.setFlash(flashOK, "✗ "+msg.label+what)
		} else {
			m.setFlash(flashOK, "✓ "+msg.label+what)
		}
		m.rebuild()
		return nil
	}
	switch {
	case client.IsCode(msg.err, "already_decided"):
		m.setFlash(flashWarn, "Already answered elsewhere: the first answer wins")
		be := m.opts.Backend
		id := msg.id
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
			defer cancel()
			r, err := be.Request(ctx, id)
			return requestMsg{req: r, err: err}
		}
	case client.IsCode(msg.err, "not_found"):
		delete(m.requests, msg.id)
		m.rebuild()
		m.setFlash(flashWarn, "That request is gone (answered or expired)")
	case client.IsCode(msg.err, "invalid"):
		// The daemon says why the answer doesn't fit the question.
		m.setFlash(flashErr, "Not sent: "+apiMessage(msg.err))
	default:
		m.setFlash(flashErr, describeErr(msg.err, "answer"))
	}
	return nil
}

// apiMessage is the daemon's own sentence for an API error.
func apiMessage(err error) string {
	var ae *client.APIError
	if errors.As(err, &ae) && ae.Msg != "" {
		return ae.Msg
	}
	return err.Error()
}

func (m *Model) findRequest(id string) (api.Request, bool) {
	for _, r := range m.rows {
		if r.req != nil && r.req.ID == id {
			return *r.req, true
		}
	}
	return api.Request{}, false
}

// describeErr turns daemon errors into a sentence that says what to do.
func describeErr(err error, action string) string {
	switch {
	case client.IsCode(err, "agent_caller"):
		return "Refused: this terminal runs inside an agent session, and agents can't approve themselves. Use your own terminal."
	case errors.Is(err, client.ErrDaemonDown):
		return "The daemon isn't running: start it with `pilothouse start`"
	}
	return "Couldn't " + action + ": " + err.Error()
}

func (m *Model) cyclePresence() tea.Cmd {
	next := map[api.PresenceMode]api.PresenceMode{
		api.PresenceAuto:      api.PresenceForceHere,
		api.PresenceForceHere: api.PresenceForceAway,
		api.PresenceForceAway: api.PresenceAuto,
	}[m.presence.Mode]
	if next == "" {
		next = api.PresenceForceHere
	}
	be := m.opts.Backend
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		p, err := be.SetPresence(ctx, next)
		return presenceMsg{p: p, err: err}
	}
}

func (m *Model) startExplain() tea.Cmd {
	s := m.selectedSession()
	var id string
	if s != nil {
		id = s.ID
	} else if r := m.selected(); r != nil && r.req != nil {
		id = r.req.SessionID
	}
	if id == "" {
		m.setFlash(flashWarn, "Select a session to explain")
		return nil
	}
	m.mode = modeExplain
	m.explain, m.explainErr = nil, nil
	be := m.opts.Backend
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		ex, err := be.Explain(ctx, id)
		return explainMsg{sessionID: id, ex: ex, err: err}
	}
}

func (m *Model) enter() tea.Cmd {
	r := m.selected()
	if r == nil {
		return nil
	}
	if asks(r.req) && (r.req.Status == api.RequestOpen || r.req.Checkpoint == api.CheckpointPlan && !attachable(r.sess)) {
		// A held question or plan opens here; a plan in the agent's own
		// dialog can still be read here.
		return m.openAsk(r.req, 0)
	}
	if attachable(r.sess) {
		m.stopStream()
		c := exec.Command(m.opts.Exe, "attach", r.sess.ID)
		return tea.ExecProcess(c, func(err error) tea.Msg { return attachDoneMsg{err: err} })
	}
	m.expanded = !m.expanded
	return m.markSeen()
}

// presenceWords is the human form of a presence value.
func presenceWords(p api.Presence) string {
	state := "here"
	if p.State == api.PresenceAway {
		state = "away"
	}
	switch p.Mode {
	case api.PresenceForceHere, api.PresenceForceAway:
		return state + " (set by you)"
	}
	return state + " (auto)"
}
