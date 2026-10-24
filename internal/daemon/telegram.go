package daemon

// Workstream F: the Telegram bot (internal/notify/telegram) inside the
// daemon. The manager follows config.yaml (notify.telegram) and
// ~/.pilothouse/keys/telegram.json — `pilothouse notify telegram` writes
// both while the daemon runs — and (re)starts the bot when they change.
// The notifier gets one stable channel that forwards to the current bot.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/config"
	"github.com/xasdta/pilothouse/internal/core"
	"github.com/xasdta/pilothouse/internal/notify"
	"github.com/xasdta/pilothouse/internal/notify/telegram"
	"github.com/xasdta/pilothouse/internal/paths"
)

type telegramManager struct {
	d *Daemon

	mu     sync.Mutex
	bot    *telegram.Bot // nil when Telegram is off or not set up
	cancel context.CancelFunc
	done   chan struct{}
	sig    string

	// cfg is the last config read: the file when it exists, else the
	// daemon's own (tests inject one without a file).
	cfg       *config.Config
	cfgMtime  time.Time
	keysErr   string
	keysCache telegram.Keys
}

func newTelegramManager(d *Daemon) *telegramManager { return &telegramManager{d: d, cfg: d.cfg} }

// startTelegram starts the bot if it is configured and keeps it in step
// with its settings until shutdown.
func (d *Daemon) startTelegram() {
	m := d.tg
	m.check(d.ctx)
	d.every(d.opts.PollInterval, m.check)
	d.goBackground(func() {
		<-d.ctx.Done()
		m.stop()
	})
}

// config returns the current config, re-reading config.yaml when it changed.
func (m *telegramManager) config() *config.Config {
	fi, err := os.Stat(paths.Config())
	if err != nil {
		return m.cfg
	}
	if fi.ModTime().Equal(m.cfgMtime) {
		return m.cfg
	}
	m.cfgMtime = fi.ModTime() // a broken file is reported once, not every tick
	c, err := config.Load()
	if err != nil {
		m.d.log.Warn("telegram: config.yaml unreadable; keeping the last settings", "err", err)
		return m.cfg
	}
	m.cfg = c
	return c
}

func (m *telegramManager) keys() telegram.Keys {
	k, err := telegram.LoadKeys(telegram.KeysPath())
	if err != nil {
		if msg := err.Error(); msg != m.keysErr {
			m.keysErr = msg
			m.d.log.Warn("telegram: key file unreadable", "err", err)
		}
		return m.keysCache
	}
	m.keysErr, m.keysCache = "", k
	return k
}

// check compares the wanted bot with the running one and restarts it when
// its settings changed. Called from one goroutine (the daemon's loop).
func (m *telegramManager) check(ctx context.Context) {
	cfg := m.config()
	t := cfg.Notify.Telegram
	k := m.keys()
	now := m.d.now()
	want := t.Enabled && k.Token != "" && (t.ChatID != 0 || k.Pending(now))
	tok := sha256.Sum256([]byte(k.Token))
	sig := fmt.Sprintf("%v|%d|%v|%s|%s|%s|%d", want, t.ChatID, cfg.TelegramDecisions(), t.APIBase, hex.EncodeToString(tok[:8]), k.PairHash, k.PairExpires.Unix())
	m.mu.Lock()
	same := sig == m.sig
	m.sig = sig
	m.mu.Unlock()
	if same {
		return
	}
	if !want {
		m.stop()
		if t.Enabled && k.Token != "" {
			m.d.log.Info("telegram: not paired yet — run `pilothouse notify telegram`")
		}
		return
	}
	d := m.d
	bot, err := telegram.New(telegram.Options{
		Client:    &telegram.Client{Token: k.Token, Base: t.APIBase},
		DB:        d.db.SQL(),
		Core:      d,
		ChatID:    t.ChatID,
		Decisions: cfg.TelegramDecisions(),
		Keys:      m.keys,
		OnPaired:  m.paired,
		Machine:   d.machine,
		PublicURL: func() string {
			if d.web != nil {
				return d.web.PublicURL()
			}
			return cfg.Web.PublicURL
		},
		DeviceName: func(id string) string {
			if d.web == nil {
				return ""
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			return d.web.DeviceName(ctx, id)
		},
		Log: d.log.With("component", "telegram"),
		Now: d.opts.Now,
	})
	if err != nil {
		m.stop()
		d.log.Error("telegram disabled", "err", err)
		return
	}
	// Swap first, then stop the old poller: notifications sent meanwhile
	// go to the new bot (sending doesn't need its poller), none is dropped.
	m.mu.Lock()
	oldCancel, oldDone := m.cancel, m.done
	m.bot, m.cancel, m.done = bot, nil, nil
	m.mu.Unlock()
	if oldCancel != nil {
		oldCancel()
		<-oldDone
	}
	bctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	m.mu.Lock()
	m.cancel, m.done = cancel, done
	m.mu.Unlock()
	go func() {
		defer close(done)
		bot.Run(bctx)
	}()
	if t.ChatID == 0 {
		d.log.Info("telegram: waiting for the pairing link to be opened", "bot", k.BotUsername, "until", k.PairExpires)
	} else {
		d.log.Info("telegram: connected", "bot", k.BotUsername, "decisions", cfg.TelegramDecisions())
	}
}

// stop ends the running bot and waits for its poller.
func (m *telegramManager) stop() {
	m.mu.Lock()
	cancel, done := m.cancel, m.done
	m.bot, m.cancel, m.done = nil, nil, nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func (m *telegramManager) current() *telegram.Bot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bot
}

// paired saves the chat that completed pairing (config chat_id) and spends
// the pairing code.
func (m *telegramManager) paired(_ context.Context, chat telegram.Chat) error {
	var cfg *config.Config
	if _, err := os.Stat(paths.Config()); err == nil {
		c, err := config.Load()
		if err != nil {
			return fmt.Errorf("read config: %w", err)
		}
		cfg = c
	} else {
		c := *m.d.cfg // no file yet: start from the daemon's own settings
		cfg = &c
	}
	cfg.Notify.Telegram.Enabled = true
	cfg.Notify.Telegram.ChatID = chat.ID
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("save chat_id: %w", err)
	}
	k, err := telegram.LoadKeys(telegram.KeysPath())
	if err != nil {
		return err
	}
	k.PairHash, k.PairExpires = "", time.Time{}
	if err := telegram.SaveKeys(telegram.KeysPath(), k); err != nil {
		return fmt.Errorf("spend pairing code: %w", err)
	}
	return nil
}

// telegramChannel is the notifier's Telegram channel: it forwards to the
// bot that is running now, and drops notifications while there is none.
type telegramChannel struct{ m *telegramManager }

var (
	_ notify.Channel    = telegramChannel{}
	_ notify.Withdrawer = telegramChannel{}
)

func (telegramChannel) Name() string { return "telegram" }
func (telegramChannel) Remote() bool { return true }

func (c telegramChannel) Send(ctx context.Context, n core.Notification) error {
	if b := c.m.current(); b != nil {
		return b.Send(ctx, n)
	}
	return nil
}

func (c telegramChannel) Withdraw(ctx context.Context, r api.Request) error {
	if b := c.m.current(); b != nil {
		return b.Withdraw(ctx, r)
	}
	return nil
}
