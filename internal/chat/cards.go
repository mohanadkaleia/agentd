package chat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"agentd/internal/ask"
	"agentd/internal/assignments"
	"agentd/internal/audit"
	"agentd/internal/gateway"
	"agentd/internal/overview"
	"agentd/internal/tools"
)

const (
	KindQuestion      = "question"
	KindProposal      = "proposal"
	KindApproval      = "approval"
	KindNotification  = "notification"
	KindReport        = "report"
	KindCommandResult = "command_result"
)

// Card is the body of a message that shows the owner something that happened, written by
// code from the audit log. Text that came from a model or a tool is labelled by Source.
type Card struct {
	N             int             `json:"n,omitempty"`
	Title         string          `json:"title"`
	Text          string          `json:"text,omitempty"`
	Source        string          `json:"source,omitempty"`
	AssignmentID  string          `json:"assignment_id,omitempty"`
	Assignment    string          `json:"assignment,omitempty"`
	TaskID        string          `json:"task_id,omitempty"`
	ActionID      string          `json:"action_id,omitempty"`
	Tool          string          `json:"tool,omitempty"`
	Resource      string          `json:"resource,omitempty"`
	Arguments     json.RawMessage `json:"arguments,omitempty"`
	ArgumentsHash string          `json:"arguments_sha256,omitempty"`
	ExpiresAt     *time.Time      `json:"expires_at,omitempty"`
	Severity      string          `json:"severity,omitempty"`
	Choices       []string        `json:"choices,omitempty"`
	QuestionID    string          `json:"question_id,omitempty"`
	Actions       []string        `json:"actions,omitempty"`
}

func (m Message) Card() Card {
	var c Card
	json.Unmarshal(m.Body, &c)
	return c
}

func decision(kind string) bool {
	return kind == KindApproval || kind == KindProposal || kind == KindQuestion
}

var (
	ErrStale      = errors.New("this card no longer matches the action waiting for approval")
	ErrNoSuchCard = errors.New("no such item in this conversation")
)

const cursorKey = "chat.cards_after"

// Project turns new audit entries into cards. It is code only: no model call, and it runs
// while halted. Replaying entries adds nothing, because a card is unique per conversation,
// kind and reference.
func (s *Service) Project(ctx context.Context) error {
	var raw string
	err := s.DB.QueryRowContext(ctx, `SELECT value FROM system_state WHERE key = ?`, cursorKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = s.DB.ExecContext(ctx, `INSERT INTO system_state (key, value, updated_at)
			VALUES (?, (SELECT COALESCE(MAX(id), 0) FROM audit_log), ?)`, cursorKey, s.Now().UnixMilli())
		return err
	}
	if err != nil {
		return err
	}
	cursor, _ := strconv.ParseInt(raw, 10, 64)
	for {
		entries, err := audit.List(ctx, s.DB, audit.Filter{AfterID: cursor, Limit: 200})
		if err != nil || len(entries) == 0 {
			return err
		}
		for _, e := range entries {
			kind, card, conv, ok, err := s.cardFor(ctx, e)
			if errors.Is(err, sql.ErrNoRows) || errors.Is(err, assignments.ErrNotFound) {
				ok, err = false, nil
			}
			if err != nil {
				return fmt.Errorf("card for audit entry %d (%s): %w", e.ID, e.Operation, err)
			}
			if err := s.addCard(ctx, ok, conv, kind, cardRef(kind, e), card, e.ID); err != nil {
				return err
			}
			cursor = e.ID
		}
	}
}

func cardRef(kind string, e audit.Entry) string {
	if kind == KindReport {
		return fmt.Sprintf("%s:%s@%d", e.Operation, e.ObjectID, e.ID)
	}
	return e.ObjectID
}

func (s *Service) addCard(ctx context.Context, ok bool, conv, kind, ref string, c Card, cursor int64) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if ok {
		if decision(kind) {
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(json_extract(body_json, '$.n')), 0) + 1
				FROM conversation_messages WHERE conversation_id = ?`, conv).Scan(&c.N); err != nil {
				return err
			}
		}
		b, err := json.Marshal(c)
		if err != nil {
			return err
		}
		seq, err := insert(ctx, tx, Message{ID: newID(), ConversationID: conv, Author: System, Kind: kind, Body: b,
			RefKind: kind, RefID: ref, CreatedAt: s.Now()})
		if err != nil {
			return err
		}
		if kind == KindProposal && seq > 0 {
			if err := audit.Record(ctx, tx, s.Now(), c.AssignmentID, "chat", "chat.proposal_created", ref,
				map[string]any{"action_id": c.ActionID, "conversation": conv, "n": c.N}); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE system_state SET value = ?, updated_at = ? WHERE key = ?`,
		strconv.FormatInt(cursor, 10), s.Now().UnixMilli(), cursorKey); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if ok {
		s.changed()
	}
	return nil
}

// cardFor decides whether an audit entry becomes a card, and where it goes.
func (s *Service) cardFor(ctx context.Context, e audit.Entry) (string, Card, string, bool, error) {
	var a assignments.Assignment
	if e.AssignmentID != "" {
		var err error
		if a, err = s.Assignments.Get(ctx, e.AssignmentID); err != nil {
			return "", Card{}, "", false, err
		}
	}
	report := func(title, text string, actions ...string) (string, Card, string, bool, error) {
		if a.Builtin != "" {
			return "", Card{}, "", false, nil
		}
		conv, err := s.Default(ctx)
		return KindReport, Card{Title: title, Text: text, Source: "agentd", AssignmentID: a.ID, Assignment: a.Name,
			Actions: append([]string{"open"}, actions...)}, conv, err == nil, err
	}
	switch {
	case e.Operation == "approval.requested":
		return s.approvalCard(ctx, e.ObjectID, a)
	case e.Operation == "action.succeeded":
		return s.appliedCard(ctx, e.ObjectID, a)
	case e.Operation == "question.asked":
		q, err := ask.Get(ctx, s.DB, e.ObjectID)
		if err != nil {
			return "", Card{}, "", false, err
		}
		conv, err := s.conversationFor(ctx, a, q.TaskID)
		return KindQuestion, Card{Title: q.Question, Text: q.Context, Choices: q.Choices, Source: "agent", QuestionID: q.ID,
			AssignmentID: a.ID, Assignment: a.Name, TaskID: q.TaskID, ExpiresAt: &q.ExpiresAt, Actions: []string{"answer"}}, conv, err == nil, err
	case e.Operation == "notification.pending":
		var title, text, severity, task, dedup string
		if err := s.DB.QueryRowContext(ctx, `SELECT title, body, severity, COALESCE(task_id, ''), dedup_key FROM notifications WHERE id = ?`,
			e.ObjectID).Scan(&title, &text, &severity, &task, &dedup); err != nil {
			return "", Card{}, "", false, err
		}
		if strings.HasPrefix(dedup, "question:") || strings.HasPrefix(dedup, "question-reminder:") {
			return "", Card{}, "", false, nil
		}
		conv, err := s.conversationFor(ctx, a, task)
		return KindNotification, Card{Title: title, Text: text, Source: "agent", Severity: severity, AssignmentID: a.ID,
			Assignment: a.Name, TaskID: task, Actions: []string{"open"}}, conv, err == nil, err
	case e.Operation == "task.blocked":
		var title, cause string
		if err := s.DB.QueryRowContext(ctx, `SELECT title, COALESCE(json_extract(result_json, '$.error'), '') FROM tasks WHERE id = ?`,
			e.ObjectID).Scan(&title, &cause); err != nil {
			return "", Card{}, "", false, err
		}
		kind, c, conv, ok, err := report("Task blocked: "+title, cause, "retry")
		c.TaskID = e.ObjectID
		return kind, c, conv, ok, err
	case e.Operation == "action.needs_reconciliation":
		var tool, resource, task string
		if err := s.DB.QueryRowContext(ctx, `SELECT tool_name, resource, task_id FROM actions WHERE id = ?`, e.ObjectID).
			Scan(&tool, &resource, &task); err != nil {
			return "", Card{}, "", false, err
		}
		kind, c, conv, ok, err := report(fmt.Sprintf("Did it happen? %s %s", tool, resource),
			"agentd could not tell whether this action took effect, so it will not retry it.", "resolve")
		c.ActionID, c.TaskID, c.Tool, c.Resource = e.ObjectID, task, tool, resource
		return kind, c, conv, ok, err
	case e.Operation == "project.completed":
		return report("Project completed: "+a.Name, "Every completion criterion has evidence.")
	case strings.HasSuffix(e.Operation, ".paused") && a.PausedBy == assignments.PausedByBudget:
		return report("Paused by its budget: "+a.Name, a.StatusReason)
	case e.Operation == "system.halted":
		var p struct {
			Reason string `json:"reason"`
		}
		json.Unmarshal(e.Payload, &p)
		return report("Emergency stop set", p.Reason+". Nothing runs until /unhalt.")
	case e.Operation == "system.resumed":
		return report("Emergency stop lifted", "Work continues.")
	case e.Operation == "mcp.server_needs_auth" || e.Operation == "mcp.server_failed":
		var p struct {
			Error string `json:"error"`
		}
		json.Unmarshal(e.Payload, &p)
		state := strings.TrimPrefix(e.Operation, "mcp.server_")
		return report(fmt.Sprintf("MCP server %s: %s", e.ObjectID, strings.ReplaceAll(state, "_", " ")), p.Error)
	}
	return "", Card{}, "", false, nil
}

func (s *Service) approvalCard(ctx context.Context, id string, a assignments.Assignment) (string, Card, string, bool, error) {
	var c Card
	var args string
	var expires int64
	if err := s.DB.QueryRowContext(ctx, `SELECT a.tool_name, a.resource, a.arguments_json, ap.arguments_hash, ap.expires_at,
		a.task_id, a.id FROM approvals ap JOIN actions a ON a.id = ap.action_id WHERE ap.id = ?`, id).
		Scan(&c.Tool, &c.Resource, &args, &c.ArgumentsHash, &expires, &c.TaskID, &c.ActionID); err != nil {
		return "", c, "", false, err
	}
	at := time.UnixMilli(expires)
	c.Title, c.Arguments, c.ExpiresAt = strings.TrimSpace(c.Tool+" "+c.Resource), json.RawMessage(args), &at
	c.AssignmentID, c.Assignment, c.Source = a.ID, a.Name, "agent"
	c.Actions = []string{"approve", "reject", "details"}
	kind := KindApproval
	tool := s.Runtime.Tools[c.Tool]
	if d, ok := tool.(tools.Describer); ok {
		c.Title, c.Text = d.Describe(c.Arguments)
	}
	if tools.NeedsOwner(tool) {
		kind, c.Actions = KindProposal, []string{"approve", "reject", "edit", "details"}
	}
	conv, err := s.conversationFor(ctx, a, c.TaskID)
	return kind, c, conv, err == nil, err
}

// appliedCard reports a confirmed proposal taking effect, with what it created.
func (s *Service) appliedCard(ctx context.Context, actionID string, a assignments.Assignment) (string, Card, string, bool, error) {
	var tool, task, external string
	var args string
	if err := s.DB.QueryRowContext(ctx, `SELECT tool_name, task_id, COALESCE(external_id, ''), arguments_json FROM actions WHERE id = ?`,
		actionID).Scan(&tool, &task, &external, &args); err != nil {
		return "", Card{}, "", false, err
	}
	t := s.Runtime.Tools[tool]
	if !tools.NeedsOwner(t) {
		return "", Card{}, "", false, nil
	}
	c := Card{Title: "Done: " + tool, Source: "agentd", ActionID: actionID, TaskID: task, Actions: []string{"open"}}
	if d, ok := t.(tools.Describer); ok {
		title, _ := d.Describe(json.RawMessage(args))
		c.Title = "Done: " + title
	}
	if created, err := s.Assignments.Get(ctx, external); err == nil {
		c.AssignmentID, c.Assignment = created.ID, created.Name
		next := ""
		if o, err := overview.Get(ctx, s.Runtime, created.ID); err == nil {
			next = o.Next.String()
		}
		c.Text = fmt.Sprintf("%s %s is %s. Next: %s", created.Kind, created.ID, created.Status, next)
	} else if external != "" {
		c.Text = "Created " + external + "."
	}
	conv, err := s.conversationFor(ctx, a, task)
	return KindReport, c, conv, err == nil, err
}

// conversationFor is the conversation of the turn that caused something, or the default one.
func (s *Service) conversationFor(ctx context.Context, a assignments.Assignment, task string) (string, error) {
	if a.Builtin != "" && task != "" {
		var conv string
		err := s.DB.QueryRowContext(ctx, `SELECT conversation_id FROM conversation_messages WHERE turn_task_id = ? LIMIT 1`, task).Scan(&conv)
		if err == nil {
			return conv, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	return s.Default(ctx)
}

// live fills in each card's current status from its source, so a card never shows a
// decision that is no longer open.
func (s *Service) live(ctx context.Context, msgs []Message) error {
	for i, m := range msgs {
		if m.RefID == "" {
			continue
		}
		switch m.Kind {
		case KindApproval, KindProposal:
			var status string
			var expires int64
			if err := s.DB.QueryRowContext(ctx, `SELECT status, expires_at FROM approvals WHERE id = ?`, m.RefID).Scan(&status, &expires); err != nil {
				return err
			}
			if status == "pending" && !s.Now().Before(time.UnixMilli(expires)) {
				status = "expired"
			}
			msgs[i].Status = status
		case KindNotification:
			if err := s.DB.QueryRowContext(ctx, `SELECT status FROM notifications WHERE id = ?`, m.RefID).Scan(&msgs[i].Status); err != nil {
				return err
			}
		case KindQuestion:
			if err := s.DB.QueryRowContext(ctx, `SELECT status FROM questions WHERE id = ?`, m.RefID).Scan(&msgs[i].Status); err != nil {
				return err
			}
		}
	}
	return nil
}

// Decide approves or rejects the approval a card shows. ref is the card's number in this
// conversation or the approval id. The current argument hash must match the card's.
func (s *Service) Decide(ctx context.Context, conv, ref string, approve bool, channel string) (string, error) {
	card, err := s.findCard(ctx, conv, ref)
	if err != nil {
		return "", err
	}
	c := card.Card()
	if card.Kind == KindQuestion {
		return "", fmt.Errorf("[%d] is a question; answer it with /answer %d <text>", c.N, c.N)
	}
	var hash string
	if err := s.DB.QueryRowContext(ctx, `SELECT arguments_hash FROM approvals WHERE id = ?`, card.RefID).Scan(&hash); err != nil {
		return "", err
	}
	if hash != c.ArgumentsHash {
		return "", ErrStale
	}
	err = s.Runtime.Gateway.Decide(ctx, card.RefID, approve, "owner:"+channel)
	switch {
	case errors.Is(err, gateway.ErrApprovalNotPending):
		live := []Message{card}
		if err := s.live(ctx, live); err != nil {
			return "", err
		}
		return "", fmt.Errorf("%w: [%d] %s is %s", ErrNotPending, c.N, c.Title, live[0].Status)
	case errors.Is(err, gateway.ErrApprovalExpired):
		return "", fmt.Errorf("%w: [%d] %s expired before the decision", ErrNotPending, c.N, c.Title)
	case err != nil:
		return "", err
	}
	if s.Wake != nil {
		s.Wake()
	}
	verb := "Rejected"
	if approve {
		verb = "Approved"
	}
	return fmt.Sprintf("%s [%d] %s.", verb, c.N, c.Title), nil
}

var ErrNotPending = errors.New("no longer waiting for a decision")

func (s *Service) findCard(ctx context.Context, conv, ref string) (Message, error) {
	ref = strings.TrimPrefix(strings.TrimSpace(ref), "#")
	var m Message
	var err error
	if n, nerr := strconv.Atoi(ref); nerr == nil {
		m, err = messageBy(ctx, s.DB, `conversation_id = ? AND kind IN ('approval', 'proposal', 'question') AND json_extract(body_json, '$.n') = ?`, conv, n)
	} else {
		m, err = messageBy(ctx, s.DB, `kind IN ('approval', 'proposal', 'question') AND ref_id = ? ORDER BY conversation_id = ? DESC LIMIT 1`, ref, conv)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return m, fmt.Errorf("%w: %s", ErrNoSuchCard, ref)
	}
	return m, err
}

// Away lists the cards a channel has not seen yet, the ones waiting for a decision first.
func (s *Service) Away(ctx context.Context, conv, channel string) ([]Message, error) {
	seen, err := s.LastRead(ctx, conv, channel)
	if err != nil {
		return nil, err
	}
	msgs, err := messages(ctx, s.DB, `conversation_id = ? AND seq > ? AND ref_id IS NOT NULL AND kind != 'notice' ORDER BY seq`, conv, seen)
	if err != nil {
		return nil, err
	}
	if err := s.live(ctx, msgs); err != nil {
		return nil, err
	}
	var open, rest []Message
	for _, m := range msgs {
		if decision(m.Kind) && m.Status == "pending" {
			open = append(open, m)
		} else {
			rest = append(rest, m)
		}
	}
	return append(open, rest...), nil
}
