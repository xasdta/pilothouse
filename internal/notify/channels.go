package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
)

// ---- Desktop ----

// Desktop shows a local notification: osascript on macOS, notify-send on
// Linux. Text is passed as process arguments, never spliced into a script
// or shell, so a hostile command string can't inject anything.
type Desktop struct {
	GOOS     string
	LookPath func(string) (string, error)
	Run      func(ctx context.Context, name string, args ...string) error
}

// NewDesktop returns a Desktop for this OS.
func NewDesktop() *Desktop {
	return &Desktop{
		GOOS:     runtime.GOOS,
		LookPath: exec.LookPath,
		Run: func(ctx context.Context, name string, args ...string) error {
			out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
			if err != nil && len(out) > 0 {
				return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
			}
			return err
		},
	}
}

func (*Desktop) Name() string { return "desktop" }
func (*Desktop) Remote() bool { return false }

// osascriptProgram reads title and body from argv: the script text is
// constant, so nothing user-controlled is ever parsed as AppleScript.
var osascriptProgram = []string{
	"-e", "on run argv",
	"-e", "display notification (item 2 of argv) with title (item 1 of argv)",
	"-e", "end run",
}

// DesktopCommand returns the command that shows title/body on goos, or
// "" when the platform has no supported notifier.
func DesktopCommand(goos, title, body string, lookPath func(string) (string, error)) (string, []string) {
	title, body = clean(title, 120), clean(body, 300)
	switch goos {
	case "darwin":
		args := append(append([]string{}, osascriptProgram...), "--", title, body)
		return "osascript", args
	case "linux", "freebsd", "openbsd", "netbsd":
		if lookPath == nil {
			return "", nil
		}
		p, err := lookPath("notify-send")
		if err != nil {
			return "", nil
		}
		// notify-send bodies may be parsed as markup by the server.
		return p, []string{"--app-name=Pilothouse", "--", title, escapeMarkup(body)}
	}
	return "", nil
}

func (d *Desktop) Send(ctx context.Context, n core.Notification) error {
	name, args := DesktopCommand(d.GOOS, n.Title, n.Body, d.LookPath)
	if name == "" {
		return nil // no desktop notifier here: not an error
	}
	return d.Run(ctx, name, args...)
}

// clean drops control characters (newlines included) and trims to n runes.
func clean(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return Short(s, n)
}

func escapeMarkup(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// ---- ntfy ----

// Ntfy publishes to an ntfy topic (https://docs.ntfy.sh/publish/).
// When the web app has a public URL, request notifications carry http
// action buttons (Allow / Deny) backed by single-use action tokens, and a
// click opens the request in the web app.
//
// Request notifications carry a sequence ID (X-Sequence-ID, ntfy server
// 2.16+): the "waiting in the terminal" notice that follows a timeout
// replaces the request in place, and Withdraw clears it from the phone
// (PUT /<topic>/<sequence>/clear) once someone answered elsewhere.
// Servers without that feature ignore the header and answer the clear
// with 404, which is not an error.
type Ntfy struct {
	Server string
	Topic  string
	Token  string // access token (tk_…) or "user:pass"
	Pusher Pusher // may be nil
	Client *http.Client
	// Backoff is the first retry delay after a 429 or 5xx (doubling, at
	// most ntfyRetries retries, within the delivery timeout). Zero means 1 s.
	Backoff time.Duration

	mu   sync.Mutex
	sent map[string]time.Time // request id → published (for Withdraw)
}

// ntfySeq is the ntfy sequence ID of a request's notifications.
func ntfySeq(requestID string) string {
	return "req-" + seqRE.ReplaceAllString(Short(requestID, 60), "")
}

var seqRE = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// ntfyRetries bounds retries of a rate-limited (429) or failing (5xx) publish.
const ntfyRetries = 3

func (*Ntfy) Name() string { return "ntfy" }
func (*Ntfy) Remote() bool { return true }

// Request builds the publish request (exported for tests and `pilothouse doctor`).
func (c *Ntfy) Request(ctx context.Context, n core.Notification) (*http.Request, error) {
	body := Line(n)
	if n.Request != nil {
		if d := Details(n.Request); len(d) > 0 {
			body += "\n" + strings.Join(d, "\n")
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.topicURL(), strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	h := req.Header
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Title", headerText(n.Title))
	if n.Request != nil && n.Request.ID != "" {
		h.Set("X-Sequence-ID", ntfySeq(n.Request.ID))
	}
	switch n.Kind {
	case core.NotifyRequest:
		h.Set("Priority", "high")
		h.Set("Tags", "lock")
	case core.NotifyBlocked:
		h.Set("Priority", "high")
		h.Set("Tags", "hourglass_flowing_sand")
	case core.NotifyWaiting:
		h.Set("Priority", "default")
		h.Set("Tags", "white_check_mark")
	default:
		h.Set("Priority", "default")
		h.Set("Tags", "bell")
	}
	var actions []string
	if c.Pusher != nil {
		if pub := c.Pusher.PublicURL(); pub != "" {
			link := pub + "/"
			switch {
			case n.Request != nil:
				link = pub + "/r/" + url.PathEscape(n.Request.ID)
			case n.Session != nil:
				link = pub + "/s/" + url.PathEscape(n.Session.ID)
			}
			h.Set("Click", link)
			// One-tap Allow/Deny only for permissions: a question needs an
			// answer and a plan should be read, so those open the app.
			if n.Kind == core.NotifyRequest && n.Request != nil && n.Request.Status == api.RequestOpen &&
				(n.Request.Checkpoint == api.CheckpointPermission || n.Request.Checkpoint == "") {
				allow := c.Pusher.ActionURL(n.Request.ID, api.BehaviorAllow)
				deny := c.Pusher.ActionURL(n.Request.ID, api.BehaviorDeny)
				if allow != "" && deny != "" {
					actions = append(actions,
						"http, Allow, "+allow+", method=POST, clear=true",
						"http, Deny, "+deny+", method=POST, clear=true")
				}
			}
			actions = append(actions, "view, Open, "+link)
		}
	}
	if len(actions) > 0 {
		h.Set("Actions", strings.Join(actions, "; "))
	}
	c.auth(req)
	return req, nil
}

func (c *Ntfy) topicURL() string {
	server := strings.TrimRight(c.Server, "/")
	if server == "" {
		server = "https://ntfy.sh"
	}
	return server + "/" + url.PathEscape(c.Topic)
}

func (c *Ntfy) auth(req *http.Request) {
	switch {
	case c.Token == "":
	case strings.Contains(c.Token, ":"):
		u, p, _ := strings.Cut(c.Token, ":")
		req.SetBasicAuth(u, p)
	default:
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
}

func (c *Ntfy) Send(ctx context.Context, n core.Notification) error {
	err := c.publish(ctx, n)
	if err == nil && n.Request != nil && (n.Kind == core.NotifyRequest || n.Kind == core.NotifyBlocked) {
		c.mu.Lock()
		if c.sent == nil {
			c.sent = map[string]time.Time{}
		}
		if len(c.sent) >= maxRemembered {
			for k, at := range c.sent {
				if time.Since(at) > 24*time.Hour || len(c.sent) >= maxRemembered {
					delete(c.sent, k)
				}
			}
		}
		c.sent[n.Request.ID] = time.Now()
		c.mu.Unlock()
	}
	return err
}

// Withdraw clears a request's notification once someone answered it (or
// it moved back to the terminal). A timed-out request is left alone: the
// "waiting in the terminal" notice already replaced it.
func (c *Ntfy) Withdraw(ctx context.Context, r api.Request) error {
	c.mu.Lock()
	_, ok := c.sent[r.ID]
	delete(c.sent, r.ID)
	c.mu.Unlock()
	if !ok || r.Status == api.RequestOpen || r.Status == api.RequestExpired {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.topicURL()+"/"+ntfySeq(r.ID)+"/clear", nil)
	if err != nil {
		return err
	}
	c.auth(req)
	status, _, err := doStatus(c.Client, req)
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		return nil // a server older than 2.16: nothing to clear
	}
	return err
}

func (c *Ntfy) publish(ctx context.Context, n core.Notification) error {
	if c.Topic == "" {
		return errors.New("ntfy: no topic configured")
	}
	delay := c.Backoff
	if delay <= 0 {
		delay = time.Second
	}
	for attempt := 0; ; attempt++ {
		req, err := c.Request(ctx, n)
		if err != nil {
			return err
		}
		status, retryAfter, err := doStatus(c.Client, req)
		if err == nil || !retryable(status) || attempt >= ntfyRetries {
			return err
		}
		wait := delay
		if retryAfter > wait && retryAfter <= 30*time.Second {
			wait = retryAfter
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (gave up retrying: %v)", err, ctx.Err())
		case <-time.After(wait):
		}
		delay *= 2
	}
}

// retryable: ntfy rate limits (429) and server errors are worth a retry.
func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// headerText makes a header-safe title: ASCII as is, anything else as an
// RFC 2047 encoded word (ntfy decodes these).
func headerText(s string) string {
	s = clean(s, 200)
	for _, r := range s {
		if r > unicode.MaxASCII {
			return mime.BEncoding.Encode("utf-8", s)
		}
	}
	return s
}

// ---- Webhooks ----

// Webhook posts Slack- and Discord-compatible JSON: Slack reads "text",
// Discord reads "content"; each ignores the other.
type Webhook struct {
	URL    string
	Client *http.Client
}

func (*Webhook) Name() string { return "webhook" }
func (*Webhook) Remote() bool { return true }

// Payload is the JSON body.
func (*Webhook) Payload(n core.Notification) []byte {
	line := Short(Line(n), 1900) // Discord's limit is 2000
	b, _ := json.Marshal(map[string]string{"text": line, "content": line})
	return b
}

func (w *Webhook) Send(ctx context.Context, n core.Notification) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(w.Payload(n)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return do(w.Client, req)
}

func do(hc *http.Client, req *http.Request) error {
	_, _, err := doStatus(hc, req)
	return err
}

// doStatus sends req and returns the HTTP status (0 when no response) and
// any Retry-After delay the server asked for.
func doStatus(hc *http.Client, req *http.Request) (int, time.Duration, error) {
	if hc == nil {
		hc = &http.Client{Timeout: defaultTimeout}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, 0, fmt.Errorf("%s: %w", req.URL.Host, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode/100 != 2 {
		var after time.Duration
		if secs, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && secs > 0 {
			after = time.Duration(secs) * time.Second
		}
		return resp.StatusCode, after, fmt.Errorf("%s: %s %s", req.URL.Host, resp.Status, strings.TrimSpace(string(body)))
	}
	return resp.StatusCode, 0, nil
}
