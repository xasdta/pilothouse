package telegram

import (
	"context"
	"errors"
	"time"
)

// WaitForStart polls the bot until a private chat sends /start with the
// pending pairing code in k, and returns that chat. It is what
// `pilothouse notify telegram` uses when no daemon is running (when one
// is, the daemon pairs: only one program may read a bot's updates).
// Updates it reads are confirmed, so the daemon won't see them again.
func WaitForStart(ctx context.Context, c *Client, k Keys, now func() time.Time) (Chat, error) {
	if now == nil {
		now = time.Now
	}
	var offset int64
	for {
		if !k.Pending(now()) {
			return Chat{}, errors.New("the pairing link expired")
		}
		ups, err := c.GetUpdates(ctx, offset, 20*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return Chat{}, ctx.Err()
			}
			if IsCode(err, 401) || IsCode(err, 404) || IsCode(err, 409) {
				return Chat{}, err
			}
			select {
			case <-ctx.Done():
				return Chat{}, ctx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}
		for _, u := range ups {
			offset = u.UpdateID + 1
			m := u.Message
			if m == nil || m.Chat.Type != "private" || (m.From != nil && m.From.IsBot) {
				continue
			}
			if code, ok := startCode(m.Text); ok && k.CheckCode(code, now()) {
				// Confirm what was read (offset is the next update).
				cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				_, _ = c.GetUpdates(cctx, offset, 0)
				cancel()
				return m.Chat, nil
			}
		}
	}
}
