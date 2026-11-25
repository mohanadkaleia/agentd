package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"agentd/internal/api"
	"agentd/internal/app"
	"agentd/internal/chat"
	"agentd/internal/model"
	"agentd/internal/system"
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
	last string
}

// view is the live region as last drawn.
func (s *tuiSession) view() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func (w *chatWorld) tui() *tuiSession {
	w.t.Helper()
	c := &chatClient{app: app.New(w.db, nil, "", tools.Registry{}, time.Now), out: io.Discard, width: 80}
	var err error
	if c.conv, err = c.app.Chat.Default(w.ctx); err != nil {
		w.t.Fatal(err)
	}
	s := &tuiSession{t: w.t, ui: newTUI(w.ctx, c, false), done: make(chan error, 1)}
	s.ui.guard = 0
	s.ui.printed = func(block string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.out.WriteString(block + "\n")
	}
	s.ui.drawn = func(view string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.last = view
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

func (s *tuiSession) waitForView(want string) {
	s.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(s.view(), want) {
		if time.Now().After(deadline) {
			s.t.Fatalf("the chat never drew %q:\n%s", want, s.view())
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
	s.waitFor("chat")
	s.waitFor("◦ Approval 1 · approved")
	out := s.leave()
	for _, want := range []string{"agentd", "While you were away (1)", "> hello", "◦ Approval 1 · approved", "● chat", "Your projects keep running"} {
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
	s.waitFor("chat")
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
	s.waitFor("No server is running")
	if view := s.view(); !strings.Contains(view, "server not running") {
		t.Fatalf("status line:\n%s", view)
	}
	s.leave()
}

func tuiFor(t *testing.T, width int) *tui {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ui := newTUI(t.Context(), &chatClient{width: width}, false)
	ui.status.alive = true
	ui.guard = 0
	return ui
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
		ui.welcome(tuiStatus{jobs: 2, waiting: 1, model: strings.Repeat("model", 20)}),
	}
	ui.in.SetValue("")
	blocks = append(blocks, ui.View())
	ui.help = true
	blocks = append(blocks, ui.View())
	ui.dialog = ui.cardDialog(cardMessage(chat.KindProposal, "pending", chat.Card{N: 3, Title: long}))
	blocks = append(blocks, ui.View())
	for _, block := range blocks {
		for _, line := range strings.Split(block, "\n") {
			if lipgloss.Width(line) > 60 {
				t.Fatalf("a line is %d columns wide in a 60 column terminal:\n%s", lipgloss.Width(line), block)
			}
		}
	}
	if !strings.Contains(blocks[1], "╭") || !strings.Contains(blocks[1], "Proposal 3") {
		t.Fatalf("an open proposal is not a box:\n%s", blocks[1])
	}
	if dialog := blocks[len(blocks)-1]; !strings.Contains(dialog, "❯ 1. Yes, approve") || !strings.Contains(dialog, "2. Edit it first") {
		t.Fatalf("the proposal dialog lacks its choices:\n%s", dialog)
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

func TestNewChatDecidesFromTheDialog(t *testing.T) {
	w := newChatWorld(t, true)
	w.pendingApproval()
	s := w.tui()
	s.waitForView("❯ 1. Yes, approve")
	// Typing puts the dialog aside: the r goes to the composer and rejects nothing.
	s.p.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	s.waitForView("> r")
	s.p.Send(tea.KeyMsg{Type: tea.KeyBackspace})
	s.waitForView("tab to decide Approval 1")
	s.p.Send(tea.KeyMsg{Type: tea.KeyTab})
	s.waitForView("❯ 1. Yes, approve")
	s.p.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	s.waitForView("❯ 2. No, reject")
	s.p.Send(tea.KeyMsg{Type: tea.KeyUp})
	s.p.Send(tea.KeyMsg{Type: tea.KeyEnter})
	s.waitFor("Approved [1]")
	s.leave()
	var status string
	w.db.QueryRow(`SELECT status FROM approvals`).Scan(&status)
	if status != "approved" {
		t.Fatalf("approval = %s; a typed r must not reject, and enter on Yes must approve", status)
	}
}

func TestNewChatDialogIgnoresEnterJustAfterItOpens(t *testing.T) {
	ui := tuiFor(t, 80)
	ui.guard = time.Hour
	card := cardMessage(chat.KindApproval, "pending", chat.Card{N: 1, Title: "Send the email"})
	card.Seq = 7
	ui.open[7] = card
	ui.offer()
	if ui.dialog == nil {
		t.Fatal("a waiting card opened no dialog")
	}
	if _, cmd := ui.Update(tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil || ui.dialog == nil {
		t.Fatal("enter decided a card in the moment its dialog opened")
	}
	ui.Update(tea.KeyMsg{Type: tea.KeyEsc})
	ui.offer()
	if ui.dialog != nil {
		t.Fatal("a dialog put aside with esc opened again by itself")
	}
	ui.in.SetValue("")
	ui.dismissed = map[int64]bool{}
	ui.in.SetValue("typing")
	ui.offer()
	if ui.dialog != nil {
		t.Fatal("a dialog opened while the owner was typing")
	}
}

func TestNewChatAsksForAModelOnce(t *testing.T) {
	ui := tuiFor(t, 80)
	ui.status.alive = false
	var started []string
	ui.startServer = func(_ context.Context, _ *sql.DB, model string) (*launched, error) {
		if model == "" {
			return nil, errNoModel
		}
		started = append(started, model)
		return &launched{log: "/tmp/serve.log", done: make(chan struct{})}, nil
	}
	ui.models = func(context.Context) []string { return []string{"anthropic", "ollama/qwen2.5:7b"} }
	ui.c.app = &app.App{}
	ui.Update(ui.start("")())
	if view := ui.View(); !strings.Contains(view, "Which model") || !strings.Contains(view, "2. ollama/qwen2.5:7b") {
		t.Fatalf("no model picker:\n%s", view)
	}
	ui.Update(tea.KeyMsg{Type: tea.KeyDown})
	_, cmd := ui.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil || ui.dialog != nil || !ui.starting {
		t.Fatal("picking a model started nothing")
	}
	if ui.start("ollama/qwen2.5:7b") != nil {
		t.Fatal("a second start ran while one was under way")
	}
	ui.starting = false
	if ui.start("ollama/qwen2.5:7b")(); len(started) != 1 || started[0] != "ollama/qwen2.5:7b" {
		t.Fatalf("started = %v", started)
	}
	var said strings.Builder
	ui.printed = func(block string) { said.WriteString(block) }
	ui.Update(tuiModels{})
	if ui.dialog != nil || !strings.Contains(said.String(), "No model found") {
		t.Fatalf("with no model available the chat did not say what to do: %q", said.String())
	}
}

func TestNewChatShowsAReplyWhileItIsWritten(t *testing.T) {
	ui := tuiFor(t, 80)
	ui.Update(tuiDelta{TaskID: "t1", Text: "Half a **sen"})
	ui.Update(tuiDelta{TaskID: "t1", Text: "tence** \x1b[2Jso far"})
	if view := ui.View(); !strings.Contains(view, "● Half a sentence so far") || strings.Contains(view, "\x1b[2J") {
		t.Fatalf("streamed text is not shown safely:\n%q", view)
	}
	ui.Update(tuiDelta{TaskID: "t1", Restarted: true})
	ui.Update(tuiDelta{TaskID: "t1", Text: "Again"})
	if view := ui.View(); strings.Contains(view, "Half") || !strings.Contains(view, "● Again") {
		t.Fatalf("a restarted reply kept its old text:\n%s", view)
	}
	text, _ := json.Marshal(chat.Text{Text: "Again, in full."})
	ui.polled(tuiPolled{msgs: []chat.Message{{ID: "m", Seq: 1, Kind: chat.KindText, Author: chat.Agent, TurnTaskID: "t1", Body: text}}})
	if view := ui.View(); strings.Contains(view, "●") {
		t.Fatalf("the streamed text stayed after the stored reply arrived:\n%s", view)
	}
}

func TestNewChatStreamsFromARunningServer(t *testing.T) {
	w := newChatWorld(t, true)
	token := strings.Repeat("t", 40)
	srv := httptest.NewServer((&api.Server{App: w.serve, Token: token, StreamPoll: 20 * time.Millisecond,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler())
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
	})
	tokenFile := filepath.Join(t.TempDir(), "api-token")
	os.WriteFile(tokenFile, []byte(token+"\n"), 0o600)
	if err := system.Beat(w.ctx, w.db, system.Liveness{PID: 1, Build: buildMark(), At: time.Now(), Listen: strings.TrimPrefix(srv.URL, "http://"), TokenFile: tokenFile}); err != nil {
		t.Fatal(err)
	}
	w.setReply(func(model.Request) model.Message { return model.Message{Content: "BLOCK"} })
	s := w.tui()
	go s.ui.stream()
	s.waitFor("agentd")
	s.say("hello")
	var turn string
	deadline := time.Now().Add(10 * time.Second)
	for turn == "" && time.Now().Before(deadline) {
		w.db.QueryRow(`SELECT turn_task_id FROM conversation_messages WHERE author = 'owner'`).Scan(&turn)
		time.Sleep(20 * time.Millisecond)
	}
	for !strings.Contains(s.view(), "● written so far") {
		if time.Now().After(deadline) {
			t.Fatalf("streamed text never reached the chat:\n%s", s.view())
		}
		w.serve.Chat.Restarted(turn)
		w.serve.Chat.Delta(turn, "written so far")
		time.Sleep(50 * time.Millisecond)
	}
}

func TestNewChatStartsTheServerWhenAsked(t *testing.T) {
	ui := tuiFor(t, 80)
	ui.status.alive = false
	ui.c.app = &app.App{}
	ui.startServer = func(context.Context, *sql.DB, string) (*launched, error) {
		return &launched{log: "/tmp/serve.log", done: make(chan struct{})}, nil
	}
	if view := ui.View(); !strings.Contains(view, "ctrl+s to start it") {
		t.Fatalf("status line:\n%s", view)
	}
	_, cmd := ui.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	ui.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if cmd == nil || !strings.Contains(ui.View(), "starting the server") {
		t.Fatalf("ctrl+s did not start the server:\n%s", ui.View())
	}
}

func TestStartServerUsesTheLastLaunch(t *testing.T) {
	w := newChatWorld(t, false)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	old, had := serveCommand, serveHasModel
	t.Cleanup(func() { serveCommand, serveHasModel = old, had })
	serveCommand, serveHasModel = []string{"/usr/bin/true", "serve"}, false
	if _, err := startServer(w.ctx, w.db, ""); err != errNoModel {
		t.Fatalf("with no model and no earlier launch: %v", err)
	}
	if err := system.Launched(w.ctx, w.db, system.Launch{Args: []string{"-model", "ollama/x", "serve"}, Dir: t.TempDir()}, time.Now()); err != nil {
		t.Fatal(err)
	}
	l, err := startServer(w.ctx, w.db, "")
	if err != nil {
		t.Fatalf("starting the way serve last ran: %v", err)
	}
	oldDB := dbFile
	t.Cleanup(func() { dbFile = oldDB })
	dbFile = "some/agentd.db"
	abs, _ := filepath.Abs(dbFile)
	if got := withDB([]string{"-db", "elsewhere.db", "-model", "m", "--db=x", "serve"}); strings.Join(got, " ") != "-db "+abs+" -model m serve" {
		t.Fatalf("a started server is not told the database in use: %v", got)
	}
	// /usr/bin/true exits at once without serving: that is reported, with no long wait.
	if err := awaitServer(w.ctx, w.db, l, time.Minute); err == nil || !strings.Contains(err.Error(), "could not start") {
		t.Fatalf("a server that exited at once: %v", err)
	}
}

// /model opens a picker of serve's models, marks the current one, and choosing sets it.
func TestNewChatModelPicker(t *testing.T) {
	w := newChatWorld(t, false)
	live := system.Liveness{PID: 1, Build: buildMark(), At: time.Now(), Models: []system.ModelInfo{
		{ID: "anthropic/claude-opus-5-5", Alias: "opus"},
		{ID: "anthropic/claude-sonnet-5-5", Alias: "sonnet"},
		{ID: "anthropic/claude-haiku-5-5", Alias: "haiku"},
	}, Tiers: map[string]string{"fast": "haiku", "default": "sonnet", "strong": "opus"}}
	if err := system.Beat(w.ctx, w.db, live); err != nil {
		t.Fatal(err)
	}
	c := &chatClient{app: app.New(w.db, nil, "", tools.Registry{}, time.Now), out: io.Discard, width: 60}
	var err error
	if c.conv, err = c.app.Chat.Default(w.ctx); err != nil {
		t.Fatal(err)
	}
	ui := newTUI(w.ctx, c, false)
	ui.status.alive, ui.guard, ui.width = true, 0, 60
	if cmd := ui.submit("/model"); cmd != nil || ui.dialog == nil {
		t.Fatal("/model did not open the picker")
	}
	view := ui.View()
	for _, want := range []string{"Select model", "❯ 1. Automatic ✔", "Haiku 5.5 for chat", "2. Opus 5.5", "3. Sonnet 5.5", "smart and fast"} {
		if !strings.Contains(view, want) {
			t.Errorf("picker lacks %q:\n%s", want, view)
		}
	}
	for _, line := range strings.Split(view, "\n") {
		if lipgloss.Width(line) > 60 {
			t.Fatalf("a line is %d columns wide in a 60 column terminal:\n%s", lipgloss.Width(line), view)
		}
	}
	ui.dialogKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("3")})
	cmd, _ := ui.dialogKey(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("choosing Sonnet did nothing")
	}
	if posted, ok := cmd().(tuiPosted); !ok || posted.err != nil {
		t.Fatalf("choosing Sonnet: %#v", posted)
	}
	var model string
	w.db.QueryRow(`SELECT COALESCE(model, '') FROM conversations WHERE id = ?`, c.conv).Scan(&model)
	if model != "sonnet" {
		t.Fatalf("conversation model is %q, want sonnet", model)
	}
	ui.submit("/model")
	if view := ui.View(); !strings.Contains(view, "❯ 3. Sonnet 5.5 ✔") {
		t.Fatalf("the picker does not start on the current model:\n%s", view)
	}
}

func TestNewChatShowsEveryRowOfALongDraft(t *testing.T) {
	ui := tuiFor(t, 60)
	for _, r := range "one two three four five six seven eight nine ten eleven twelve thirteen fourteen" {
		ui.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	view := ui.View()
	if !strings.Contains(view, "> one two") || !strings.Contains(view, "fourteen") {
		t.Fatalf("a wrapped draft lost a row:\n%s", view)
	}
	rows := strings.Count(view, "\n")
	ui.Update(tea.KeyMsg{Type: tea.KeyCtrlJ})
	if got := strings.Count(ui.View(), "\n"); got != rows+1 {
		t.Fatalf("a new line added %d rows", got-rows)
	}
	ui.in.SetValue("")
	if got := strings.Count(ui.View(), "\n"); got != rows-1 {
		t.Fatalf("an empty composer is %d rows shorter than a two-row draft", rows-got)
	}
}

func TestNewChatKeepsWhatWasWrittenBeforeAnInterruption(t *testing.T) {
	ui := tuiFor(t, 80)
	var said strings.Builder
	ui.printed = func(block string) { said.WriteString(block + "\n") }
	ui.Update(tuiDelta{TaskID: "t1", Text: "Rivers are long"})
	notice, _ := json.Marshal(chat.Text{Text: "Stopped."})
	ui.polled(tuiPolled{msgs: []chat.Message{{ID: "n", Seq: 2, Kind: chat.KindNotice, Author: chat.Agent, TurnTaskID: "t1", Body: notice}}})
	ui.Update(tuiPrinted{})
	if out := said.String(); !strings.Contains(out, "● Rivers are long") || !strings.Contains(out, "cut short") ||
		strings.Index(out, "Rivers") > strings.Index(out, "Stopped.") {
		t.Fatalf("the interrupted text was not kept above the notice:\n%s", out)
	}
	if strings.Contains(ui.View(), "Rivers") {
		t.Fatalf("the interrupted text is still in the live region:\n%s", ui.View())
	}
}

func TestNewChatSummarisesALongAbsence(t *testing.T) {
	ui := tuiFor(t, 80)
	ui.c.app = app.New(newChatWorld(t, false).db, nil, "", tools.Registry{}, time.Now)
	var said strings.Builder
	ui.printed = func(block string) { said.WriteString(block + "\n") }
	var away []chat.Message
	for i := 1; i <= 20; i++ {
		text, _ := json.Marshal(chat.Text{Text: fmt.Sprintf("note %d.", i)})
		away = append(away, chat.Message{ID: fmt.Sprint(i), Seq: int64(i), Kind: chat.KindNotice, Author: chat.Agent, Body: text})
	}
	card := cardMessage(chat.KindApproval, "pending", chat.Card{N: 1, Title: "Send the email"})
	card.Seq = 3
	away[2] = card
	ui.Update(tuiLoaded{away: away, last: 20, status: tuiStatus{alive: true}})
	ui.Update(tuiPrinted{})
	out := said.String()
	for _, want := range []string{"While you were away (20)", "Send the email", "earlier messages not shown", "note 20.", "note 17."} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "note 5.") {
		t.Fatalf("an old note was printed in full:\n%s", out)
	}
	if ui.dialog == nil {
		t.Fatal("the waiting approval opened no dialog")
	}
}

func TestNewChatRestartSaysWhyTheServerDidNotStart(t *testing.T) {
	w := newChatWorld(t, false)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := w.tui()
	os.MkdirAll(stateDir(), 0o700)
	log := filepath.Join(stateDir(), "serve.log")
	os.WriteFile(log, []byte("agentd: unknown model provider \"bogus\"\n"), 0o600)
	s.ui.startServer = func(context.Context, *sql.DB, string) (*launched, error) {
		done := make(chan struct{})
		close(done)
		return &launched{log: log, done: done}, nil
	}
	s.waitFor("agentd")
	s.say("/restart")
	s.waitFor(`could not start: unknown model provider "bogus"`)
	if strings.Contains(s.view(), "starting the server") {
		t.Fatalf("the chat still says it is starting:\n%s", s.view())
	}
	s.leave()
}
