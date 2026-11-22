package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"agentd/internal/gateway"
	"agentd/internal/policy"
	"agentd/internal/system"
	"agentd/internal/testutil"
	"agentd/internal/tools"
)

func childPid(t *testing.T, tool tools.Tool) int {
	t.Helper()
	res, err := call(t, tool, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ Content string }
	json.Unmarshal(res.Output, &out)
	pid, _ := strconv.Atoi(out.Content)
	return pid
}

func state(t *testing.T, s *Server) string {
	t.Helper()
	servers, err := ListServers(context.Background(), s.store.db)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range servers {
		if st.Server == s.Name {
			return st.State
		}
	}
	return ""
}

func supervised(t *testing.T, s *Server) {
	s.poll, s.backoff = 10*time.Millisecond, [2]time.Duration{time.Millisecond, 20 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.supervise(ctx)
}

func TestCrashedChildIsRestarted(t *testing.T) {
	s := child(t)
	found, err := s.start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	acceptAll(t, s.store.db, "child")
	pid := index(found)["child__pid"]
	supervised(t, s)
	first := childPid(t, pid)
	if state(t, s) != StateReady {
		t.Fatalf("state = %s, want ready", state(t, s))
	}

	syscall.Kill(first, syscall.SIGKILL)
	eventually(t, "a new child process", func() bool {
		if s.live() == nil {
			return false
		}
		res, err := call(t, pid, `{}`)
		return err == nil && !jsonContains(res.Output, strconv.Itoa(first))
	})
	if state(t, s) != StateReady {
		t.Fatalf("state after restart = %s, want ready", state(t, s))
	}
	var readies int
	s.store.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE operation = 'mcp.server_ready'`).Scan(&readies)
	if readies != 1 {
		t.Fatalf("mcp.server_ready audit entries = %d, want 1 (the restart; the start awaited review)", readies)
	}
}

func jsonContains(raw json.RawMessage, s string) bool {
	var out struct{ Content string }
	json.Unmarshal(raw, &out)
	return out.Content == s
}

func TestServerThatKeepsFailingStopsUntilRestarted(t *testing.T) {
	w := newWorldWith(t, newFake(), ToolFilter{}, policy.Policy{Tools: map[string]policy.Rule{"broken__x": {Mode: policy.Allow}}}, true)
	s := newServer("broken", ServerConfig{Command: "/usr/bin/false"}, t.TempDir(), newStore(w.app.DB, time.Now), quiet)
	var attempts atomic.Int32
	connect := s.connect
	s.connect = func(ctx context.Context) (mcp.Transport, error) {
		attempts.Add(1)
		return connect(ctx)
	}
	supervised(t, s)
	eventually(t, "the server to give up", func() bool { return s.gaveUp() })
	settled := attempts.Load()
	time.Sleep(100 * time.Millisecond)
	if attempts.Load() != settled {
		t.Fatalf("attempts kept growing after giving up: %d then %d", settled, attempts.Load())
	}
	if state(t, s) != StateFailed {
		t.Fatalf("state = %s, want failed", state(t, s))
	}
	if n := w.count(`SELECT COUNT(*) FROM notifications WHERE dedup_key = 'mcp-failed:broken'`); n != 1 {
		t.Fatalf("failure notifications = %d, want 1", n)
	}

	if err := RequestRestart(context.Background(), w.app.DB, "broken"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a restart attempt", func() bool { return attempts.Load() > settled })
}

func TestHungCallRestartsTheServer(t *testing.T) {
	s := child(t)
	s.config.CallTimeout = policy.Duration{Duration: 200 * time.Millisecond}
	found, err := s.start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	acceptAll(t, s.store.db, "child")
	supervised(t, s)
	all := index(found)
	before := childPid(t, all["child__pid"])

	if _, err := call(t, all["child__hang"], `{}`); err == nil || errors.Is(err, tools.ErrNotExecuted) {
		t.Fatalf("hung call err = %v, want an ambiguous error", err)
	}
	eventually(t, "the hung child to be replaced", func() bool {
		if s.live() == nil {
			return false
		}
		res, err := call(t, all["child__pid"], `{}`)
		return err == nil && !jsonContains(res.Output, strconv.Itoa(before))
	})
	if err := syscall.Kill(before, 0); err == nil {
		eventually(t, "the hung child to exit", func() bool { return syscall.Kill(before, 0) != nil })
	}
}

func TestServerDownAtStartupKeepsItsToolNames(t *testing.T) {
	db := testutil.Open(t, testutil.DBPath(t))
	f := newFake()
	first, _ := f.startOn(t, db, ToolFilter{}, true)
	first.close()

	var up atomic.Bool
	s := newServer("fake", ServerConfig{}, t.TempDir(), newStore(db, time.Now), quiet)
	s.pinTTL = 0
	s.connect = func(ctx context.Context) (mcp.Transport, error) {
		if !up.Load() {
			return nil, errors.New("connection refused")
		}
		client, server := mcp.NewInMemoryTransports()
		if _, err := f.srv.Connect(ctx, server, nil); err != nil {
			return nil, err
		}
		return client, nil
	}
	if _, err := s.start(context.Background()); err == nil {
		t.Fatal("start succeeded while the server was down")
	}
	s.failed(context.Background(), errors.New("connection refused"))
	restored := index(s.restore(context.Background()))
	echo := restored["fake__echo"]
	if echo == nil {
		t.Fatalf("restored tools = %v, want the names from the earlier run", restored)
	}
	if _, reason := echo.(tools.Pinned).Pin(); reason == "" || tools.Ready(echo) {
		t.Fatal("a tool of a down server is offered")
	}
	defer s.close()
	supervised(t, s)
	up.Store(true)
	eventually(t, "the server to come up", func() bool { return tools.Ready(echo) })
	if _, err := call(t, echo, `{"back":true}`); err != nil {
		t.Fatalf("restored tool after the server came up: %v", err)
	}
}

func TestConcurrentCallsAreLimitedPerServer(t *testing.T) {
	r := newRemote(t, true)
	s, all, err := r.server(t, ServerConfig{MaxConcurrent: 2, CallTimeout: policy.Duration{Duration: 100 * time.Millisecond}}, Vault{}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if cap(s.sem) != 2 {
		t.Fatalf("slots = %d, want max_concurrent", cap(s.sem))
	}
	s.sem <- struct{}{}
	s.sem <- struct{}{}
	if _, err := call(t, all["remote__echo"], `{}`); !errors.Is(err, tools.ErrNotExecuted) || r.calls.Load() != 0 {
		t.Fatalf("call with every slot taken = %v, server saw %d; want not executed and nothing sent", err, r.calls.Load())
	}
	<-s.sem
	if _, err := call(t, all["remote__echo"], `{}`); err != nil {
		t.Fatalf("call with a free slot: %v", err)
	}
}

func TestEmergencyStopSendsNothing(t *testing.T) {
	w := newWorld(t, ToolFilter{}, policy.Policy{Tools: map[string]policy.Rule{"fake__echo": {Mode: policy.Allow}}})
	if err := system.Halt(context.Background(), w.app.DB, time.Now(), "test", "stop"); err != nil {
		t.Fatal(err)
	}
	if out, _ := w.handle("fake__echo", `{}`); out.Kind != gateway.Paused {
		t.Fatalf("outcome while halted = %v, want paused", out.Kind)
	}
	if len(w.fake.recorded()) != 0 {
		t.Fatal("a call reached the server during an emergency stop")
	}
	system.Resume(context.Background(), w.app.DB, time.Now(), "test")
	if _, status := w.rehandle(1, "fake__echo", `{}`); status != gateway.Succeeded {
		t.Fatalf("after resume = %s, want succeeded", status)
	}
}

func TestStderrGoesToACappedFile(t *testing.T) {
	path := t.TempDir() + "/mcp-x.log"
	f := &cappedFile{path: path, limit: 100}
	for i := range 10 {
		fmt.Fprintf(f, "line %02d of server output\n", i)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() > 100 {
		t.Fatalf("log size = %v, %v; want at most 100 bytes", info, err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatal("the previous log was not kept")
	}
}

func TestToolsOfADroppedServerWait(t *testing.T) {
	s := child(t)
	found, err := s.start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	acceptAll(t, s.store.db, "child")
	pid := index(found)["child__pid"]
	if !tools.Ready(pid) {
		t.Fatal("accepted tool of a connected server is not offered")
	}
	syscall.Kill(childPid(t, pid), syscall.SIGKILL)
	eventually(t, "the session to drop", func() bool { return s.live() == nil })
	if _, reason := pid.(tools.Pinned).Pin(); reason == "" || tools.Ready(pid) {
		t.Fatalf("tool of a dropped server: reason %q, want it unavailable so calls wait", reason)
	}
}
