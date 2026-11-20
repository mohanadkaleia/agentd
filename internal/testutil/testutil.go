package testutil

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"agentd/internal/store"
)

type Clock struct {
	mu sync.Mutex
	t  time.Time
}

func NewClock() *Clock {
	return &Clock{t: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)}
}

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func DBPath(t *testing.T) string {
	return filepath.Join(t.TempDir(), "agentd.db")
}

func Open(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
