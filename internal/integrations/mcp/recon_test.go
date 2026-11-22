package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"agentd/internal/gateway"
	"agentd/internal/policy"
	"agentd/internal/system"
)

// chat is a fake messaging service: post stores a message, find looks one up by the
// client reference agentd injects. mode controls how post fails.
type chat struct {
	mu      sync.Mutex
	posts   map[string]string
	attempt int
	mode    string
	lookups int
}

func newChat() (*fake, *chat) {
	c := &chat{posts: map[string]string{}}
	f := newFake()
	f.add("post", "Post a message.", func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Text     string            `json:"text"`
			Metadata map[string]string `json:"metadata"`
		}
		json.Unmarshal(req.Params.Arguments, &in)
		c.mu.Lock()
		defer c.mu.Unlock()
		c.attempt++
		switch c.mode {
		case "fail-before":
			return nil, errors.New("connection reset")
		case "apply-then-fail":
			c.posts[in.Metadata["client_ref"]] = in.Text
			return nil, errors.New("connection reset after write")
		}
		c.posts[in.Metadata["client_ref"]] = in.Text
		return text("posted"), nil
	})
	f.add("find", "Find a message by client reference.", func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Ref string `json:"client_ref"`
		}
		json.Unmarshal(req.Params.Arguments, &in)
		c.mu.Lock()
		defer c.mu.Unlock()
		c.lookups++
		matches := []map[string]string{}
		if _, ok := c.posts[in.Ref]; ok {
			matches = append(matches, map[string]string{"id": "msg-" + in.Ref[len(in.Ref)-4:]})
		}
		b, _ := json.Marshal(map[string]any{"matches": matches})
		return text(string(b)), nil
	})
	return f, c
}

func (c *chat) count() (posts, lookups int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.posts), c.lookups
}

func reconciled(miss time.Duration) ToolFilter {
	r := &ReconcileRule{}
	r.Marker.Arg = "metadata.client_ref"
	r.Lookup.Tool, r.Lookup.FoundWhen, r.Lookup.ExternalID = "find", "matches.0", "matches.0.id"
	r.Lookup.Args = map[string]any{"client_ref": "{{marker}}"}
	r.MissIsAuthoritativeAfter = policy.Duration{Duration: miss}
	return ToolFilter{Meta: map[string]ToolMeta{"post": {Reconcile: r}, "find": {Effect: "read"}}}
}

var postRule = policy.Policy{Tools: map[string]policy.Rule{"fake__post": {Mode: policy.Allow}, "fake__echo": {Mode: policy.Allow},
	"fake__crash": {Mode: policy.Allow}},
	Limits: policy.Limits{ToolRates: map[string]policy.RateLimit{"fake__post": {Max: 1, Per: policy.Duration{Duration: time.Hour}}}}}

func TestLookupFindsAWriteThatHappenedBeforeTheError(t *testing.T) {
	f, c := newChat()
	w := newWorldWith(t, f, reconciled(0), postRule, true)
	c.mode = "apply-then-fail"
	out, status := w.handle("fake__post", `{"text":"hello"}`)
	if status != gateway.Succeeded {
		t.Fatalf("status = %s (%s), want succeeded through the lookup", status, out.Content)
	}
	var external string
	w.app.DB.QueryRow(`SELECT COALESCE(external_id, '') FROM actions WHERE tool_name = 'fake__post'`).Scan(&external)
	if posts, _ := c.count(); posts != 1 || !strings.HasPrefix(external, "msg-") {
		t.Fatalf("posts = %d, external id %q; want one post found by its marker", posts, external)
	}
	var stored string
	w.app.DB.QueryRow(`SELECT arguments_json FROM actions WHERE tool_name = 'fake__post'`).Scan(&stored)
	if strings.Contains(stored, "agentd-") {
		t.Fatal("the marker was written into the action's arguments; approvals would no longer match")
	}
	if n := w.count(`SELECT COUNT(*) FROM actions`); n != 1 {
		t.Fatalf("actions = %d, want the lookup to create none", n)
	}
}

func TestLookupMissIsNotProof(t *testing.T) {
	f, c := newChat()
	w := newWorldWith(t, f, reconciled(0), postRule, true)
	c.mode = "fail-before"
	if out, status := w.handle("fake__post", `{"text":"hello"}`); out.Kind != gateway.Blocked || status != gateway.NeedsReconciliation {
		t.Fatalf("miss = %v %s, want blocked for a decision", out.Kind, status)
	}
	if c.attempt != 1 {
		t.Fatalf("post attempts = %d, want no automatic retry", c.attempt)
	}

	c.mu.Lock()
	c.posts[markerFor("run_1:1:call_1")] = "hello"
	c.mu.Unlock()
	w.clock.Advance(2 * time.Minute)
	system.Halt(context.Background(), w.app.DB, w.clock.Now(), "test", "stop")
	_, before := c.count()
	if settled, err := w.app.Gateway.Recheck(context.Background()); err != nil || len(settled) != 0 {
		t.Fatalf("recheck while halted = %+v, %v", settled, err)
	}
	if _, after := c.count(); after != before {
		t.Fatal("a reconcile lookup ran during an emergency stop")
	}
	system.Resume(context.Background(), w.app.DB, w.clock.Now(), "test")
	settled, err := w.app.Gateway.Recheck(context.Background())
	if err != nil || len(settled) != 1 || !settled[0].Happened {
		t.Fatalf("recheck = %+v, %v; want the late write confirmed", settled, err)
	}
}

func TestAuthoritativeMissAfterAWhileMeansNotHappened(t *testing.T) {
	f, c := newChat()
	w := newWorldWith(t, f, reconciled(10*time.Minute), postRule, true)
	c.mode = "fail-before"
	if _, status := w.handle("fake__post", `{"text":"hello"}`); status != gateway.NeedsReconciliation {
		t.Fatalf("fresh miss = %s, want needs_reconciliation", status)
	}
	w.clock.Advance(5 * time.Minute)
	if settled, _ := w.app.Gateway.Recheck(context.Background()); len(settled) != 0 {
		t.Fatalf("a miss younger than miss_is_authoritative_after settled %+v", settled)
	}
	w.clock.Advance(6 * time.Minute)
	settled, err := w.app.Gateway.Recheck(context.Background())
	if err != nil || len(settled) != 1 || settled[0].Happened {
		t.Fatalf("recheck = %+v, %v; want settled as not happened", settled, err)
	}
	var status string
	w.app.DB.QueryRow(`SELECT status FROM actions WHERE tool_name = 'fake__post'`).Scan(&status)
	if status != gateway.Failed || c.attempt != 1 {
		t.Fatalf("status %s after %d attempts; want failed and no automatic retry", status, c.attempt)
	}
}

func TestMarkerIsStablePerActionAndRefusedFromTheModel(t *testing.T) {
	if markerFor("run:1:a") != markerFor("run:1:a") || markerFor("run:1:a") == markerFor("run:1:b") {
		t.Fatal("markers must repeat for one action and differ between actions")
	}
	f, c := newChat()
	w := newWorldWith(t, f, reconciled(0), postRule, true)
	if _, status := w.handle("fake__post", `{"text":"x","metadata":{"client_ref":"forged"}}`); status != gateway.Failed {
		t.Fatalf("model-supplied marker = %s, want refused", status)
	}
	if posts, _ := c.count(); posts != 0 {
		t.Fatal("a call with a forged marker reached the server")
	}
}

func TestIdempotentWriteMayRunAgain(t *testing.T) {
	f := newFake()
	w := newWorldWith(t, f, ToolFilter{Meta: map[string]ToolMeta{"crash": {Idempotent: true}}}, postRule, true)
	if _, status := w.handle("fake__crash", `{}`); status != gateway.Failed {
		t.Fatalf("idempotent write with an ambiguous error = %s, want failed (safe to retry)", status)
	}
	if n := w.count(`SELECT COUNT(*) FROM audit_log WHERE operation = 'action.assumed_idempotent'`); n != 1 {
		t.Fatalf("assumed_idempotent audit entries = %d, want 1", n)
	}
	w.handle("fake__crash", `{}`)
	if got := len(w.fake.recorded()); got != 2 {
		t.Fatalf("server calls = %d, want 2 (the retry ran)", got)
	}
}

func TestReconcileConfigValidation(t *testing.T) {
	base := func(meta string) string {
		return `{"servers":{"chat":{"command":"/x","tools":{"meta":` + meta + `}}}}`
	}
	good := `{"post":{"reconcile":{"marker":{"arg":"metadata.client_ref"},"lookup":{"tool":"find","args":{"client_ref":"{{marker}}"},"found_when":"matches.0"}}},"find":{"effect":"read"}}`
	write := func(body string) string {
		p := t.TempDir() + "/mcp.json"
		if err := writeFile(p, body); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if _, err := Load(write(base(good))); err != nil {
		t.Fatalf("valid reconcile config: %v", err)
	}
	for _, meta := range []string{
		`{"post":{"idempotent":true,"effect":"read"}}`,
		`{"post":{"idempotent":true,"reconcile":{"marker":{"arg":"a"},"lookup":{"tool":"find","args":{"r":"{{marker}}"},"found_when":"m"}}},"find":{"effect":"read"}}`,
		`{"post":{"reconcile":{"marker":{"arg":"a"},"lookup":{"tool":"find","args":{"r":"{{marker}}"},"found_when":"m"}}}}`,
		`{"post":{"reconcile":{"marker":{"arg":"a"},"lookup":{"tool":"find","args":{"r":"fixed"},"found_when":"m"}}},"find":{"effect":"read"}}`,
		`{"post":{"reconcile":{"marker":{"arg":"a"},"lookup":{"tool":"post","args":{"r":"{{marker}}"},"found_when":"m"}}}}`,
	} {
		if _, err := Load(write(base(meta))); err == nil {
			t.Errorf("Load accepted meta %s", meta)
		}
	}
}

func writeFile(path, body string) error { return os.WriteFile(path, []byte(body), 0o600) }
