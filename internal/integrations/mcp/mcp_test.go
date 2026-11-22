package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"agentd/internal/app"
	"agentd/internal/demo"
	"agentd/internal/gateway"
	"agentd/internal/model"
	"agentd/internal/model/scripted"
	"agentd/internal/policy"
	"agentd/internal/testutil"
	"agentd/internal/tools"
)

func TestMain(m *testing.M) {
	if os.Getenv("AGENTD_FAKE_MCP") == "1" {
		runChild()
		return
	}
	os.Exit(m.Run())
}

func runChild() {
	srv := mcp.NewServer(&mcp.Implementation{Name: "child", Version: "1"}, nil)
	srv.AddTool(&mcp.Tool{Name: "env", InputSchema: object}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return text(strings.Join(os.Environ(), "\n")), nil
	})
	srv.AddTool(&mcp.Tool{Name: "hang", InputSchema: object}, func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		os.WriteFile("hanging", []byte(fmt.Sprint(os.Getpid())), 0o600)
		<-ctx.Done()
		return text("unreachable"), nil
	})
	srv.Run(context.Background(), &mcp.StdioTransport{})
}

var object = map[string]any{"type": "object"}

func noop(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { return text(""), nil }

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type fake struct {
	srv   *mcp.Server
	mu    sync.Mutex
	calls []string
}

func newFake() *fake {
	f := &fake{srv: mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "1"}, nil)}
	f.add("echo", "Echo the arguments back.", func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if strings.Contains(string(req.Params.Arguments), "slow") {
			time.Sleep(50 * time.Millisecond)
		}
		return text(string(req.Params.Arguments)), nil
	})
	f.add("big", strings.Repeat("d", 5000), func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return text(strings.Repeat("x", 30000)), nil
	})
	f.add("fail", "Always fails.", func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "channel not found"}}}, nil
	})
	f.add("caps", "Reports what the client offered.", func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		b, err := json.Marshal(req.Session.InitializeParams().Capabilities)
		return text(string(b)), err
	})
	return f
}

func (f *fake) add(name, description string, h mcp.ToolHandler) {
	f.srv.AddTool(&mcp.Tool{Name: name, Description: description, InputSchema: object},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			f.mu.Lock()
			f.calls = append(f.calls, name+" "+string(req.Params.Arguments))
			f.mu.Unlock()
			return h(ctx, req)
		})
}

func (f *fake) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fake) start(t *testing.T, filter ToolFilter) map[string]tools.Tool {
	t.Helper()
	s := newServer("fake", ServerConfig{Tools: filter}, t.TempDir(), quiet)
	s.connect = func(ctx context.Context) (mcp.Transport, error) {
		client, server := mcp.NewInMemoryTransports()
		if _, err := f.srv.Connect(ctx, server, nil); err != nil {
			return nil, err
		}
		return client, nil
	}
	found, err := s.start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.close)
	return index(found)
}

func index(found []tools.Tool) map[string]tools.Tool {
	out := map[string]tools.Tool{}
	for _, t := range found {
		out[t.Definition().Name] = t
	}
	return out
}

func call(t *testing.T, tool tools.Tool, args string) (tools.Result, error) {
	t.Helper()
	return tool.Execute(context.Background(), tools.Invocation{Name: tool.Definition().Name, Arguments: json.RawMessage(args)})
}

func TestDiscoveryRegistersPrefixedTools(t *testing.T) {
	all := newFake().start(t, ToolFilter{})
	for _, name := range []string{"fake__echo", "fake__big", "fake__fail", "fake__caps"} {
		if all[name] == nil {
			t.Fatalf("missing %s in %v", name, all)
		}
	}
	if d := all["fake__big"].Definition().Description; len(d) > maxDescription+20 || !strings.HasSuffix(d, "[truncated]") {
		t.Fatalf("long description not capped: %d chars", len(d))
	}

	some := newFake().start(t, ToolFilter{Include: []string{"echo", "fail"}, Exclude: []string{"fail"}})
	if len(some) != 1 || some["fake__echo"] == nil {
		t.Fatalf("filtered tools = %v, want only fake__echo", some)
	}
}

func TestToolsWithUnusableDefinitionsAreSkipped(t *testing.T) {
	f := &fake{srv: mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "1"}, nil)}
	f.srv.AddTool(&mcp.Tool{Name: "ok", InputSchema: object}, noop)
	f.srv.AddTool(&mcp.Tool{Name: strings.Repeat("n", 70), InputSchema: object}, noop)
	f.srv.AddTool(&mcp.Tool{Name: "huge", InputSchema: map[string]any{"type": "object", "description": strings.Repeat("s", maxSchemaBytes)}}, noop)
	found := f.start(t, ToolFilter{})
	if len(found) != 1 || found["fake__ok"] == nil {
		t.Fatalf("registered %v, want only fake__ok", found)
	}
}

func TestResultsAreLabelledAndCapped(t *testing.T) {
	all := newFake().start(t, ToolFilter{})
	res, err := call(t, all["fake__big"], `{}`)
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ Note, Content string }
	json.Unmarshal(res.Output, &out)
	if !strings.Contains(out.Note, "untrusted") || len(out.Content) > maxOutput+20 || !strings.HasSuffix(out.Content, "[truncated]") {
		t.Fatalf("output note %q, content %d chars; want labelled and capped", out.Note, len(out.Content))
	}
}

func TestClientOffersServersNoModelFilesOrPrompts(t *testing.T) {
	all := newFake().start(t, ToolFilter{})
	res, err := call(t, all["fake__caps"], `{}`)
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ Content string }
	json.Unmarshal(res.Output, &out)
	for _, offered := range []string{"sampling", "elicitation", "listChanged"} {
		if strings.Contains(out.Content, offered) {
			t.Fatalf("client capabilities = %s, must not offer %s", out.Content, offered)
		}
	}
}

func TestConcurrentCallsGetTheirOwnResults(t *testing.T) {
	echo := newFake().start(t, ToolFilter{})["fake__echo"]
	var wg sync.WaitGroup
	var wrong atomic.Int32
	for i := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := call(t, echo, fmt.Sprintf(`{"n":%d,"mode":"slow"}`, i))
			if err != nil || !strings.Contains(string(res.Output), fmt.Sprintf(`\"n\":%d`, i)) {
				wrong.Add(1)
			}
		}()
	}
	wg.Wait()
	if wrong.Load() != 0 {
		t.Fatalf("%d of 3 concurrent calls got the wrong result", wrong.Load())
	}
}

type world struct {
	t    *testing.T
	app  *app.App
	fake *fake
	task string
	asg  string
	seq  int
}

func newWorld(t *testing.T, rules map[string]policy.Rule) *world {
	f := newFake()
	found := f.start(t, ToolFilter{})
	registry := tools.Registry{}
	for name, tool := range found {
		registry[name] = tool
	}
	db := testutil.Open(t, testutil.DBPath(t))
	idle := scripted.Provider(func(model.Request) model.Message { return model.Message{Content: "done"} })
	a := app.New(db, idle, "scripted", registry, testutil.NewClock().Now)
	asg, task, err := demo.Seed(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Assignments.SetPolicy(context.Background(), asg, policy.Policy{Tools: rules}, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO runs (id, task_id, model_id, status, started_at) VALUES ('run_1', ?, 'scripted', 'running', 0)`, task); err != nil {
		t.Fatal(err)
	}
	return &world{t: t, app: a, fake: f, task: task, asg: asg}
}

func (w *world) handle(tool, args string) (gateway.Outcome, string) {
	w.t.Helper()
	w.seq++
	id := fmt.Sprintf("call_%d", w.seq)
	out, err := w.app.Gateway.Handle(context.Background(), gateway.Call{RunID: "run_1", TaskID: w.task, AssignmentID: w.asg, Seq: w.seq,
		Call: model.ToolCall{ID: id, Name: tool, Arguments: json.RawMessage(args)}})
	if err != nil {
		w.t.Fatal(err)
	}
	var status string
	w.app.DB.QueryRow(`SELECT status FROM actions WHERE idempotency_key = ?`, fmt.Sprintf("run_1:%d:%s", w.seq, id)).Scan(&status)
	return out, status
}

func TestPolicyGatesMCPTools(t *testing.T) {
	w := newWorld(t, map[string]policy.Rule{"fake__echo": {Mode: policy.Allow}, "fake__fail": {Mode: policy.Allow}})

	if _, status := w.handle("fake__big", `{}`); status != gateway.Denied {
		t.Fatalf("call without a rule = %s, want denied", status)
	}
	if got := w.fake.recorded(); len(got) != 0 {
		t.Fatalf("server received %v for a denied call", got)
	}

	out, status := w.handle("fake__echo", `{"to":"C123","text":"hi"}`)
	if status != gateway.Succeeded || !strings.Contains(out.Content, "untrusted") {
		t.Fatalf("allowed call = %s %s", status, out.Content)
	}
	if got := w.fake.recorded(); len(got) != 1 || got[0] != `echo {"to":"C123","text":"hi"}` {
		t.Fatalf("server received %v, want the exact arguments once", got)
	}

	if out, status := w.handle("fake__fail", `{}`); out.Kind != gateway.Blocked || status != gateway.NeedsReconciliation {
		t.Fatalf("write reporting an error = %v %s, want blocked for a decision", out.Kind, status)
	}
}

func child(t *testing.T) *Server {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-must-not-leak")
	return newServer("child", ServerConfig{Command: exe, Dir: t.TempDir(), Env: map[string]string{"AGENTD_FAKE_MCP": "1"}}, t.TempDir(), quiet)
}

func TestChildGetsOnlyConfiguredEnvironment(t *testing.T) {
	s := child(t)
	found, err := s.start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	res, err := call(t, index(found)["child__env"], `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(res.Output), "sk-must-not-leak") || !strings.Contains(string(res.Output), "AGENTD_FAKE_MCP=1") {
		t.Fatalf("child environment = %s", res.Output)
	}
}

func TestChildKilledMidCallIsAmbiguousThenNotConnected(t *testing.T) {
	s := child(t)
	found, err := s.start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	hang := index(found)["child__hang"]

	done := make(chan error, 1)
	go func() {
		_, err := call(t, hang, `{}`)
		done <- err
	}()
	marker := filepath.Join(s.config.Dir, "hanging")
	var pid int
	for range 200 {
		if b, err := os.ReadFile(marker); err == nil {
			fmt.Sscan(string(b), &pid)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("child never started the call")
	}
	syscall.Kill(pid, syscall.SIGKILL)

	err = <-done
	if err == nil || errors.Is(err, tools.ErrNotExecuted) {
		t.Fatalf("killed mid-call err = %v, want an ambiguous error", err)
	}
	if _, f, _ := hang.Reconcile(context.Background(), tools.Invocation{}); f != tools.Unknown {
		t.Fatalf("reconcile = %v, want unknown", f)
	}
	for range 200 {
		if s.live() == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := call(t, hang, `{}`); !errors.Is(err, tools.ErrNotExecuted) {
		t.Fatalf("call to a dead server err = %v, want ErrNotExecuted", err)
	}
}

func TestConfigValidation(t *testing.T) {
	write := func(body string, mode os.FileMode) string {
		p := filepath.Join(t.TempDir(), "mcp.json")
		os.WriteFile(p, []byte(body), mode)
		os.Chmod(p, mode)
		return p
	}
	ok := `{"servers":{"slack":{"command":"/opt/slack-mcp","env":{"TOKEN":"x"},"call_timeout":"30s"}}}`
	if c, err := Load(write(ok, 0o600)); err != nil || c.Servers["slack"].CallTimeout.Duration != 30*time.Second {
		t.Fatalf("valid config: %v", err)
	}
	for body, mode := range map[string]os.FileMode{
		ok:                                       0o644,
		`{"servers":{"a__b":{"command":"/x"}}}`:  0o600,
		`{"servers":{"Slack":{"command":"/x"}}}`: 0o600,
		`{"servers":{"slack_":{"command":"/x"}}}`:                  0o600,
		`{"servers":{"slack":{"command":"npx"}}}`:                  0o600,
		`{"servers":{"slack":{"command":"/x"}}}`:                   0o666,
		`{"servers":{"slack":{"command":"/x","y":1}}}`:             0o600,
		`{"servers":{"slack":{"command":"/x","env":{"A B":"1"}}}}`: 0o600,
	} {
		if _, err := Load(write(body, mode)); err == nil {
			t.Errorf("Load accepted %s (mode %o)", body, mode)
		}
	}
}
