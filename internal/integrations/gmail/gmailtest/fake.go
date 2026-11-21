package gmailtest

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"strings"
	"sync"
	"testing"

	"agentd/internal/integrations/gmail"
)

type Message struct {
	ID, Thread, From, To, Subject, MessageID, Body string
	Labels                                         []string
}

type Fake struct {
	mu        sync.Mutex
	messages  map[string]*Message
	order     []string
	drafts    map[string]string
	history   []string
	historyID int
	Expired   bool
	FailSend  bool
	SentLags  bool
	SendCalls int
	nextID    int
}

func New(t *testing.T) (*Fake, *gmail.Client) {
	f := &Fake{messages: map[string]*Message{}, drafts: map[string]string{}, historyID: 100}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, &gmail.Client{HTTP: srv.Client(), BaseURL: srv.URL}
}

func (f *Fake) Receive(from, subject, body string) string {
	return f.Reply("", from, subject, body)
}

func (f *Fake) Reply(to, from, subject, body string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("in%d", f.nextID)
	thread := "t-" + id
	if m, ok := f.messages[to]; ok {
		thread = m.Thread
	}
	f.messages[id] = &Message{ID: id, Thread: thread, From: from, To: "me@example.com", Subject: subject,
		MessageID: "<" + id + "@mail.example>", Body: body, Labels: []string{"INBOX", "UNREAD"}}
	f.order = append(f.order, id)
	f.history = append(f.history, id)
	f.historyID++
	return id
}

func (f *Fake) Sent() []*Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*Message
	for _, id := range f.order {
		m := f.messages[id]
		for _, l := range m.Labels {
			if l == "SENT" {
				out = append(out, m)
			}
		}
	}
	return out
}

func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	q := r.URL.Query()
	switch {
	case path == "/profile":
		writeJSON(w, map[string]string{"emailAddress": "me@example.com", "historyId": fmt.Sprint(f.historyID)})
	case path == "/history":
		if f.Expired {
			http.Error(w, `{"error":{"code":404}}`, 404)
			return
		}
		var since int
		fmt.Sscan(q.Get("startHistoryId"), &since)
		var entries []any
		for i, id := range f.history {
			if 101+i > since {
				m := f.messages[id]
				entries = append(entries, map[string]any{"messagesAdded": []any{map[string]any{"message": map[string]string{"id": m.ID, "threadId": m.Thread}}}})
			}
		}
		writeJSON(w, map[string]any{"history": entries, "historyId": fmt.Sprint(f.historyID)})
	case path == "/messages" && r.Method == "GET":
		var refs []any
		for _, id := range f.order {
			if f.matches(f.messages[id], q.Get("q")) {
				refs = append(refs, map[string]string{"id": id, "threadId": f.messages[id].Thread})
			}
		}
		writeJSON(w, map[string]any{"messages": refs})
	case path == "/messages/send":
		f.SendCalls++
		if f.FailSend {
			f.store(r, []string{"SENT"})
			http.Error(w, "upstream timeout", 503)
			return
		}
		writeJSON(w, map[string]string{"id": f.store(r, []string{"SENT"}).ID})
	case path == "/drafts" && r.Method == "POST":
		var body struct {
			Message json.RawMessage `json:"message"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		r.Body = readCloser(body.Message)
		m := f.store(r, []string{"DRAFT"})
		f.drafts["d-"+m.ID] = m.ID
		writeJSON(w, map[string]string{"id": "d-" + m.ID})
	case path == "/drafts" && r.Method == "GET":
		var drafts []any
		for d, id := range f.drafts {
			if f.matches(f.messages[id], q.Get("q")) {
				drafts = append(drafts, map[string]string{"id": d})
			}
		}
		writeJSON(w, map[string]any{"drafts": drafts})
	case strings.HasPrefix(path, "/messages/"):
		m, ok := f.messages[strings.TrimPrefix(path, "/messages/")]
		if !ok {
			http.Error(w, "not found", 404)
			return
		}
		writeJSON(w, f.render(m, q.Get("format") == "full"))
	case strings.HasPrefix(path, "/threads/"):
		thread := strings.TrimPrefix(path, "/threads/")
		var msgs []any
		for _, id := range f.order {
			if f.messages[id].Thread == thread {
				msgs = append(msgs, f.render(f.messages[id], true))
			}
		}
		writeJSON(w, map[string]any{"messages": msgs})
	default:
		http.Error(w, "unexpected "+r.Method+" "+path, 500)
	}
}

func (f *Fake) matches(m *Message, q string) bool {
	for _, term := range strings.Fields(q) {
		switch {
		case term == "in:sent":
			if f.SentLags || !has(m.Labels, "SENT") {
				return false
			}
		case term == "in:inbox":
			if !has(m.Labels, "INBOX") {
				return false
			}
		case strings.HasPrefix(term, "rfc822msgid:"):
			if strings.Trim(m.MessageID, "<>") != strings.TrimPrefix(term, "rfc822msgid:") {
				return false
			}
		case strings.HasPrefix(term, "from:"):
			if !strings.Contains(m.From, strings.TrimPrefix(term, "from:")) {
				return false
			}
		}
	}
	return true
}

func (f *Fake) store(r *http.Request, labels []string) *Message {
	var body struct {
		Raw      string `json:"raw"`
		ThreadID string `json:"threadId"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	raw, _ := base64.URLEncoding.DecodeString(body.Raw)
	parsed, _ := mail.ReadMessage(strings.NewReader(string(raw)))
	f.nextID++
	id := fmt.Sprintf("out%d", f.nextID)
	thread := body.ThreadID
	if thread == "" {
		thread = "t-" + id
	}
	m := &Message{ID: id, Thread: thread, Labels: labels}
	if parsed != nil {
		m.To, m.Subject, m.MessageID = parsed.Header.Get("To"), parsed.Header.Get("Subject"), parsed.Header.Get("Message-Id")
		m.From = parsed.Header.Get("From") + "|in-reply-to=" + parsed.Header.Get("In-Reply-To")
	}
	f.messages[id] = m
	f.order = append(f.order, id)
	return m
}

func (f *Fake) render(m *Message, full bool) map[string]any {
	headers := []map[string]string{{"name": "From", "value": m.From}, {"name": "To", "value": m.To},
		{"name": "Subject", "value": m.Subject}, {"name": "Message-ID", "value": m.MessageID}}
	payload := map[string]any{"mimeType": "multipart/alternative", "headers": headers}
	if full {
		payload["parts"] = []any{
			map[string]any{"mimeType": "text/html", "body": map[string]string{"data": base64.URLEncoding.EncodeToString([]byte("<p>html</p>"))}},
			map[string]any{"mimeType": "text/plain; charset=UTF-8", "body": map[string]string{"data": base64.URLEncoding.EncodeToString([]byte(m.Body))}},
		}
	}
	return map[string]any{"id": m.ID, "threadId": m.Thread, "labelIds": m.Labels, "snippet": firstWords(m.Body),
		"internalDate": "1795200000000", "payload": payload}
}

func firstWords(s string) string {
	if len(s) > 40 {
		return s[:40]
	}
	return s
}

func has(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

type rc struct{ *strings.Reader }

func (rc) Close() error { return nil }

func readCloser(b []byte) rc { return rc{strings.NewReader(string(b))} }

func (f *Fake) ThreadOf(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m, ok := f.messages[id]; ok {
		return m.Thread
	}
	return ""
}

func (f *Fake) Sends() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.SendCalls
}
