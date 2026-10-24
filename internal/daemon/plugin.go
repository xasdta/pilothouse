package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
	"github.com/xasdta/pilothouse/internal/store"
)

// Agents integrated through a plugin running inside the agent (OpenCode;
// workstream E). The plugin reports hooks over POST /v1/plugin/hook and
// keeps a command channel open — POST /v1/plugin/commands, a long poll —
// that delivers the replies a person sends to its sessions; it runs them
// with OpenCode's client.session.promptAsync and acknowledges each one.
//
// Both routes are unguarded: the plugin runs inside the agent, which the
// agent-caller guard exists to keep out. What protects replies is the
// channel token. Each plugin process makes a random token and sends it
// with every call. The first token to report an agent session owns it;
// another token takes over only once the owner's channel is gone (OpenCode
// was quit and the session resumed in a new process). While two live
// tokens claim one session it is contested: replies to it are refused and
// the daemon logs why. A channel receives only the commands of sessions
// its token owns and can acknowledge only those. The hub keeps a SHA-256
// of each token; tokens never reach the database, events, logs or the API.

const (
	// PluginTokenHeader carries a plugin's channel token.
	PluginTokenHeader = "X-Pilothouse-Plugin"
	pluginPollMax     = 30 * time.Second // longest command poll kept open
	pluginForget      = 10 * time.Minute // dead channels owning nothing are dropped
	maxPluginQueue    = 16               // commands waiting per channel
	maxPluginChannels = 256
	maxPluginOwners   = 10000
	maxPluginAnnounce = 200
	maxPluginReply    = 64 << 10
	maxPluginAckError = 300
)

// Timing; variables so tests can shorten them.
var (
	pluginLive    = 15 * time.Second // a channel polled this recently is live
	pluginSweep   = 5 * time.Second  // how often liveness changes are noticed
	pluginAckWait = 10 * time.Second // how long a reply waits for the plugin
)

// pluginAgents are the agents whose Pilothouse integration is a plugin.
var pluginAgents = map[api.AgentKind]bool{api.AgentOpenCode: true}

// pluginTokenRE: 32 random bytes, base64url without padding.
var pluginTokenRE = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

type pluginKey struct {
	agent   api.AgentKind
	session string // the agent's own session id
}

type pluginOwner struct {
	channel string          // token hash
	rivals  map[string]bool // other live channels that claimed it: contested
	at      time.Time
}

// pluginCommand is one reply on its way to a plugin.
type pluginCommand struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"` // the agent's session id
	Text      string `json:"text,omitempty"`
	Interrupt bool   `json:"interrupt,omitempty"`
}

type pluginAck struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

type pluginChannel struct {
	id       string // token hash
	created  time.Time
	polls    int // polls in flight
	lastPoll time.Time // zero until the first poll
	live     bool // liveness last reported to sessions (caps)
	queue    []pluginCommand
	wake     chan struct{} // closed when the queue grows
	acks     map[string]chan pluginAck
}

// pluginHub tracks plugin channels and which agent sessions they own. Its
// zero value is ready to use; it never touches the database while holding
// its lock (session saves read it through pluginCaps).
type pluginHub struct {
	mu       sync.Mutex
	sweeping bool
	owners   map[pluginKey]*pluginOwner
	channels map[string]*pluginChannel
}

func hashToken(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

// liveLocked reports whether channel id polls (or just did): replies can
// reach it.
func (h *pluginHub) liveLocked(id string, now time.Time) bool {
	c := h.channels[id]
	return c != nil && (c.polls > 0 || (!c.lastPoll.IsZero() && now.Sub(c.lastPoll) < pluginLive))
}

// presentLocked reports whether channel id still counts as the owner of
// its sessions: live, or reported a hook moments ago and is about to poll.
func (h *pluginHub) presentLocked(id string, now time.Time) bool {
	c := h.channels[id]
	return c != nil && (h.liveLocked(id, now) || now.Sub(c.created) < pluginLive)
}

// channelLocked returns the channel for id, creating it if there is room.
func (h *pluginHub) channelLocked(id string, now time.Time) *pluginChannel {
	if c := h.channels[id]; c != nil {
		return c
	}
	if h.channels == nil {
		h.channels = map[string]*pluginChannel{}
	}
	if len(h.channels) >= maxPluginChannels {
		h.forgetLocked(now, true)
		if len(h.channels) >= maxPluginChannels {
			return nil
		}
	}
	c := &pluginChannel{id: id, created: now, wake: make(chan struct{}), acks: map[string]chan pluginAck{}}
	h.channels[id] = c
	return c
}

// lastSeen is when a channel last showed itself.
func lastSeen(c *pluginChannel) time.Time {
	if c.lastPoll.After(c.created) {
		return c.lastPoll
	}
	return c.created
}

// forgetLocked drops dead channels that own no session (all dead channels
// owning nothing when pressed is set, else only long-dead ones).
func (h *pluginHub) forgetLocked(now time.Time, pressed bool) {
	owning := map[string]bool{}
	for _, o := range h.owners {
		owning[o.channel] = true
	}
	for id, c := range h.channels {
		if owning[id] || h.presentLocked(id, now) || len(c.queue) > 0 || len(c.acks) > 0 {
			continue
		}
		if pressed || now.Sub(lastSeen(c)) > pluginForget {
			delete(h.channels, id)
		}
	}
}

// claim records that channel id reported key. It reports whether what a
// person can do with the session may have changed.
func (h *pluginHub) claim(key pluginKey, id string, now time.Time) (changed bool, contested bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.channelLocked(id, now) == nil {
		return false, false
	}
	if h.owners == nil {
		h.owners = map[pluginKey]*pluginOwner{}
	}
	o := h.owners[key]
	switch {
	case o == nil:
		if len(h.owners) >= maxPluginOwners {
			h.evictOwnerLocked()
		}
		h.owners[key] = &pluginOwner{channel: id, at: now}
		return true, false
	case o.channel == id:
		was := len(o.rivals) > 0
		h.pruneRivalsLocked(o, now)
		return was != (len(o.rivals) > 0), len(o.rivals) > 0
	case !h.presentLocked(o.channel, now):
		// The owner is gone (its OpenCode quit); a resumed session in a
		// new process takes over.
		h.owners[key] = &pluginOwner{channel: id, at: now}
		return true, false
	default:
		if o.rivals == nil {
			o.rivals = map[string]bool{}
		}
		was := len(o.rivals) > 0
		o.rivals[id] = true
		return !was, true
	}
}

func (h *pluginHub) pruneRivalsLocked(o *pluginOwner, now time.Time) {
	for r := range o.rivals {
		if !h.presentLocked(r, now) {
			delete(o.rivals, r)
		}
	}
}

func (h *pluginHub) evictOwnerLocked() {
	var oldest pluginKey
	var at time.Time
	for k, o := range h.owners {
		if at.IsZero() || o.at.Before(at) {
			oldest, at = k, o.at
		}
	}
	delete(h.owners, oldest)
}

// reachable reports whether key's owner channel is live and uncontested.
func (h *pluginHub) reachable(key pluginKey, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	o := h.owners[key]
	if o == nil {
		return false
	}
	h.pruneRivalsLocked(o, now)
	return len(o.rivals) == 0 && h.liveLocked(o.channel, now)
}

// ownedLocked lists the sessions channel id owns.
func (h *pluginHub) ownedLocked(id string) []pluginKey {
	var out []pluginKey
	for k, o := range h.owners {
		if o.channel == id {
			out = append(out, k)
		}
	}
	return out
}

var (
	errPluginNotConnected = fmt.Errorf("%w: the agent's Pilothouse plugin is not connected (is the agent still running?)", core.ErrUnsupported)
	errPluginContested    = fmt.Errorf("%w: two agent processes report this session, so Pilothouse won't guess which one gets the reply", core.ErrUnsupported)
	errPluginBusy         = fmt.Errorf("%w: too many replies are waiting for this agent; try again in a moment", core.ErrUnsupported)
)

// send queues cmd for key's owner and returns where its ack will arrive.
func (h *pluginHub) send(key pluginKey, cmd pluginCommand, now time.Time) (string, <-chan pluginAck, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	o := h.owners[key]
	if o == nil {
		return "", nil, errPluginNotConnected
	}
	h.pruneRivalsLocked(o, now)
	if len(o.rivals) > 0 {
		return "", nil, errPluginContested
	}
	c := h.channels[o.channel]
	if c == nil || !h.liveLocked(o.channel, now) {
		return "", nil, errPluginNotConnected
	}
	if len(c.queue) >= maxPluginQueue {
		return "", nil, errPluginBusy
	}
	ack := make(chan pluginAck, 1)
	c.queue = append(c.queue, cmd)
	c.acks[cmd.ID] = ack
	close(c.wake)
	c.wake = make(chan struct{})
	return c.id, ack, nil
}

// cancel withdraws a command nobody acknowledged in time. It reports
// whether the plugin had already taken it (it may still run).
func (h *pluginHub) cancel(channel, cmdID string) (taken bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.channels[channel]
	if c == nil {
		return false
	}
	delete(c.acks, cmdID)
	for i, q := range c.queue {
		if q.ID == cmdID {
			c.queue = append(c.queue[:i:i], c.queue[i+1:]...)
			return false
		}
	}
	return true
}

// pollStart registers a poll. becameLive lists the sessions to re-save
// when the channel was dead before.
func (h *pluginHub) pollStart(id string, now time.Time) (c *pluginChannel, becameLive []pluginKey) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c = h.channelLocked(id, now)
	if c == nil {
		return nil, nil
	}
	wasLive := c.live && h.liveLocked(id, now)
	c.polls++
	c.lastPoll = now
	c.live = true
	if !wasLive {
		becameLive = h.ownedLocked(id)
	}
	return c, becameLive
}

// take waits until c has commands, the wait ends or ctx or stop is done,
// and hands the queued commands over.
func (h *pluginHub) take(ctx context.Context, stop <-chan struct{}, c *pluginChannel, wait time.Duration) []pluginCommand {
	t := time.NewTimer(wait)
	defer t.Stop()
	for {
		h.mu.Lock()
		if len(c.queue) > 0 {
			q := c.queue
			c.queue = nil
			h.mu.Unlock()
			return q
		}
		wake := c.wake
		h.mu.Unlock()
		select {
		case <-wake:
		case <-t.C:
			return nil
		case <-ctx.Done():
			return nil
		case <-stop:
			// Shutdown: the HTTP server doesn't cancel in-flight requests.
			return nil
		}
	}
}

func (h *pluginHub) pollEnd(c *pluginChannel, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c.polls--
	c.lastPoll = now
}

// requeue puts back commands a poll took but could not deliver.
func (h *pluginHub) requeue(c *pluginChannel, cmds []pluginCommand) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var keep []pluginCommand
	for _, q := range cmds {
		if _, waiting := c.acks[q.ID]; waiting {
			keep = append(keep, q)
		}
	}
	c.queue = append(keep, c.queue...)
}

// ack delivers a plugin's acknowledgement; only the channel that holds the
// command can acknowledge it.
func (h *pluginHub) ack(id, cmdID string, a pluginAck) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.channels[id]
	if c == nil {
		return false
	}
	ch, ok := c.acks[cmdID]
	if !ok {
		return false
	}
	delete(c.acks, cmdID)
	ch <- a
	return true
}

// sweep finds channels that stopped polling and returns the sessions whose
// reply capability changed with them.
func (h *pluginHub) sweep(now time.Time) []pluginKey {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []pluginKey
	for id, c := range h.channels {
		if c.live && !h.liveLocked(id, now) {
			c.live = false
			out = append(out, h.ownedLocked(id)...)
			// Replies nobody will collect fail now rather than at their
			// deadline.
			for _, q := range c.queue {
				if ack := c.acks[q.ID]; ack != nil {
					ack <- pluginAck{Error: "the plugin disconnected"}
					delete(c.acks, q.ID)
				}
			}
			c.queue = nil
		}
	}
	h.forgetLocked(now, false)
	return out
}

// ---------- daemon side ----------

// isPluginAgent reports whether the agent is integrated through a plugin.
func isPluginAgent(k api.AgentKind) bool { return pluginAgents[k] }

func (d *Daemon) routesPlugin(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/plugin/hook", d.handlePluginHook)
	mux.HandleFunc("POST /v1/plugin/commands", d.handlePluginCommands)
	mux.HandleFunc("POST /v1/plugin/commands/{id}/ack", d.handlePluginAck)
}

// pluginToken reads and checks the channel token; "" when absent or
// malformed.
func pluginToken(r *http.Request) string {
	tok := r.Header.Get(PluginTokenHeader)
	if !pluginTokenRE.MatchString(tok) {
		return ""
	}
	return hashToken(tok)
}

// handlePluginHook is POST /v1/hook for plugin agents: the hook also tells
// the hub which channel the session belongs to. The claim is made before
// the hook is routed, so the session is saved with its reply capability.
func (d *Daemon) handlePluginHook(w http.ResponseWriter, r *http.Request) {
	var env adapters.HookEnvelope
	if err := decodeJSON(w, r, &env, maxHookBody); err != nil {
		writeErr(w, err)
		return
	}
	if !isPluginAgent(env.Agent) || d.adapters[env.Agent] == nil {
		writeErr(w, badRequest("%q is not a plugin agent", env.Agent))
		return
	}
	if env.ReceivedAt.IsZero() {
		env.ReceivedAt = d.now()
	}
	if id := pluginToken(r); id != "" {
		if ev, err := d.adapters[env.Agent].ParseHook(env); err == nil && ev.AgentSessionID != "" && ev.Kind != adapters.HookSessionEnd {
			d.pluginClaim(pluginKey{env.Agent, ev.AgentSessionID}, id)
		}
	}
	writeJSON(w, http.StatusOK, d.Hook(r.Context(), env))
}

// pluginClaim claims key for channel id and logs a contested session.
func (d *Daemon) pluginClaim(key pluginKey, id string) {
	d.startPluginSweep()
	if _, contested := d.plugins.claim(key, id, d.now()); contested {
		d.log.Warn("two plugin channels report the same agent session; replies to it are refused until one goes away",
			"agent", key.agent, "agent_session", key.session)
	}
}

// pluginPollBody is the body of POST /v1/plugin/commands.
type pluginPollBody struct {
	// Sessions are the agent sessions the plugin has open. After a daemon
	// restart they are claimed again from here, before their next hook.
	Sessions []string `json:"sessions"`
	Wait     int      `json:"wait"` // seconds, at most pluginPollMax
}

// handlePluginCommands is the plugin's long poll: it answers as soon as a
// reply is queued for one of the channel's sessions, or after the wait.
func (d *Daemon) handlePluginCommands(w http.ResponseWriter, r *http.Request) {
	id := pluginToken(r)
	if id == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing or malformed "+PluginTokenHeader+" header")
		return
	}
	var b pluginPollBody
	if err := decodeJSON(w, r, &b, maxBody); err != nil {
		writeErr(w, err)
		return
	}
	agent := api.AgentOpenCode // the only plugin agent; the token is per agent process
	d.startPluginSweep()
	if len(b.Sessions) > maxPluginAnnounce {
		b.Sessions = b.Sessions[:maxPluginAnnounce]
	}
	// The poll counts first, so the claims below see a live channel.
	c, touch := d.plugins.pollStart(id, d.now())
	if c == nil {
		writeError(w, http.StatusServiceUnavailable, "busy", "too many plugin channels")
		return
	}
	defer d.plugins.pollEnd(c, d.now())
	for _, s := range b.Sessions {
		if s == "" || len(s) > 200 || !utf8.ValidString(s) {
			continue
		}
		key := pluginKey{agent, s}
		// Only sessions the daemon knows: an announce never creates one.
		if _, err := d.db.SessionByAgentID(r.Context(), agent, s); err != nil {
			continue
		}
		changed, contested := d.plugins.claim(key, id, d.now())
		if changed {
			touch = append(touch, key)
		}
		if contested && changed {
			d.log.Warn("two plugin channels report the same agent session; replies to it are refused until one goes away",
				"agent", agent, "agent_session", s)
		}
	}
	d.touchPluginSessions(r.Context(), touch)

	wait := time.Duration(b.Wait) * time.Second
	if wait <= 0 || wait > pluginPollMax {
		wait = pluginPollMax
	}
	cmds := d.plugins.take(r.Context(), d.ctx.Done(), c, wait)
	if cmds == nil {
		cmds = []pluginCommand{}
	}
	if err := writeJSONErr(w, http.StatusOK, map[string]any{"commands": cmds}); err != nil {
		d.plugins.requeue(c, cmds)
	}
}

// writeJSONErr is writeJSON that reports a failed write, so a poll whose
// client went away can put its commands back.
func writeJSONErr(w http.ResponseWriter, status int, v any) error {
	ew := &errWriter{ResponseWriter: w}
	writeJSON(ew, status, v)
	if ew.err == nil {
		ew.err = http.NewResponseController(w).Flush()
	}
	return ew.err
}

type errWriter struct {
	http.ResponseWriter
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	n, err := e.ResponseWriter.Write(p)
	if err != nil && e.err == nil {
		e.err = err
	}
	return n, err
}

func (d *Daemon) handlePluginAck(w http.ResponseWriter, r *http.Request) {
	id := pluginToken(r)
	if id == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing or malformed "+PluginTokenHeader+" header")
		return
	}
	var a pluginAck
	if err := decodeJSON(w, r, &a, 4<<10); err != nil {
		writeErr(w, err)
		return
	}
	if !d.plugins.ack(id, r.PathValue("id"), a) {
		writeError(w, http.StatusNotFound, "not_found", "no such command on this channel")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// startPluginSweep starts the loop that notices channels going away, once.
func (d *Daemon) startPluginSweep() {
	d.plugins.mu.Lock()
	start := !d.plugins.sweeping && d.ctx != nil && d.ctx.Err() == nil
	d.plugins.sweeping = d.plugins.sweeping || start
	d.plugins.mu.Unlock()
	if !start {
		return
	}
	d.every(pluginSweep, func(ctx context.Context) {
		d.touchPluginSessions(ctx, d.plugins.sweep(d.now()))
	})
}

// touchPluginSessions re-saves sessions whose reply capability changed.
func (d *Daemon) touchPluginSessions(ctx context.Context, keys []pluginKey) {
	for _, k := range keys {
		err := d.db.Write(ctx, func(tx *store.Tx) error {
			s, err := tx.SessionByAgentID(ctx, k.agent, k.session)
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			return d.touchSession(ctx, tx, s.ID)
		})
		if err != nil && ctx.Err() == nil {
			d.log.Warn("update session caps", "agent", k.agent, "agent_session", k.session, "err", err)
		}
	}
}

// pluginReply delivers a reply through the session's plugin channel and
// waits for the plugin to confirm it reached the agent. Reply (reply.go)
// calls it for sessions whose agent uses a plugin.
func (d *Daemon) pluginReply(ctx context.Context, s api.Session, r api.Reply, by string) (api.ReplyResult, error) {
	if !isPluginAgent(s.Agent) || s.AgentSessionID == "" {
		return api.ReplyResult{}, core.ErrUnsupported
	}
	if s.ExitedAt != nil {
		return api.ReplyResult{}, fmt.Errorf("%w: the session has ended", core.ErrUnsupported)
	}
	text := strings.TrimRight(r.Text, " \t\r\n")
	switch {
	case text == "" && !r.Interrupt:
		return api.ReplyResult{}, core.Invalid("nothing to send")
	case len(text) > maxPluginReply || !utf8.ValidString(text):
		return api.ReplyResult{}, core.Invalid("the message is too long")
	}
	cmd := pluginCommand{ID: newID("pc_"), SessionID: s.AgentSessionID, Text: text, Interrupt: r.Interrupt}
	channel, ack, err := d.plugins.send(pluginKey{s.Agent, s.AgentSessionID}, cmd, d.now())
	if err != nil {
		return api.ReplyResult{}, err
	}
	t := time.NewTimer(pluginAckWait)
	defer t.Stop()
	select {
	case a := <-ack:
		if !a.OK {
			msg := strings.TrimSpace(a.Error)
			if len(msg) > maxPluginAckError {
				msg = msg[:maxPluginAckError]
			}
			return api.ReplyResult{}, fmt.Errorf("%w: the agent did not take the reply: %s", core.ErrUnsupported, firstNonEmpty(msg, "no reason given"))
		}
	case <-t.C:
		if d.plugins.cancel(channel, cmd.ID) {
			return api.ReplyResult{}, fmt.Errorf("%w: the plugin took the reply but did not confirm it within %s; it may still arrive", core.ErrUnsupported, pluginAckWait)
		}
		return api.ReplyResult{}, errPluginNotConnected
	case <-ctx.Done():
		d.plugins.cancel(channel, cmd.ID)
		return api.ReplyResult{}, ctx.Err()
	}
	d.log.Info("reply delivered through plugin", "session", s.ID, "agent", s.Agent, "interrupt", r.Interrupt, "bytes", len(text), "by", by)
	note := "Sent to " + d.agentName(s.Agent) + " as your next prompt"
	if r.Interrupt && text == "" {
		note = "Stopped " + d.agentName(s.Agent) + "'s current turn"
	} else if r.Interrupt {
		note = "Stopped " + d.agentName(s.Agent) + "'s turn and sent your prompt"
	}
	return api.ReplyResult{Via: api.ReplyPlugin, Note: note}, nil
}

func (d *Daemon) agentName(k api.AgentKind) string {
	if a := d.adapters[k]; a != nil {
		return a.DisplayName()
	}
	return string(k)
}

// pluginCaps sets Reply (and Interrupt) via plugin while the session's
// plugin channel is connected and nobody else claims the session.
func (d *Daemon) pluginCaps(ctx context.Context, r store.Reader, s api.Session, c *api.SessionCaps) {
	if !isPluginAgent(s.Agent) || s.AgentSessionID == "" {
		return
	}
	if d.plugins.reachable(pluginKey{s.Agent, s.AgentSessionID}, d.now()) {
		c.Reply, c.ReplyVia, c.Interrupt = true, api.ReplyPlugin, true
	}
}

// pluginStats is for tests and explain-style debugging: channels and owners.
func (h *pluginHub) stats() (channels, owners int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.channels), len(h.owners)
}
