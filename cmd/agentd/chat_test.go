package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"agentd/internal/app"
	"agentd/internal/assignments"
	"agentd/internal/chat"
	"agentd/internal/model"
	"agentd/internal/policy"
	"agentd/internal/system"
	"agentd/internal/tasks"
	"agentd/internal/testutil"
	"agentd/internal/tools"
)

type chatWorld struct {
	t     *testing.T
	ctx   context.Context
	db    *sql.DB
	serve *app.App
	mu    sync.Mutex
	reply func(req model.Request) model.Message
}

func (w *chatWorld) Complete(ctx context.Context, req model.Request) (model.Response, error) {
	w.mu.Lock()
	reply := w.reply
	w.mu.Unlock()
	msg := model.Message{Content: "hello from the front desk"}
	if reply != nil {
		msg = reply(req)
	}
	if msg.Content == "BLOCK" {
		<-ctx.Done()
		return model.Response{}, ctx.Err()
	}
	stop := model.StopEnd
	if len(msg.ToolCalls) > 0 {
		stop = model.StopToolUse
	}
	return model.Response{Message: msg, StopReason: stop}, nil
}

func (w *chatWorld) setReply(f func(model.Request) model.Message) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.reply = f
}

// newChatWorld starts a serving agentd in the background, as `agentd serve` would.
func newChatWorld(t *testing.T, serving bool) *chatWorld {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	w := &chatWorld{t: t, ctx: ctx, db: testutil.Open(t, testutil.DBPath(t))}
	w.serve = app.New(w.db, w, "cloud", tools.NewRegistry(&approvalTool{}), time.Now)
	if _, err := w.serve.Chat.FrontDesk(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		<-done
	})
	if !serving {
		close(done)
		return w
	}
	if err := system.Beat(ctx, w.db, system.Liveness{PID: 1, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	go func() {
		defer close(done)
		w.serve.Runtime.Serve(ctx, "test", 1, 50*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	return w
}

func (w *chatWorld) chat(stdin string, args ...string) (string, error) {
	w.t.Helper()
	var out bytes.Buffer
	err := runChat(w.ctx, w.db, args, strings.NewReader(stdin), &out, false, stdin == "", 80)
	return out.String(), err
}

func code(err error) int {
	var exit *exitError
	if errors.As(err, &exit) {
		return exit.code
	}
	if err != nil {
		return 1
	}
	return 0
}

type approvalTool struct{}

func (approvalTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{Name: "send", Description: "Send.", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (approvalTool) Effect() tools.Effect                     { return tools.EffectWrite }
func (approvalTool) Resource(json.RawMessage) (string, error) { return "someone@example.com", nil }
func (approvalTool) Execute(context.Context, tools.Invocation) (tools.Result, error) {
	return tools.Result{Output: json.RawMessage(`{}`)}, nil
}
func (approvalTool) Reconcile(context.Context, tools.Invocation) (tools.Result, tools.Finding, error) {
	return tools.Result{}, tools.Unknown, nil
}

// pendingApproval makes a project task whose model call needs an approval, and a card for it.
func (w *chatWorld) pendingApproval() {
	w.t.Helper()
	w.serve.Chat.Project(w.ctx)
	project, err := w.serve.Assignments.CreateProject(w.ctx, assignments.NewProject{Name: "p", Objective: "o",
		Policy: policy.Policy{Tools: map[string]policy.Rule{"send": {Mode: policy.RequireApproval}}}})
	if err != nil {
		w.t.Fatal(err)
	}
	w.setReply(func(req model.Request) model.Message {
		if strings.HasPrefix(req.Messages[0].Content, "You are agentd's front desk") {
			return model.Message{Content: "front desk"}
		}
		if req.Messages[len(req.Messages)-1].Role == model.RoleTool {
			return model.Message{Content: "done"}
		}
		return model.Message{ToolCalls: []model.ToolCall{{ID: "c", Name: "send", Arguments: json.RawMessage(`{}`)}}}
	})
	if _, err := w.serve.Tasks.Create(w.ctx, tasks.NewTask{AssignmentID: project, Title: "t", Objective: "o"}); err != nil {
		w.t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		w.db.QueryRow(`SELECT COUNT(*) FROM approvals WHERE status = 'pending'`).Scan(&n)
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			w.t.Fatal("no approval")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := w.serve.Chat.Project(w.ctx); err != nil {
		w.t.Fatal(err)
	}
}

func TestOneShotPrintsTheReply(t *testing.T) {
	w := newChatWorld(t, true)
	out, err := w.chat("", "hello")
	if code(err) != 0 || !strings.Contains(out, "hello from the front desk") {
		t.Fatalf("exit %d, output %q", code(err), out)
	}
}

func TestChatWithNothingServingSaysSo(t *testing.T) {
	w := newChatWorld(t, false)
	_, err := w.chat("", "hello")
	if code(err) != exitNotRunning || !strings.Contains(err.Error(), "agentd serve") {
		t.Fatalf("exit %d: %v", code(err), err)
	}
	if _, err := w.chat("", "-no-wait", "hello"); code(err) != 0 {
		t.Fatalf("-no-wait exit %d: %v", code(err), err)
	}
	var n int
	w.db.QueryRow(`SELECT COUNT(*) FROM conversation_messages WHERE author = 'owner'`).Scan(&n)
	if n != 1 {
		t.Fatalf("stored owner messages = %d, want the -no-wait one", n)
	}
}

func TestPipedInputWithJSONOutputIsCleanJSONLines(t *testing.T) {
	w := newChatWorld(t, true)
	w.setReply(func(model.Request) model.Message {
		return model.Message{Content: "\x1b[2J\x1b]0;owned\x07ok\rdone\u202e"}
	})
	out, err := w.chat("summarise this", "-json")
	if code(err) != 0 {
		t.Fatalf("exit %d: %v", code(err), err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var reply string
	for _, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("not JSON: %q", line)
		}
		for _, r := range line {
			if r < 0x20 || r == 0x7f {
				t.Fatalf("control character in %q", line)
			}
		}
		if rec["type"] == "message" {
			reply = line
		}
	}
	if !strings.Contains(reply, `"text":"okdone"`) {
		t.Fatalf("reply = %s", reply)
	}
}

func TestStandardInputIsAttachedToTheMessage(t *testing.T) {
	w := newChatWorld(t, true)
	var seen string
	w.setReply(func(req model.Request) model.Message {
		seen = req.Messages[len(req.Messages)-1].Content
		return model.Message{Content: "summary"}
	})
	var out bytes.Buffer
	err := runChat(w.ctx, w.db, []string{"summarise this"}, strings.NewReader("line one\nline two\n"), &out, false, false, 80)
	if err != nil || !strings.Contains(seen, "summarise this\n\nAttached") || !strings.Contains(seen, "line two") {
		t.Fatalf("err %v, model saw %q", err, seen)
	}
	big := strings.Repeat("x", maxAttachment+1)
	err = runChat(w.ctx, w.db, []string{"too big"}, strings.NewReader(big), &out, false, false, 80)
	if code(err) != 1 || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("oversized input = %v", err)
	}
}

func TestEscapeSequencesArePrintedInert(t *testing.T) {
	for in, want := range map[string]string{
		"plain\ttext\nnext":                           "plain\ttext\nnext",
		"\x1b[31mred\x1b[0m":                          "red",
		"\x1b]0;title\x07after":                       "after",
		"\x1b]8;;http://evil\x1b\\link\x1b]8;;\x1b\\": "link",
		"approve? [y/N]\r[2] APPROVAL fake":           "approve? [y/N][2] APPROVAL fake",
		"\x1b[2K\x1b[1Ahidden":                        "hidden",
		"\u009b31mc1":                                 "c1",
		"left\u202eright":                             "leftright",
		"bell\x07 and nul\x00":                        "bell and nul",
	} {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCtrlCDuringATurnCancelsIt(t *testing.T) {
	w := newChatWorld(t, true)
	w.setReply(func(model.Request) model.Message { return model.Message{Content: "BLOCK"} })
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var n int
			w.db.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&n)
			if n == 1 {
				time.Sleep(100 * time.Millisecond)
				syscall.Kill(syscall.Getpid(), syscall.SIGINT)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	out, err := w.chat("", "-timeout", "10s", "a long question")
	if code(err) != exitTurnFailed || !strings.Contains(out, "Stopped") {
		t.Fatalf("exit %d, output %q", code(err), out)
	}
	var status string
	w.db.QueryRow(`SELECT status FROM tasks WHERE priority = ?`, tasks.PriorityInteractive).Scan(&status)
	if status != "cancelled" {
		t.Fatalf("turn = %s", status)
	}
}

func TestApproveFromTheCLIIsAuditedWithItsChannel(t *testing.T) {
	w := newChatWorld(t, true)
	w.pendingApproval()
	out, err := w.chat("", "/approve 1")
	if code(err) != 0 || !strings.Contains(out, "Approved [1]") {
		t.Fatalf("exit %d, output %q", code(err), out)
	}
	var actor string
	w.db.QueryRow(`SELECT actor FROM audit_log WHERE operation = 'approval.approved'`).Scan(&actor)
	if actor != "owner:cli" {
		t.Fatalf("approval audited as %q", actor)
	}
	if _, err := w.chat("", "/aprove 1"); code(err) != 1 {
		t.Fatalf("a misspelt command exited %d", code(err))
	}
}

type scriptedTerminal struct {
	out   bytes.Buffer
	mu    sync.Mutex
	lines []func() string
}

func (s *scriptedTerminal) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.out.Write(p)
}

func (s *scriptedTerminal) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.out.String()
}

func (s *scriptedTerminal) SetPrompt(string) {}

func (s *scriptedTerminal) ReadLine() (string, error) {
	if len(s.lines) == 0 {
		return "", io.EOF
	}
	next := s.lines[0]
	s.lines = s.lines[1:]
	return next(), nil
}

type noRaw struct{}

func (noRaw) enter() error { return nil }
func (noRaw) leave()       {}

func (w *chatWorld) session(lines ...func() string) string {
	w.t.Helper()
	term := &scriptedTerminal{lines: lines}
	c := &chatClient{app: app.New(w.db, nil, "", tools.Registry{}, time.Now), out: term, width: 80}
	var err error
	if c.conv, err = c.app.Chat.Default(w.ctx); err != nil {
		w.t.Fatal(err)
	}
	if err := c.session(w.ctx, term, noRaw{}); err != nil {
		w.t.Fatal(err)
	}
	return term.String()
}

func say(text string) func() string { return func() string { return text } }

func TestInteractiveSessionAnswersAndApprovesFromACard(t *testing.T) {
	w := newChatWorld(t, true)
	w.pendingApproval()
	out := w.session(say("a"), say("hello"))
	for _, want := range []string{"1 waiting on you", "While you were away (1)", "[1] APPROVAL", "(a)pprove",
		"Approved [1]", "front desk", "Your projects keep running"} {
		if !strings.Contains(out, want) {
			t.Fatalf("session output lacks %q:\n%s", want, out)
		}
	}
	var status string
	w.db.QueryRow(`SELECT status FROM approvals`).Scan(&status)
	if status != "approved" {
		t.Fatalf("approval = %s", status)
	}
}

func TestALetterNeverApprovesUnlessTheCardIsTheLastThingShown(t *testing.T) {
	w := newChatWorld(t, true)
	w.pendingApproval()
	w.session(say("a quick question"), say("a"), say("approve it a"))
	var status string
	w.db.QueryRow(`SELECT status FROM approvals`).Scan(&status)
	if status != "pending" {
		t.Fatalf("approval = %s after letters typed when a reply, not the card, was last", status)
	}
}

func TestUsageListsChat(t *testing.T) {
	if !strings.HasPrefix(usage, "Talk to agentd: agentd\n") || !strings.Contains(usage, "-no-wait") {
		t.Fatal("usage does not lead with the chat")
	}
}

var _ = chat.Agent

func TestEditingAProposalReturnsTheSavedJSON(t *testing.T) {
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", `f() { printf '{"spec": {"name": "edited"}}' > "$1"; }; f`)
	got, err := editInEditor(chat.Card{Arguments: json.RawMessage(`{"spec":{"name":"draft"}}`)})
	if err != nil || got != `{"spec":{"name":"edited"}}` {
		t.Fatalf("edited = %q, %v", got, err)
	}
	t.Setenv("EDITOR", "true")
	if got, err := editInEditor(chat.Card{Arguments: json.RawMessage(`{"a":1}`)}); err != nil || got != "" {
		t.Fatalf("unchanged = %q, %v; want nothing to send", got, err)
	}
	t.Setenv("EDITOR", `f() { printf 'not json' > "$1"; }; f`)
	if _, err := editInEditor(chat.Card{Arguments: json.RawMessage(`{"a":1}`)}); err == nil {
		t.Fatal("invalid JSON was accepted")
	}
}

func TestAnIdleStandardInputIsNotWaitedFor(t *testing.T) {
	w := newChatWorld(t, true)
	idle, _ := io.Pipe()
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- runChat(w.ctx, w.db, []string{"hello"}, idle, &out, false, false, 80) }()
	select {
	case err := <-done:
		if err != nil || !strings.Contains(out.String(), "hello from the front desk") {
			t.Fatalf("err %v, output %q", err, out.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("chat waited for an idle standard input")
	}
}

func TestAProposalCardEndsTheWait(t *testing.T) {
	w := newChatWorld(t, true)
	w.serve.Chat.Project(w.ctx)
	draft := `{"spec":{"kind":"project","name":"refund","objective":"Get the refund.",
		"policy":{"tools":{"notify":{"mode":"allow"}},"limits":{"max_llm_spend_usd":2}},
		"completion":{"criteria":[{"description":"Refund received"}]}}}`
	w.setReply(func(req model.Request) model.Message {
		if req.Messages[len(req.Messages)-1].Role == model.RoleTool {
			return model.Message{Content: "Created."}
		}
		return model.Message{ToolCalls: []model.ToolCall{{ID: "p", Name: "propose_assignment", Arguments: json.RawMessage(draft)}}}
	})
	out, err := w.chat("", "-timeout", "20s", "start a refund project")
	if code(err) != 0 || !strings.Contains(out, "[1] PROPOSAL") || !strings.Contains(out, "/approve 1") {
		t.Fatalf("exit %d, output %q", code(err), out)
	}
}
