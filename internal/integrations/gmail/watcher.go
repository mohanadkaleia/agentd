package gmail

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"agentd/internal/scheduler"
)

const historyKey = "gmail:history_id"

type Watcher struct {
	Account   *Account
	DB        *sql.DB
	Scheduler *scheduler.Scheduler
}

func (w *Watcher) Tick(ctx context.Context) (int, error) {
	start, err := w.cursor(ctx)
	if err != nil {
		return 0, err
	}
	if start == "" {
		p, err := w.Account.Client.Profile(ctx)
		if err != nil {
			return 0, err
		}
		return 0, w.save(ctx, p.HistoryID)
	}
	ids, latest, err := w.Account.Client.History(ctx, start)
	if errors.Is(err, ErrHistoryExpired) {
		return w.recover(ctx)
	}
	if err != nil {
		return 0, err
	}
	n, err := w.ingest(ctx, ids)
	if err != nil {
		return n, err
	}
	return n, w.save(ctx, latest)
}

func (w *Watcher) recover(ctx context.Context) (int, error) {
	p, err := w.Account.Client.Profile(ctx)
	if err != nil {
		return 0, err
	}
	ids, err := w.Account.Client.Search(ctx, "in:inbox newer_than:2d", 50)
	if err != nil {
		return 0, err
	}
	n, err := w.ingest(ctx, ids)
	if err != nil {
		return n, err
	}
	return n, w.save(ctx, p.HistoryID)
}

func (w *Watcher) ingest(ctx context.Context, ids []string) (int, error) {
	n := 0
	for _, id := range ids {
		m, err := w.Account.Client.Message(ctx, id, false)
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == 404 {
			continue
		}
		if err != nil {
			return n, err
		}
		if m.HasLabel("SENT") || m.HasLabel("DRAFT") || !m.HasLabel("INBOX") {
			continue
		}
		payload, _ := json.Marshal(map[string]any{
			"message_id": m.ID, "thread_id": m.ThreadID, "from": m.From, "subject": m.Subject,
			"snippet": m.Snippet, "labels": m.Labels, "date": m.Date.Format(time.RFC3339),
		})
		_, dup, err := w.Scheduler.Ingest(ctx, scheduler.Event{Source: "gmail", Kind: "email.received", ExternalID: m.ID, Payload: payload})
		if err != nil {
			return n, err
		}
		if !dup {
			n++
		}
	}
	return n, nil
}

func (w *Watcher) cursor(ctx context.Context) (string, error) {
	var v string
	err := w.DB.QueryRowContext(ctx, `SELECT value FROM system_state WHERE key = ?`, historyKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (w *Watcher) save(ctx context.Context, historyID string) error {
	_, err := w.DB.ExecContext(ctx, `INSERT INTO system_state (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		historyKey, historyID, w.Account.Now().UnixMilli())
	return err
}
