package chat_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"strings"
	"sync"
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
	"agentd/migrations"
)

type provider struct {
	mu       sync.Mutex
	reply    func(req model.Request) (model.Message, error)
	cost     int64
	calls    int
	requests []model.Request
}

func (p *provider) Complete(_ context.Context, req model.Request) (model.Response, error) {
	p.mu.Lock()
	p.calls++
	p.requests = append(p.requests, req)
	reply, cost := p.reply, p.cost
	p.mu.Unlock()
	msg := model.Message{Content: "ok"}
	if reply != nil {
		var err error
		if msg, err = reply(req); err != nil {
			return model.Response{}, err
		}
	}
	return model.Response{Message: msg, StopReason: model.StopEnd, Usage: model.Usage{CostMicros: cost}}, nil
}

func (p *provider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *provider) last() model.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests[len(p.requests)-1]
}

type world struct {
	t       *testing.T
	ctx     context.Context
	clock   *testutil.Clock
	app     *app.App
	model   *provider
	send    *sendTool
	conv    string
	chatAsg string
}

func newWorld(t *testing.T) *world {
	w := &world{t: t, ctx: context.Background(), clock: testutil.NewClock(), model: &provider{}, send: &sendTool{}}
	db := testutil.Open(t, testutil.DBPath(t))
	w.app = app.New(db, w.model, "cloud", tools.NewRegistry(w.send), w.clock.Now)
	var err error
	if w.chatAsg, err = w.app.Chat.ChatAssignment(w.ctx); err != nil {
		t.Fatal(err)
	}
	if w.conv, err = w.app.Chat.Default(w.ctx); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *world) say(text string) chat.Message {
	w.t.Helper()
	m, err := w.app.Chat.Post(w.ctx, chat.Post{ConversationID: w.conv, Text: text, Channel: "cli"})
	if err != nil {
		w.t.Fatal(err)
	}
	return m
}

func (w *world) work() bool {
	w.t.Helper()
	worked, err := w.app.Runtime.RunOnce(w.ctx, "worker")
	if err != nil {
		w.t.Fatal(err)
	}
	return worked
}

// drain runs turns until none is ready; drainAll also runs background work such as summaries.
func (w *world) drain() {
	for {
		worked, err := w.app.Runtime.RunOnceInteractive(w.ctx, "interactive")
		if err != nil {
			w.t.Fatal(err)
		}
		if !worked {
			return
		}
	}
}

func (w *world) drainAll() {
	for w.work() {
	}
}

func (w *world) thread() []chat.Message {
	w.t.Helper()
	msgs, err := w.app.Chat.Messages(w.ctx, w.conv, 0, 1000)
	if err != nil {
		w.t.Fatal(err)
	}
	return msgs
}

func (w *world) by(author, kind string) []chat.Message {
	var out []chat.Message
	for _, m := range w.thread() {
		if m.Author == author && m.Kind == kind {
			out = append(out, m)
		}
	}
	return out
}

func (w *world) count(query string, args ...any) int {
	w.t.Helper()
	var n int
	if err := w.app.DB.QueryRowContext(w.ctx, query, args...).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

func TestSameClientMessageMakesOneTurn(t *testing.T) {
	w := newWorld(t)
	first, err := w.app.Chat.Post(w.ctx, chat.Post{ConversationID: w.conv, Text: "hello", Channel: "web", ClientMsgID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := w.app.Chat.Post(w.ctx, chat.Post{ConversationID: w.conv, Text: "hello", Channel: "web", ClientMsgID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID || again.Seq != first.Seq {
		t.Fatalf("retried post returned %+v, want the stored %+v", again, first)
	}
	if n := w.count(`SELECT COUNT(*) FROM tasks WHERE assignment_id = ?`, w.chatAsg); n != 1 {
		t.Fatalf("turns = %d, want 1", n)
	}
	w.drain()
	if replies := w.by(chat.Agent, chat.KindText); len(replies) != 1 || replies[0].Text() != "ok" {
		t.Fatalf("replies = %+v, want one", replies)
	}
}

func TestReplyWrittenOnceWhenTheProcessDiesBeforeCommitting(t *testing.T) {
	w := newWorld(t)
	w.say("how is it going?")
	crash := true
	hook := w.app.Tasks.OnRelease
	w.app.Tasks.OnRelease = func(ctx context.Context, tx *sql.Tx, r tasks.Released) error {
		if crash {
			crash = false
			return errors.New("process killed")
		}
		return hook(ctx, tx, r)
	}
	if _, err := w.app.Runtime.RunOnce(w.ctx, "first"); err == nil {
		t.Fatal("the release that was killed reported success")
	}
	if n := len(w.by(chat.Agent, chat.KindText)); n != 0 {
		t.Fatalf("a reply was committed before the turn ended: %d", n)
	}
	w.clock.Advance(time.Minute)
	w.drain()
	if n := len(w.by(chat.Agent, chat.KindText)); n != 1 {
		t.Fatalf("replies after restart = %d, want exactly 1", n)
	}
	if w.model.count() != 1 {
		t.Fatalf("model calls = %d, want the run resumed (1 call), not restarted", w.model.count())
	}
	if n := w.count(`SELECT COUNT(*) FROM runs r JOIN tasks t ON t.id = r.task_id WHERE t.assignment_id = ?`, w.chatAsg); n != 1 {
		t.Fatalf("runs = %d, want 1", n)
	}
}

func TestMessagesDuringATurnJoinTheNextTurn(t *testing.T) {
	w := newWorld(t)
	w.say("first")
	w.model.reply = func(req model.Request) (model.Message, error) {
		if w.model.calls == 1 {
			for _, text := range []string{"second", "third", "fourth"} {
				w.say(text)
			}
			if worked, err := w.app.Runtime.RunOnce(w.ctx, "second-worker"); worked || err != nil {
				t.Errorf("a second worker ran a turn while the first was running: %v, %v", worked, err)
			}
		}
		return model.Message{Content: fmt.Sprintf("reply %d", w.model.calls)}, nil
	}
	w.work()
	if n := w.count(`SELECT COUNT(*) FROM tasks WHERE assignment_id = ?`, w.chatAsg); n != 2 {
		t.Fatalf("turns = %d, want the three later messages in one following turn", n)
	}
	w.drain()
	var owner []string
	for _, m := range w.model.last().Messages {
		if m.Role == model.RoleUser && !strings.Contains(m.Content, "Situation") && !strings.HasPrefix(m.Content, "[ids of") {
			owner = append(owner, m.Content)
		}
	}
	if got := strings.Join(owner, ","); got != "first,second,third,fourth" {
		t.Fatalf("the second turn saw %q, want all messages in order", got)
	}
	if w.model.count() != 2 {
		t.Fatalf("model calls = %d, want 2 (no parallel turn)", w.model.count())
	}
	if replies := w.by(chat.Agent, chat.KindText); len(replies) != 2 || replies[1].Seq < 5 {
		t.Fatalf("replies = %+v", replies)
	}
}

func TestEmergencyStopKeepsMessagesAndMakesNoModelCall(t *testing.T) {
	w := newWorld(t)
	system.Halt(w.ctx, w.app.DB, w.clock.Now(), "owner", "testing")
	w.say("are you there?")
	w.say("hello?")
	w.drain()
	if w.model.count() != 0 {
		t.Fatalf("model calls while halted = %d", w.model.count())
	}
	notices := w.by(chat.System, chat.KindNotice)
	if len(notices) != 1 || !strings.Contains(notices[0].Text(), "agentd unhalt") {
		t.Fatalf("notices = %+v, want one explaining the stop", notices)
	}
	system.Resume(w.ctx, w.app.DB, w.clock.Now(), "owner")
	w.drain()
	if n := len(w.by(chat.Agent, chat.KindText)); n != 1 || w.model.count() != 1 {
		t.Fatalf("after the stop: %d replies, %d calls; want 1 and 1", n, w.model.count())
	}
}

func TestUsedUpChatBudgetStopsRepliesButNotOtherWork(t *testing.T) {
	w := newWorld(t)
	other, err := w.app.Assignments.CreateProject(w.ctx, assignments.NewProject{Name: "refund", Objective: "o",
		Policy: policy.Policy{Limits: policy.Limits{MaxLLMSpendUSD: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	w.model.cost = 6_000_000
	w.say("hello")
	w.drain()
	w.say("and again")
	w.drain()
	if err := w.app.Chat.Tick(w.ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.app.Chat.Tick(w.ctx); err != nil {
		t.Fatal(err)
	}
	if w.model.count() != 1 {
		t.Fatalf("model calls = %d, want none after the budget ran out", w.model.count())
	}
	notices := w.by(chat.System, chat.KindNotice)
	if len(notices) != 1 || !strings.Contains(notices[0].Text(), "budget") || !strings.Contains(notices[0].Text(), "agentd policy") {
		t.Fatalf("notices = %+v, want one saying the budget is used up and how to raise it", notices)
	}
	a, _ := w.app.Assignments.Get(w.ctx, other)
	b, _ := w.app.Runtime.Budget(w.ctx, a)
	if a.Status != assignments.Active || b.SpentMicros != 0 {
		t.Fatalf("other assignment: %s, spent %d; want untouched", a.Status, b.SpentMicros)
	}
}

func TestFailedTurnSaysWhatHappened(t *testing.T) {
	w := newWorld(t)
	w.model.reply = func(model.Request) (model.Message, error) { return model.Message{}, errors.New("connection refused") }
	w.say("hello")
	for range 3 {
		w.app.Runtime.RunOnce(w.ctx, "worker")
		w.clock.Advance(time.Hour)
	}
	errs := w.by(chat.System, chat.KindError)
	if len(errs) != 1 || !strings.Contains(errs[0].Text(), "could not reach the model") || !strings.Contains(errs[0].Text(), "agentd retry") {
		t.Fatalf("errors = %+v, want one plain explanation", errs)
	}
}

func TestCancelStopsTheTurnAndTheNextOneRuns(t *testing.T) {
	w := newWorld(t)
	w.say("do something long")
	if err := w.app.Chat.Cancel(w.ctx, w.conv, "owner"); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT COUNT(*) FROM tasks WHERE assignment_id = ? AND status = 'cancelled'`, w.chatAsg); n != 1 {
		t.Fatalf("cancelled turns = %d", n)
	}
	if notices := w.by(chat.System, chat.KindNotice); len(notices) != 1 || !strings.Contains(notices[0].Text(), "Stopped") {
		t.Fatalf("notices = %+v", notices)
	}
	if err := w.app.Chat.Cancel(w.ctx, w.conv, "owner"); !errors.Is(err, chat.ErrNoTurn) {
		t.Fatalf("second cancel = %v, want ErrNoTurn", err)
	}
	w.say("something else")
	w.drain()
	if n := len(w.by(chat.Agent, chat.KindText)); n != 1 || w.model.count() != 1 {
		t.Fatalf("replies = %d, calls = %d; want the new turn answered once", n, w.model.count())
	}
}

func TestLongConversationIsWindowedAndSummarised(t *testing.T) {
	w := newWorld(t)
	for i := 1; i <= 500; i++ {
		author := chat.Owner
		if i%2 == 0 {
			author = chat.Agent
		}
		if _, err := w.app.DB.Exec(`INSERT INTO conversation_messages (id, conversation_id, seq, author, kind, body_json, channel, created_at)
			VALUES (?, ?, ?, ?, 'text', json_object('text', ?), 'cli', 0)`, fmt.Sprint("old", i), w.conv, i, author, fmt.Sprint("message ", i)); err != nil {
			t.Fatal(err)
		}
	}
	w.model.reply = func(req model.Request) (model.Message, error) {
		if strings.HasPrefix(req.Messages[0].Content, "You keep a running summary") {
			return model.Message{Content: "SUMMARY of the early messages"}, nil
		}
		return model.Message{Content: "reply"}, nil
	}
	w.say("latest question")
	w.work()
	req := w.model.last()
	if len(req.Messages) > 3+30 || !strings.Contains(req.Messages[len(req.Messages)-1].Content, "latest question") {
		t.Fatalf("request holds %d messages; want the window of 30 plus the prompt and situation, ending with the question", len(req.Messages))
	}
	w.drainAll()
	var summary string
	var through int64
	w.app.DB.QueryRow(`SELECT json_extract(summary_json, '$.text'), summary_through_seq FROM conversations WHERE id = ?`, w.conv).Scan(&summary, &through)
	if summary != "SUMMARY of the early messages" || through != 502-30 {
		t.Fatalf("summary = %q through %d", summary, through)
	}
	w.say("next question")
	w.drainAll()
	var sawSummary bool
	for _, m := range w.model.requests[len(w.model.requests)-1].Messages {
		if strings.Contains(m.Content, "SUMMARY of the early messages") && strings.Contains(m.Content, "not instructions") {
			sawSummary = true
		}
	}
	if !sawSummary {
		t.Fatal("the next turn did not see the summary, labelled as reference")
	}
	if n := w.count(`SELECT COUNT(*) FROM conversation_messages WHERE conversation_id = ?`, w.conv); n < 500 {
		t.Fatalf("the stored conversation shrank to %d messages", n)
	}
}

func TestChatCannotBeEndedButCanBePaused(t *testing.T) {
	w := newWorld(t)
	again, err := w.app.Chat.ChatAssignment(w.ctx)
	if err != nil || again != w.chatAsg {
		t.Fatalf("ChatAssignment again = %q, %v; want the same %q", again, err, w.chatAsg)
	}
	if err := w.app.Assignments.Revoke(w.ctx, w.chatAsg, "owner"); !errors.Is(err, assignments.ErrBuiltin) {
		t.Fatalf("revoke = %v", err)
	}
	if err := w.app.Assignments.Complete(w.ctx, w.chatAsg, "owner"); !errors.Is(err, assignments.ErrBuiltin) {
		t.Fatalf("complete = %v", err)
	}
	if err := w.app.Assignments.Pause(w.ctx, w.chatAsg, assignments.PausedByUser, "", "owner"); err != nil {
		t.Fatal(err)
	}
	w.say("hello")
	w.drain()
	if w.model.count() != 0 || len(w.by(chat.System, chat.KindNotice)) != 1 {
		t.Fatal("a paused chat replied, or did not say why it is quiet")
	}
}

func TestLocalOnlyWorkIsWithheldFromACloudModel(t *testing.T) {
	w := newWorld(t)
	if _, err := w.app.Assignments.CreateProject(w.ctx, assignments.NewProject{Name: "medical", Objective: "private",
		Policy: policy.Policy{Models: policy.ModelPolicy{LocalOnly: true}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.app.Assignments.CreateProject(w.ctx, assignments.NewProject{Name: "refund", Objective: "o"}); err != nil {
		t.Fatal(err)
	}
	situation := func() string {
		for _, m := range w.model.last().Messages {
			if strings.HasPrefix(m.Content, "Situation") {
				return m.Content
			}
		}
		return ""
	}
	w.say("what is going on?")
	w.drain()
	got := situation()
	if !strings.Contains(got, `"medical"`) || !strings.Contains(got, "details withheld") || !strings.Contains(got, `"refund"`) ||
		strings.Count(got, "next:") != 1 {
		t.Fatalf("cloud situation:\n%s", got)
	}
	w.app.Runtime.ModelLocal = true
	w.say("and now?")
	w.drain()
	if got := situation(); strings.Contains(got, "withheld") || strings.Count(got, "next:") != 2 {
		t.Fatalf("local situation:\n%s", got)
	}
}

func TestTurnIsClaimedQuicklyWhileOrdinaryWorkersAreBusy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	defer close(release)
	p := &provider{reply: func(req model.Request) (model.Message, error) {
		if !strings.HasPrefix(req.Messages[0].Content, "You are agentd, the owner's personal agent") {
			<-release
		}
		return model.Message{Content: "ok"}, nil
	}}
	db := testutil.Open(t, testutil.DBPath(t))
	a := app.New(db, p, "cloud", tools.Registry{}, time.Now)
	conv, err := a.Chat.Default(ctx)
	if err != nil {
		t.Fatal(err)
	}
	project, err := a.Assignments.CreateProject(ctx, assignments.NewProject{Name: "long", Objective: "o"})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if _, err := a.Tasks.Create(ctx, tasks.NewTask{AssignmentID: project, Title: fmt.Sprint("long ", i), Objective: "o"}); err != nil {
			t.Fatal(err)
		}
	}
	go a.Runtime.Serve(ctx, "test", 3, time.Second, quietLog())
	waitFor(t, 3*time.Second, func() bool {
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE assignment_id = ? AND status = 'running'`, project).Scan(&n)
		return n == 3
	})
	claimed := func(id string) time.Duration {
		start := time.Now()
		waitFor(t, 2*time.Second, func() bool {
			var n int
			db.QueryRow(`SELECT COUNT(*) FROM runs WHERE task_id = ?`, id).Scan(&n)
			return n == 1
		})
		return time.Since(start)
	}
	m, err := a.Chat.Post(ctx, chat.Post{ConversationID: conv, Text: "fast?", Channel: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if d := claimed(m.TurnTaskID); d > slack*100*time.Millisecond {
		t.Errorf("turn posted in this process claimed after %s, want within %s", d, slack*100*time.Millisecond)
	}
	waitFor(t, 2*time.Second, func() bool {
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM conversation_messages WHERE author = 'agent'`).Scan(&n)
		return n == 1
	})
	other := &chat.Service{DB: a.DB, Tasks: a.Tasks, Assignments: a.Assignments, Runtime: a.Runtime, Now: time.Now}
	m, err = other.Post(ctx, chat.Post{ConversationID: conv, Text: "from another process", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	if d := claimed(m.TurnTaskID); d > slack*400*time.Millisecond {
		t.Errorf("turn written by another process claimed after %s, want within %s", d, slack*400*time.Millisecond)
	}
}

func waitFor(t *testing.T, limit time.Duration, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s", limit)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestWindowIsCappedByBytesButKeepsTheQuestion(t *testing.T) {
	w := newWorld(t)
	w.app.Chat.WindowBytes = 1000
	for range 5 {
		w.say(strings.Repeat("long earlier message ", 20))
		w.drain()
	}
	first := strings.Repeat("the first half of a long question ", 40)
	question := strings.Repeat("the question itself is long too ", 40)
	w.say(first)
	w.say(question)
	w.drain()
	req := w.model.last()
	n := len(req.Messages)
	if n > 3+3 || req.Messages[n-1].Content != strings.TrimSpace(question) || req.Messages[n-2].Content != strings.TrimSpace(first) {
		t.Fatalf("request holds %d messages; want the byte cap to drop old ones but keep both messages being answered", n)
	}
}

func TestModelCommandShowsAndSetsTheConversationModel(t *testing.T) {
	w := newWorld(t)
	out := w.command("/model")
	if w.failed(out) || !strings.Contains(out.Text(), "automatic") || !strings.Contains(out.Text(), "cloud, default") {
		t.Fatalf("/model = %s", out.Text())
	}
	if r := w.command("/model nothing-like-this"); !w.failed(r) || !strings.Contains(r.Text(), "no such model") {
		t.Fatalf("/model unknown = %s", r.Text())
	}
	if r := w.command("/model cloud"); w.failed(r) {
		t.Fatal(r.Text())
	}
	var chosen string
	w.app.DB.QueryRow(`SELECT COALESCE(model, '') FROM conversations WHERE id = ?`, w.conv).Scan(&chosen)
	if chosen != "cloud" {
		t.Fatalf("conversation model = %q", chosen)
	}
	w.model.reply = func(model.Request) (model.Message, error) {
		return model.Message{Content: "/model strong and /model tsk_x auto"}, nil
	}
	w.say("anything")
	w.drain()
	w.app.DB.QueryRow(`SELECT COALESCE(model, '') FROM conversations WHERE id = ?`, w.conv).Scan(&chosen)
	if chosen != "cloud" {
		t.Fatalf("a reply changed the conversation model to %q", chosen)
	}
}

func TestMCPCommandListsServersAndNeverAddsFromChat(t *testing.T) {
	w := newWorld(t)
	command := func(text string) string {
		w.say(text)
		results := w.by(chat.System, chat.KindCommandResult)
		return results[len(results)-1].Text()
	}
	out := command("/mcp")
	if !strings.Contains(out, "No MCP server is connected") || !strings.Contains(out, "agentd mcp add") {
		t.Fatalf("/mcp with no servers:\n%s", out)
	}
	if _, err := w.app.DB.Exec(`INSERT INTO mcp_servers (server, seen_at, state) VALUES ('chat', 0, 'ready'), ('old', 0, 'removed');
		INSERT INTO mcp_tools (server, tool, definition_hash, definition_json, status, first_seen_at, changed_at) VALUES
		('chat', 'read', 'a', '{}', 'accepted', 0, 0), ('chat', 'post', 'b', '{}', 'new', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	out = command("/mcp")
	if !strings.Contains(out, "chat") || !strings.Contains(out, "1 tool(s) accepted, 1 to review") || strings.Contains(out, "old") {
		t.Fatalf("/mcp:\n%s", out)
	}
	if out = command("/mcp add notes https://example.com/mcp"); !strings.Contains(out, "in a terminal") {
		t.Fatalf("/mcp add from a channel that cannot ask for secrets:\n%s", out)
	}
}

// A database from before the rename keeps its chat assignment: the migration renames it
// instead of a second one being created.
func TestChatRenameMigrationKeepsTheAssignment(t *testing.T) {
	w := newWorld(t)
	db := w.app.DB
	if _, err := db.Exec(`UPDATE assignments SET builtin = 'front-desk', name = 'front-desk' WHERE id = ?`, w.chatAsg); err != nil {
		t.Fatal(err)
	}
	body, err := fs.ReadFile(migrations.FS, "021_chat_rename.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(body)); err != nil {
		t.Fatal(err)
	}
	again, err := w.app.Chat.ChatAssignment(w.ctx)
	if err != nil || again != w.chatAsg {
		t.Fatalf("ChatAssignment after the migration = %q, %v; want the same %q", again, err, w.chatAsg)
	}
	var name string
	var n int
	db.QueryRow(`SELECT name FROM assignments WHERE id = ?`, w.chatAsg).Scan(&name)
	db.QueryRow(`SELECT COUNT(*) FROM assignments WHERE builtin IS NOT NULL`).Scan(&n)
	if name != "chat" || n != 1 {
		t.Fatalf("after the migration: name %q, %d built-in assignments; want chat, 1", name, n)
	}
}
