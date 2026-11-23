package main

import (
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"agentd/internal/app"
	"agentd/internal/chat"
	"agentd/internal/tools"
)

// tuiSession runs the new chat as a real program with no terminal: keys are sent to it
// and what it prints into the scrollback is collected.
type tuiSession struct {
	t    *testing.T
	p    *tea.Program
	ui   *tui
	done chan error
	mu   sync.Mutex
	out  strings.Builder
}

func (w *chatWorld) tui() *tuiSession {
	w.t.Helper()
	c := &chatClient{app: app.New(w.db, nil, "", tools.Registry{}, time.Now), out: io.Discard, width: 80}
	var err error
	if c.conv, err = c.app.Chat.Default(w.ctx); err != nil {
		w.t.Fatal(err)
	}
	s := &tuiSession{t: w.t, ui: newTUI(w.ctx, c, false), done: make(chan error, 1)}
	s.ui.printed = func(block string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.out.WriteString(block + "\n")
	}
	s.p = tea.NewProgram(s.ui, tea.WithContext(w.ctx), tea.WithInput(nil), tea.WithOutput(io.Discard))
	go func() { s.done <- func() error { _, err := s.p.Run(); return err }() }()
	w.t.Cleanup(func() { s.p.Kill() })
	return s
}

func (s *tuiSession) printed() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.out.String()
}

func (s *tuiSession) say(text string) {
	s.p.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text)})
	s.p.Send(tea.KeyMsg{Type: tea.KeyEnter})
}

func (s *tuiSession) waitFor(want string) {
	s.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(s.printed(), want) {
		if time.Now().After(deadline) {
			s.t.Fatalf("the chat never printed %q:\n%s", want, s.printed())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (s *tuiSession) leave() string {
	s.t.Helper()
	s.p.Send(tea.KeyMsg{Type: tea.KeyCtrlD})
	select {
	case err := <-s.done:
		if err != nil {
			s.t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		s.t.Fatal("the chat did not exit on ctrl+d")
	}
	return s.printed()
}

func TestNewChatAnswersAndApprovesFromACard(t *testing.T) {
	w := newChatWorld(t, true)
	w.pendingApproval()
	s := w.tui()
	s.waitFor("Approval 1")
	s.say("a")
	s.waitFor("Approved [1]")
	s.say("hello")
	s.waitFor("front desk")
	out := s.leave()
	for _, want := range []string{"Baseet", "While you were away (1)", "a approve", "> hello", "● front desk", "Your projects keep running"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	var status, actor string
	w.db.QueryRow(`SELECT status FROM approvals`).Scan(&status)
	w.db.QueryRow(`SELECT actor FROM audit_log WHERE operation = 'approval.approved'`).Scan(&actor)
	if status != "approved" || actor != "owner:cli" {
		t.Fatalf("approval = %s by %s", status, actor)
	}
}

func TestNewChatNeverApprovesFromALetterInsideAMessage(t *testing.T) {
	w := newChatWorld(t, true)
	w.pendingApproval()
	s := w.tui()
	s.waitFor("Approval 1")
	s.say("a quick question")
	s.waitFor("front desk")
	s.say("a")
	s.say("approve it a")
	s.leave()
	var status string
	w.db.QueryRow(`SELECT status FROM approvals`).Scan(&status)
	if status != "pending" {
		t.Fatalf("approval = %s after letters typed when a reply, not the card, was last", status)
	}
}

func TestNewChatSaysWhenNothingIsServing(t *testing.T) {
	w := newChatWorld(t, false)
	s := w.tui()
	s.waitFor("agentd is not running")
	if view := s.ui.View(); !strings.Contains(view, "server not running") {
		t.Fatalf("status line:\n%s", view)
	}
	s.leave()
}

func tuiFor(t *testing.T, width int) *tui {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	return newTUI(t.Context(), &chatClient{width: width}, false)
}

func cardMessage(kind, status string, card chat.Card) chat.Message {
	body, _ := json.Marshal(card)
	return chat.Message{Kind: kind, Status: status, Body: body, Author: chat.Agent}
}

func TestNewChatPrintsEscapeSequencesInert(t *testing.T) {
	ui := tuiFor(t, 80)
	hostile := "safe\x1b[2J\x1b]0;owned\x07 text \u202egnp"
	text, _ := json.Marshal(chat.Text{Text: "# Title\n\n" + hostile})
	for name, out := range map[string]string{
		"reply": ui.render(chat.Message{Kind: chat.KindText, Author: chat.Agent, Body: text}),
		"card":  ui.render(cardMessage(chat.KindApproval, "pending", chat.Card{N: 1, Title: hostile, Text: hostile, Assignment: hostile})),
		"owner": ui.render(chat.Message{Kind: chat.KindText, Author: chat.Owner, Body: text}),
	} {
		if strings.ContainsAny(out, "\x1b\x07\u202e") {
			t.Fatalf("%s carries a control sequence: %q", name, out)
		}
		if !strings.Contains(out, "safe") {
			t.Fatalf("%s lost its text: %q", name, out)
		}
	}
}

func TestNewChatFitsANarrowTerminal(t *testing.T) {
	ui := tuiFor(t, 60)
	long := strings.Repeat("a long sentence that has to wrap ", 8)
	text, _ := json.Marshal(chat.Text{Text: long + "\n\n- one\n- two\n\n```\ncode\n```"})
	ui.in.SetValue("/")
	blocks := []string{
		ui.render(chat.Message{Kind: chat.KindText, Author: chat.Agent, Body: text}),
		ui.render(cardMessage(chat.KindProposal, "pending", chat.Card{N: 3, Title: long, Text: long, Assignment: "airline-refund"})),
		ui.render(cardMessage(chat.KindApproval, "approved", chat.Card{N: 4, Title: long})),
		ui.View(),
	}
	for _, block := range blocks {
		for _, line := range strings.Split(block, "\n") {
			if lipgloss.Width(line) > 60 {
				t.Fatalf("a line is %d columns wide in a 60 column terminal:\n%s", lipgloss.Width(line), block)
			}
		}
	}
	if !strings.Contains(blocks[1], "╭") || !strings.Contains(blocks[1], "e edit") {
		t.Fatalf("an open proposal is not a box with its keys:\n%s", blocks[1])
	}
	if strings.Contains(blocks[2], "\n") || !strings.Contains(blocks[2], "approved") {
		t.Fatalf("a decided card is not one line:\n%s", blocks[2])
	}
}

func TestNewChatSlashMenuCompletes(t *testing.T) {
	ui := tuiFor(t, 80)
	ui.in.SetValue("/app")
	if view := ui.View(); !strings.Contains(view, "/approve <n or id>") || strings.Contains(view, "/reject") {
		t.Fatalf("menu for /app:\n%s", view)
	}
	ui.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := ui.in.Value(); got != "/approve " {
		t.Fatalf("enter on a partial command gave %q", got)
	}
	ui.in.SetValue("first line\\")
	ui.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := ui.in.Value(); got != "first line\n" {
		t.Fatalf("a trailing backslash gave %q", got)
	}
}

func TestNewChatRendersMarkdown(t *testing.T) {
	ui := tuiFor(t, 60)
	got := ui.markdown("# Plan\n\nDo **this** and *that* with `code`, see [docs](https://example.com).\n\n" +
		"1. first\n- second\n\n> quoted\n\n```go\nfmt.Println(1)\n```\n\n| Job | State |\n|---|---|\n| refund | **open** |\n")
	want := "Plan\n\nDo this and that with `code`, see docs\n(https://example.com).\n\n" +
		"1. first\n• second\n\n│ quoted\n\n│ fmt.Println(1)\n\nJob    │ State\nrefund │ open"
	if got != want {
		t.Fatalf("markdown:\n%s\n\nwant:\n%s", got, want)
	}
}
