package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"agentd/internal/ids"
	"agentd/internal/model"
)

type WebSearch struct{}

func (WebSearch) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        "web_search",
		Description: "Search the web and return result snippets.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
	}
}

func (WebSearch) Effect() Effect { return EffectRead }

func (WebSearch) Resource(args json.RawMessage) (string, error) {
	var in struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", err
	}
	if strings.TrimSpace(in.Query) == "" {
		return "", errors.New("query is required")
	}
	return "", nil
}

func (WebSearch) Execute(_ context.Context, call Invocation) (Result, error) {
	var in struct {
		Query string `json:"query"`
	}
	json.Unmarshal(call.Arguments, &in)
	out, _ := json.Marshal(map[string]any{
		"results": []string{fmt.Sprintf("Simulated result for %q: refunds are due within 7 days of a cancelled flight.", in.Query)},
	})
	return Result{Output: out}, nil
}

func (WebSearch) Reconcile(context.Context, Invocation) (Result, bool, error) {
	return Result{}, false, nil
}

type Mailbox struct {
	DB    *sql.DB
	Delay time.Duration
}

type email struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

func NewMailbox(ctx context.Context, db *sql.DB, delay time.Duration) (*Mailbox, error) {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS mock_mailbox (
		idempotency_key TEXT PRIMARY KEY,
		message_id TEXT NOT NULL,
		recipient TEXT NOT NULL,
		subject TEXT NOT NULL,
		body TEXT NOT NULL,
		sent_at INTEGER NOT NULL
	)`)
	if err != nil {
		return nil, err
	}
	return &Mailbox{DB: db, Delay: delay}, nil
}

func (*Mailbox) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        "send_email",
		Description: "Send an email to a single recipient.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"to":{"type":"string"},"subject":{"type":"string"},"body":{"type":"string"}},"required":["to","subject","body"]}`),
	}
}

func (*Mailbox) Effect() Effect { return EffectWrite }

func (*Mailbox) Resource(args json.RawMessage) (string, error) {
	var in email
	if err := json.Unmarshal(args, &in); err != nil {
		return "", err
	}
	if !strings.Contains(in.To, "@") || strings.ContainsAny(in.To, ", ;") {
		return "", fmt.Errorf("invalid recipient %q", in.To)
	}
	if in.Subject == "" || in.Body == "" {
		return "", errors.New("subject and body are required")
	}
	return strings.ToLower(in.To), nil
}

func (m *Mailbox) Execute(ctx context.Context, call Invocation) (Result, error) {
	var in email
	if err := json.Unmarshal(call.Arguments, &in); err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrNotExecuted, err)
	}
	messageID := ids.New("msg")
	_, err := m.DB.ExecContext(ctx, `INSERT INTO mock_mailbox (idempotency_key, message_id, recipient, subject, body, sent_at)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (idempotency_key) DO NOTHING`,
		call.IdempotencyKey, messageID, in.To, in.Subject, in.Body, time.Now().UnixMilli())
	if err != nil {
		return Result{}, err
	}
	if m.Delay > 0 {
		time.Sleep(m.Delay)
	}
	res, found, err := m.Reconcile(ctx, call)
	if err != nil || !found {
		return Result{}, fmt.Errorf("send not confirmed: %v", err)
	}
	return res, nil
}

func (m *Mailbox) Reconcile(ctx context.Context, call Invocation) (Result, bool, error) {
	var messageID string
	err := m.DB.QueryRowContext(ctx, `SELECT message_id FROM mock_mailbox WHERE idempotency_key = ?`, call.IdempotencyKey).Scan(&messageID)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	out, _ := json.Marshal(map[string]string{"status": "sent", "message_id": messageID})
	return Result{Output: out, ExternalID: messageID}, true, nil
}

func SentCount(ctx context.Context, db *sql.DB, recipient string) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mock_mailbox WHERE recipient = ?`, recipient).Scan(&n)
	return n, err
}
