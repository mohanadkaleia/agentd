// Package chat is the owner's conversation with agentd. Each owner message becomes one
// turn: an ordinary task of the built-in front desk assignment, so turns are leased,
// checkpointed, budgeted and audited like any other work. The package never calls a model
// itself.
package chat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"agentd/internal/assignments"
	"agentd/internal/audit"
	"agentd/internal/ids"
	"agentd/internal/policy"
	"agentd/internal/runtime"
	"agentd/internal/tasks"
)

const FrontDeskKey = "front-desk"

const (
	Owner  = "owner"
	Agent  = "agent"
	System = "system"
)

const (
	KindText   = "text"
	KindError  = "error"
	KindNotice = "notice"
)

var (
	ErrNotFound = errors.New("conversation not found")
	ErrEmpty    = errors.New("message is empty")
	ErrNoTurn   = errors.New("no turn is running in this conversation")
)

const maxMessageBytes = 64 << 10

type Service struct {
	DB          *sql.DB
	Tasks       *tasks.Store
	Assignments *assignments.Store
	Runtime     *runtime.Runtime
	Now         func() time.Time
	// Wake tells the interactive worker in this process that a turn is ready.
	Wake func()
	// Edit applies an owner-edited version of a pending proposal (approval id, new arguments).
	Edit func(ctx context.Context, approvalID string, args json.RawMessage, by string) (string, error)

	Window         int
	WindowBytes    int
	SummarizeBatch int
	TurnSteps      int
}

type Message struct {
	ID             string          `json:"id"`
	ConversationID string          `json:"conversation_id"`
	Seq            int64           `json:"seq"`
	Author         string          `json:"author"`
	Kind           string          `json:"kind"`
	Body           json.RawMessage `json:"body"`
	RefKind        string          `json:"ref_kind,omitempty"`
	RefID          string          `json:"ref_id,omitempty"`
	Channel        string          `json:"channel,omitempty"`
	ClientMsgID    string          `json:"client_msg_id,omitempty"`
	TurnTaskID     string          `json:"turn_task_id,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	// Status is a card's current status, read from its source when the message is read.
	Status string `json:"status,omitempty"`
}

// Text is the body of text, error, notice and command result messages.
type Text struct {
	Text    string `json:"text"`
	ReplyTo string `json:"reply_to,omitempty"`
	Failed  bool   `json:"failed,omitempty"`
}

func (m Message) Text() string {
	var t Text
	json.Unmarshal(m.Body, &t)
	return t.Text
}

// ProposeTools draft changes the owner must approve; PauseTool narrows and runs at once.
var ProposeTools = []string{"propose_assignment", "propose_policy", "propose_schedule", "propose_route", "propose_task", "propose_resume"}

const PauseTool = "pause_assignment"

func FrontDeskPolicy() policy.Policy {
	rules := map[string]policy.Rule{PauseTool: {Mode: policy.Allow}}
	for _, name := range ReadTools {
		rules[name] = policy.Rule{Mode: policy.Allow}
	}
	for _, name := range ProposeTools {
		rules[name] = policy.Rule{Mode: policy.RequireApproval}
	}
	return policy.Policy{Tools: rules,
		Limits: policy.Limits{MaxLLMSpendUSD: 5, BudgetPeriod: policy.Duration{Duration: 720 * time.Hour}}}
}

// FrontDesk returns the id of the built-in assignment that runs conversation turns,
// creating it the first time.
func (s *Service) FrontDesk(ctx context.Context) (string, error) {
	return s.Assignments.EnsureBuiltin(ctx, FrontDeskKey, assignments.NewResponsibility{
		Name:      FrontDeskKey,
		Objective: "Talk with the owner: answer questions about agentd's work from its records, and propose new work for the owner to confirm.",
		Policy:    FrontDeskPolicy(),
		Notifications: assignments.NotificationPolicy{MinSeverity: assignments.SeverityAttention,
			DedupWindow: policy.Duration{Duration: 24 * time.Hour}, MaxPerDay: 10},
	})
}

func (s *Service) NewConversation(ctx context.Context, title string) (string, error) {
	id := ids.New("cnv")
	now := s.Now().UnixMilli()
	_, err := s.DB.ExecContext(ctx, `INSERT INTO conversations (id, title, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		id, title, now, now)
	return id, err
}

// Default returns the most recently used open conversation, starting one if there is none.
func (s *Service) Default(ctx context.Context) (string, error) {
	var id string
	err := s.DB.QueryRowContext(ctx, `SELECT id FROM conversations WHERE status = 'open'
		ORDER BY updated_at DESC, rowid DESC LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return s.NewConversation(ctx, "")
	}
	return id, err
}

type Post struct {
	ConversationID string
	Text           string
	Channel        string
	ClientMsgID    string
	// ReplyTo is the message this one answers, such as an approval card.
	ReplyTo string
}

// shortcuts are the only plain replies code applies to an approval card. Anything else is an
// ordinary message.
var shortcuts = map[string]bool{"approve": true, "yes": true, "reject": false, "no": false}

// Post stores an owner message and, in the same transaction, the turn that will answer
// it. Posting the same ClientMsgID again returns the stored message and makes no new turn.
// A message sent while a turn runs joins the next turn, which starts when that one ends.
func (s *Service) Post(ctx context.Context, p Post) (Message, error) {
	text := strings.TrimSpace(p.Text)
	if text == "" {
		return Message{}, ErrEmpty
	}
	if len(text) > maxMessageBytes {
		return Message{}, fmt.Errorf("message is %d bytes; the limit is %d", len(text), maxMessageBytes)
	}
	if strings.HasPrefix(text, "/") {
		return s.direct(ctx, p, text, func(ctx context.Context) (string, error) {
			return s.Command(ctx, p.ConversationID, p.Channel, text)
		})
	}
	if approve, ok := shortcuts[strings.ToLower(strings.Trim(text, " .!"))]; ok && p.ReplyTo != "" {
		card, err := messageBy(ctx, s.DB, `id = ? AND conversation_id = ?`, p.ReplyTo, p.ConversationID)
		if err == nil && card.Kind == KindApproval {
			return s.direct(ctx, p, text, func(ctx context.Context) (string, error) {
				return s.Decide(ctx, p.ConversationID, card.RefID, approve, p.Channel)
			})
		}
	}
	desk, err := s.FrontDesk(ctx)
	if err != nil {
		return Message{}, err
	}
	now := s.Now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Message{}, err
	}
	defer tx.Rollback()
	if p.ClientMsgID != "" {
		if m, err := messageBy(ctx, tx, `conversation_id = ? AND client_msg_id = ?`, p.ConversationID, p.ClientMsgID); err == nil {
			return m, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return Message{}, err
		}
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversations WHERE id = ?`, p.ConversationID).Scan(&exists); err != nil {
		return Message{}, err
	}
	if exists == 0 {
		return Message{}, ErrNotFound
	}
	m := Message{ID: ids.New("msg"), ConversationID: p.ConversationID, Author: Owner, Kind: KindText,
		Body: reply(text, p.ReplyTo), Channel: p.Channel, ClientMsgID: p.ClientMsgID, CreatedAt: now}
	turn, parked, err := s.turnFor(ctx, tx, desk, m)
	if err != nil {
		return Message{}, err
	}
	m.TurnTaskID = turn
	if m.Seq, err = insert(ctx, tx, m); err != nil {
		return Message{}, err
	}
	if parked {
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status = 'waiting_event', updated_at = ? WHERE id = ?`,
			now.UnixMilli(), turn); err != nil {
			return Message{}, err
		}
	}
	if err := audit.Record(ctx, tx, now, desk, "owner", "chat.message_received", m.ID,
		map[string]any{"conversation": m.ConversationID, "channel": m.Channel, "bytes": len(text)}); err != nil {
		return Message{}, err
	}
	if err := s.explainWait(ctx, tx, desk, m.ConversationID); err != nil {
		return Message{}, err
	}
	if err := tx.Commit(); err != nil {
		return Message{}, err
	}
	if s.Wake != nil {
		s.Wake()
	}
	return m, nil
}

// direct stores an owner message that code answers without a turn (a slash command or a
// reply to a card), runs it, and stores the result.
func (s *Service) direct(ctx context.Context, p Post, text string, run func(context.Context) (string, error)) (Message, error) {
	now := s.Now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Message{}, err
	}
	defer tx.Rollback()
	if p.ClientMsgID != "" {
		if m, err := messageBy(ctx, tx, `conversation_id = ? AND client_msg_id = ?`, p.ConversationID, p.ClientMsgID); err == nil {
			return m, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return Message{}, err
		}
	}
	m := Message{ID: ids.New("msg"), ConversationID: p.ConversationID, Author: Owner, Kind: KindText,
		Body: reply(text, p.ReplyTo), Channel: p.Channel, ClientMsgID: p.ClientMsgID, CreatedAt: now}
	if m.Seq, err = insert(ctx, tx, m); err != nil {
		return Message{}, err
	}
	if m.Seq == 0 {
		return Message{}, ErrNotFound
	}
	if err := audit.Record(ctx, tx, now, "", "owner", "chat.command_received", m.ID,
		map[string]any{"conversation": m.ConversationID, "channel": m.Channel}); err != nil {
		return Message{}, err
	}
	if err := tx.Commit(); err != nil {
		return Message{}, err
	}
	out, runErr := run(ctx)
	result := Text{Text: out}
	if runErr != nil {
		result = Text{Text: runErr.Error(), Failed: true}
	}
	b, _ := json.Marshal(result)
	tx, err = s.DB.BeginTx(ctx, nil)
	if err != nil {
		return m, err
	}
	defer tx.Rollback()
	if _, err := insert(ctx, tx, Message{ID: ids.New("msg"), ConversationID: m.ConversationID, Author: System,
		Kind: KindCommandResult, Body: b, Channel: m.Channel, CreatedAt: s.Now()}); err != nil {
		return m, err
	}
	return m, tx.Commit()
}

func reply(text, to string) json.RawMessage {
	b, _ := json.Marshal(Text{Text: text, ReplyTo: to})
	return b
}

// turnFor picks the turn a new owner message belongs to: a turn that has not started yet,
// or a new one, parked when another turn of this conversation is still open. A turn waiting
// for the owner's decision on a card does not hold up the conversation.
func (s *Service) turnFor(ctx context.Context, tx *sql.Tx, desk string, m Message) (string, bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT t.id, EXISTS (SELECT 1 FROM runs r WHERE r.task_id = t.id)
		FROM tasks t WHERE t.id IN (SELECT DISTINCT turn_task_id FROM conversation_messages
			WHERE conversation_id = ? AND author = 'owner')
		AND t.status NOT IN ('completed', 'blocked', 'cancelled', 'waiting_approval') ORDER BY t.created_at, t.rowid`, m.ConversationID)
	if err != nil {
		return "", false, err
	}
	var open int
	var queued string
	for rows.Next() {
		var id string
		var started bool
		if err := rows.Scan(&id, &started); err != nil {
			rows.Close()
			return "", false, err
		}
		open++
		if !started && queued == "" {
			queued = id
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	if queued != "" {
		return queued, false, nil
	}
	steps := s.TurnSteps
	if steps == 0 {
		steps = 8
	}
	id, err := s.Tasks.CreateTx(ctx, tx, tasks.NewTask{AssignmentID: desk, OriginKey: "chat:" + m.ID,
		Title: "Reply in conversation", Objective: "Reply to the owner's latest message.",
		Priority: tasks.PriorityInteractive, MaxSteps: steps})
	return id, open > 0, err
}

// Cancel stops the conversation's current turn. Messages already stored stay; the next
// queued turn, if any, starts.
func (s *Service) Cancel(ctx context.Context, conversationID, actor string) error {
	var id string
	err := s.DB.QueryRowContext(ctx, `SELECT t.id FROM tasks t WHERE t.id IN (SELECT DISTINCT turn_task_id
			FROM conversation_messages WHERE conversation_id = ? AND author = 'owner')
		AND t.status NOT IN ('completed', 'blocked', 'cancelled')
		ORDER BY EXISTS (SELECT 1 FROM runs r WHERE r.task_id = t.id) DESC, t.created_at, t.rowid LIMIT 1`, conversationID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNoTurn
	}
	if err != nil {
		return err
	}
	if err := s.Tasks.Cancel(ctx, id, actor); err != nil && !errors.Is(err, tasks.ErrNotFound) {
		return err
	}
	if s.Wake != nil {
		s.Wake()
	}
	return nil
}

// Messages returns up to limit messages after a sequence number, oldest first.
func (s *Service) Messages(ctx context.Context, conversationID string, after int64, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 100
	}
	msgs, err := messages(ctx, s.DB, `conversation_id = ? AND seq > ? ORDER BY seq LIMIT ?`, conversationID, after, limit)
	if err != nil {
		return nil, err
	}
	return msgs, s.live(ctx, msgs)
}

// MarkRead records the last message a channel has shown, so each client can show what is new.
func (s *Service) MarkRead(ctx context.Context, conversationID, channel string, seq int64) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO conversation_reads (conversation_id, channel, seen_seq, updated_at)
		VALUES (?, ?, ?, ?) ON CONFLICT (conversation_id, channel) DO UPDATE
		SET seen_seq = max(seen_seq, excluded.seen_seq), updated_at = excluded.updated_at`,
		conversationID, channel, seq, s.Now().UnixMilli())
	return err
}

func (s *Service) LastRead(ctx context.Context, conversationID, channel string) (int64, error) {
	var seq int64
	err := s.DB.QueryRowContext(ctx, `SELECT seen_seq FROM conversation_reads WHERE conversation_id = ? AND channel = ?`,
		conversationID, channel).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return seq, err
}

func body(text string) json.RawMessage {
	b, _ := json.Marshal(Text{Text: text})
	return b
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func insert(ctx context.Context, tx *sql.Tx, m Message) (int64, error) {
	var seq int64
	err := tx.QueryRowContext(ctx, `INSERT INTO conversation_messages (id, conversation_id, seq, author, kind, body_json,
		ref_kind, ref_id, channel, client_msg_id, turn_task_id, created_at)
		VALUES (?, ?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM conversation_messages WHERE conversation_id = ?),
			?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, NULLIF(?, ''), NULLIF(?, ''), ?)
		ON CONFLICT DO NOTHING RETURNING seq`,
		m.ID, m.ConversationID, m.ConversationID, m.Author, m.Kind, string(m.Body), m.RefKind, m.RefID, m.Channel,
		m.ClientMsgID, m.TurnTaskID, m.CreatedAt.UnixMilli()).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE conversations SET updated_at = ? WHERE id = ?`, m.CreatedAt.UnixMilli(), m.ConversationID)
	return seq, err
}

const messageColumns = `id, conversation_id, seq, author, kind, body_json, COALESCE(ref_kind, ''), COALESCE(ref_id, ''),
	channel, COALESCE(client_msg_id, ''), COALESCE(turn_task_id, ''), created_at`

func scan(row interface{ Scan(...any) error }) (Message, error) {
	var m Message
	var body string
	var at int64
	err := row.Scan(&m.ID, &m.ConversationID, &m.Seq, &m.Author, &m.Kind, &body, &m.RefKind, &m.RefID,
		&m.Channel, &m.ClientMsgID, &m.TurnTaskID, &at)
	m.Body, m.CreatedAt = json.RawMessage(body), time.UnixMilli(at)
	return m, err
}

func messageBy(ctx context.Context, q querier, where string, args ...any) (Message, error) {
	return scan(q.QueryRowContext(ctx, `SELECT `+messageColumns+` FROM conversation_messages WHERE `+where, args...))
}

func messages(ctx context.Context, q querier, where string, args ...any) ([]Message, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+messageColumns+` FROM conversation_messages WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		m, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
