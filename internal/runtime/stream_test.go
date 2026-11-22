package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentd/internal/app"
	"agentd/internal/assignments"
	"agentd/internal/chat"
	"agentd/internal/model"
	"agentd/internal/model/scripted"
	"agentd/internal/runtime"
	"agentd/internal/system"
	"agentd/internal/tasks"
	"agentd/internal/testutil"
	"agentd/internal/tools"
)

// counting wraps a streaming model and counts how each call was made.
type counting struct {
	scripted.Streaming
	streams, completes atomic.Int32
	onDelta            func()
}

func (c *counting) Complete(ctx context.Context, req model.Request) (model.Response, error) {
	c.completes.Add(1)
	return c.Streaming.Complete(ctx, req)
}

func (c *counting) Stream(ctx context.Context, req model.Request, emit func(model.Delta)) (model.Response, error) {
	c.streams.Add(1)
	return c.Streaming.Stream(ctx, req, func(d model.Delta) {
		if c.onDelta != nil {
			c.onDelta()
		}
		emit(d)
	})
}

type streamWorld struct {
	t     *testing.T
	ctx   context.Context
	clock *testutil.Clock
	app   *app.App
	model *counting
	conv  string
}

func newStreamWorld(t *testing.T, reply string, chunk int) *streamWorld {
	w := &streamWorld{t: t, ctx: context.Background(), clock: testutil.NewClock()}
	w.model = &counting{Streaming: scripted.Streaming{Chunk: chunk,
		Script: func(model.Request) model.Message { return model.Message{Content: reply} }}}
	db := testutil.Open(t, testutil.DBPath(t))
	w.app = app.New(db, w.model, "cloud", tools.Registry{}, w.clock.Now)
	var err error
	if w.conv, err = w.app.Chat.Default(w.ctx); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *streamWorld) ask(text string) chat.Message {
	w.t.Helper()
	m, err := w.app.Chat.Post(w.ctx, chat.Post{ConversationID: w.conv, Text: text, Channel: "web"})
	if err != nil {
		w.t.Fatal(err)
	}
	return m
}

func (w *streamWorld) turn() error {
	_, err := w.app.Runtime.RunOnceInteractive(w.ctx, "interactive")
	return err
}

func (w *streamWorld) replies() []string {
	msgs, _ := w.app.Chat.Messages(w.ctx, w.conv, 0, 100)
	var out []string
	for _, m := range msgs {
		if m.Author == chat.Agent {
			out = append(out, m.Text())
		}
	}
	return out
}

func collect(ch <-chan chat.Delta) []chat.Delta {
	var out []chat.Delta
	for {
		select {
		case d := <-ch:
			out = append(out, d)
		default:
			return out
		}
	}
}

func TestDeltasArriveInOrderAndMakeTheReply(t *testing.T) {
	reply := "The refund project is waiting for the airline to answer."
	w := newStreamWorld(t, reply, 4)
	deltas, stop := w.app.Chat.SubscribeDeltas()
	defer stop()
	m := w.ask("how is it going?")
	if err := w.turn(); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	got := collect(deltas)
	for _, d := range got {
		if d.TaskID != m.TurnTaskID || d.Restarted {
			t.Fatalf("delta %+v", d)
		}
		text.WriteString(d.Text)
	}
	if text.String() != reply || len(got) < 10 {
		t.Fatalf("%d deltas make %q", len(got), text.String())
	}
	if r := w.replies(); len(r) != 1 || r[0] != reply {
		t.Fatalf("stored replies = %q", r)
	}
	var stored int
	w.app.DB.QueryRow(`SELECT COUNT(*) FROM run_messages WHERE message_json LIKE '%"role":"assistant"%'`).Scan(&stored)
	if stored != 1 {
		t.Fatalf("assistant messages stored = %d; deltas must not be checkpointed", stored)
	}
}

func TestOnlyInteractiveTurnsStream(t *testing.T) {
	w := newStreamWorld(t, "done", 2)
	project, _ := w.app.Assignments.CreateProject(w.ctx, assignments.NewProject{Name: "p", Objective: "o"})
	w.app.Tasks.Create(w.ctx, tasks.NewTask{AssignmentID: project, Title: "t", Objective: "o"})
	if _, err := w.app.Runtime.RunOnce(w.ctx, "worker"); err != nil {
		t.Fatal(err)
	}
	if w.model.streams.Load() != 0 || w.model.completes.Load() != 1 {
		t.Fatalf("background task: %d streams, %d completes", w.model.streams.Load(), w.model.completes.Load())
	}
}

func TestNoFallbackOnceTextWasShown(t *testing.T) {
	w := newStreamWorld(t, "partial answer that fails", 3)
	w.model.Fail, w.model.FailAfter = errors.New("connection reset"), 2
	var fallbackCalls atomic.Int32
	w.app.Runtime.Fallbacks = []runtime.Candidate{{ID: "backup", Provider: scripted.Provider(func(model.Request) model.Message {
		fallbackCalls.Add(1)
		return model.Message{Content: "from the backup"}
	})}}
	deltas, stop := w.app.Chat.SubscribeDeltas()
	defer stop()
	w.ask("hello")
	if err := w.turn(); err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("turn = %v", err)
	}
	if fallbackCalls.Load() != 0 {
		t.Fatal("switched to the fallback model after text was shown")
	}
	got := collect(deltas)
	if len(got) == 0 || !got[len(got)-1].Restarted {
		t.Fatalf("deltas = %+v; want shown text followed by a restart", got)
	}
}

func TestFallbackBeforeAnyTextWorksAsBefore(t *testing.T) {
	w := newStreamWorld(t, "never shown", 3)
	w.model.Fail, w.model.FailAfter = errors.New("overloaded"), 0
	w.app.Runtime.Fallbacks = []runtime.Candidate{{ID: "backup", Provider: scripted.Provider(func(model.Request) model.Message {
		return model.Message{Content: "from the backup"}
	})}}
	w.ask("hello")
	if err := w.turn(); err != nil {
		t.Fatal(err)
	}
	if r := w.replies(); len(r) != 1 || r[0] != "from the backup" {
		t.Fatalf("replies = %q", r)
	}
}

func TestKilledMidStreamLeavesNoPartialReply(t *testing.T) {
	w := newStreamWorld(t, "a whole reply", 2)
	deltas, stop := w.app.Chat.SubscribeDeltas()
	defer stop()
	n := 0
	w.model.onDelta = func() {
		if n++; n == 3 {
			panic("process killed mid-stream")
		}
	}
	w.ask("hello")
	if err := w.turn(); err == nil {
		t.Fatal("a killed stream reported success")
	}
	if got := collect(deltas); len(got) == 0 || !got[len(got)-1].Restarted {
		t.Fatalf("deltas = %+v; want a restart after the shown text", got)
	}
	w.model.onDelta = nil
	w.clock.Advance(time.Minute)
	for range 3 {
		if err := w.turn(); err == nil {
			break
		}
		w.clock.Advance(time.Minute)
	}
	if r := w.replies(); len(r) != 1 || r[0] != "a whole reply" {
		t.Fatalf("replies after restart = %q", r)
	}
	var runs, assistant int
	w.app.DB.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&runs)
	w.app.DB.QueryRow(`SELECT COUNT(*) FROM run_messages WHERE message_json LIKE '%"role":"assistant"%'`).Scan(&assistant)
	if runs != 1 || assistant != 1 {
		t.Fatalf("runs %d, assistant messages %d; want one run with one final message", runs, assistant)
	}
}

func TestASubscriberThatNeverReadsDoesNotSlowTheTurn(t *testing.T) {
	w := newStreamWorld(t, strings.Repeat("word ", 2000), 1)
	_, stop := w.app.Chat.SubscribeDeltas()
	defer stop()
	w.ask("hello")
	start := time.Now()
	if err := w.turn(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("10000 deltas with a stuck subscriber took %s", d)
	}
}

func TestStreamedCallsAreBudgetedLikeAnyOther(t *testing.T) {
	w := newStreamWorld(t, "ok", 1)
	w.ask("hello")
	if err := w.turn(); err != nil {
		t.Fatal(err)
	}
	var status string
	w.app.DB.QueryRow(`SELECT status FROM model_reservations`).Scan(&status)
	if status != "settled" {
		t.Fatalf("reservation after a stream = %q", status)
	}
	w.model.Fail, w.model.FailAfter = errors.New("reset"), 1
	w.ask("again")
	w.turn()
	w.app.DB.QueryRow(`SELECT status FROM model_reservations ORDER BY id DESC LIMIT 1`).Scan(&status)
	if status != "unknown" {
		t.Fatalf("reservation after a failed stream = %q, want it kept as unknown", status)
	}
}

func TestEmergencyStopDuringAStreamRunsNoToolCall(t *testing.T) {
	w := newStreamWorld(t, "", 1)
	calls, _ := json.Marshal(map[string]string{})
	w.model.Script = func(req model.Request) model.Message {
		return model.Message{Content: "Let me look.", ToolCalls: []model.ToolCall{{ID: "c", Name: "list_assignments", Arguments: calls}}}
	}
	w.model.onDelta = func() { system.Halt(w.ctx, w.app.DB, w.clock.Now(), "owner", "stop now") }
	w.ask("what is going on?")
	w.turn()
	var ran int
	w.app.DB.QueryRow(`SELECT COUNT(*) FROM actions WHERE status IN ('dispatching', 'succeeded')`).Scan(&ran)
	if ran != 0 {
		t.Fatalf("%d tool calls ran after the emergency stop", ran)
	}
}

func TestSubscribersJoiningAndLeavingDuringAStream(t *testing.T) {
	w := newStreamWorld(t, strings.Repeat("x", 500), 1)
	stopAll := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stopAll:
					return
				default:
				}
				ch, stop := w.app.Chat.SubscribeDeltas()
				collect(ch)
				stop()
			}
		}()
	}
	w.ask("hello")
	if err := w.turn(); err != nil {
		t.Fatal(err)
	}
	close(stopAll)
	wg.Wait()
}
