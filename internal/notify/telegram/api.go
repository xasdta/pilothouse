package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultAPIBase is the Bot API server.
const DefaultAPIBase = "https://api.telegram.org"

const (
	callTimeout = 10 * time.Second
	// maxRetries bounds retries of a rate-limited (429) or failing (5xx) call.
	maxRetries = 3
	// maxRetryAfter caps how long one 429 may make us wait.
	maxRetryAfter = 60 * time.Second
	maxBody       = 4 << 20 // a getUpdates page is at most 100 updates
)

// Client calls the Telegram Bot API (https://core.telegram.org/bots/api).
// The bot token is part of every URL, so errors never include the URL:
// they name the method instead, and the token is scrubbed from any text
// that might echo it.
type Client struct {
	Token string
	Base  string       // default DefaultAPIBase
	HTTP  *http.Client // default: no client timeout; every call has its own deadline
	// Backoff is the first retry delay after a 5xx (doubling). Zero means 1 s.
	// A 429 waits for the retry_after the server asked for.
	Backoff time.Duration
}

// Error is a failed Bot API call.
type Error struct {
	Method      string
	Code        int // HTTP status / error_code; 0 for network errors
	Description string
	RetryAfter  time.Duration
}

func (e *Error) Error() string {
	if e.Code == 0 {
		return "telegram " + e.Method + ": " + e.Description
	}
	return fmt.Sprintf("telegram %s: %d %s", e.Method, e.Code, e.Description)
}

// IsCode reports whether err is a Bot API error with the given code.
func IsCode(err error, code int) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// notModified is the 400 Telegram returns when an edit changes nothing.
func notModified(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == 400 && strings.Contains(e.Description, "message is not modified")
}

// gone is the 400 for an edit of a message that no longer exists.
func gone(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == 400 &&
		(strings.Contains(e.Description, "message to edit not found") || strings.Contains(e.Description, "message can't be edited"))
}

// ---- types (the subset Pilothouse uses) ----

type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username,omitempty"`
}

type Chat struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"` // private, group, supergroup, channel
	Title     string `json:"title,omitempty"`
	Username  string `json:"username,omitempty"`
	FirstName string `json:"first_name,omitempty"`
}

type Message struct {
	MessageID      int64    `json:"message_id"`
	From           *User    `json:"from,omitempty"`
	Chat           Chat     `json:"chat"`
	Date           int64    `json:"date"`
	Text           string   `json:"text,omitempty"`
	ReplyToMessage *Message `json:"reply_to_message,omitempty"`
}

type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message,omitempty"`
	Data    string   `json:"data,omitempty"`
}

type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
}

type Button struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
	URL          string `json:"url,omitempty"`
}

type InlineKeyboard struct {
	Rows [][]Button `json:"inline_keyboard"`
}

type ForceReply struct {
	ForceReply  bool   `json:"force_reply"`
	Placeholder string `json:"input_field_placeholder,omitempty"`
}

type ReplyParameters struct {
	MessageID                int64 `json:"message_id"`
	AllowSendingWithoutReply bool  `json:"allow_sending_without_reply,omitempty"`
}

// SendMessage is sendMessage's parameters.
type SendMessage struct {
	ChatID              int64            `json:"chat_id"`
	Text                string           `json:"text"`
	ParseMode           string           `json:"parse_mode,omitempty"`
	ReplyMarkup         any              `json:"reply_markup,omitempty"` // *InlineKeyboard or *ForceReply
	ReplyParameters     *ReplyParameters `json:"reply_parameters,omitempty"`
	DisableNotification bool             `json:"disable_notification,omitempty"`
	LinkPreviewOptions  *linkPreview     `json:"link_preview_options,omitempty"`
}

type linkPreview struct {
	IsDisabled bool `json:"is_disabled"`
}

// EditMessageText is editMessageText's parameters.
type EditMessageText struct {
	ChatID             int64           `json:"chat_id"`
	MessageID          int64           `json:"message_id"`
	Text               string          `json:"text"`
	ParseMode          string          `json:"parse_mode,omitempty"`
	ReplyMarkup        *InlineKeyboard `json:"reply_markup,omitempty"`
	LinkPreviewOptions *linkPreview    `json:"link_preview_options,omitempty"`
}

// ---- methods ----

// GetMe checks the token and returns the bot.
func (c *Client) GetMe(ctx context.Context) (User, error) {
	var u User
	err := c.call(ctx, "getMe", struct{}{}, &u, callTimeout)
	return u, err
}

// GetUpdates long-polls for updates after offset-1, waiting up to wait.
func (c *Client) GetUpdates(ctx context.Context, offset int64, wait time.Duration) ([]Update, error) {
	params := map[string]any{
		"offset":          offset,
		"timeout":         int(wait.Seconds()),
		"allowed_updates": []string{"message", "callback_query"},
	}
	var out []Update
	err := c.call(ctx, "getUpdates", params, &out, wait+callTimeout)
	return out, err
}

// Send sends a message and returns it.
func (c *Client) Send(ctx context.Context, m SendMessage) (Message, error) {
	if m.ParseMode == "" {
		m.ParseMode = "HTML"
	}
	if m.LinkPreviewOptions == nil {
		m.LinkPreviewOptions = &linkPreview{IsDisabled: true}
	}
	var out Message
	err := c.call(ctx, "sendMessage", m, &out, callTimeout)
	return out, err
}

// Edit replaces a message's text and buttons. Editing to the same content
// is not an error.
func (c *Client) Edit(ctx context.Context, e EditMessageText) error {
	if e.ParseMode == "" {
		e.ParseMode = "HTML"
	}
	if e.LinkPreviewOptions == nil {
		e.LinkPreviewOptions = &linkPreview{IsDisabled: true}
	}
	var out json.RawMessage
	err := c.call(ctx, "editMessageText", e, &out, callTimeout)
	if notModified(err) {
		return nil
	}
	return err
}

// Answer answers a button press (stops the spinner; text is a short toast,
// or a dialog when alert is set).
func (c *Client) Answer(ctx context.Context, callbackID, text string, alert bool) error {
	var out bool
	return c.call(ctx, "answerCallbackQuery", map[string]any{
		"callback_query_id": callbackID, "text": text, "show_alert": alert,
	}, &out, callTimeout)
}

// DeleteWebhook turns off a webhook so getUpdates works (Telegram refuses
// getUpdates with 409 while a webhook is set).
func (c *Client) DeleteWebhook(ctx context.Context) error {
	var out bool
	return c.call(ctx, "deleteWebhook", struct{}{}, &out, callTimeout)
}

// response is every Bot API reply.
type response struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  *struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

func (c *Client) call(ctx context.Context, method string, params, out any, timeout time.Duration) error {
	delay := c.Backoff
	if delay <= 0 {
		delay = time.Second
	}
	for attempt := 0; ; attempt++ {
		err := c.once(ctx, method, params, out, timeout)
		var e *Error
		if err == nil || !errors.As(err, &e) || attempt >= maxRetries || ctx.Err() != nil {
			return err
		}
		var wait time.Duration
		switch {
		case e.Code == http.StatusTooManyRequests:
			wait = max(e.RetryAfter, time.Second)
			if wait > maxRetryAfter {
				return err
			}
		case e.Code >= 500:
			wait = delay
			delay *= 2
		default:
			return err
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("%w (gave up retrying: %v)", err, ctx.Err())
		case <-t.C:
		}
	}
}

func (c *Client) once(ctx context.Context, method string, params, out any, timeout time.Duration) error {
	if c.Token == "" {
		return &Error{Method: method, Description: "no bot token"}
	}
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	base := strings.TrimRight(c.Base, "/")
	if base == "" {
		base = DefaultAPIBase
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/bot"+c.Token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return &Error{Method: method, Description: c.scrub(err.Error())}
	}
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // *url.Error prints the URL, which holds the token
		}
		return &Error{Method: method, Description: c.scrub(err.Error())}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return &Error{Method: method, Description: "reading the reply: " + c.scrub(err.Error())}
	}
	var r response
	if err := json.Unmarshal(raw, &r); err != nil {
		return &Error{Method: method, Code: resp.StatusCode, Description: "unreadable reply (" + resp.Status + ")"}
	}
	if !r.OK {
		e := &Error{Method: method, Code: r.ErrorCode, Description: c.scrub(r.Description)}
		if e.Code == 0 {
			e.Code = resp.StatusCode
		}
		if r.Parameters != nil && r.Parameters.RetryAfter > 0 {
			e.RetryAfter = time.Duration(r.Parameters.RetryAfter) * time.Second
		}
		return e
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(r.Result, out); err != nil {
		return &Error{Method: method, Code: resp.StatusCode, Description: "unexpected result: " + err.Error()}
	}
	return nil
}

func (c *Client) scrub(s string) string {
	if c.Token == "" {
		return s
	}
	return strings.ReplaceAll(s, c.Token, "<token>")
}
