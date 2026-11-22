// Package telegram connects agentd's conversation to a Telegram bot on the owner's phone.
// It works outbound-only: it long-polls Telegram for updates and sends messages, so the
// HTTP API never needs a public address. Who counts as the owner, what the phone may do and
// how much is sent are decided by internal/chat; this package only moves messages.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"agentd/internal/chat"
)

const (
	Name           = "telegram"
	DefaultBaseURL = "https://api.telegram.org"
	pollSeconds    = 25
)

type Bot struct {
	// Token is the bot token. It is only ever put in request paths and never in errors.
	Token   string
	BaseURL string
	HTTP    *http.Client
	Chat    *chat.Service
	Log     *slog.Logger
	// Poll is how often outbound messages are checked when nothing signals a change.
	Poll time.Duration
}

type response struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// RateLimited is returned when Telegram asks to wait before trying again.
type RateLimited struct{ After time.Duration }

func (e *RateLimited) Error() string { return fmt.Sprintf("telegram: rate limited for %s", e.After) }

func (b *Bot) call(ctx context.Context, method string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	base := b.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/bot"+b.Token+"/"+method, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("telegram %s: could not build the request", method)
	}
	req.Header.Set("Content-Type", "application/json")
	client := b.HTTP
	if client == nil {
		client = &http.Client{Timeout: (pollSeconds + 10) * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("telegram %s: %v", method, err)
	}
	defer resp.Body.Close()
	var r response
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("telegram %s: %s with an unreadable body", method, resp.Status)
	}
	if r.Parameters.RetryAfter > 0 {
		return &RateLimited{After: time.Duration(r.Parameters.RetryAfter) * time.Second}
	}
	if !r.OK {
		return fmt.Errorf("telegram %s: %s", method, r.Description)
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

type update struct {
	UpdateID int64     `json:"update_id"`
	Message  *message  `json:"message"`
	Edited   *message  `json:"edited_message"`
	Callback *callback `json:"callback_query"`
}

type message struct {
	MessageID     int64           `json:"message_id"`
	From          *user           `json:"from"`
	Chat          chatInfo        `json:"chat"`
	Text          string          `json:"text"`
	ForwardOrigin json.RawMessage `json:"forward_origin"`
}

type user struct {
	ID    int64 `json:"id"`
	IsBot bool  `json:"is_bot"`
}

type chatInfo struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type callback struct {
	ID      string   `json:"id"`
	From    user     `json:"from"`
	Message *message `json:"message"`
	Data    string   `json:"data"`
}

// inbound reduces a Telegram update to what internal/chat judges.
func inbound(u update) (chat.Inbound, bool) {
	id := func(n int64) string { return strconv.FormatInt(n, 10) }
	switch {
	case u.Message != nil || u.Edited != nil:
		m := u.Message
		if m == nil {
			m = u.Edited
		}
		in := chat.Inbound{ChatID: id(m.Chat.ID), Private: m.Chat.Type == "private", MessageID: id(m.MessageID),
			Text: m.Text, Forwarded: len(m.ForwardOrigin) > 0 && string(m.ForwardOrigin) != "null", Edited: u.Edited != nil}
		if m.From != nil && !m.From.IsBot {
			in.SenderID = id(m.From.ID)
		}
		return in, true
	case u.Callback != nil && u.Callback.Message != nil:
		c := u.Callback
		return chat.Inbound{SenderID: id(c.From.ID), ChatID: id(c.Message.Chat.ID), Private: c.Message.Chat.Type == "private",
			Action: c.Data, ActionID: c.ID}, true
	}
	return chat.Inbound{}, false
}

// Run receives and sends until ctx ends, backing off on errors.
func (b *Bot) Run(ctx context.Context) {
	b.Chat.Running(Name, true)
	defer b.Chat.Running(Name, false)
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.sendLoop(ctx)
	}()
	b.receiveLoop(ctx)
	<-done
}

func (b *Bot) receiveLoop(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		if err := b.ReceiveOnce(ctx, pollSeconds); err != nil {
			wait := backoff
			var rl *RateLimited
			if errors.As(err, &rl) {
				wait = rl.After
			} else {
				backoff = min(backoff*2, 5*time.Minute)
			}
			if ctx.Err() == nil {
				b.Log.Warn("telegram receive", "err", err, "retry_in", wait)
			}
			sleep(ctx, wait)
			continue
		}
		backoff = time.Second
	}
}

// ReceiveOnce asks for new updates (waiting up to timeout seconds), handles each, and moves
// the stored cursor past it only after it was handled, so a restart neither skips nor, for
// messages, repeats anything.
func (b *Bot) ReceiveOnce(ctx context.Context, timeout int) error {
	c, err := b.Chat.Channel(ctx, Name)
	if errors.Is(err, chat.ErrUnknownChannel) {
		c = chat.ChannelState{}
	} else if err != nil {
		return err
	}
	var updates []update
	if err := b.call(ctx, "getUpdates", map[string]any{"offset": c.Offset, "timeout": timeout,
		"allowed_updates": []string{"message", "edited_message", "callback_query"}}, &updates); err != nil {
		return err
	}
	for _, u := range updates {
		if in, ok := inbound(u); ok {
			reply, err := b.Chat.Inbound(ctx, Name, in)
			if err != nil {
				return err
			}
			switch {
			case in.ActionID != "":
				if err := b.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": in.ActionID, "text": reply.Text}, nil); err != nil {
					b.Log.Warn("telegram answer", "err", err)
				}
			case reply.Text != "":
				if err := b.send(ctx, reply.ChatID, reply.Text, nil); err != nil {
					return err
				}
			}
		}
		if err := b.Chat.SetOffset(ctx, Name, u.UpdateID+1); err != nil {
			return err
		}
	}
	return nil
}

func (b *Bot) sendLoop(ctx context.Context) {
	changed, stop := b.Chat.Subscribe()
	defer stop()
	poll := b.Poll
	if poll == 0 {
		poll = 3 * time.Second
	}
	backoff := time.Second
	for ctx.Err() == nil {
		wait := poll
		if err := b.SendPending(ctx); err != nil {
			var rl *RateLimited
			if errors.As(err, &rl) {
				wait = rl.After
			} else {
				wait, backoff = backoff, min(backoff*2, 5*time.Minute)
			}
			if ctx.Err() == nil {
				b.Log.Warn("telegram send", "err", err, "retry_in", wait)
			}
		} else {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
		case <-changed:
		case <-time.After(wait):
		}
	}
}

// SendPending sends what the paired chat has not had yet and records each one once sent.
func (b *Bot) SendPending(ctx context.Context) error {
	pending, err := b.Chat.Outbound(ctx, Name)
	if err != nil {
		return err
	}
	for _, o := range pending {
		if o.Text != "" {
			if err := b.send(ctx, o.ChatID, o.Text, o.Buttons); err != nil {
				return err
			}
		}
		if err := b.Chat.Delivered(ctx, Name, o.Seq); err != nil {
			return err
		}
	}
	return nil
}

// send posts plain text: no parse mode, so nothing in it is read as markup, and no link
// previews, so nothing is fetched on the owner's behalf.
func (b *Bot) send(ctx context.Context, chatID, text string, buttons []chat.Button) error {
	body := map[string]any{"chat_id": chatID, "text": text, "link_preview_options": map[string]bool{"is_disabled": true}}
	if len(buttons) > 0 {
		var row []map[string]string
		for _, btn := range buttons {
			row = append(row, map[string]string{"text": btn.Label, "callback_data": btn.Action})
		}
		body["reply_markup"] = map[string]any{"inline_keyboard": [][]map[string]string{row}}
	}
	return b.call(ctx, "sendMessage", body, nil)
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
