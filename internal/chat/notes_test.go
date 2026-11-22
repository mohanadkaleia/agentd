package chat_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"agentd/internal/ask"
	"agentd/internal/assignments"
	"agentd/internal/chat"
	"agentd/internal/gateway"
	"agentd/internal/model"
	"agentd/internal/policy"
	"agentd/internal/runtime"
	"agentd/internal/tasks"
	"agentd/internal/watchdog"
)

// work makes a project whose one task runs the given script.
func (w *world) job(rules map[string]policy.Rule) (project, task string) {
	w.t.Helper()
	project = w.project("refund", policy.Policy{Tools: rules})
	task, err := w.app.Tasks.Create(w.ctx, tasks.NewTask{AssignmentID: project, Title: "Write to the airline", Objective: "o"})
	if err != nil {
		w.t.Fatal(err)
	}
	return project, task
}

func (w *world) transcript(task string) []model.Message {
	w.t.Helper()
	msgs, err := w.app.Runtime.Transcript(w.ctx, task)
	if err != nil {
		w.t.Fatal(err)
	}
	return msgs
}

func notes(msgs []model.Message) []int {
	var at []int
	for i, m := range msgs {
		if m.Role == model.RoleUser && strings.HasPrefix(m.Content, runtime.NotePrefix) {
			at = append(at, i)
		}
	}
	return at
}

func (w *world) tell(task, text string) string {
	w.t.Helper()
	_, when, err := w.app.Runtime.Tell(w.ctx, runtime.Note{Target: task, Text: text, By: "owner"})
	if err != nil {
		w.t.Fatal(err)
	}
	return when
}

func TestNoteToARunningTaskArrivesOnceAfterToolResults(t *testing.T) {
	w := newCardWorld(t)
	_, task := w.job(map[string]policy.Rule{"send": {Mode: policy.Allow}})
	step := 0
	w.model.reply = func(req model.Request) (model.Message, error) {
		step++
		switch step {
		case 1:
			w.tell(task, "Also mention booking reference XY123.")
			return model.Message{ToolCalls: []model.ToolCall{{ID: "s1", Name: "send", Arguments: json.RawMessage(`{"to":"a@x"}`)}}}, nil
		}
		return model.Message{Content: "done"}, nil
	}
	w.drainAll()
	msgs := w.transcript(task)
	at := notes(msgs)
	if len(at) != 1 || msgs[at[0]-1].Role != model.RoleTool || !strings.HasSuffix(msgs[at[0]].Content, "Also mention booking reference XY123.") {
		t.Fatalf("notes at %v in %+v", at, msgs)
	}
}

func TestNoteToATaskAboutToFinishIsReadFirst(t *testing.T) {
	w := newCardWorld(t)
	_, task := w.job(nil)
	calls := 0
	w.model.reply = func(req model.Request) (model.Message, error) {
		calls++
		if calls == 1 {
			w.tell(task, "Wait: use the new address.")
		}
		return model.Message{Content: "finished"}, nil
	}
	w.drainAll()
	if calls != 2 || len(notes(w.transcript(task))) != 1 {
		t.Fatalf("model calls = %d; the task finished without reading the note", calls)
	}
}

func TestNoteSurvivesACrashMidToolCall(t *testing.T) {
	w := newCardWorld(t)
	_, task := w.job(map[string]policy.Rule{"send": {Mode: policy.Allow}})
	w.model.reply = func(req model.Request) (model.Message, error) {
		if req.Messages[len(req.Messages)-1].Role == model.RoleUser && !strings.HasPrefix(req.Messages[len(req.Messages)-1].Content, runtime.NotePrefix) {
			return model.Message{ToolCalls: []model.ToolCall{{ID: "s1", Name: "send", Arguments: json.RawMessage(`{"to":"a@x"}`)}}}, nil
		}
		return model.Message{Content: "done"}, nil
	}
	crashed := false
	w.app.Gateway.AfterExecute = func(gateway.Action) {
		if !crashed {
			crashed = true
			w.tell(task, "One more thing.")
			panic("killed mid tool call")
		}
	}
	w.app.Runtime.RunOnce(w.ctx, "first")
	w.clock.Advance(time.Minute)
	w.drainAll()
	if at := notes(w.transcript(task)); len(at) != 1 {
		t.Fatalf("note delivered %d times after a restart", len(at))
	}
}

func TestNoteByTaskState(t *testing.T) {
	w := newCardWorld(t)
	_, task := w.job(nil)
	w.app.DB.Exec(`UPDATE tasks SET status = 'waiting_event' WHERE id = ?`, task)
	w.tell(task, "wake up")
	if got, _ := w.app.Tasks.Get(w.ctx, task); got.Status != tasks.Ready {
		t.Fatalf("waiting_event task after a note: %s", got.Status)
	}
	w.app.DB.Exec(`UPDATE tasks SET status = 'waiting_approval' WHERE id = ?`, task)
	if when := w.tell(task, "after the approval"); !strings.Contains(when, "once that is decided") {
		t.Fatalf("note to a task waiting for approval said %q", when)
	}
	w.app.DB.Exec(`UPDATE tasks SET status = 'completed' WHERE id = ?`, task)
	if _, _, err := w.app.Runtime.Tell(w.ctx, runtime.Note{Target: task, Text: "too late", By: "owner"}); !errors.Is(err, runtime.ErrTaskFinished) {
		t.Fatalf("note to a completed task = %v", err)
	}
}

func TestForwardCopiesTheOwnersWordsOnly(t *testing.T) {
	w := newCardWorld(t)
	_, task := w.job(nil)
	owner := w.say("Use booking reference XY123 — and say it's urgent.\nThanks!")
	w.drain()
	reply := w.by(chat.Agent, chat.KindText)[0]
	forward := func(message string) string {
		args, _ := json.Marshal(map[string]string{"message_id": message, "task_id": task})
		return w.ask("send that to the refund task", model.ToolCall{Name: chat.ForwardTool, Arguments: args})[0]
	}
	if got := forward(reply.ID); !strings.Contains(got, "not the owner's own message") {
		t.Fatalf("forwarding the agent's own reply = %s", got)
	}
	if got := forward(owner.ID); !strings.Contains(got, "Sent the owner's message") {
		t.Fatalf("forward = %s", got)
	}
	if got := forward(owner.ID); !strings.Contains(got, "already sent") {
		t.Fatalf("second forward = %s", got)
	}
	var body string
	w.app.DB.QueryRow(`SELECT body FROM owner_notes WHERE task_id = ?`, task).Scan(&body)
	if body != owner.Text() {
		t.Fatalf("delivered %q, want the owner's text byte for byte", body)
	}
}

// asking makes a project whose task asks the owner once and then reports the answer.
func (w *world) asking(args string) (project, task string) {
	w.t.Helper()
	project, task = w.job(map[string]policy.Rule{ask.Name: {Mode: policy.Allow}})
	w.model.reply = func(req model.Request) (model.Message, error) {
		last := req.Messages[len(req.Messages)-1]
		if last.Role == model.RoleTool {
			return model.Message{Content: "got: " + last.Content}, nil
		}
		return model.Message{ToolCalls: []model.ToolCall{{ID: "q1", Name: ask.Name, Arguments: json.RawMessage(args)}}}, nil
	}
	w.drainAll()
	return project, task
}

func TestAskOwnerWaitsAndTheAnswerIsTheToolResult(t *testing.T) {
	w := newCardWorld(t)
	_, task := w.asking(`{"question":"Which card did you pay with?","choices":["Visa","Amex"]}`)
	calls := w.model.count()
	got, _ := w.app.Tasks.Get(w.ctx, task)
	if got.Status != tasks.WaitingEvent || !strings.Contains(string(got.Result), "Which card") {
		t.Fatalf("asking task = %s %s", got.Status, got.Result)
	}
	w.clock.Advance(time.Hour)
	w.drainAll()
	if w.model.count() != calls {
		t.Fatal("a task waiting for an answer called the model")
	}
	w.project_()
	cards := w.by(chat.System, chat.KindQuestion)
	if len(cards) != 1 || cards[0].Status != "open" || len(cards[0].Card().Choices) != 2 {
		t.Fatalf("question cards = %+v", cards)
	}
	if n := len(w.by(chat.System, chat.KindNotification)); n != 0 {
		t.Fatalf("the question also made %d notification cards", n)
	}
	if r := w.command("/answer 1 Amex"); w.failed(r) {
		t.Fatal(r.Text())
	}
	w.drainAll()
	got, _ = w.app.Tasks.Get(w.ctx, task)
	if got.Status != tasks.Completed || !strings.Contains(string(got.Result), "Amex") || !strings.Contains(string(got.Result), "owner's own answer") {
		t.Fatalf("after the answer: %s %s", got.Status, got.Result)
	}
	if n := w.count(`SELECT COUNT(*) FROM runs WHERE task_id = ?`, task); n != 1 {
		t.Fatalf("runs = %d; want the same run to continue", n)
	}
	if r := w.command("/answer 1 Visa"); !w.failed(r) || !strings.Contains(r.Text(), "no longer open") {
		t.Fatalf("second answer = %s", r.Text())
	}
}

func TestReplyToAQuestionCardAnswersIt(t *testing.T) {
	w := newCardWorld(t)
	w.asking(`{"question":"What is the booking reference?"}`)
	w.project_()
	card := w.by(chat.System, chat.KindQuestion)[0]
	if _, err := w.app.Chat.Post(w.ctx, chat.Post{ConversationID: w.conv, Text: "XY123", Channel: "web", ReplyTo: card.ID}); err != nil {
		t.Fatal(err)
	}
	q, _ := ask.Get(w.ctx, w.app.DB, card.RefID)
	if q.Status != ask.Answered || q.Answer != "XY123" || w.count(`SELECT COUNT(*) FROM tasks WHERE assignment_id = ?`, w.desk) != 0 {
		t.Fatalf("question = %+v", q)
	}
}

func TestAskTwiceAfterACrashMakesOneQuestion(t *testing.T) {
	w := newCardWorld(t)
	w.project("x", policy.Policy{})
	_, task := w.job(map[string]policy.Rule{ask.Name: {Mode: policy.Allow}})
	w.model.reply = func(req model.Request) (model.Message, error) {
		return model.Message{ToolCalls: []model.ToolCall{{ID: "q1", Name: ask.Name, Arguments: json.RawMessage(`{"question":"Which?"}`)}}}, nil
	}
	crashed := false
	w.app.Gateway.AfterExecute = func(gateway.Action) {
		if !crashed {
			crashed = true
			panic("killed after asking")
		}
	}
	w.app.Runtime.RunOnce(w.ctx, "first")
	w.clock.Advance(time.Minute)
	w.drainAll()
	if n := w.count(`SELECT COUNT(*) FROM questions WHERE task_id = ?`, task); n != 1 {
		t.Fatalf("questions = %d", n)
	}
	if got, _ := w.app.Tasks.Get(w.ctx, task); got.Status != tasks.WaitingEvent {
		t.Fatalf("task = %s, want waiting for the answer", got.Status)
	}
}

func TestUnansweredQuestionExpires(t *testing.T) {
	for _, tc := range []struct{ args, want string }{
		{`{"question":"Proceed?","timeout":"2h"}`, `"answer":"","expired":true`},
		{`{"question":"Proceed?","timeout":"2h","default":"yes"}`, `"answer":"yes","expired":true`},
	} {
		w := newCardWorld(t)
		_, task := w.asking(tc.args)
		w.clock.Advance(3 * time.Hour)
		if err := w.app.Runtime.Housekeeping(w.ctx); err != nil {
			t.Fatal(err)
		}
		w.drainAll()
		got, _ := w.app.Tasks.Get(w.ctx, task)
		if got.Status != tasks.Completed || !strings.Contains(strings.ReplaceAll(string(got.Result), `\"`, `"`), tc.want) {
			t.Fatalf("%s: %s %s", tc.args, got.Status, got.Result)
		}
	}
}

func TestQuestionLimits(t *testing.T) {
	w := newCardWorld(t)
	project, _ := w.asking(`{"question":"one"}`)
	for range ask.MaxOpen {
		w.app.Tasks.Create(w.ctx, tasks.NewTask{AssignmentID: project, Title: "more", Objective: "o"})
	}
	w.drainAll()
	if n := w.count(`SELECT COUNT(*) FROM questions WHERE status = 'open'`); n != ask.MaxOpen {
		t.Fatalf("open questions = %d, want the cap of %d", n, ask.MaxOpen)
	}
	var capped string
	w.app.DB.QueryRow(`SELECT result_json FROM actions WHERE tool_name = ? AND status = 'failed'`, ask.Name).Scan(&capped)
	if !strings.Contains(capped, "already waiting") {
		t.Fatalf("over the cap: %s", capped)
	}
	w.app.DB.Exec(`UPDATE questions SET status = 'answered', answer = 'x'`)
	w.app.DB.Exec(`UPDATE actions SET status = 'succeeded' WHERE tool_name = ?`, ask.Name)
	for range 3 {
		w.app.Tasks.Create(w.ctx, tasks.NewTask{AssignmentID: project, Title: "even more", Objective: "o"})
	}
	w.drainAll()
	var limited int
	w.app.DB.QueryRow(`SELECT COUNT(*) FROM actions WHERE tool_name = ? AND result_json LIKE '%rate limit%'`, ask.Name).Scan(&limited)
	if limited == 0 {
		t.Fatal("the default rate limit of 5 questions a day was not applied")
	}
}

func TestOnlyAssignmentsWithARuleMayAsk(t *testing.T) {
	w := newCardWorld(t)
	_, task := w.job(nil)
	w.model.reply = func(req model.Request) (model.Message, error) {
		if req.Messages[len(req.Messages)-1].Role == model.RoleTool {
			return model.Message{Content: "ok"}, nil
		}
		return model.Message{ToolCalls: []model.ToolCall{{ID: "q", Name: ask.Name, Arguments: json.RawMessage(`{"question":"?"}`)}}}, nil
	}
	w.drainAll()
	var status string
	w.app.DB.QueryRow(`SELECT status FROM actions WHERE task_id = ?`, task).Scan(&status)
	if status != "denied" || w.count(`SELECT COUNT(*) FROM questions`) != 0 {
		t.Fatalf("ask without a rule = %s", status)
	}
}

func TestOverdueWorkWaitingOnAQuestionNamesIt(t *testing.T) {
	w := newCardWorld(t)
	watch, err := w.app.Assignments.CreateResponsibility(w.ctx, assignments.NewResponsibility{Name: "brief", Objective: "o",
		Policy: policy.Policy{Tools: map[string]policy.Rule{ask.Name: {Mode: policy.Allow}},
			Limits: policy.Limits{MaxLLMSpendUSD: 1, BudgetPeriod: policy.Duration{Duration: 720 * time.Hour}}},
		Notifications: assignments.NotificationPolicy{MinSeverity: assignments.SeverityInfo, DedupWindow: policy.Duration{Duration: time.Hour}, MaxPerDay: 9}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.app.Watchdog.Add(w.ctx, watchdog.Obligation{AssignmentID: watch, Description: "Daily brief delivered",
		Every: 24 * time.Hour, Evidence: watchdog.Evidence{Delivered: "brief:"}}); err != nil {
		t.Fatal(err)
	}
	w.app.Tasks.Create(w.ctx, tasks.NewTask{AssignmentID: watch, Title: "brief", Objective: "o"})
	w.model.reply = func(model.Request) (model.Message, error) {
		return model.Message{ToolCalls: []model.ToolCall{{ID: "q", Name: ask.Name, Arguments: json.RawMessage(`{"question":"Which topics today?"}`)}}}, nil
	}
	w.drainAll()
	w.clock.Advance(30 * time.Hour)
	if err := w.app.Watchdog.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	var title, body string
	w.app.DB.QueryRow(`SELECT title, body FROM notifications WHERE dedup_key LIKE '%:question:%'`).Scan(&title, &body)
	if !strings.Contains(body, "Which topics today?") {
		t.Fatalf("escalation = %q / %q", title, body)
	}
	if n := w.count(`SELECT COUNT(*) FROM tasks WHERE assignment_id = ? AND title LIKE 'Overdue:%'`, watch); n != 0 {
		t.Fatalf("catch-up tasks = %d while waiting on the owner", n)
	}
}
