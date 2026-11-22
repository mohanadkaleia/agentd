package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"agentd/internal/audit"
	"agentd/internal/scheduler"
	"agentd/internal/system"
)

const (
	maxSeen          = 2000
	defaultMaxEvents = 20
	maxPollBackoff   = 30 * time.Minute
)

// Poll starts every configured poll. Polls call the MCP client directly, not a tool's
// Execute, so they are restricted to tools the owner declared reads and accepted.
func (m *Manager) Poll(ctx context.Context, sch *scheduler.Scheduler) {
	for _, s := range m.Servers {
		if s.config.Disabled {
			continue
		}
		for _, p := range s.config.Polls {
			go s.pollLoop(ctx, p, sch)
		}
	}
}

// PollNow runs every poll once, for callers that drive time themselves.
func (m *Manager) PollNow(ctx context.Context, sch *scheduler.Scheduler) int {
	total := 0
	for _, s := range m.Servers {
		for _, p := range s.config.Polls {
			if n, err := s.pollOnce(ctx, p, sch); err == nil {
				total += n
			}
		}
	}
	return total
}

func (s *Server) pollLoop(ctx context.Context, p Poll, sch *scheduler.Scheduler) {
	wait := p.Every.Duration
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		emitted, err := s.pollOnce(ctx, p, sch)
		wait = p.Every.Duration
		if err != nil && !errors.Is(err, errPaused) {
			failures := s.pollFailed(ctx, p, err)
			wait = min(p.Every.Duration<<min(failures, 10), maxPollBackoff)
			s.log.Warn("mcp poll failed", "poll", p.key(), "failures", failures, "err", err)
			continue
		}
		if emitted > 0 {
			s.log.Info("mcp poll", "poll", p.key(), "events", emitted)
		}
	}
}

var errPaused = errors.New("poll paused")

// pollOnce runs one poll and returns how many new events it emitted.
func (s *Server) pollOnce(ctx context.Context, p Poll, sch *scheduler.Scheduler) (int, error) {
	if st, err := system.Get(ctx, s.store.db); err != nil || st.Halted {
		if err != nil {
			return 0, err
		}
		return 0, errPaused
	}
	if _, unavailable := s.pin(p.Tool); unavailable != "" {
		return 0, fmt.Errorf("%w: %s", errPaused, unavailable)
	}
	cs := s.live()
	if cs == nil {
		return 0, errPaused
	}
	callCtx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()
	res, err := cs.CallTool(callCtx, &mcp.CallToolParams{Name: p.Tool, Arguments: p.Args})
	if err != nil {
		return 0, err
	}
	if res.IsError {
		return 0, fmt.Errorf("%s reported an error: %s", p.Tool, truncate(render(res), 300))
	}
	var result any
	if res.StructuredContent != nil {
		b, _ := json.Marshal(res.StructuredContent)
		json.Unmarshal(b, &result)
	} else if err := json.Unmarshal([]byte(full(res)), &result); err != nil {
		return 0, fmt.Errorf("%s did not return JSON", p.Tool)
	}
	found := lookupPath(result, p.Items)
	items, ok := found.([]any)
	if !ok && (found != nil || !exists(result, p.Items)) {
		return 0, fmt.Errorf("%q in the result is not a list", p.Items)
	}

	mark, err := s.store.pollMark(ctx, s.Name, p.key())
	if err != nil {
		return 0, err
	}
	limit := p.MaxEvents
	if limit == 0 {
		limit = defaultMaxEvents
	}
	emitted := 0
	for _, item := range items {
		id := scalar(lookupPath(item, p.ID))
		if id == "" || mark.seen[id] {
			continue
		}
		if !mark.primed {
			mark.add(id)
			continue
		}
		if emitted == limit {
			break
		}
		payload := map[string]string{}
		for field, path := range p.Payload {
			payload[field] = scalar(lookupPath(item, path))
		}
		mark.add(id)
		if excluded(payload, p.Exclude) {
			continue
		}
		body, _ := json.Marshal(payload)
		_, dup, err := sch.Ingest(ctx, scheduler.Event{Source: p.Event.Source, Kind: p.Event.Kind,
			ExternalID: s.Name + ":" + p.key() + ":" + id, Payload: body})
		if err != nil {
			return emitted, err
		}
		if !dup {
			emitted++
		}
	}
	mark.primed = true
	if err := s.store.savePollMark(ctx, s.Name, p.key(), mark); err != nil {
		return emitted, err
	}
	if emitted > 0 {
		err = audit.Record(ctx, s.store.db, s.store.now(), "", "mcp", "mcp.poll", s.Name+"__"+p.Tool,
			map[string]any{"server": s.Name, "poll": p.key(), "events": emitted})
	}
	return emitted, err
}

func (s *Server) pollFailed(ctx context.Context, p Poll, cause error) int {
	failures, err := s.store.pollFailure(ctx, s.Name, p.key(), cause.Error())
	if err != nil {
		s.log.Error("mcp poll state", "err", err)
		return 1
	}
	if failures == 3 {
		if err := s.store.notifyRouted(ctx, p.Event.Source, "mcp-poll-failing:"+s.Name+":"+p.key(),
			fmt.Sprintf("Feed %s/%s is failing", s.Name, p.key()),
			fmt.Sprintf("Polling %s has failed %d times in a row (%s), so new %s events are not arriving. That is a broken feed, not a quiet one.",
				p.Tool, failures, truncate(cause.Error(), 200), p.Event.Source)); err != nil {
			s.log.Error("mcp poll notification", "err", err)
		}
	}
	return failures
}

func exists(v any, path string) bool {
	keys := strings.Split(path, ".")
	parent, ok := lookupPath(v, strings.Join(keys[:len(keys)-1], ".")).(map[string]any)
	if len(keys) == 1 {
		parent, ok = v.(map[string]any)
	}
	if !ok {
		return false
	}
	_, ok = parent[keys[len(keys)-1]]
	return ok
}

func excluded(payload, exclude map[string]string) bool {
	for field, value := range exclude {
		if payload[field] == value {
			return true
		}
	}
	return false
}

func scalar(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}

type pollMark struct {
	seen   map[string]bool
	order  []string
	primed bool
}

func (m *pollMark) add(id string) {
	m.seen[id] = true
	m.order = append(m.order, id)
	if len(m.order) > maxSeen {
		delete(m.seen, m.order[0])
		m.order = m.order[1:]
	}
}

func (st *store) pollMark(ctx context.Context, server, poll string) (*pollMark, error) {
	m := &pollMark{seen: map[string]bool{}}
	var seen string
	err := st.db.QueryRowContext(ctx, `SELECT seen_json, primed FROM mcp_polls WHERE server = ? AND poll = ?`, server, poll).Scan(&seen, &m.primed)
	if errors.Is(err, sql.ErrNoRows) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(seen), &m.order); err != nil {
		return nil, err
	}
	for _, id := range m.order {
		m.seen[id] = true
	}
	return m, nil
}

func (st *store) savePollMark(ctx context.Context, server, poll string, m *pollMark) error {
	seen, _ := json.Marshal(m.order)
	_, err := st.db.ExecContext(ctx, `INSERT INTO mcp_polls (server, poll, seen_json, primed, last_success_at, last_error, failures)
		VALUES (?, ?, ?, 1, ?, '', 0)
		ON CONFLICT (server, poll) DO UPDATE SET seen_json = excluded.seen_json, primed = 1,
			last_success_at = excluded.last_success_at, last_error = '', failures = 0`,
		server, poll, string(seen), st.now().UnixMilli())
	return err
}

func (st *store) pollFailure(ctx context.Context, server, poll, cause string) (int, error) {
	var failures int
	err := st.db.QueryRowContext(ctx, `INSERT INTO mcp_polls (server, poll, last_error, failures) VALUES (?, ?, ?, 1)
		ON CONFLICT (server, poll) DO UPDATE SET last_error = excluded.last_error, failures = mcp_polls.failures + 1
		RETURNING failures`, server, poll, truncate(cause, 500)).Scan(&failures)
	return failures, err
}

// notifyRouted tells each live assignment that routes events from this source.
func (st *store) notifyRouted(ctx context.Context, source, key, title, body string) error {
	rows, err := st.db.QueryContext(ctx, `SELECT DISTINCT r.assignment_id FROM event_routes r JOIN assignments a ON a.id = r.assignment_id
		WHERE r.source = ? AND a.status IN ('active', 'paused')`, source)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if _, err := st.outbox.Notify(ctx, notifyNotification(id, key, title, body)); err != nil {
			return err
		}
	}
	return nil
}

type PollStatus struct {
	Server        string     `json:"server"`
	Poll          string     `json:"poll"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	Failures      int        `json:"failures"`
}

func ListPolls(ctx context.Context, db *sql.DB) ([]PollStatus, error) {
	rows, err := db.QueryContext(ctx, `SELECT server, poll, last_success_at, last_error, failures FROM mcp_polls ORDER BY server, poll`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PollStatus{}
	for rows.Next() {
		var p PollStatus
		var last sql.NullInt64
		if err := rows.Scan(&p.Server, &p.Poll, &last, &p.LastError, &p.Failures); err != nil {
			return nil, err
		}
		if last.Valid {
			t := time.UnixMilli(last.Int64)
			p.LastSuccessAt = &t
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
