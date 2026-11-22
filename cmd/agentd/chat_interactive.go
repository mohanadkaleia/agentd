package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"agentd/internal/chat"
	"agentd/internal/system"
)

// lineReader is the part of term.Terminal the session uses, so tests can drive it.
type lineReader interface {
	io.Writer
	ReadLine() (string, error)
	SetPrompt(string)
}

// rawMode switches the terminal between line editing and normal output.
type rawMode interface {
	enter() error
	leave()
}

type ttyMode struct {
	fd    int
	state *term.State
}

func (m *ttyMode) enter() (err error) {
	m.state, err = term.MakeRaw(m.fd)
	return err
}

func (m *ttyMode) leave() {
	if m.state != nil {
		term.Restore(m.fd, m.state)
		m.state = nil
	}
}

func (c *chatClient) interactive(ctx context.Context, stdin io.Reader) error {
	in, ok := stdin.(*os.File)
	if !ok {
		return errors.New("interactive chat needs a terminal")
	}
	t := term.NewTerminal(struct {
		io.Reader
		io.Writer
	}{in, c.out}, "> ")
	t.History = loadHistory()
	t.SetBracketedPasteMode(true)
	defer t.SetBracketedPasteMode(false)
	c.interrupt = make(chan os.Signal, 1)
	signal.Notify(c.interrupt, os.Interrupt)
	defer signal.Stop(c.interrupt)
	return c.session(ctx, t, &ttyMode{fd: int(in.Fd())})
}

// session is the interactive loop: a header and what arrived while the owner was away,
// then prompt, send, follow the turn, repeat. Cards that arrive while the owner sits at
// the prompt are printed above it.
func (c *chatClient) session(ctx context.Context, t lineReader, mode rawMode) error {
	if err := c.header(ctx); err != nil {
		return err
	}
	var pending strings.Builder
	for {
		watchCtx, stopWatch := context.WithCancel(ctx)
		var watching sync.WaitGroup
		watching.Add(1)
		go func() {
			defer watching.Done()
			c.watch(watchCtx, t)
		}()
		if err := mode.enter(); err != nil {
			stopWatch()
			return err
		}
		line, err := t.ReadLine()
		mode.leave()
		stopWatch()
		watching.Wait()
		c.markRead(ctx)
		pasted := errors.Is(err, term.ErrPasteIndicator)
		if err != nil && !pasted {
			if errors.Is(err, io.EOF) {
				c.goodbye()
				return nil
			}
			return err
		}
		if pasted || strings.HasSuffix(line, `\`) {
			pending.WriteString(strings.TrimSuffix(line, `\`) + "\n")
			t.SetPrompt("… ")
			continue
		}
		text := strings.TrimSpace(pending.String() + line)
		pending.Reset()
		t.SetPrompt("> ")
		if text == "" {
			continue
		}
		if done, err := c.local(ctx, text); done || err != nil {
			if errors.Is(err, io.EOF) {
				c.goodbye()
				return nil
			}
			if err != nil {
				c.print("! " + err.Error())
			}
			continue
		}
		if alive, err := c.alive(ctx); err != nil {
			return err
		} else if !alive && !strings.HasPrefix(text, "/") {
			c.print("· " + notRunningHint)
			c.print("· Your message is saved and will be answered when agentd starts.")
		}
		m, err := c.post(ctx, chat.Post{Text: text})
		if err != nil {
			c.print("! " + err.Error())
			continue
		}
		if _, err := c.follow(ctx, m, c.timeout); err != nil {
			return err
		}
		c.markRead(ctx)
	}
}

// local handles input that never leaves this process: quitting, clearing the screen, and
// the one-letter decisions on the card shown last.
func (c *chatClient) local(ctx context.Context, text string) (bool, error) {
	switch text {
	case "/quit", "/exit":
		return true, io.EOF
	case "/clear":
		fmt.Fprint(c.out, "\x1b[H\x1b[2J")
		return true, nil
	}
	if len(text) != 1 {
		return false, nil
	}
	c.mu.Lock()
	last := c.last
	c.mu.Unlock()
	if last == nil {
		return false, nil
	}
	n := last.Card().N
	switch text {
	case "d":
		c.print(c.details(*last))
		return true, nil
	case "a", "r":
		command := map[string]string{"a": "/approve", "r": "/reject"}[text]
		m, err := c.post(ctx, chat.Post{Text: fmt.Sprintf("%s %d", command, n)})
		if err != nil {
			return true, err
		}
		_, err = c.follow(ctx, m, 0)
		return true, err
	}
	return false, nil
}

func (c *chatClient) header(ctx context.Context) error {
	var live, waiting int
	if err := c.app.DB.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM assignments WHERE builtin IS NULL AND status IN ('active', 'paused')),
		(SELECT COUNT(*) FROM approvals WHERE status = 'pending')
		+ (SELECT COUNT(*) FROM actions WHERE status = 'needs_reconciliation')`).Scan(&live, &waiting); err != nil {
		return err
	}
	st, err := system.Get(ctx, c.app.DB)
	if err != nil {
		return err
	}
	stop := "off"
	if st.Halted {
		stop = "ON (" + sanitize(st.Reason) + ")"
	}
	c.print(c.bold("agentd") + fmt.Sprintf(" · %d assignments · %d waiting on you · emergency stop %s · /help for commands", live, waiting, stop))
	if alive, err := c.alive(ctx); err != nil {
		return err
	} else if !alive {
		c.print("· " + notRunningHint)
	}
	away, err := c.app.Chat.Away(ctx, c.conv, chatChannel)
	if err != nil {
		return err
	}
	if len(away) > 0 {
		c.print(c.dim(fmt.Sprintf("While you were away (%d):", len(away))))
		for _, m := range away {
			c.show(m)
		}
	}
	var last int64
	if err := c.app.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM conversation_messages WHERE conversation_id = ?`,
		c.conv).Scan(&last); err != nil {
		return err
	}
	c.mu.Lock()
	c.after = max(c.after, last)
	c.mu.Unlock()
	c.markRead(ctx)
	return nil
}

// watch prints messages that arrive while the owner is at the prompt, above the line being typed.
func (c *chatClient) watch(ctx context.Context, t lineReader) {
	out := c.out
	c.out = t
	defer func() { c.out = out }()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
		msgs, err := c.app.Chat.Messages(ctx, c.conv, c.after, 50)
		if err != nil {
			return
		}
		for _, m := range msgs {
			c.show(m)
		}
	}
}

func (c *chatClient) markRead(ctx context.Context) {
	c.mu.Lock()
	seq := c.after
	c.mu.Unlock()
	c.app.Chat.MarkRead(ctx, c.conv, chatChannel, seq)
}

// goodbye tells the owner, the first time, that leaving the chat does not stop their work.
func (c *chatClient) goodbye() {
	marker := filepath.Join(stateDir(), "chat_exit_noted")
	if _, err := os.Stat(marker); err == nil || stateDir() == "" {
		return
	}
	c.print(c.dim("· Your projects keep running; leaving the chat stops nothing."))
	if os.MkdirAll(stateDir(), 0o700) == nil {
		os.WriteFile(marker, nil, 0o600)
	}
}
