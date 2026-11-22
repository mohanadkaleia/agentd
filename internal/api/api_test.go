package api_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentd/internal/api"
	"agentd/internal/app"
	"agentd/internal/assignments"
	"agentd/internal/integrations/mcp"
	"agentd/internal/model"
	"agentd/internal/model/scripted"
	"agentd/internal/notify"
	"agentd/internal/system"
	"agentd/internal/tasks"
	"agentd/internal/testutil"
	"agentd/internal/testutil/airline"
	"agentd/internal/tools"
)

const token = "test-token-0123456789abcdef0123456789abcdef"

type env struct {
	t     *testing.T
	app   *app.App
	srv   *httptest.Server
	clock *testutil.Clock
}

func setup(t *testing.T, script scripted.Provider) *env {
	clock := testutil.NewClock()
	db := testutil.Open(t, testutil.DBPath(t))
	mailbox, err := tools.NewMailbox(context.Background(), db, 0)
	if err != nil {
		t.Fatal(err)
	}
	a := app.New(db, script, "scripted", tools.NewRegistry(tools.WebSearch{}, mailbox), clock.Now)
	s := &api.Server{App: a, Token: token, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), StreamPoll: 10 * time.Millisecond}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &env{t: t, app: a, srv: srv, clock: clock}
}

func (e *env) do(method, path string, body any, out any) int {
	e.t.Helper()
	var r io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		r = strings.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		r = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, r)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			e.t.Fatalf("%s %s: decode: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

func (e *env) drain() {
	for range 50 {
		worked, _ := e.app.Runtime.RunOnce(context.Background(), "w")
		if !worked {
			return
		}
	}
}

var responsibility = map[string]any{
	"kind": "responsibility", "name": "watch", "objective": "Watch something.",
	"policy": map[string]any{
		"tools":  map[string]any{"notify": map[string]string{"mode": "allow"}},
		"limits": map[string]any{"max_llm_spend_usd": 5, "budget_period": "720h"},
	},
	"notifications": map[string]any{"min_severity": "attention", "dedup_window": "24h", "max_per_day": 3},
	"routes":        []any{map[string]any{"source": "feed", "kind": "item.changed", "coalesce_field": "subject", "title": "Look", "objective": "Look at it."}},
	"schedules":     []any{map[string]any{"every": "24h", "title": "Daily", "objective": "Summarise."}},
}

func TestAuthIsRequired(t *testing.T) {
	e := setup(t, airline.Script)
	for _, header := range []string{"", "Bearer wrong", "Basic " + token, "Bearer " + token + "x"} {
		req, _ := http.NewRequest("GET", e.srv.URL+"/api/assignments", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("Authorization %q -> %d, want 401", header, resp.StatusCode)
		}
	}
	empty := httptest.NewServer((&api.Server{App: e.app, Log: slog.Default()}).Handler())
	defer empty.Close()
	req, _ := http.NewRequest("GET", empty.URL+"/api/assignments", nil)
	req.Header.Set("Authorization", "Bearer ")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("a server without a configured token accepted a request")
	}
}

func TestAssignmentLifecycleOverHTTP(t *testing.T) {
	e := setup(t, airline.Script)
	var created struct{ ID string }
	if code := e.do("POST", "/api/assignments", responsibility, &created); code != http.StatusCreated || created.ID == "" {
		t.Fatalf("create -> %d %+v", code, created)
	}

	var detail map[string]any
	if code := e.do("GET", "/api/assignments/"+created.ID, nil, &detail); code != 200 {
		t.Fatalf("get -> %d", code)
	}
	if detail["kind"] != "responsibility" || detail["budget_period"] != "720h0m0s" || detail["next"].(map[string]any)["what"] != "scheduled check: Daily" {
		t.Fatalf("detail = %v", detail)
	}

	var list []map[string]any
	e.do("GET", "/api/assignments", nil, &list)
	if len(list) != 1 || list[0]["id"] != created.ID {
		t.Fatalf("list = %v", list)
	}

	var out map[string]any
	if code := e.do("POST", "/api/assignments/"+created.ID+"/pause", nil, &out); code != 200 || out["status"] != "paused" {
		t.Fatalf("pause -> %d %v", code, out)
	}
	if code := e.do("POST", "/api/assignments/"+created.ID+"/complete", nil, &out); code != http.StatusConflict {
		t.Fatalf("completing a responsibility -> %d, want 409", code)
	}
	if code := e.do("POST", "/api/assignments/"+created.ID+"/explode", nil, &out); code != http.StatusNotFound {
		t.Fatalf("unknown action -> %d, want 404", code)
	}
	if code := e.do("POST", "/api/assignments/"+created.ID+"/revoke", nil, &out); code != 200 || out["status"] != "revoked" {
		t.Fatalf("revoke -> %d %v", code, out)
	}
	if code := e.do("GET", "/api/assignments/rsp_missing", nil, &out); code != http.StatusNotFound {
		t.Fatalf("missing -> %d, want 404", code)
	}
}

func TestInputValidation(t *testing.T) {
	e := setup(t, airline.Script)
	var out map[string]any
	noBudget := map[string]any{}
	for k, v := range responsibility {
		noBudget[k] = v
	}
	noBudget["policy"] = map[string]any{"tools": map[string]any{}}
	cases := []struct {
		name, method, path string
		body               any
	}{
		{"unknown field", "POST", "/api/assignments", `{"kind":"project","name":"n","objective":"o","surprise":1}`},
		{"responsibility without budget", "POST", "/api/assignments", noBudget},
		{"bad kind", "POST", "/api/assignments", `{"kind":"vibe","name":"n","objective":"o"}`},
		{"trailing data", "POST", "/api/events", `{"source":"s","kind":"k"} {}`},
		{"payload not an object", "POST", "/api/events", `{"source":"s","kind":"k","payload":[1]}`},
		{"missing kind", "POST", "/api/events", `{"source":"s"}`},
		{"oversized body", "POST", "/api/events", `{"source":"s","kind":"k","payload":{"x":"` + strings.Repeat("a", 2<<20) + `"}}`},
		{"bad limit", "GET", "/api/tasks?limit=0", nil},
	}
	for _, c := range cases {
		if code := e.do(c.method, c.path, c.body, &out); code != http.StatusBadRequest {
			t.Errorf("%s -> %d (%v), want 400", c.name, code, out)
		}
	}
}

func TestEventsApprovalsTasksAndUsage(t *testing.T) {
	e := setup(t, airline.Script)
	ctx := context.Background()
	projectID, taskID, err := airline.Seed(ctx, e.app)
	if err != nil {
		t.Fatal(err)
	}

	var ev map[string]any
	body := map[string]any{"source": "gmail", "kind": "email.received", "external_id": "m1", "payload": map[string]string{"thread_id": "x"}}
	if code := e.do("POST", "/api/events", body, &ev); code != http.StatusAccepted || ev["duplicate"] != false {
		t.Fatalf("event -> %d %v", code, ev)
	}
	e.do("POST", "/api/events", body, &ev)
	if ev["duplicate"] != true {
		t.Fatalf("repeat event = %v, want duplicate", ev)
	}

	e.drain()
	var approvals []map[string]any
	e.do("GET", "/api/approvals", nil, &approvals)
	if len(approvals) != 1 || approvals[0]["tool"] != "send_email" || approvals[0]["assignment_id"] != projectID {
		t.Fatalf("approvals = %v", approvals)
	}
	id := approvals[0]["id"].(string)
	var out map[string]any
	if code := e.do("POST", "/api/approvals/"+id+"/approve", nil, &out); code != 200 {
		t.Fatalf("approve -> %d %v", code, out)
	}
	if code := e.do("POST", "/api/approvals/"+id+"/approve", nil, &out); code != http.StatusConflict {
		t.Fatalf("second approve -> %d, want 409", code)
	}
	e.drain()

	var task map[string]any
	e.do("GET", "/api/tasks/"+taskID, nil, &task)
	transcript := task["transcript"].([]any)
	if task["status"] != tasks.Completed || len(transcript) < 6 {
		t.Fatalf("task = %v (transcript %d)", task["status"], len(transcript))
	}
	for _, m := range transcript {
		if _, leaked := m.(map[string]any)["native"]; leaked {
			t.Fatal("provider-native payload leaked into the API transcript")
		}
	}
	var list []map[string]any
	e.do("GET", "/api/tasks?assignment="+projectID+"&status=ready", nil, &list)
	if len(list) != 1 || !strings.HasPrefix(list[0]["title"].(string), "Follow-up") {
		t.Fatalf("ready tasks = %v", list)
	}
	var usage []map[string]any
	e.do("GET", "/api/usage", nil, &usage)
	if len(usage) != 1 || usage[0]["model_calls"].(float64) < 5 {
		t.Fatalf("usage = %v", usage)
	}
	var activity []map[string]any
	e.do("GET", "/api/activity?assignment="+projectID+"&limit=5", nil, &activity)
	if len(activity) != 5 {
		t.Fatalf("activity = %d rows", len(activity))
	}
}

func TestNotificationsCanBeAcknowledged(t *testing.T) {
	e := setup(t, airline.Script)
	ctx := context.Background()
	id, _ := e.app.Assignments.CreateProject(ctx, assignments.NewProject{Name: "p", Objective: "o"})
	res, err := e.app.Outbox.Notify(ctx, notify.Notification{AssignmentID: id, DedupKey: "k", Severity: assignments.SeverityAction, Title: "t", Body: "b"})
	if err != nil {
		t.Fatal(err)
	}
	var list []map[string]any
	e.do("GET", "/api/notifications", nil, &list)
	if len(list) != 1 || list[0]["id"] != res.ID {
		t.Fatalf("notifications = %v", list)
	}
	var out map[string]any
	if code := e.do("POST", "/api/notifications/"+res.ID+"/delivered", nil, &out); code != 200 {
		t.Fatalf("ack -> %d", code)
	}
	if code := e.do("POST", "/api/notifications/"+res.ID+"/delivered", nil, &out); code != http.StatusConflict {
		t.Fatalf("second ack -> %d, want 409", code)
	}
	e.do("GET", "/api/notifications", nil, &list)
	if len(list) != 0 {
		t.Fatal("acknowledged notification still pending")
	}
}

func TestEmergencyStopHaltsMidRunAndResumes(t *testing.T) {
	var e *env
	halted := false
	e = setup(t, func(req model.Request) model.Message {
		if !halted {
			halted = true
			var out map[string]any
			if code := e.do("POST", "/api/system/emergency-stop", map[string]string{"reason": "testing"}, &out); code != 200 || out["halted"] != true {
				t.Errorf("emergency stop -> %d %v", code, out)
			}
		}
		return airline.Script(req)
	})
	ctx := context.Background()
	_, taskID, _ := airline.Seed(ctx, e.app)

	e.drain()
	task, _ := e.app.Tasks.Get(ctx, taskID)
	if task.Status != tasks.Ready {
		t.Fatalf("task after halt = %s, want ready", task.Status)
	}
	var n int
	e.app.DB.QueryRow(`SELECT COUNT(*) FROM actions WHERE status != 'prepared'`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d actions progressed past prepared while halted", n)
	}
	if worked, _ := e.app.Runtime.RunOnce(ctx, "w"); worked {
		t.Fatal("a task was claimed while halted")
	}
	if st, _ := system.Get(ctx, e.app.DB); !st.Halted || st.Reason != "testing" {
		t.Fatalf("system status = %+v", st)
	}

	var out map[string]any
	if code := e.do("POST", "/api/system/resume", nil, &out); code != 200 || out["halted"] != false {
		t.Fatalf("resume -> %d %v", code, out)
	}
	e.drain()
	task, _ = e.app.Tasks.Get(ctx, taskID)
	if task.Status != tasks.WaitingApproval {
		t.Fatalf("task after resume = %s, want waiting_approval", task.Status)
	}
	var calls int
	e.app.DB.QueryRow(`SELECT COUNT(*) FROM model_usage`).Scan(&calls)
	if calls != 3 {
		t.Fatalf("model calls = %d, want 3 (the halted step is not repeated)", calls)
	}
}

func TestActivityStreamDeliversNewEntries(t *testing.T) {
	e := setup(t, airline.Script)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/api/events/stream", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q", ct)
	}

	if _, err := e.app.Assignments.CreateProject(context.Background(), assignments.NewProject{Name: "streamed", Objective: "o"}); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(resp.Body)
	var id, data string
	for scanner.Scan() {
		line := scanner.Text()
		if v, ok := strings.CutPrefix(line, "id: "); ok {
			id = v
		}
		if v, ok := strings.CutPrefix(line, "data: "); ok {
			data = v
			break
		}
	}
	if id == "" || !strings.Contains(data, `"operation":"project.created"`) {
		t.Fatalf("stream gave id=%q data=%q", id, data)
	}
}

func TestRetryAndResolveErrors(t *testing.T) {
	e := setup(t, airline.Script)
	_, taskID, _ := airline.Seed(context.Background(), e.app)
	var out map[string]any
	if code := e.do("POST", "/api/tasks/"+taskID+"/retry", map[string]string{"note": "go"}, &out); code != http.StatusConflict {
		t.Fatalf("retrying a ready task -> %d %v, want 409", code, out)
	}
	if code := e.do("POST", "/api/actions/act_missing/resolve", map[string]any{"note": "x"}, &out); code != http.StatusBadRequest {
		t.Fatalf("resolve without happened -> %d, want 400", code)
	}
	if code := e.do("POST", "/api/actions/act_missing/resolve", map[string]any{"happened": true}, &out); code != http.StatusNotFound {
		t.Fatalf("resolve unknown action -> %d, want 404", code)
	}
	var unresolved []map[string]any
	if code := e.do("GET", "/api/actions/unresolved", nil, &unresolved); code != 200 || len(unresolved) != 0 {
		t.Fatalf("unresolved -> %d %v", code, unresolved)
	}
}

func TestShowReportsHalt(t *testing.T) {
	e := setup(t, airline.Script)
	projectID, _, _ := airline.Seed(context.Background(), e.app)
	var out map[string]any
	e.do("POST", "/api/system/emergency-stop", map[string]string{"reason": "drill"}, &out)
	var detail map[string]any
	e.do("GET", "/api/assignments/"+projectID, nil, &detail)
	if detail["next"].(map[string]any)["what"] != "halted (emergency stop): drill" {
		t.Fatalf("next while halted = %v", detail["next"])
	}
}

func TestMCPToolsAreReviewedOverHTTP(t *testing.T) {
	e := setup(t, nil)
	if _, err := e.app.DB.Exec(`INSERT INTO mcp_tools (server, tool, definition_hash, definition_json, status, first_seen_at, changed_at)
		VALUES ('chat', 'post', 'h1', '{"name":"post","inputSchema":{"type":"object"}}', 'new', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	var pins []mcp.Pin
	if code := e.do("GET", "/api/mcp/tools", nil, &pins); code != 200 || len(pins) != 1 || pins[0].Status != mcp.StatusNew {
		t.Fatalf("list = %d %+v", code, pins)
	}
	if code := e.do("POST", "/api/mcp/tools/chat/post/accept", map[string]string{}, nil); code != 400 {
		t.Fatalf("accept without a hash = %d, want 400", code)
	}
	if code := e.do("POST", "/api/mcp/tools/chat/post/accept", map[string]string{"definition_hash": "stale"}, nil); code != 409 {
		t.Fatalf("accept with a stale hash = %d, want 409", code)
	}
	if code := e.do("POST", "/api/mcp/tools/chat/post/accept", map[string]string{"definition_hash": "h1"}, nil); code != 200 {
		t.Fatalf("accept = %d, want 200", code)
	}
	e.do("GET", "/api/mcp/tools?server=chat", nil, &pins)
	if pins[0].Status != mcp.StatusAccepted || pins[0].AcceptedBy != "api" {
		t.Fatalf("after accept = %+v", pins[0])
	}
}

func TestMCPServersOverHTTP(t *testing.T) {
	e := setup(t, nil)
	req, _ := http.NewRequest("GET", e.srv.URL+"/api/mcp/servers", nil)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 401 {
		t.Fatalf("servers without a token = %v %v, want 401", resp.StatusCode, err)
	}
	if code := e.do("POST", "/api/mcp/servers/nope/restart", nil, nil); code != 404 {
		t.Fatalf("restart of an unknown server = %d, want 404", code)
	}
	e.app.DB.Exec(`INSERT INTO mcp_servers (server, seen_at, state, transport, last_error) VALUES ('chat', 0, 'failed', 'stdio', 'boom')`)
	var servers []mcp.ServerStatus
	if code := e.do("GET", "/api/mcp/servers", nil, &servers); code != 200 || len(servers) != 1 || servers[0].State != "failed" {
		t.Fatalf("servers = %d %+v", code, servers)
	}
	if code := e.do("POST", "/api/mcp/servers/chat/restart", nil, nil); code != 200 {
		t.Fatalf("restart = %d", code)
	}
	var flag int
	e.app.DB.QueryRow(`SELECT restart_requested FROM mcp_servers WHERE server = 'chat'`).Scan(&flag)
	if flag != 1 {
		t.Fatal("restart was not requested")
	}
}
