// Package telegram is Pilothouse's two-way Telegram channel: requests
// arrive as messages with buttons (Allow / Allow for session / Deny,
// question options, plan Approve / Request changes), a reply to a
// "finished" message prompts that agent, and every message is edited when
// its request closes anywhere ("✓ Allowed from iPhone").
//
// Security model (docs/PLAN-0.2.md §5):
//   - The bot token lives in ~/.pilothouse/keys/telegram.json (0600) and
//     never reaches logs, events or errors (see Client).
//   - The bot talks to exactly one chat: the private chat that sent
//     /start <one-time code> from `pilothouse notify telegram`. Every
//     other chat is ignored, without an answer.
//   - Buttons carry single-use tokens (stored hashed) bound to the chat,
//     the message, the request and the action, valid until the request
//     expires. Pressing a button spends its whole keyboard; replays and
//     stale keyboards do nothing.
//   - notify.telegram.decisions=false makes the bot notify-only: no
//     buttons that answer, no replies to agents.
//   - Network failures never block the daemon: sends run on the notifier's
//     bounded workers with timeouts, polling runs on its own goroutine with
//     backoff, and 429s wait for Telegram's retry_after.
package telegram

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
	"github.com/xasdta/pilothouse/internal/notify"
)

// Options configures a Bot.
type Options struct {
	Client *Client
	DB     *sql.DB // shared SQLite handle; the bot owns tables prefixed telegram_
	Core   core.Core
	// ChatID is the paired chat; 0 until someone completes pairing.
	ChatID int64
	// Decisions lets buttons and replies act (notify.telegram.decisions).
	Decisions bool
	// Keys returns the current keys (for the pending pairing code).
	Keys func() Keys
	// OnPaired persists a newly paired chat (config chat_id) and spends
	// the pairing code. Required for pairing.
	OnPaired func(ctx context.Context, chat Chat) error
	Machine  string
	// PublicURL is the web app's https URL for "Open in the app" links, or "".
	PublicURL func() string
	// DeviceName names a paired device for "✓ Allowed from iPhone".
	DeviceName func(id string) string
	Log        *slog.Logger
	Now        func() time.Time
	// PollWait is the long-poll wait (default 25 s).
	PollWait time.Duration
	// ErrorBackoff is the first wait after a failed poll (default 2 s,
	// doubling to 5 min).
	ErrorBackoff time.Duration
	// MaxReplyAge drops replies that reach the daemon late, e.g. typed while
	// it was down (default 10 min): a stale prompt must not reach an agent.
	MaxReplyAge time.Duration
}

// Bot is a notify.Channel and notify.Withdrawer, plus the update poller.
type Bot struct {
	o    Options
	db   *db
	log  *slog.Logger
	chat atomic.Int64
	mu   sync.Mutex // serializes edits of request messages (refresh vs. close)
	// sendMu is held while a request message is sent and its buttons are
	// bound to it; a press waits for it, so a button tapped the instant it
	// appears is never refused as unknown.
	sendMu sync.Mutex

	pruned time.Time
}

var (
	_ notify.Channel    = (*Bot)(nil)
	_ notify.Withdrawer = (*Bot)(nil)
)

// New migrates the telegram_* tables and returns a bot. It does not touch
// the network.
func New(o Options) (*Bot, error) {
	if o.Client == nil || o.DB == nil || o.Core == nil {
		return nil, errors.New("telegram: Client, DB and Core are required")
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Keys == nil {
		o.Keys = func() Keys { return Keys{} }
	}
	if o.PublicURL == nil {
		o.PublicURL = func() string { return "" }
	}
	if o.PollWait <= 0 {
		o.PollWait = 25 * time.Second
	}
	if o.ErrorBackoff <= 0 {
		o.ErrorBackoff = 2 * time.Second
	}
	if o.MaxReplyAge <= 0 {
		o.MaxReplyAge = 10 * time.Minute
	}
	if o.Machine == "" {
		o.Machine = "this computer"
	}
	if err := Migrate(context.Background(), o.DB); err != nil {
		return nil, err
	}
	b := &Bot{o: o, db: &db{sql: o.DB, now: o.Now}, log: o.Log}
	b.chat.Store(o.ChatID)
	return b, nil
}

func (*Bot) Name() string { return "telegram" }
func (*Bot) Remote() bool { return true }

// Chat is the paired chat (0 = not paired).
func (b *Bot) Chat() int64 { return b.chat.Load() }

func (b *Bot) by() string { return "telegram:" + strconv.FormatInt(b.Chat(), 10) }

// ---- outgoing ----

// Send delivers a notification to the paired chat.
func (b *Bot) Send(ctx context.Context, n core.Notification) error {
	chat := b.Chat()
	if chat == 0 {
		return nil
	}
	switch n.Kind {
	case core.NotifyRequest:
		if n.Request == nil || n.Request.Status != api.RequestOpen {
			return nil
		}
		return b.sendRequest(ctx, chat, n)
	case core.NotifyBlocked:
		if n.Request == nil {
			return nil
		}
		// A request we sent with buttons is edited when it closes instead
		// ("⏱ Timed out — answer in the terminal").
		if had, err := b.db.hasRequestMessage(ctx, n.Request.ID); err != nil || had {
			return err
		}
		_, err := b.send(ctx, SendMessage{ChatID: chat, Text: blockedText(n)})
		return err
	case core.NotifyWaiting:
		if n.Session == nil {
			return nil
		}
		canReply := b.o.Decisions && n.Session.Caps.Reply
		m, err := b.send(ctx, SendMessage{ChatID: chat, Text: sessionText(n, canReply)})
		if err != nil {
			return err
		}
		return b.db.saveMessage(ctx, msgRow{ChatID: chat, MessageID: m.MessageID, Kind: kindSession, SessionID: n.Session.ID})
	case core.NotifyTest:
		_, err := b.send(ctx, SendMessage{ChatID: chat, Text: testText(n, b.o.Machine)})
		return err
	}
	text := esc(notify.Line(n))
	if text == "" {
		return nil
	}
	_, err := b.send(ctx, SendMessage{ChatID: chat, Text: text})
	return err
}

func (b *Bot) send(ctx context.Context, m SendMessage) (Message, error) {
	return b.o.Client.Send(ctx, m)
}

func (b *Bot) sendRequest(ctx context.Context, chat int64, n core.Notification) error {
	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	r := *n.Request
	st := answerState{Answers: map[string]string{}}
	text := fitRequest(n, st, b.o.Now())
	kb, data, err := b.keyboard(ctx, chat, r, st)
	if err != nil {
		return err
	}
	m, err := b.send(ctx, SendMessage{ChatID: chat, Text: text, ReplyMarkup: kb})
	if err != nil {
		b.db.discard(context.WithoutCancel(ctx), data)
		return err
	}
	// The message exists now: record it even if the caller's deadline just
	// passed, or its buttons would never work.
	ctx = context.WithoutCancel(ctx)
	if err := b.db.saveMessage(ctx, msgRow{ChatID: chat, MessageID: m.MessageID, Kind: kindRequest, RequestID: r.ID, SessionID: r.SessionID, Text: text, State: st.String()}); err != nil {
		return err
	}
	return b.db.bind(ctx, data, chat, m.MessageID)
}

// keyboard mints tokens for a request's buttons. It returns nil (no
// keyboard) when there are no buttons.
func (b *Bot) keyboard(ctx context.Context, chat int64, r api.Request, st answerState) (*InlineKeyboard, []string, error) {
	spec := requestKeyboard(r, st, b.o.Decisions, appURL(b.o.PublicURL()))
	if len(spec) == 0 {
		return nil, nil, nil
	}
	exp := r.ExpiresAt
	if now := b.o.Now(); exp.IsZero() || !exp.After(now) {
		exp = now.Add(10 * time.Minute)
	}
	kb := &InlineKeyboard{}
	var data []string
	for _, row := range spec {
		var out []Button
		for _, s := range row {
			if s.url != "" {
				out = append(out, Button{Text: s.text, URL: s.url})
				continue
			}
			tok, err := b.db.mint(ctx, chat, r.ID, s.action, exp)
			if err != nil {
				b.db.discard(context.WithoutCancel(ctx), data)
				return nil, nil, err
			}
			data = append(data, tok)
			out = append(out, Button{Text: s.text, CallbackData: tok})
		}
		kb.Rows = append(kb.Rows, out)
	}
	return kb, data, nil
}

// Withdraw edits every message of a closed request to say how it ended
// and removes its buttons.
func (b *Bot) Withdraw(ctx context.Context, r api.Request) error {
	if r.Status == api.RequestOpen {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	msgs, err := b.db.requestMessages(ctx, r.ID)
	if err != nil || len(msgs) == 0 {
		return err
	}
	var errs []error
	for _, m := range msgs {
		err := b.o.Client.Edit(ctx, EditMessageText{ChatID: m.ChatID, MessageID: m.MessageID,
			Text: closedText(m.Text, r, b.o.DeviceName), ReplyMarkup: &InlineKeyboard{Rows: [][]Button{}}})
		if err != nil && !gone(err) {
			errs = append(errs, err)
		}
	}
	// Close even when an edit failed: the buttons are spent either way.
	if err := b.db.closeRequest(context.WithoutCancel(ctx), r.ID); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// refresh re-renders an open request message with fresh buttons (after a
// toggle, or after a press that didn't close the request).
func (b *Bot) refresh(ctx context.Context, chat, messageID int64, r api.Request, st answerState) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	m, ok, err := b.db.message(ctx, chat, messageID)
	if err != nil || !ok || m.Closed {
		return err
	}
	var sess *api.Session
	if s, err := b.o.Core.Session(ctx, r.SessionID); err == nil {
		sess = &s
	}
	text := fitRequest(core.Notification{Kind: core.NotifyRequest, Request: &r, Session: sess}, st, b.o.Now())
	kb, data, err := b.keyboard(ctx, chat, r, st)
	if err != nil {
		return err
	}
	if kb == nil {
		kb = &InlineKeyboard{Rows: [][]Button{}}
	}
	if err := b.db.bind(ctx, data, chat, messageID); err != nil {
		return err
	}
	if err := b.o.Client.Edit(ctx, EditMessageText{ChatID: chat, MessageID: messageID, Text: text, ReplyMarkup: kb}); err != nil {
		return err
	}
	m.Text, m.State = text, st.String()
	return b.db.saveMessage(ctx, m)
}

// ---- incoming ----

// Run long-polls for updates until ctx ends. Errors are logged and
// retried with backoff; Run never returns early on its own.
func (b *Bot) Run(ctx context.Context) {
	offset := b.db.offset(ctx)
	delay := b.o.ErrorBackoff
	for ctx.Err() == nil {
		ups, err := b.o.Client.GetUpdates(ctx, offset, b.o.PollWait)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			switch {
			case IsCode(err, 401), IsCode(err, 404):
				b.log.Error("telegram: the bot token was rejected (revoked?) — run `pilothouse notify telegram` again", "err", err)
			case IsCode(err, 409):
				b.log.Warn("telegram: another program is reading this bot's updates, or a webhook is set; Pilothouse needs the bot to itself", "err", err)
			default:
				b.log.Warn("telegram: polling failed", "err", err, "retry_in", delay)
			}
			t := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
			delay = min(delay*2, 5*time.Minute)
			continue
		}
		delay = b.o.ErrorBackoff
		for _, u := range ups {
			if ctx.Err() != nil {
				return
			}
			b.Handle(ctx, u)
			offset = u.UpdateID + 1
			if err := b.db.setOffset(ctx, offset); err != nil {
				b.log.Warn("telegram: save offset", "err", err)
			}
		}
		b.maybePrune(ctx)
	}
}

func (b *Bot) maybePrune(ctx context.Context) {
	now := b.o.Now()
	if now.Sub(b.pruned) < time.Hour {
		return
	}
	b.pruned = now
	if err := b.db.prune(ctx); err != nil {
		b.log.Warn("telegram: prune", "err", err)
	}
}

// Handle processes one update (exported for tests and the CLI).
func (b *Bot) Handle(ctx context.Context, u Update) {
	switch {
	case u.Message != nil:
		m := u.Message
		if m.From != nil && m.From.IsBot {
			return
		}
		chat := b.Chat()
		if chat == 0 || m.Chat.ID != chat {
			b.maybePair(ctx, m)
			return
		}
		if m.From != nil && m.From.ID != chat {
			return
		}
		b.onMessage(ctx, m)
	case u.CallbackQuery != nil:
		q := u.CallbackQuery
		chat := b.Chat()
		if chat == 0 || q.Message == nil || q.Message.Chat.ID != chat || q.From.ID != chat {
			return // not our chat: no answer at all
		}
		b.onCallback(ctx, q)
	}
}

// maybePair binds the private chat that sends /start <pending code>.
// Anything else from an unpaired chat is ignored without an answer.
func (b *Bot) maybePair(ctx context.Context, m *Message) {
	code, ok := startCode(m.Text)
	if !ok || code == "" || m.Chat.Type != "private" || b.o.OnPaired == nil {
		return
	}
	if !b.o.Keys().CheckCode(code, b.o.Now()) {
		b.log.Info("telegram: ignored /start with an unknown or expired pairing code")
		return
	}
	if err := b.o.OnPaired(ctx, m.Chat); err != nil {
		b.log.Error("telegram: pairing failed", "err", err)
		return
	}
	b.chat.Store(m.Chat.ID)
	b.log.Info("telegram: paired", "chat", m.Chat.ID)
	// Pairing changes the settings, so the daemon restarts the bot right
	// away: the welcome must not die with this poller's context.
	b.reply(context.WithoutCancel(ctx), m, "✅ <b>Connected to "+esc(b.o.Machine)+".</b>\nWhen you're away, requests that need you show up here"+
		b.decisionsNote()+".\nSend /status to see what's waiting.")
}

func (b *Bot) decisionsNote() string {
	if b.o.Decisions {
		return " with buttons to answer them, and you can reply to a “finished” message to send that agent a follow-up"
	}
	return " (notify only: answering from Telegram is turned off)"
}

func (b *Bot) reply(ctx context.Context, to *Message, text string) {
	_, err := b.send(ctx, SendMessage{ChatID: to.Chat.ID, Text: text,
		ReplyParameters: &ReplyParameters{MessageID: to.MessageID, AllowSendingWithoutReply: true}})
	if err != nil {
		b.log.Warn("telegram: reply", "err", err)
	}
}

func (b *Bot) onMessage(ctx context.Context, m *Message) {
	text := strings.TrimSpace(m.Text)
	if m.ReplyToMessage != nil {
		b.onReply(ctx, m, text)
		return
	}
	cmd := ""
	if f := strings.Fields(text); len(f) > 0 {
		cmd, _, _ = strings.Cut(f[0], "@")
	}
	switch cmd {
	case "/start":
		b.reply(ctx, m, "✅ Connected to "+esc(b.o.Machine)+". Send /status to see what's waiting.")
	case "/status":
		b.status(ctx, m)
	default:
		b.reply(ctx, m, b.help())
	}
}

func (b *Bot) help() string {
	s := "Pilothouse sends requests here when you're away from " + esc(b.o.Machine) + "."
	if b.o.Decisions {
		s += "\n• Tap the buttons on a request to answer it.\n• Reply to a “finished” message to send that agent a follow-up."
	} else {
		s += "\n• Answering from Telegram is turned off (notify.telegram.decisions: false)."
	}
	return s + "\n• /status — what's waiting right now."
}

// status lists open requests and session counts, and re-sends open
// requests that have no message with buttons here yet.
func (b *Bot) status(ctx context.Context, m *Message) {
	snap, err := b.o.Core.Snapshot(ctx)
	if err != nil {
		b.reply(ctx, m, "Couldn't read the inbox: internal error.")
		b.log.Error("telegram: snapshot", "err", err)
		return
	}
	sessions := map[string]api.Session{}
	counts := map[api.State]int{}
	for _, s := range snap.Sessions {
		sessions[s.ID] = s
		counts[s.State]++
	}
	var open []api.Request
	for _, r := range snap.Requests {
		if r.Status == api.RequestOpen {
			open = append(open, r)
		}
	}
	sort.Slice(open, func(i, j int) bool { return open[i].CreatedAt.Before(open[j].CreatedAt) })

	var sb strings.Builder
	sb.WriteString("🧭 <b>" + esc(snap.Machine) + "</b>")
	if snap.Presence.State == api.PresenceAway {
		sb.WriteString(" — you're away")
	} else {
		sb.WriteString(" — you're at the desk")
	}
	switch len(open) {
	case 0:
		sb.WriteString("\nNothing is waiting for you.")
	case 1:
		sb.WriteString("\n1 request is waiting:")
	default:
		sb.WriteString("\n" + strconv.Itoa(len(open)) + " requests are waiting:")
	}
	for i, r := range open {
		if i == 10 {
			sb.WriteString("\n…")
			break
		}
		var sp *api.Session
		if s, ok := sessions[r.SessionID]; ok {
			sp = &s
		}
		line := notify.AgentName(r.Agent) + " " + notify.Action(&r)
		if w := notify.Where(&r, sp); w != "" {
			line += " · " + w
		}
		if r.Route != api.RouteDevices {
			line += " (in the terminal)"
		}
		sb.WriteString("\n• " + esc(notify.Short(line, 200)))
	}
	var parts []string
	for _, st := range []api.State{api.StateWorking, api.StateBlocked, api.StateWaiting} {
		if n := counts[st]; n > 0 {
			parts = append(parts, strconv.Itoa(n)+" "+string(st))
		}
	}
	if len(parts) > 0 {
		sb.WriteString("\nSessions: " + strings.Join(parts, ", ") + ".")
	}
	b.reply(ctx, m, sb.String())

	// Held requests without buttons here (sent before pairing, or a send
	// that failed) come again so they can be answered.
	sent := 0
	for _, r := range open {
		if r.Route != api.RouteDevices || sent == 5 {
			continue
		}
		if had, err := b.db.hasRequestMessage(ctx, r.ID); err != nil || had {
			continue
		}
		var sp *api.Session
		if s, ok := sessions[r.SessionID]; ok {
			sp = &s
		}
		if err := b.sendRequest(ctx, m.Chat.ID, core.Notification{Kind: core.NotifyRequest, Request: &r, Session: sp}); err != nil {
			b.log.Warn("telegram: resend request", "request", r.ID, "err", err)
		}
		sent++
	}
}

func (b *Bot) onReply(ctx context.Context, m *Message, text string) {
	ref, ok, err := b.db.message(ctx, m.Chat.ID, m.ReplyToMessage.MessageID)
	if err != nil {
		b.log.Error("telegram: load message", "err", err)
		return
	}
	if !ok {
		b.reply(ctx, m, "That message isn't one Pilothouse can route a reply from (or it's older than two weeks). Reply to a “finished” message, or send /status.")
		return
	}
	if !b.o.Decisions {
		b.reply(ctx, m, "Answering from Telegram is turned off (notify.telegram.decisions: false), so this wasn't sent.")
		return
	}
	if sent := time.Unix(m.Date, 0); m.Date > 0 && b.o.Now().Sub(sent) > b.o.MaxReplyAge {
		b.reply(ctx, m, "This reply reached Pilothouse late (sent at "+sent.Local().Format("15:04")+") and wasn't delivered. Send it again if you still want to.")
		return
	}
	if text == "" {
		b.reply(ctx, m, "Only text can be sent to an agent.")
		return
	}
	switch ref.Kind {
	case kindFeedback:
		b.planFeedback(ctx, m, ref.RequestID, text)
		return
	case kindRequest:
		r, err := b.o.Core.Request(ctx, ref.RequestID)
		if err != nil {
			b.reply(ctx, m, "That request is gone.")
			return
		}
		if r.Status == api.RequestOpen {
			if r.Checkpoint == api.CheckpointPlan {
				b.planFeedback(ctx, m, r.ID, text)
				return
			}
			b.reply(ctx, m, "Use the buttons to answer this request. To give the agent a new instruction, reply to its “finished” message.")
			return
		}
		b.sendReply(ctx, m, r.SessionID, text)
	case kindSession:
		b.sendReply(ctx, m, ref.SessionID, text)
	}
}

func (b *Bot) planFeedback(ctx context.Context, m *Message, requestID, text string) {
	out, err := b.o.Core.Decide(ctx, api.Decision{RequestID: requestID, Behavior: api.BehaviorDeny,
		Message: clip(text, 2000), By: b.by(), At: b.o.Now().UTC()})
	if err != nil {
		b.reply(ctx, m, esc(decideErr(err)))
		b.closeIfDone(ctx, requestID, err)
		return
	}
	if err := b.Withdraw(ctx, out); err != nil {
		b.log.Warn("telegram: edit closed request", "request", requestID, "err", err)
	}
	b.reply(ctx, m, "✎ Sent. "+esc(notify.AgentName(out.Agent))+" will revise the plan.")
}

func (b *Bot) sendReply(ctx context.Context, m *Message, sessionID, text string) {
	rep, ok := b.o.Core.(core.Replier)
	if !ok || sessionID == "" {
		b.reply(ctx, m, "Replies to agents aren't available in this Pilothouse daemon.")
		return
	}
	res, err := rep.Reply(ctx, sessionID, api.Reply{Text: clip(text, 4000)}, b.by())
	switch {
	case errors.Is(err, core.ErrUnsupported):
		reason := strings.TrimPrefix(strings.TrimPrefix(err.Error(), core.ErrUnsupported.Error()), ": ")
		if reason == "" {
			reason = "this session can't receive replies yet (sessions started with `pilothouse run`, and Claude Code sessions with a messaging socket, can)"
		}
		b.reply(ctx, m, "Couldn't send this: "+esc(reason)+".")
	case errors.Is(err, core.ErrNotFound):
		b.reply(ctx, m, "That session is gone.")
	case errors.Is(err, core.ErrInvalid):
		b.reply(ctx, m, "Couldn't send this: "+esc(strings.TrimPrefix(err.Error(), core.ErrInvalid.Error()+": "))+".")
	case err != nil:
		b.log.Error("telegram: reply to session", "session", sessionID, "err", err)
		b.reply(ctx, m, "Couldn't send this: internal error.")
	default:
		s := "✓ Sent"
		if res.Note != "" {
			s += " — " + esc(notify.Short(res.Note, 200))
		}
		b.reply(ctx, m, s)
	}
}

func (b *Bot) onCallback(ctx context.Context, q *CallbackQuery) {
	chat, msgID := q.Message.Chat.ID, q.Message.MessageID
	if !b.o.Decisions {
		b.answer(ctx, q, "Answering from Telegram is turned off.", false)
		return
	}
	b.sendMu.Lock() // wait for a message still being recorded (see sendMu)
	b.sendMu.Unlock()
	reqID, action, res, err := b.db.spend(ctx, q.Data, chat, msgID)
	if err != nil {
		b.log.Error("telegram: spend token", "err", err)
		b.answer(ctx, q, "Something went wrong; try again.", false)
		return
	}
	switch res {
	case tokenUnknown:
		b.answer(ctx, q, "This button no longer works.", false)
		return
	case tokenUsed:
		b.answer(ctx, q, "This button was already used.", false)
		return
	case tokenExpired:
		b.answer(ctx, q, "This request has expired.", false)
		return
	}
	r, err := b.o.Core.Request(ctx, reqID)
	if err != nil {
		b.answer(ctx, q, "That request is gone.", false)
		return
	}
	if r.Status != api.RequestOpen {
		b.answer(ctx, q, "Already closed: "+notify.ResolvedLine(r, b.o.DeviceName), false)
		_ = b.Withdraw(ctx, r)
		return
	}
	m, _, err := b.db.message(ctx, chat, msgID)
	if err != nil {
		b.log.Error("telegram: load message", "err", err)
	}
	st := parseState(m.State)

	switch {
	case action == "allow":
		b.decide(ctx, q, r, st, api.Decision{Behavior: api.BehaviorAllow, Scope: api.ScopeOnce})
	case action == "session":
		b.decide(ctx, q, r, st, api.Decision{Behavior: api.BehaviorAllow, Scope: api.ScopeSession})
	case action == "deny":
		b.decide(ctx, q, r, st, api.Decision{Behavior: api.BehaviorDeny})
	case action == "approve":
		b.decide(ctx, q, r, st, api.Decision{Behavior: api.BehaviorAllow})
	case action == "changes":
		b.askForChanges(ctx, q, r, st)
	case strings.HasPrefix(action, "opt:") || strings.HasPrefix(action, "done:"):
		b.onOption(ctx, q, r, st, action)
	default:
		b.answer(ctx, q, "This button no longer works.", false)
		b.keepButtons(ctx, chat, msgID, r, st)
	}
}

func (b *Bot) answer(ctx context.Context, q *CallbackQuery, text string, alert bool) {
	if err := b.o.Client.Answer(ctx, q.ID, notify.Short(text, 190), alert); err != nil {
		b.log.Debug("telegram: answer callback", "err", err)
	}
}

// keepButtons puts fresh buttons back after a press that left the request
// open (the press spent the old keyboard).
func (b *Bot) keepButtons(ctx context.Context, chat, msgID int64, r api.Request, st answerState) {
	if err := b.refresh(ctx, chat, msgID, r, st); err != nil {
		b.log.Warn("telegram: refresh buttons", "request", r.ID, "err", err)
	}
}

func (b *Bot) decide(ctx context.Context, q *CallbackQuery, r api.Request, st answerState, d api.Decision) {
	d.RequestID, d.By, d.At = r.ID, b.by(), b.o.Now().UTC()
	out, err := b.o.Core.Decide(ctx, d)
	if err != nil {
		b.answer(ctx, q, decideErr(err), errors.Is(err, core.ErrInvalid))
		if !b.closeIfDone(ctx, r.ID, err) {
			b.keepButtons(ctx, q.Message.Chat.ID, q.Message.MessageID, r, st)
		}
		return
	}
	b.log.Info("request decided", "request", r.ID, "behavior", d.Behavior, "scope", d.Scope, "by", d.By)
	if err := b.Withdraw(ctx, out); err != nil {
		b.log.Warn("telegram: edit closed request", "request", r.ID, "err", err)
	}
	b.answer(ctx, q, notify.Mark(out)+" "+notify.Outcome(out), false)
}

// closeIfDone edits the message of a request someone else answered first.
func (b *Bot) closeIfDone(ctx context.Context, id string, err error) bool {
	if !errors.Is(err, core.ErrAlreadyClosed) {
		return false
	}
	if r, err := b.o.Core.Request(ctx, id); err == nil && r.Status != api.RequestOpen {
		_ = b.Withdraw(ctx, r)
	}
	return true
}

func decideErr(err error) string {
	switch {
	case errors.Is(err, core.ErrAlreadyClosed):
		return "Already answered elsewhere."
	case errors.Is(err, core.ErrNotFound):
		return "That request is gone."
	case errors.Is(err, core.ErrInvalid):
		return "Not accepted: " + strings.TrimPrefix(err.Error(), core.ErrInvalid.Error()+": ")
	}
	return "Couldn't answer: " + strings.TrimPrefix(fmt.Sprint(err), "invalid: ")
}

// askForChanges asks for plan feedback with a ForceReply prompt; the reply
// denies the plan with that message.
func (b *Bot) askForChanges(ctx context.Context, q *CallbackQuery, r api.Request, st answerState) {
	chat, msgID := q.Message.Chat.ID, q.Message.MessageID
	p, err := b.send(ctx, SendMessage{ChatID: chat, Text: "✎ What should change in the plan? Reply to this message.",
		ReplyMarkup:     &ForceReply{ForceReply: true, Placeholder: "Describe the changes"},
		ReplyParameters: &ReplyParameters{MessageID: msgID, AllowSendingWithoutReply: true}})
	if err != nil {
		b.answer(ctx, q, "Couldn't ask for your changes; reply to the plan message instead.", false)
	} else {
		if err := b.db.saveMessage(ctx, msgRow{ChatID: chat, MessageID: p.MessageID, Kind: kindFeedback, RequestID: r.ID, SessionID: r.SessionID}); err != nil {
			b.log.Error("telegram: save prompt", "err", err)
		}
		b.answer(ctx, q, "Reply with the changes you want.", false)
	}
	b.keepButtons(ctx, chat, msgID, r, st)
}

// onOption handles question buttons: pick (single-select), toggle
// (multi-select) and done. Answers advance through the questions; the
// last one answers the request.
func (b *Bot) onOption(ctx context.Context, q *CallbackQuery, r api.Request, st answerState, action string) {
	chat, msgID := q.Message.Chat.ID, q.Message.MessageID
	parts := strings.Split(action, ":")
	qi, err1 := strconv.Atoi(parts[1])
	if err1 != nil || qi != st.Q || qi >= len(r.Questions) {
		b.answer(ctx, q, "Pick again: the question changed.", false)
		b.keepButtons(ctx, chat, msgID, r, st)
		return
	}
	question := r.Questions[qi]
	switch parts[0] {
	case "opt":
		oi := -1
		if len(parts) == 3 {
			oi, _ = strconv.Atoi(parts[2])
		}
		if oi < 0 || oi >= len(question.Options) {
			b.answer(ctx, q, "Pick again: the options changed.", false)
			b.keepButtons(ctx, chat, msgID, r, st)
			return
		}
		if question.MultiSelect {
			if st.selected(oi) {
				var keep []int
				for _, s := range st.Sel {
					if s != oi {
						keep = append(keep, s)
					}
				}
				st.Sel = keep
			} else {
				st.Sel = append(st.Sel, oi)
			}
			b.answer(ctx, q, "", false)
			b.keepButtons(ctx, chat, msgID, r, st)
			return
		}
		st.Answers[question.Key()] = question.Options[oi].Label
	case "done":
		if !question.MultiSelect {
			b.keepButtons(ctx, chat, msgID, r, st)
			b.answer(ctx, q, "Pick an option.", false)
			return
		}
		if len(st.Sel) == 0 {
			b.answer(ctx, q, "Pick at least one option, or answer in the app.", true)
			b.keepButtons(ctx, chat, msgID, r, st)
			return
		}
		sort.Ints(st.Sel)
		var labels []string
		for _, i := range st.Sel {
			labels = append(labels, question.Options[i].Label)
		}
		st.Answers[question.Key()] = api.JoinAnswers(labels)
	}
	st.Q++
	st.Sel = nil
	if st.Q < len(r.Questions) {
		b.answer(ctx, q, "", false)
		b.keepButtons(ctx, chat, msgID, r, st)
		return
	}
	b.decide(ctx, q, r, answerState{Q: 0, Answers: map[string]string{}}, api.Decision{Behavior: api.BehaviorAllow, Answers: st.Answers})
}
