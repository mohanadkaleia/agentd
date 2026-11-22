package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"agentd/internal/policy"
	"agentd/internal/scheduler"
	"agentd/internal/system"
)

type channel struct {
	mu       sync.Mutex
	messages []map[string]any
	calls    int
	broken   bool
}

func (c *channel) post(ts, user, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages = append(c.messages, map[string]any{"ts": ts, "user": user, "text": text, "channel": "C1", "secret": "not-mapped"})
}

func newChannel() (*fake, *channel) {
	c := &channel{}
	f := newFake()
	f.add("read_channel", "Recent messages.", func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.calls++
		if c.broken {
			return nil, errors.New("upstream unavailable")
		}
		b, _ := json.Marshal(map[string]any{"messages": c.messages})
		return text(string(b)), nil
	})
	return f, c
}

var channelPoll = Poll{Every: policy.Duration{Duration: time.Minute}, Tool: "read_channel", Items: "messages", ID: "ts",
	Event: PollEvent{Source: "chat", Kind: "message.posted"}, MaxEvents: 5,
	Payload: map[string]string{"channel": "channel", "user": "user", "text": "text"}, Exclude: map[string]string{"user": "BOT"}}

func channelFilter() ToolFilter {
	return ToolFilter{Meta: map[string]ToolMeta{"read_channel": {Effect: "read"}}}
}

func (w *world) events() []map[string]string {
	w.t.Helper()
	rows, err := w.app.DB.Query(`SELECT payload_json FROM events WHERE source = 'chat' ORDER BY rowid`)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]string
	for rows.Next() {
		var body string
		rows.Scan(&body)
		var m map[string]string
		json.Unmarshal([]byte(body), &m)
		out = append(out, m)
	}
	return out
}

func (w *world) poll(p Poll) (int, error) {
	w.t.Helper()
	return w.server.pollOnce(context.Background(), p, w.app.Scheduler)
}

func TestPollEmitsOnlyNewItemsAndOnlyMappedFields(t *testing.T) {
	f, c := newChannel()
	w := newWorldWith(t, f, channelFilter(), policy.Policy{}, true)
	c.post("1", "U1", "old")
	if n, err := w.poll(channelPoll); n != 0 || err != nil {
		t.Fatalf("first poll emitted %d, %v; want nothing (no backfill)", n, err)
	}
	c.post("2", "U1", "hello")
	c.post("3", "BOT", "my own echo")
	if n, err := w.poll(channelPoll); n != 1 || err != nil {
		t.Fatalf("second poll emitted %d, %v; want the one new message", n, err)
	}
	if n, _ := w.poll(channelPoll); n != 0 {
		t.Fatalf("an overlapping poll emitted %d, want 0", n)
	}
	events := w.events()
	if len(events) != 1 || events[0]["text"] != "hello" || events[0]["secret"] != "" {
		t.Fatalf("events = %v, want one with only the mapped fields", events)
	}

	again := newServer("fake", ServerConfig{Tools: channelFilter()}, t.TempDir(), w.server.store, quiet)
	again.connect, again.pinTTL = w.server.connect, 0
	if _, err := again.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer again.close()
	c.post("4", "U2", "after restart")
	if n, _ := again.pollOnce(context.Background(), channelPoll, w.app.Scheduler); n != 1 {
		t.Fatalf("poll after a restart emitted %d, want exactly the new message", n)
	}
}

func TestPollCatchUpIsBoundedAndGroupedIntoOneTask(t *testing.T) {
	f, c := newChannel()
	w := newWorldWith(t, f, channelFilter(), policy.Policy{}, true)
	if _, err := w.app.Scheduler.AddRoute(context.Background(), scheduler.Route{AssignmentID: w.asg, Source: "chat", Kind: "message.posted",
		Match: map[string]string{"channel": "C1"}, CoalesceField: "channel", Title: "Reply", Objective: "Read the channel"}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.app.Scheduler.AddRoute(context.Background(), scheduler.Route{AssignmentID: w.asg, Source: "chat", Kind: "message.posted",
		Match: map[string]string{"channel": "C9"}, Title: "Other", Objective: "Never"}); err != nil {
		t.Fatal(err)
	}
	w.poll(channelPoll)
	for i := range 200 {
		c.post(fmt.Sprint(100+i), "U1", "burst")
	}
	if n, err := w.poll(channelPoll); n != channelPoll.MaxEvents {
		t.Fatalf("one poll after an outage emitted %d (%v), want at most %d", n, err, channelPoll.MaxEvents)
	}
	if err := w.app.Scheduler.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	var tasks int
	w.app.DB.QueryRow(`SELECT COUNT(*) FROM tasks WHERE assignment_id = ? AND title = 'Reply'`, w.asg).Scan(&tasks)
	var other int
	w.app.DB.QueryRow(`SELECT COUNT(*) FROM tasks WHERE title = 'Other'`).Scan(&other)
	if tasks != 1 || other != 0 {
		t.Fatalf("tasks = %d matching, %d non-matching; want the burst grouped into one and none for the other route", tasks, other)
	}
	if n, _ := w.poll(channelPoll); n != channelPoll.MaxEvents {
		t.Fatalf("the next poll emitted %d, want the next %d", n, channelPoll.MaxEvents)
	}
}

func TestPollsPauseWhenTheyMayNotRun(t *testing.T) {
	f, c := newChannel()
	w := newWorldWith(t, f, channelFilter(), policy.Policy{}, false)
	if _, err := w.poll(channelPoll); !errors.Is(err, errPaused) || c.calls != 0 {
		t.Fatalf("poll of an unreviewed tool = %v after %d calls, want paused with none", err, c.calls)
	}
	acceptAll(t, w.app.DB, "fake")
	system.Halt(context.Background(), w.app.DB, time.Now(), "test", "stop")
	if _, err := w.poll(channelPoll); !errors.Is(err, errPaused) || c.calls != 0 {
		t.Fatalf("poll during an emergency stop = %v after %d calls, want paused with none", err, c.calls)
	}
	system.Resume(context.Background(), w.app.DB, time.Now(), "test")
	w.server.needSignIn("revoked", "x")
	if _, err := w.poll(channelPoll); !errors.Is(err, errPaused) || c.calls != 0 {
		t.Fatalf("poll while the server needs sign-in = %v after %d calls, want paused with none", err, c.calls)
	}
}

func TestFailingFeedNotifiesThatItIsFailing(t *testing.T) {
	f, c := newChannel()
	w := newWorldWith(t, f, channelFilter(), policy.Policy{}, true)
	w.app.Scheduler.AddRoute(context.Background(), scheduler.Route{AssignmentID: w.asg, Source: "chat", Kind: "message.posted", Title: "t", Objective: "o"})
	c.broken = true
	for range 5 {
		_, err := w.poll(channelPoll)
		if err == nil {
			t.Fatal("a broken feed polled without error")
		}
		w.server.pollFailed(context.Background(), channelPoll, err)
	}
	var title, body string
	n := w.count(`SELECT COUNT(*) FROM notifications WHERE dedup_key LIKE 'mcp-poll-failing:%'`)
	w.app.DB.QueryRow(`SELECT title, body FROM notifications WHERE dedup_key LIKE 'mcp-poll-failing:%'`).Scan(&title, &body)
	if n != 1 || !strings.Contains(title, "failing") || !strings.Contains(body, "not a quiet one") {
		t.Fatalf("notifications = %d (%q / %q), want one saying the feed is failing", n, title, body)
	}
	polls, _ := ListPolls(context.Background(), w.app.DB)
	if len(polls) != 1 || polls[0].Failures != 5 || polls[0].LastError == "" {
		t.Fatalf("poll status = %+v", polls)
	}
}

func TestPollWithAWrongItemsPathFails(t *testing.T) {
	f, _ := newChannel()
	w := newWorldWith(t, f, channelFilter(), policy.Policy{}, true)
	typo := channelPoll
	typo.Items = "mesages"
	if _, err := w.poll(typo); err == nil || errors.Is(err, errPaused) {
		t.Fatalf("poll with a wrong items path = %v, want an error rather than a quiet feed", err)
	}
	if _, err := w.poll(channelPoll); err != nil {
		t.Fatalf("an empty channel (null list) must poll cleanly: %v", err)
	}
}

func TestPollConfigValidation(t *testing.T) {
	write := func(body string) string {
		p := t.TempDir() + "/mcp.json"
		writeFile(p, body)
		return p
	}
	ok := `{"servers":{"chat":{"command":"/x","tools":{"meta":{"read":{"effect":"read"}}},
		"poll":[{"every":"2m","tool":"read","items":"messages","id":"ts","event":{"source":"chat","kind":"message.posted"},"payload":{"user":"user"},"exclude":{"user":"BOT"}}]}}}`
	if _, err := Load(write(ok)); err != nil {
		t.Fatalf("valid poll: %v", err)
	}
	for _, bad := range []string{
		strings.Replace(ok, `"effect":"read"`, `"effect":"write"`, 1),
		strings.Replace(ok, `"2m"`, `"1s"`, 1),
		strings.Replace(ok, `"exclude":{"user":"BOT"}`, `"exclude":{"channel":"C1"}`, 1),
		strings.Replace(ok, `"source":"chat"`, `"source":"Chat Feed"`, 1),
	} {
		if _, err := Load(write(bad)); err == nil {
			t.Errorf("Load accepted %s", bad)
		}
	}
}
