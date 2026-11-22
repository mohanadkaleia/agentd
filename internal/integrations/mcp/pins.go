package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"agentd/internal/assignments"
	"agentd/internal/audit"
	"agentd/internal/notify"
)

const (
	StatusNew      = "new"
	StatusAccepted = "accepted"
	StatusChanged  = "changed"
	StatusRemoved  = "removed"
)

var ErrHashMismatch = errors.New("the tool's definition changed since it was shown; review it again")

// Definition is a server's tool exactly as it described it, before any truncation or
// cleaning for the prompt. Its hash is what the owner accepts.
type Definition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations json.RawMessage `json:"annotations,omitempty"`
}

type Pin struct {
	Server       string          `json:"server"`
	Tool         string          `json:"tool"`
	Status       string          `json:"status"`
	Hash         string          `json:"definition_hash"`
	Definition   json.RawMessage `json:"definition"`
	AcceptedHash string          `json:"accepted_hash,omitempty"`
	Accepted     json.RawMessage `json:"accepted_definition,omitempty"`
	AcceptedAt   *time.Time      `json:"accepted_at,omitempty"`
	AcceptedBy   string          `json:"accepted_by,omitempty"`
	ChangedAt    time.Time       `json:"changed_at"`

	Description         string `json:"description_visible"`
	AcceptedDescription string `json:"accepted_description_visible,omitempty"`
}

func definitionOf(t *mcp.Tool) (Definition, string, error) {
	schema, err := canonical(t.InputSchema)
	if err != nil {
		return Definition{}, "", err
	}
	d := Definition{Name: t.Name, Description: t.Description, InputSchema: schema}
	if t.Annotations != nil {
		if d.Annotations, err = canonical(t.Annotations); err != nil {
			return Definition{}, "", err
		}
	}
	body, err := canonical(d)
	if err != nil {
		return Definition{}, "", err
	}
	sum := sha256.Sum256(body)
	return d, hex.EncodeToString(sum[:]), nil
}

func canonical(v any) (json.RawMessage, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, err
	}
	return json.Marshal(generic)
}

// clean removes control characters and the invisible Unicode tag and bidirectional
// override characters that can hide text from a human reviewer but not from a model.
func clean(s string, limit int) string {
	s = strings.Map(func(r rune) rune {
		if hidden(r) {
			return -1
		}
		return r
	}, s)
	return truncate(s, limit)
}

// Visible shows every hidden character as an escape, so a reviewer sees what a model would.
func Visible(s string) string {
	var b strings.Builder
	for _, r := range s {
		if hidden(r) {
			fmt.Fprintf(&b, "\\u{%X}", r)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func hidden(r rune) bool {
	if r == '\n' || r == '\t' {
		return false
	}
	return unicode.IsControl(r) || r >= 0xE0000 && r <= 0xE007F || r >= 0x202A && r <= 0x202E ||
		r >= 0x2066 && r <= 0x2069 || r >= 0x200B && r <= 0x200F || r == 0xFEFF
}

func cleanSchema(raw json.RawMessage) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var walk func(any) any
	walk = func(v any) any {
		switch v := v.(type) {
		case map[string]any:
			for k, inner := range v {
				if s, ok := inner.(string); ok && (k == "description" || k == "title") {
					v[k] = clean(s, 500)
				} else {
					v[k] = walk(inner)
				}
			}
		case []any:
			for i := range v {
				v[i] = walk(v[i])
			}
		}
		return v
	}
	return json.Marshal(walk(v))
}

type store struct {
	db     *sql.DB
	now    func() time.Time
	outbox *notify.Outbox
}

func newStore(db *sql.DB, now func() time.Time) *store {
	a := &assignments.Store{DB: db, Now: now}
	return &store{db: db, now: now, outbox: &notify.Outbox{DB: db, Assignments: a, Now: now}}
}

type listed struct {
	def  Definition
	hash string
}

// sync records what the server lists now and returns the tools that need the owner's review.
func (st *store) sync(ctx context.Context, server, instructions string, tools map[string]listed) ([]string, error) {
	now := st.now()
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO mcp_servers (server, instructions, seen_at) VALUES (?, ?, ?)
		ON CONFLICT (server) DO UPDATE SET instructions = excluded.instructions, seen_at = excluded.seen_at`,
		server, instructions, now.UnixMilli()); err != nil {
		return nil, err
	}
	known := map[string]Pin{}
	rows, err := tx.QueryContext(ctx, `SELECT tool, status, definition_hash, COALESCE(accepted_hash, '') FROM mcp_tools WHERE server = ?`, server)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p Pin
		if err := rows.Scan(&p.Tool, &p.Status, &p.Hash, &p.AcceptedHash); err != nil {
			rows.Close()
			return nil, err
		}
		known[p.Tool] = p
	}
	rows.Close()

	var review []string
	for name, l := range tools {
		body, _ := json.Marshal(l.def)
		old, seen := known[name]
		status := StatusNew
		if old.AcceptedHash == l.hash {
			status = StatusAccepted
		} else if old.AcceptedHash != "" {
			status = StatusChanged
		}
		if status != StatusAccepted {
			review = append(review, name)
		}
		switch {
		case !seen:
			_, err = tx.ExecContext(ctx, `INSERT INTO mcp_tools (server, tool, definition_hash, definition_json, status, first_seen_at, changed_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`, server, name, l.hash, string(body), status, now.UnixMilli(), now.UnixMilli())
			if err == nil {
				err = audit.Record(ctx, tx, now, "", "mcp", "mcp.tool_seen", server+"__"+name, map[string]string{"server": server, "tool": name, "hash": l.hash})
			}
		case old.Hash != l.hash || old.Status != status:
			_, err = tx.ExecContext(ctx, `UPDATE mcp_tools SET definition_hash = ?, definition_json = ?, status = ?, changed_at = ?
				WHERE server = ? AND tool = ?`, l.hash, string(body), status, now.UnixMilli(), server, name)
			if err == nil && old.Hash != l.hash {
				err = audit.Record(ctx, tx, now, "", "mcp", "mcp.tool_changed", server+"__"+name,
					map[string]string{"server": server, "tool": name, "from": old.Hash, "to": l.hash, "accepted": old.AcceptedHash})
			}
		}
		if err != nil {
			return nil, err
		}
	}
	for name, old := range known {
		if _, still := tools[name]; still || old.Status == StatusRemoved {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE mcp_tools SET status = 'removed', changed_at = ? WHERE server = ? AND tool = ?`,
			now.UnixMilli(), server, name); err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, now, "", "mcp", "mcp.tool_removed", server+"__"+name, map[string]string{"server": server, "tool": name}); err != nil {
			return nil, err
		}
	}
	sort.Strings(review)
	return review, tx.Commit()
}

func (st *store) accepted(ctx context.Context, server string) (map[string]string, error) {
	rows, err := st.db.QueryContext(ctx, `SELECT tool, accepted_hash FROM mcp_tools WHERE server = ? AND accepted_hash IS NOT NULL`, server)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var tool, hash string
		if err := rows.Scan(&tool, &hash); err != nil {
			return nil, err
		}
		out[tool] = hash
	}
	return out, rows.Err()
}

// notifyReview tells each assignment whose policy names one of the tools, once per change set.
func (st *store) notifyReview(ctx context.Context, server string, review []string, hashes map[string]listed) error {
	if len(review) == 0 {
		return nil
	}
	h := sha256.New()
	for _, name := range review {
		fmt.Fprintf(h, "%s=%s;", name, hashes[name].hash)
	}
	return st.notifyAssignments(ctx, server, review, "mcp-review:"+server+":"+hex.EncodeToString(h.Sum(nil))[:16],
		fmt.Sprintf("Review tools from MCP server %q", server),
		fmt.Sprintf("New or changed: %s. They will not run until you accept them: agentd mcp review %s", strings.Join(review, ", "), server))
}

// notifyAssignments notifies every live assignment whose policy names one of the server's
// tools (any of them when tools is nil).
func (st *store) notifyAssignments(ctx context.Context, server string, tools []string, key, title, body string) error {
	rows, err := st.db.QueryContext(ctx, `SELECT id FROM assignments WHERE status IN ('active', 'paused')`)
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
		a, err := st.outbox.Assignments.Get(ctx, id)
		if err != nil {
			return err
		}
		uses := false
		for name := range a.Policy.Tools {
			tool, ok := strings.CutPrefix(name, server+"__")
			if ok && (tools == nil || slices.Contains(tools, tool)) {
				uses = true
				break
			}
		}
		if !uses {
			continue
		}
		if _, err := st.outbox.Notify(ctx, notify.Notification{AssignmentID: id, DedupKey: key, Severity: assignments.SeverityAction,
			Title: title, Body: body}); err != nil {
			return err
		}
	}
	return nil
}

func ListPins(ctx context.Context, db *sql.DB, server string) ([]Pin, error) {
	rows, err := db.QueryContext(ctx, `SELECT server, tool, status, definition_hash, definition_json, COALESCE(accepted_hash, ''),
		accepted_json, accepted_at, COALESCE(accepted_by, ''), changed_at FROM mcp_tools
		WHERE ? = '' OR server = ? ORDER BY server, tool`, server, server)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Pin{}
	for rows.Next() {
		var p Pin
		var def string
		var accepted sql.NullString
		var acceptedAt sql.NullInt64
		var changed int64
		if err := rows.Scan(&p.Server, &p.Tool, &p.Status, &p.Hash, &def, &p.AcceptedHash, &accepted, &acceptedAt, &p.AcceptedBy, &changed); err != nil {
			return nil, err
		}
		p.Definition = json.RawMessage(def)
		if accepted.Valid {
			p.Accepted = json.RawMessage(accepted.String)
		}
		if acceptedAt.Valid {
			t := time.UnixMilli(acceptedAt.Int64)
			p.AcceptedAt = &t
		}
		p.ChangedAt = time.UnixMilli(changed)
		p.Description = visibleDescription(p.Definition)
		p.AcceptedDescription = visibleDescription(p.Accepted)
		out = append(out, p)
	}
	return out, rows.Err()
}

func visibleDescription(raw json.RawMessage) string {
	var d Definition
	if len(raw) == 0 || json.Unmarshal(raw, &d) != nil {
		return ""
	}
	return Visible(d.Description)
}

func Instructions(ctx context.Context, db *sql.DB, server string) (string, error) {
	var s string
	err := db.QueryRowContext(ctx, `SELECT instructions FROM mcp_servers WHERE server = ?`, server).Scan(&s)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return s, err
}

// Accept records that the owner reviewed exactly the definition with this hash.
func Accept(ctx context.Context, db *sql.DB, now time.Time, server, tool, hash, by string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE mcp_tools SET status = 'accepted', accepted_hash = definition_hash,
		accepted_json = definition_json, accepted_at = ?, accepted_by = ?
		WHERE server = ? AND tool = ? AND definition_hash = ? AND status != 'removed'`,
		now.UnixMilli(), by, server, tool, hash)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%s__%s: %w", server, tool, ErrHashMismatch)
	}
	if err := audit.Record(ctx, tx, now, "", by, "mcp.tool_accepted", server+"__"+tool,
		map[string]string{"server": server, "tool": tool, "hash": hash}); err != nil {
		return err
	}
	return tx.Commit()
}

func notifyNotification(assignmentID, key, title, body string) notify.Notification {
	return notify.Notification{AssignmentID: assignmentID, DedupKey: key, Severity: assignments.SeverityAction, Title: title, Body: body}
}
