package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"agentd/internal/chat"
	"agentd/internal/model"
)

func replies(req model.Request) model.Message {
	return model.Message{Content: "hello from the front desk"}
}

func (e *env) conversation() string {
	e.t.Helper()
	var list []struct{ ID string }
	if code := e.do("GET", "/api/conversations", nil, &list); code != http.StatusOK || len(list) == 0 {
		e.t.Fatalf("conversations = %d %v", code, list)
	}
	return list[0].ID
}

func (e *env) post(conv string, body map[string]any) (int, chat.Message) {
	e.t.Helper()
	var out struct {
		Message chat.Message `json:"message"`
		Error   string       `json:"error"`
	}
	code := e.do("POST", "/api/conversations/"+conv+"/messages", body, &out)
	return code, out.Message
}

type sse struct {
	id    string
	event string
	data  string
}

// stream reads events from a conversation stream until want arrive or the time is up.
func (e *env) stream(conv, lastID string, want int) []sse {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/api/conversations/"+conv+"/stream", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []sse
	var cur sse
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			cur.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			cur.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
		case line == "" && cur.event != "":
			out = append(out, cur)
			cur = sse{}
			if len(out) >= want {
				return out
			}
		}
	}
	return out
}

func messages(events []sse) []chat.Message {
	var out []chat.Message
	for _, ev := range events {
		if ev.event == "message" {
			var m chat.Message
			json.Unmarshal([]byte(ev.data), &m)
			out = append(out, m)
		}
	}
	return out
}

func TestChatNeedsAuthAndCSRF(t *testing.T) {
	e := setup(t, replies)
	conv := e.conversation()
	url := e.srv.URL + "/api/conversations/" + conv + "/messages"
	if resp, _ := http.Post(url, "application/json", strings.NewReader(`{"text":"hi"}`)); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("send without a token = %d", resp.StatusCode)
	}
	c, _ := browser(t, e)
	if code := send(t, c, "POST", url, nil); code != http.StatusUnauthorized {
		t.Fatalf("send from a session without CSRF = %d", code)
	}
}

func TestSameClientMessageIDMakesOneMessageAndOneTurn(t *testing.T) {
	e := setup(t, replies)
	conv := e.conversation()
	_, first := e.post(conv, map[string]any{"text": "hello", "client_msg_id": "c-1"})
	code, again := e.post(conv, map[string]any{"text": "hello", "client_msg_id": "c-1"})
	if code != http.StatusOK || again.ID != first.ID {
		t.Fatalf("retry = %d %s, want the same message %s", code, again.ID, first.ID)
	}
	var turns int
	e.app.DB.QueryRow(`SELECT COUNT(*) FROM tasks WHERE priority = 100`).Scan(&turns)
	if turns != 1 {
		t.Fatalf("turns = %d", turns)
	}
}

func TestOversizedMessageIsRefusedWithAReadableError(t *testing.T) {
	e := setup(t, replies)
	var out struct{ Error string }
	code := e.do("POST", "/api/conversations/"+e.conversation()+"/messages", map[string]any{"text": strings.Repeat("x", 40<<10)}, &out)
	if code != http.StatusRequestEntityTooLarge || !strings.Contains(out.Error, "limit is 32 KB") {
		t.Fatalf("oversized = %d %q", code, out.Error)
	}
}

func TestStreamResumesWithoutGapsOrDuplicates(t *testing.T) {
	e := setup(t, replies)
	conv := e.conversation()
	e.post(conv, map[string]any{"text": "/help"})
	first := messages(e.stream(conv, "", 2))
	if len(first) != 2 || first[0].Seq != 1 || first[1].Seq != 2 {
		t.Fatalf("first stream = %+v", first)
	}
	e.post(conv, map[string]any{"text": "/usage"})
	e.post(conv, map[string]any{"text": "/status"})
	rest := messages(e.stream(conv, "2", 4))
	var seqs []int64
	for _, m := range rest {
		seqs = append(seqs, m.Seq)
	}
	if len(seqs) != 4 || seqs[0] != 3 || seqs[3] != 6 {
		t.Fatalf("resumed seqs = %v, want 3..6", seqs)
	}
}

func TestAClientThatNeverReadsDoesNotDelayATurn(t *testing.T) {
	e := setup(t, replies)
	conv := e.conversation()
	for range 3 {
		req, _ := http.NewRequest("GET", e.srv.URL+"/api/conversations/"+conv+"/stream", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
	}
	for i := range 50 {
		e.post(conv, map[string]any{"text": "/help " + strings.Repeat("x", i)})
	}
	start := time.Now()
	e.post(conv, map[string]any{"text": "hello"})
	for {
		worked, err := e.app.Runtime.RunOnceInteractive(context.Background(), "w")
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			break
		}
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the turn took %s with clients that never read", d)
	}
	var reply int
	e.app.DB.QueryRow(`SELECT COUNT(*) FROM conversation_messages WHERE author = 'agent'`).Scan(&reply)
	if reply != 1 {
		t.Fatalf("replies = %d", reply)
	}
}

func TestCancelStopsTheTurnOnce(t *testing.T) {
	e := setup(t, replies)
	conv := e.conversation()
	e.post(conv, map[string]any{"text": "a long question"})
	if code := e.do("POST", "/api/conversations/"+conv+"/turn/cancel", nil, nil); code != http.StatusOK {
		t.Fatalf("cancel = %d", code)
	}
	if code := e.do("POST", "/api/conversations/"+conv+"/turn/cancel", nil, nil); code != http.StatusConflict {
		t.Fatalf("second cancel = %d, want 409", code)
	}
	var stopped int
	for _, m := range messages(e.stream(conv, "", 2)) {
		if m.Kind == chat.KindNotice && strings.Contains(m.Text(), "Stopped") {
			stopped++
		}
	}
	if stopped != 1 {
		t.Fatalf("cancelled events = %d", stopped)
	}
}

func TestChatResponsesNeverCarryTheToken(t *testing.T) {
	e := setup(t, replies)
	conv := e.conversation()
	e.post(conv, map[string]any{"text": "hello"})
	e.drain()
	for _, path := range []string{"/api/conversations", "/api/conversations/" + conv + "/messages", "/api/conversations/" + conv + "/away"} {
		var raw json.RawMessage
		e.do("GET", path, nil, &raw)
		if strings.Contains(string(raw), token) {
			t.Fatalf("%s returned the API token", path)
		}
	}
}
