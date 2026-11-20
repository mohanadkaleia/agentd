package gmail_test

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

type fakeMessage struct {
	id, thread, from, to, subject, messageID, body string
	labels                                         []string
}

type fakeGmail struct {
	mu        sync.Mutex
	messages  map[string]*fakeMessage
	order     []string
	drafts    map[string]string
	history   []string
	historyID int
	expired   bool
	failSend  bool
	sendCalls int
	nextID    int
}

func newFake(t *testing.T) (*fakeGmail, *gmail.Client) {
	f := &fakeGmail{messages: map[string]*fakeMessage{}, drafts: map[string]string{}, historyID: 100}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, &gmail.Client{HTTP: srv.Client(), BaseURL: srv.URL}
}

func (f *fakeGmail) receive(from, subject, body string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("in%d", f.nextID)
	f.messages[id] = &fakeMessage{id: id, thread: "t-" + id, from: from, to: "me@example.com", subject: subject,
		messageID: "<" + id + "@mail.example>", body: body, labels: []string{"INBOX", "UNREAD"}}
	f.order = append(f.order, id)
	f.history = append(f.history, id)
	f.historyID++
	return id
}

func (f *fakeGmail) sent() []*fakeMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*fakeMessage
	for _, id := range f.order {
		m := f.messages[id]
		for _, l := range m.labels {
			if l == "SENT" {
				out = append(out, m)
			}
		}
	}
	return out
}

func (f *fakeGmail) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	q := r.URL.Query()
	switch {
	case path == "/profile":
		writeJSON(w, map[string]string{"emailAddress": "me@example.com", "historyId": fmt.Sprint(f.historyID)})
	case path == "/history":
		if f.expired {
			http.Error(w, `{"error":{"code":404}}`, 404)
			return
		}
		var since int
		fmt.Sscan(q.Get("startHistoryId"), &since)
		var entries []any
		for i, id := range f.history {
			if 101+i > since {
				m := f.messages[id]
				entries = append(entries, map[string]any{"messagesAdded": []any{map[string]any{"message": map[string]string{"id": m.id, "threadId": m.thread}}}})
			}
		}
		writeJSON(w, map[string]any{"history": entries, "historyId": fmt.Sprint(f.historyID)})
	case path == "/messages" && r.Method == "GET":
		var refs []any
		for _, id := range f.order {
			if f.matches(f.messages[id], q.Get("q")) {
				refs = append(refs, map[string]string{"id": id, "threadId": f.messages[id].thread})
			}
		}
		writeJSON(w, map[string]any{"messages": refs})
	case path == "/messages/send":
		f.sendCalls++
		if f.failSend {
			f.store(r, []string{"SENT"})
			http.Error(w, "upstream timeout", 503)
			return
		}
		writeJSON(w, map[string]string{"id": f.store(r, []string{"SENT"}).id})
	case path == "/drafts" && r.Method == "POST":
		var body struct {
			Message json.RawMessage `json:"message"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		r.Body = readCloser(body.Message)
		m := f.store(r, []string{"DRAFT"})
		f.drafts["d-"+m.id] = m.id
		writeJSON(w, map[string]string{"id": "d-" + m.id})
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
			if f.messages[id].thread == thread {
				msgs = append(msgs, f.render(f.messages[id], true))
			}
		}
		writeJSON(w, map[string]any{"messages": msgs})
	default:
		http.Error(w, "unexpected "+r.Method+" "+path, 500)
	}
}

func (f *fakeGmail) matches(m *fakeMessage, q string) bool {
	for _, term := range strings.Fields(q) {
		switch {
		case term == "in:sent":
			if !has(m.labels, "SENT") {
				return false
			}
		case term == "in:inbox":
			if !has(m.labels, "INBOX") {
				return false
			}
		case strings.HasPrefix(term, "rfc822msgid:"):
			if strings.Trim(m.messageID, "<>") != strings.TrimPrefix(term, "rfc822msgid:") {
				return false
			}
		case strings.HasPrefix(term, "from:"):
			if !strings.Contains(m.from, strings.TrimPrefix(term, "from:")) {
				return false
			}
		}
	}
	return true
}

func (f *fakeGmail) store(r *http.Request, labels []string) *fakeMessage {
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
	m := &fakeMessage{id: id, thread: thread, labels: labels}
	if parsed != nil {
		m.to, m.subject, m.messageID = parsed.Header.Get("To"), parsed.Header.Get("Subject"), parsed.Header.Get("Message-Id")
		m.from = parsed.Header.Get("From") + "|in-reply-to=" + parsed.Header.Get("In-Reply-To")
	}
	f.messages[id] = m
	f.order = append(f.order, id)
	return m
}

func (f *fakeGmail) render(m *fakeMessage, full bool) map[string]any {
	headers := []map[string]string{{"name": "From", "value": m.from}, {"name": "To", "value": m.to},
		{"name": "Subject", "value": m.subject}, {"name": "Message-ID", "value": m.messageID}}
	payload := map[string]any{"mimeType": "multipart/alternative", "headers": headers}
	if full {
		payload["parts"] = []any{
			map[string]any{"mimeType": "text/html", "body": map[string]string{"data": base64.URLEncoding.EncodeToString([]byte("<p>html</p>"))}},
			map[string]any{"mimeType": "text/plain; charset=UTF-8", "body": map[string]string{"data": base64.URLEncoding.EncodeToString([]byte(m.body))}},
		}
	}
	return map[string]any{"id": m.id, "threadId": m.thread, "labelIds": m.labels, "snippet": firstWords(m.body),
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
