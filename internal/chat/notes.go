package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"agentd/internal/ask"
	"agentd/internal/model"
	"agentd/internal/runtime"
	"agentd/internal/tools"
)

const ForwardTool = "forward_owner_message"

func (s *Service) answer(ctx context.Context, questionID, text, channel string) (string, error) {
	q, err := ask.Answer(ctx, s.DB, s.Now(), questionID, text, "owner:"+channel)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Answered %q. Its task continues with your answer.", q.Question), nil
}

func (s *Service) cmdAnswer(ctx context.Context, c call) (string, error) {
	if len(c.args) < 2 {
		return "", errors.New("usage: /answer <n or question id> <your answer>")
	}
	id := c.args[0]
	if !strings.HasPrefix(id, "qst_") {
		card, err := s.findCard(ctx, c.conv, id)
		if err != nil {
			return "", err
		}
		if card.Kind != KindQuestion {
			return "", fmt.Errorf("[%d] is not a question", card.Card().N)
		}
		id = card.RefID
	}
	return s.answer(ctx, id, strings.TrimSpace(strings.TrimPrefix(c.rest, c.args[0])), c.channel)
}

func (s *Service) cmdTell(ctx context.Context, c call) (string, error) {
	if len(c.args) < 2 {
		return "", errors.New("usage: /tell <task or assignment> <your note>")
	}
	target := c.args[0]
	if !strings.HasPrefix(target, "tsk_") {
		id, err := s.resolveAssignment(ctx, target)
		if err != nil {
			return "", err
		}
		target = id
	}
	task, when, err := s.Runtime.Tell(ctx, runtime.Note{Target: target, Text: strings.TrimSpace(strings.TrimPrefix(c.rest, c.args[0])),
		By: "owner:" + c.channel})
	if err != nil {
		return "", err
	}
	return "Sent to " + task + ". " + when, nil
}

// Forward is the chat's way to pass the owner's own words to a task or a question.
// The model picks only the target: the text is copied by code from the owner's message.
type Forward struct{ s *Service }

func (s *Service) Forward() *Forward { return &Forward{s: s} }

type forwardArgs struct {
	MessageID    string `json:"message_id"`
	TaskID       string `json:"task_id"`
	AssignmentID string `json:"assignment_id"`
	QuestionID   string `json:"question_id"`
}

func (f *Forward) Definition() model.ToolDefinition {
	return model.ToolDefinition{Name: ForwardTool,
		Description: "Pass one of the owner's own messages, word for word, to a running task (task_id or assignment_id) or as the answer to an open question (question_id). " +
			"You choose only where it goes; the text is the owner's message exactly.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"message_id":{"type":"string","description":"id of the owner's message in this conversation"},
"task_id":{"type":"string"},"assignment_id":{"type":"string"},"question_id":{"type":"string"}},"required":["message_id"]}`)}
}

func (f *Forward) Effect() tools.Effect { return tools.EffectWrite }
func (f *Forward) OnlyFor() string      { return ChatKey }
func (f *Forward) NoResource() bool     { return true }

func (f *Forward) Resource(raw json.RawMessage) (string, error) {
	var in forwardArgs
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", err
	}
	targets := 0
	for _, t := range []string{in.TaskID, in.AssignmentID, in.QuestionID} {
		if t != "" {
			targets++
		}
	}
	if in.MessageID == "" || targets != 1 {
		return "", errors.New("give message_id and exactly one of task_id, assignment_id or question_id")
	}
	return "", nil
}

func (f *Forward) Execute(ctx context.Context, call tools.Invocation) (tools.Result, error) {
	var in forwardArgs
	if err := json.Unmarshal(call.Arguments, &in); err != nil {
		return tools.Result{}, fmt.Errorf("%w: %v", tools.ErrNotExecuted, err)
	}
	text, err := f.ownerText(ctx, call.TaskID, in.MessageID)
	if err != nil {
		return tools.Result{}, fmt.Errorf("%w: %v", tools.ErrNotExecuted, err)
	}
	var out string
	if in.QuestionID != "" {
		q, err := ask.Answer(ctx, f.s.DB, f.s.Now(), in.QuestionID, text, "owner (forwarded)")
		if err != nil {
			return tools.Result{}, fmt.Errorf("%w: %v", tools.ErrNotExecuted, err)
		}
		if _, err := f.s.DB.ExecContext(ctx, `UPDATE questions SET source_message_id = ? WHERE id = ?`, in.MessageID, q.ID); err != nil {
			return tools.Result{}, err
		}
		out = "The owner's message answered the question " + q.ID + "."
	} else {
		target := in.TaskID
		if target == "" {
			target = in.AssignmentID
		}
		task, when, err := f.s.Runtime.Tell(ctx, runtime.Note{Target: target, Text: text, Source: in.MessageID, By: "owner (forwarded)"})
		if err != nil {
			return tools.Result{}, fmt.Errorf("%w: %v", tools.ErrNotExecuted, err)
		}
		out = "Sent the owner's message to " + task + ". " + when
	}
	b, _ := json.Marshal(map[string]string{"result": out})
	return tools.Result{Output: b}, nil
}

// ownerText returns the text of an owner message in the conversation of this turn.
func (f *Forward) ownerText(ctx context.Context, turn, messageID string) (string, error) {
	m, err := messageBy(ctx, f.s.DB, `id = ? AND conversation_id = (SELECT conversation_id FROM conversation_messages
		WHERE turn_task_id = ? LIMIT 1)`, messageID, turn)
	if err != nil {
		return "", fmt.Errorf("no message %s in this conversation", messageID)
	}
	if m.Author != Owner || m.Kind != KindText {
		return "", fmt.Errorf("message %s is not the owner's own message; only those can be forwarded", messageID)
	}
	return m.Text(), nil
}

func (f *Forward) Reconcile(ctx context.Context, call tools.Invocation) (tools.Result, tools.Finding, error) {
	var in forwardArgs
	json.Unmarshal(call.Arguments, &in)
	var n int
	if err := f.s.DB.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM owner_notes WHERE source_message_id = ?)
		+ (SELECT COUNT(*) FROM questions WHERE source_message_id = ?)`, in.MessageID, in.MessageID).Scan(&n); err != nil {
		return tools.Result{}, tools.Unknown, err
	}
	if n > 0 {
		return tools.Result{Output: json.RawMessage(`{"result":"forwarded"}`)}, tools.Happened, nil
	}
	return tools.Result{}, tools.NotHappened, nil
}
