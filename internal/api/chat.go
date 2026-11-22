package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agentd/internal/chat"
)

// webChannel is how messages from the command center are recorded.
const webChannel = "web"

const maxMessageText = 32 << 10

type conversationRow struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Model    string    `json:"model,omitempty"`
	LastAt   time.Time `json:"last_at"`
	LastSeq  int64     `json:"last_seq"`
	LastRead int64     `json:"last_read"`
	Unread   int       `json:"unread"`
}

func (s *Server) listConversations(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	if _, err := s.App.Chat.Default(ctx); err != nil {
		return err
	}
	rows, err := s.App.DB.QueryContext(ctx, `SELECT c.id, c.title, COALESCE(c.model, ''), c.updated_at,
		COALESCE((SELECT MAX(seq) FROM conversation_messages m WHERE m.conversation_id = c.id), 0),
		COALESCE((SELECT seen_seq FROM conversation_reads cr WHERE cr.conversation_id = c.id AND cr.channel = ?), 0)
		FROM conversations c WHERE c.status = 'open' ORDER BY c.updated_at DESC, c.rowid DESC LIMIT 50`, webChannel)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []conversationRow{}
	for rows.Next() {
		var c conversationRow
		var at int64
		if err := rows.Scan(&c.ID, &c.Title, &c.Model, &at, &c.LastSeq, &c.LastRead); err != nil {
			return err
		}
		c.LastAt, c.Unread = time.UnixMilli(at), int(max(c.LastSeq-c.LastRead, 0))
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return ok(w, out)
}

func (s *Server) newConversation(w http.ResponseWriter, r *http.Request) error {
	id, err := s.App.Chat.NewConversation(r.Context(), "")
	if err != nil {
		return err
	}
	return ok(w, map[string]string{"id": id})
}

func (s *Server) conversationMessages(w http.ResponseWriter, r *http.Request) error {
	after, err := int64Param(r, "after", 0)
	if err != nil {
		return err
	}
	limit, err := limitParam(r, 200, 500)
	if err != nil {
		return err
	}
	msgs, err := s.App.Chat.Messages(r.Context(), r.PathValue("id"), after, limit)
	if err != nil {
		return err
	}
	return ok(w, msgs)
}

func (s *Server) away(w http.ResponseWriter, r *http.Request) error {
	msgs, err := s.App.Chat.Away(r.Context(), r.PathValue("id"), webChannel)
	if err != nil {
		return err
	}
	if msgs == nil {
		msgs = []chat.Message{}
	}
	return ok(w, msgs)
}

func (s *Server) postMessage(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Text        string `json:"text"`
		ClientMsgID string `json:"client_msg_id"`
		ReplyTo     string `json:"reply_to"`
	}
	if err := decode(r, &body); err != nil {
		return err
	}
	if len(body.Text) > maxMessageText {
		return statusError{http.StatusRequestEntityTooLarge,
			fmt.Sprintf("the message is %d KB; the limit is %d KB. Send a shorter excerpt.", len(body.Text)>>10, maxMessageText>>10)}
	}
	m, err := s.App.Chat.Post(r.Context(), chat.Post{ConversationID: r.PathValue("id"), Text: body.Text, Channel: webChannel,
		ClientMsgID: body.ClientMsgID, ReplyTo: body.ReplyTo})
	switch {
	case errors.Is(err, chat.ErrEmpty):
		return badRequest("the message is empty")
	case errors.Is(err, chat.ErrNotFound):
		return statusError{http.StatusNotFound, "no such conversation"}
	case err != nil:
		return err
	}
	return ok(w, map[string]any{"message": m, "turn_task_id": m.TurnTaskID})
}

func (s *Server) cancelTurn(w http.ResponseWriter, r *http.Request) error {
	err := s.App.Chat.Cancel(r.Context(), r.PathValue("id"), "owner:"+webChannel)
	if errors.Is(err, chat.ErrNoTurn) {
		return statusError{http.StatusConflict, err.Error()}
	}
	if err != nil {
		return err
	}
	return ok(w, map[string]string{"status": "cancelled"})
}

func (s *Server) markRead(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Seq int64 `json:"seq"`
	}
	if err := decode(r, &body); err != nil {
		return err
	}
	if err := s.App.Chat.MarkRead(r.Context(), r.PathValue("id"), webChannel, body.Seq); err != nil {
		return err
	}
	return ok(w, map[string]int64{"seq": body.Seq})
}

// conversationStream pushes a conversation as it happens: new messages, cards whose status
// changed, tool activity and whether a turn is working. It wakes on in-process changes and
// polls as a fallback; event ids are message sequence numbers, so a reconnect with
// Last-Event-ID resumes without gaps or duplicates.
func (s *Server) conversationStream(w http.ResponseWriter, r *http.Request) error {
	flusher, okFlush := w.(http.Flusher)
	if !okFlush {
		return statusError{http.StatusInternalServerError, "streaming unsupported"}
	}
	ctx := r.Context()
	conv := r.PathValue("id")
	after, err := int64Param(r, "after", 0)
	if err != nil {
		return err
	}
	if id := r.Header.Get("Last-Event-ID"); id != "" {
		if after, err = strconv.ParseInt(id, 10, 64); err != nil {
			return badRequest("invalid Last-Event-ID")
		}
	}
	var exists int
	if err := s.App.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversations WHERE id = ?`, conv).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return statusError{http.StatusNotFound, "no such conversation"}
	}
	changed, unsubscribe := s.App.Chat.Subscribe()
	defer unsubscribe()
	streamed, unsubscribeDeltas := s.App.Chat.SubscribeDeltas()
	defer unsubscribeDeltas()
	turns := map[string]bool{}
	mine := func(task string) bool {
		if v, ok := turns[task]; ok {
			return v
		}
		turns[task] = s.App.Chat.ConversationOf(ctx, task) == conv
		return turns[task]
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	poll := s.StreamPoll
	if poll == 0 {
		poll = time.Second
	}
	open := map[int64]chat.Message{}
	actions := map[string]string{}
	working := false
	quiet := time.Now()
	for {
		wrote := false
		send := func(event string, id int64, v any) {
			body, _ := json.Marshal(v)
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", id, event, body)
			wrote = true
		}
		msgs, err := s.App.Chat.Messages(ctx, conv, after, 200)
		if err != nil {
			if ctx.Err() == nil {
				s.Log.Error("conversation stream", "err", err)
			}
			return nil
		}
		for _, m := range msgs {
			after = m.Seq
			send("message", after, m)
			if m.Status == "pending" || m.Status == "open" {
				open[m.Seq] = m
			}
		}
		if len(open) > 0 {
			cards := make([]chat.Message, 0, len(open))
			for _, m := range open {
				cards = append(cards, m)
			}
			if err := s.App.Chat.Live(ctx, cards); err != nil {
				return nil
			}
			for _, m := range cards {
				if m.Status != open[m.Seq].Status {
					send("message", after, m)
					delete(open, m.Seq)
				}
			}
		}
		state, tools, err := s.turnActivity(r, conv, actions)
		if err != nil {
			return nil
		}
		for _, t := range tools {
			send("turn", after, t)
		}
		if state != working {
			working = state
			send("turn", after, map[string]any{"working": working})
		}
		if wrote {
			quiet = time.Now()
			flusher.Flush()
		} else if time.Since(quiet) > 15*time.Second {
			quiet = time.Now()
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		}
		for waiting := true; waiting; {
			select {
			case <-ctx.Done():
				return nil
			case d := <-streamed:
				if !mine(d.TaskID) {
					continue
				}
				body, _ := json.Marshal(d)
				event := "delta"
				if d.Restarted {
					event = "turn"
				}
				fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", after, event, body)
				flusher.Flush()
				quiet = time.Now()
			case <-changed:
				waiting = false
			case <-time.After(poll):
				waiting = false
			}
		}
	}
}

// turnActivity reports whether a turn of the conversation is working, and tool calls its
// open turns made since last seen.
func (s *Server) turnActivity(r *http.Request, conv string, seen map[string]string) (bool, []map[string]string, error) {
	rows, err := s.App.DB.QueryContext(r.Context(), `SELECT t.id, t.status, COALESCE(a.id, ''), COALESCE(a.tool_name, ''), COALESCE(a.status, '')
		FROM tasks t LEFT JOIN actions a ON a.task_id = t.id
		WHERE t.id IN (SELECT turn_task_id FROM conversation_messages WHERE conversation_id = ? AND author = 'owner')
		AND t.status IN ('ready', 'running', 'retryable') ORDER BY a.created_at`, conv)
	if err != nil {
		return false, nil, err
	}
	defer rows.Close()
	working := false
	var out []map[string]string
	for rows.Next() {
		var task, status string
		var id, tool, state sql.NullString
		if err := rows.Scan(&task, &status, &id, &tool, &state); err != nil {
			return false, nil, err
		}
		working = working || status == "running"
		if id.String == "" || seen[id.String] == state.String {
			continue
		}
		seen[id.String] = state.String
		out = append(out, map[string]string{"turn_task_id": task, "action_id": id.String, "tool": tool.String, "status": state.String})
	}
	return working, out, rows.Err()
}

type modelRow struct {
	ID    string `json:"id"`
	Alias string `json:"alias,omitempty"`
	Local bool   `json:"local"`
}

func (s *Server) listModels(w http.ResponseWriter, r *http.Request) error {
	out := struct {
		Models []modelRow        `json:"models"`
		Tiers  map[string]string `json:"tiers"`
	}{Models: []modelRow{}, Tiers: map[string]string{}}
	for _, c := range s.App.Runtime.Catalogue() {
		if c.ID != "" {
			out.Models = append(out.Models, modelRow{ID: c.ID, Alias: c.Alias, Local: c.Local})
		}
	}
	for t, m := range s.App.Runtime.Tiers {
		out.Tiers[t] = m
	}
	return ok(w, out)
}

func (s *Server) setTaskModel(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Model string `json:"model"`
	}
	if err := decode(r, &body); err != nil {
		return err
	}
	msg, err := s.App.Runtime.SetTaskModel(r.Context(), r.PathValue("id"), body.Model, "owner:"+webChannel)
	if err != nil && !errors.Is(err, sql.ErrNoRows) && !strings.Contains(err.Error(), "not found") {
		return badRequest("%s", err.Error())
	}
	if err != nil {
		return err
	}
	return ok(w, map[string]string{"message": msg})
}

func (s *Server) setConversationModel(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Model string `json:"model"`
	}
	if err := decode(r, &body); err != nil {
		return err
	}
	msg, err := s.App.Chat.SetConversationModel(r.Context(), r.PathValue("id"), body.Model, "owner:"+webChannel)
	if errors.Is(err, chat.ErrNotFound) {
		return statusError{http.StatusNotFound, "no such conversation"}
	}
	if err != nil {
		return badRequest("%s", err.Error())
	}
	return ok(w, map[string]string{"message": msg})
}
