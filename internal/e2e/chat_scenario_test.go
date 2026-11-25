package e2e_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"agentd/internal/app"
	"agentd/internal/ask"
	"agentd/internal/chat"
	"agentd/internal/chat/chattest"
	"agentd/internal/gateway"
	"agentd/internal/model"
	"agentd/internal/policy"
	"agentd/internal/scheduler"
	"agentd/internal/tasks"
	"agentd/internal/tools"
)

const refunds = "refunds@airline.example"

var (
	projectID = regexp.MustCompile(`prj_[0-9a-f]+`)
	ownerIDs  = regexp.MustCompile(`for forward_owner_message: (.*)\]`)
)

// called lists the tools a task's model has called so far, in order.
func called(req model.Request) []string {
	var out []string
	for _, m := range req.Messages {
		for _, c := range m.ToolCalls {
			out = append(out, c.Name)
		}
	}
	return out
}

func seen(req model.Request, text string) bool {
	for _, m := range req.Messages {
		if strings.Contains(m.Content, text) {
			return true
		}
	}
	return false
}

var refundProposal = map[string]any{
	"spec": map[string]any{
		"kind": "project", "name": "lh123-refund", "objective": "Get a refund for cancelled flight LH123.",
		"policy": map[string]any{
			"tools": map[string]any{
				"send_email": map[string]any{"mode": "approval", "resources": []string{refunds}, "otherwise": "deny"},
				"ask_owner":  map[string]string{"mode": "allow"}, "notify": map[string]string{"mode": "allow"},
			},
			"limits": map[string]any{"max_llm_spend_usd": 3},
		},
		"completion": map[string]any{"criteria": []any{map[string]any{"id": "refund", "description": "Refund posted to my card",
			"event": map[string]any{"source": "bank", "kind": "transaction.posted", "match": map[string]string{"merchant": "Airline"}}}}},
	},
	"first_task": map[string]any{"title": "Ask the airline for the refund", "objective": "Email the refunds desk and get the refund."},
}

// owner's chat, scripted as a dialogue keyed by what the owner said.
func chatTurn(owner string, req model.Request) model.Message {
	result, afterTool := chattest.LastTool(req)
	switch {
	case strings.Contains(owner, "refund for my cancelled flight"):
		if !afterTool {
			return chattest.Call("p", "propose_assignment", refundProposal)
		}
		return model.Message{Content: "I drafted the project. Approve the proposal card to start."}
	case strings.Contains(owner, "how is it going"):
		if !afterTool {
			var situation string
			for _, m := range req.Messages {
				if strings.HasPrefix(m.Content, "Situation") {
					situation = m.Content
				}
			}
			return chattest.Call("g", "get_assignment", map[string]string{"id": projectID.FindString(situation)})
		}
		var out struct {
			Result struct {
				Next string `json:"next"`
			} `json:"result"`
		}
		json.Unmarshal([]byte(result), &out)
		return model.Message{Content: "It is in hand. Next: " + out.Result.Next}
	case strings.Contains(owner, "booking reference XY123"):
		if !afterTool {
			var ids []string
			var project string
			for _, m := range req.Messages {
				if g := ownerIDs.FindStringSubmatch(m.Content); g != nil {
					ids = strings.Split(g[1], ", ")
				}
				if strings.HasPrefix(m.Content, "Situation") {
					project = projectID.FindString(m.Content)
				}
			}
			return chattest.Call("f", chat.ForwardTool, map[string]string{"message_id": ids[len(ids)-1], "assignment_id": project})
		}
		return model.Message{Content: "I passed that on to the refund task, word for word."}
	}
	return model.Message{Content: "Noted."}
}

// the refund task: email, ask about a voucher, follow up with what the owner added, wait.
func refundTask(req model.Request) model.Message {
	done := called(req)
	sends := strings.Count(strings.Join(done, ","), "send_email")
	switch {
	case len(done) == 0:
		return chattest.Call("e1", "send_email", map[string]string{"to": refunds, "subject": "Refund for LH123", "body": "Please refund my cancelled flight LH123."})
	case sends == 1 && !strings.Contains(strings.Join(done, ","), ask.Name):
		return chattest.Call("q1", ask.Name, map[string]any{"question": "The airline may offer a voucher. Accept a voucher instead of a refund?", "choices": []string{"Yes", "No"}})
	case sends == 1 && seen(req, "booking reference XY123"):
		return chattest.Call("e2", "send_email", map[string]string{"to": refunds, "subject": "Re: Refund for LH123",
			"body": "Booking reference XY123. I do not accept a voucher; please refund to my card."})
	}
	return model.Message{Content: "Asked for the refund twice; waiting for the airline."}
}

func TestAWeekOfWorkingTogetherThroughChat(t *testing.T) {
	crashProposal, crashTurn := false, false
	w := chattest.New(t, func(a *app.App) []tools.Tool {
		mailbox, err := tools.NewMailbox(context.Background(), a.DB, 0)
		if err != nil {
			t.Fatal(err)
		}
		return []tools.Tool{mailbox}
	}, func(w *chattest.World) {
		w.App.Gateway.AfterExecute = func(a gateway.Action) {
			if a.ToolName == "propose_assignment" && crashProposal {
				crashProposal = false
				panic("killed after the assignment was written")
			}
		}
		hook := w.App.Tasks.OnRelease
		w.App.Tasks.OnRelease = func(ctx context.Context, tx *sql.Tx, r tasks.Released) error {
			if crashTurn && r.Status == tasks.Completed {
				crashTurn = false
				return errKilled
			}
			return hook(ctx, tx, r)
		}
	})
	w.Agent.Turn, w.Agent.Work = chatTurn, refundTask

	// Day 0: a sentence becomes a proposal; the owner approves; a crash in between.
	w.Say("Please get me a refund for my cancelled flight LH123.")
	w.Settle()
	proposals := w.Of(chat.KindProposal)
	if len(proposals) != 1 || w.Count(`SELECT COUNT(*) FROM assignments WHERE builtin IS NULL`) != 0 {
		var status, result string
		w.App.DB.QueryRow(`SELECT status, COALESCE(result_json, '') FROM actions WHERE tool_name = 'propose_assignment'`).Scan(&status, &result)
		t.Logf("propose_assignment: %s %s", status, result)
		t.Fatalf("after the request: %d proposals, assignments exist too early", len(proposals))
	}
	crashProposal = true
	if out, ok := w.Command("/approve 1"); !ok {
		t.Fatal(out)
	}
	w.Settle()
	w.Restart()
	w.Agent.Turn, w.Agent.Work = chatTurn, refundTask
	w.Settle()
	if n := w.Count(`SELECT COUNT(*) FROM assignments WHERE builtin IS NULL`); n != 1 {
		t.Fatalf("assignments after the crash = %d, want exactly one", n)
	}
	var project string
	w.App.DB.QueryRow(`SELECT id FROM assignments WHERE builtin IS NULL`).Scan(&project)

	// The first email waits for an approval decided from the thread.
	approvals := w.Of(chat.KindApproval)
	if len(approvals) != 1 || approvals[0].Status != "pending" {
		t.Fatalf("approval cards = %+v", approvals)
	}
	if out, ok := w.Command("/approve 2"); !ok {
		t.Fatal(out)
	}
	w.Settle()
	questions := w.Of(chat.KindQuestion)
	if len(questions) != 1 || questions[0].Status != "open" {
		t.Fatalf("question cards = %+v", questions)
	}

	// Day 1: how is it going? The reply quotes the overview's next step; a crash mid-turn.
	w.Advance(24 * time.Hour)
	workCalls := w.Agent.Calls("work")
	crashTurn = true
	w.Say("Hi, how is it going with the refund?")
	w.Settle()
	w.Restart()
	w.Agent.Turn, w.Agent.Work = chatTurn, refundTask
	w.Settle()
	replies := w.Replies()
	last := replies[len(replies)-1]
	if !strings.Contains(last, "an answer from you") || strings.Count(strings.Join(replies, "\n"), "It is in hand") != 1 {
		t.Fatalf("status replies = %q", replies)
	}
	if w.Agent.Calls("work") != workCalls {
		t.Fatal("the task waiting for an answer called the model")
	}

	// A note, word for word, then the owner's answer; the task continues and follows up.
	note := w.Say("Oh, and also mention booking reference XY123 when you write again.")
	w.Settle()
	if out, ok := w.Command("/answer 3 No"); !ok {
		t.Fatal(out)
	}
	w.Settle()
	var delivered string
	w.App.DB.QueryRow(`SELECT body FROM owner_notes WHERE task_id IN (SELECT id FROM tasks WHERE assignment_id = ?)`, project).Scan(&delivered)
	if delivered != note.Text() {
		t.Fatalf("note delivered as %q", delivered)
	}
	if out, ok := w.Command("/approve 4"); !ok {
		t.Fatal(out)
	}
	w.Settle()

	// Mid-week: the emergency stop through the thread.
	w.Advance(48 * time.Hour)
	calls := w.Agent.Calls("chat") + w.Agent.Calls("work")
	if out, ok := w.Command("/halt checking something"); !ok {
		t.Fatal(out)
	}
	w.Say("are you there?")
	w.Settle()
	status, _ := w.Command("/status")
	if !strings.Contains(status, "EMERGENCY STOP") || w.Agent.Calls("chat")+w.Agent.Calls("work") != calls {
		t.Fatalf("during the stop: %q, %d model calls", status, w.Agent.Calls("chat")+w.Agent.Calls("work")-calls)
	}
	if reports := w.Of(chat.KindReport); !hasTitle(reports, "Emergency stop set") {
		t.Fatal("no card for the emergency stop")
	}
	if out, ok := w.Command("/unhalt"); !ok {
		t.Fatal(out)
	}
	w.Settle()

	// The chat budget runs out: commands and cards keep working.
	w.Agent.Cost = 1_000_000
	chatAsg, _ := w.App.Chat.ChatAssignment(w.Ctx)
	tight := chat.ChatPolicy()
	tight.Limits = policy.Limits{MaxLLMSpendUSD: 0.5, BudgetPeriod: policy.Duration{Duration: 720 * time.Hour}}
	if _, err := w.App.Assignments.SetPolicy(w.Ctx, chatAsg, tight, "owner"); err != nil {
		t.Fatal(err)
	}
	w.Say("anything new?")
	w.Settle()
	w.Say("still there?")
	w.Settle()
	if out, ok := w.Command("/waiting"); !ok || out == "" {
		t.Fatalf("/waiting with the chat budget used up = %q", out)
	}

	// Day 5: the bank posts the refund; the project completes by evidence.
	w.Clock.Advance(48 * time.Hour)
	if _, _, err := w.App.Scheduler.Ingest(w.Ctx, scheduler.Event{Source: "bank", Kind: "transaction.posted", ExternalID: "bank-1",
		Payload: json.RawMessage(`{"merchant":"Airline","amount":"412.00"}`)}); err != nil {
		t.Fatal(err)
	}
	w.Settle()
	if !hasTitle(w.Of(chat.KindReport), "Project completed: lh123-refund") {
		t.Fatal("no report card for the completed project")
	}

	metrics := map[string]int{
		"turns":                  w.Count(`SELECT COUNT(*) FROM tasks WHERE assignment_id = ? AND priority = ?`, chatAsg, tasks.PriorityInteractive),
		"model_calls":            w.Agent.Calls("chat") + w.Agent.Calls("work"),
		"proposals":              len(w.Of(chat.KindProposal)),
		"proposals_confirmed":    w.Count(`SELECT COUNT(*) FROM approvals ap JOIN actions a ON a.id = ap.action_id WHERE a.tool_name LIKE 'propose_%' AND ap.status = 'approved'`),
		"approvals":              w.Count(`SELECT COUNT(*) FROM approvals WHERE status = 'approved'`),
		"emails_sent":            w.Count(`SELECT COUNT(*) FROM mock_mailbox`),
		"notes_delivered":        w.Count(`SELECT COUNT(*) FROM owner_notes WHERE delivered_at IS NOT NULL`),
		"questions_asked":        w.Count(`SELECT COUNT(*) FROM questions`),
		"replies":                len(w.Replies()),
		"spend_chat_microusd":    w.Count(`SELECT COALESCE(SUM(cost_micros), 0) FROM model_usage WHERE assignment_id = ?`, chatAsg),
		"spend_project_microusd": w.Count(`SELECT COALESCE(SUM(cost_micros), 0) FROM model_usage WHERE assignment_id = ?`, project),
		"duplicate_emails":       w.Count(`SELECT COUNT(*) - COUNT(DISTINCT body) FROM mock_mailbox`),
		"duplicate_replies":      w.Count(`SELECT COUNT(*) FROM (SELECT turn_task_id FROM conversation_messages WHERE author = 'agent' GROUP BY turn_task_id HAVING COUNT(*) > 1)`),
		"duplicate_assignments":  w.Count(`SELECT COUNT(*) - COUNT(DISTINCT name) FROM assignments`),
		"duplicate_notes":        w.Count(`SELECT COUNT(*) - COUNT(DISTINCT id) FROM owner_notes`),
	}
	report, _ := json.Marshal(metrics)
	t.Logf("chat scenario metrics: %s", report)
	if metrics["duplicate_emails"]+metrics["duplicate_replies"]+metrics["duplicate_assignments"]+metrics["duplicate_notes"] != 0 {
		t.Fatalf("duplicates: %s", report)
	}
	if metrics["emails_sent"] != 2 || metrics["notes_delivered"] != 1 || metrics["questions_asked"] != 1 || metrics["proposals_confirmed"] != 1 {
		t.Fatalf("metrics = %s", report)
	}
	var text int
	w.App.DB.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE payload_json LIKE '%booking reference%' OR payload_json LIKE '%cancelled flight LH123%'`).Scan(&text)
	if text != 0 {
		t.Fatal("the audit log holds message text")
	}
}

type killed struct{}

func (killed) Error() string { return "process killed before the reply was committed" }

var errKilled error = killed{}

func hasTitle(cards []chat.Message, title string) bool {
	for _, c := range cards {
		if c.Card().Title == title {
			return true
		}
	}
	return false
}
