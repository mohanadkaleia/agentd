// Package chattest runs conversations with agentd in tests: a scripted model whose front desk
// turns read as a dialogue keyed by the owner's words, a fake clock, and helpers to send
// messages, run turns and work, read cards and simulate a restart.
package chattest

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"agentd/internal/app"
	"agentd/internal/chat"
	"agentd/internal/model"
	"agentd/internal/testutil"
	"agentd/internal/tools"
)

// Agent is a scripted model. Front desk turns go to Turn with the owner's latest message;
// every other task goes to Work. Cost is charged per call, in micro-dollars.
type Agent struct {
	mu    sync.Mutex
	Turn  func(owner string, req model.Request) model.Message
	Work  func(req model.Request) model.Message
	Cost  int64
	calls map[string]int
}

func (a *Agent) Complete(_ context.Context, req model.Request) (model.Response, error) {
	a.mu.Lock()
	if a.calls == nil {
		a.calls = map[string]int{}
	}
	desk := FrontDesk(req)
	key := "work"
	if desk {
		key = "front desk"
	}
	a.calls[key]++
	turn, work, cost := a.Turn, a.Work, a.Cost
	a.mu.Unlock()
	msg := model.Message{Content: "ok"}
	switch {
	case desk && turn != nil:
		msg = turn(OwnerSaid(req), req)
	case !desk && work != nil:
		msg = work(req)
	}
	stop := model.StopEnd
	if len(msg.ToolCalls) > 0 {
		stop = model.StopToolUse
	}
	return model.Response{Message: msg, StopReason: stop, Usage: model.Usage{CostMicros: cost}}, nil
}

// Calls counts model calls, for the front desk ("front desk") or other work ("work").
func (a *Agent) Calls(kind string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[kind]
}

// FrontDesk reports whether a request is a conversation turn.
func FrontDesk(req model.Request) bool {
	return len(req.Messages) > 0 && strings.HasPrefix(req.Messages[0].Content, "You are agentd's front desk")
}

// OwnerSaid returns the owner's latest words in a turn's request.
func OwnerSaid(req model.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role == model.RoleUser && !strings.HasPrefix(m.Content, "[") && !strings.HasPrefix(m.Content, "Situation") &&
			!strings.HasPrefix(m.Content, "Summary of") {
			return m.Content
		}
	}
	return ""
}

// LastTool returns the content of the last tool result in a request, if the last message is one.
func LastTool(req model.Request) (string, bool) {
	last := req.Messages[len(req.Messages)-1]
	return last.Content, last.Role == model.RoleTool
}

// Call builds a model message that calls one tool.
func Call(id, name string, args any) model.Message {
	b, _ := json.Marshal(args)
	return model.Message{ToolCalls: []model.ToolCall{{ID: id, Name: name, Arguments: b}}}
}

// World is agentd with one conversation, running on a fake clock.
type World struct {
	T     *testing.T
	Ctx   context.Context
	Clock *testutil.Clock
	Path  string
	App   *app.App
	Agent *Agent
	Conv  string
	tools func(app *app.App) []tools.Tool
	ready func(*World)
}

// New starts agentd. tools, when given, builds the extra tools to register once the
// database is open; ready runs after every start, for wiring hooks.
func New(t *testing.T, extra func(*app.App) []tools.Tool, ready func(*World)) *World {
	w := &World{T: t, Ctx: context.Background(), Clock: testutil.NewClock(), Path: testutil.DBPath(t), Agent: &Agent{},
		tools: extra, ready: ready}
	w.Start()
	return w
}

// Start opens the database and builds agentd, as a fresh process would.
func (w *World) Start() {
	w.T.Helper()
	db := testutil.Open(w.T, w.Path)
	registry := tools.Registry{}
	if w.tools != nil {
		probe := app.New(db, w.Agent, "cloud", tools.Registry{}, w.Clock.Now)
		registry = tools.NewRegistry(w.tools(probe)...)
	}
	w.App = app.New(db, w.Agent, "cloud", registry, w.Clock.Now)
	var err error
	if _, err = w.App.Chat.FrontDesk(w.Ctx); err != nil {
		w.T.Fatal(err)
	}
	if w.Conv, err = w.App.Chat.Default(w.Ctx); err != nil {
		w.T.Fatal(err)
	}
	if err := w.App.Chat.Project(w.Ctx); err != nil {
		w.T.Fatal(err)
	}
	if w.ready != nil {
		w.ready(w)
	}
}

// Restart simulates the process dying and starting again: leases run out, a new process
// opens the same database.
func (w *World) Restart() {
	w.Clock.Advance(time.Minute)
	w.Start()
}

// Say sends an owner message from the web channel.
func (w *World) Say(text string) chat.Message {
	w.T.Helper()
	m, err := w.App.Chat.Post(w.Ctx, chat.Post{ConversationID: w.Conv, Text: text, Channel: "web"})
	if err != nil {
		w.T.Fatal(err)
	}
	return m
}

// Settle runs housekeeping, turns and background work until nothing is ready, as a running
// agentd would between two things the owner does. Errors from attempts are tolerated: a
// failed attempt is retried like in production.
func (w *World) Settle() {
	w.T.Helper()
	for range 200 {
		if err := w.App.Runtime.Housekeeping(w.Ctx); err != nil {
			w.T.Logf("housekeeping: %v", err)
		}
		turn, _ := w.App.Runtime.RunOnceInteractive(w.Ctx, "interactive")
		work, _ := w.App.Runtime.RunOnce(w.Ctx, "worker")
		if !turn && !work {
			if err := w.App.Runtime.Housekeeping(w.Ctx); err != nil {
				w.T.Logf("housekeeping: %v", err)
			}
			return
		}
	}
	w.T.Fatal("work did not settle")
}

// Advance moves the fake clock and settles.
func (w *World) Advance(d time.Duration) {
	w.Clock.Advance(d)
	w.Settle()
}

// Thread returns the whole conversation with live card status.
func (w *World) Thread() []chat.Message {
	w.T.Helper()
	msgs, err := w.App.Chat.Messages(w.Ctx, w.Conv, 0, 10000)
	if err != nil {
		w.T.Fatal(err)
	}
	return msgs
}

// Of returns the messages of one kind.
func (w *World) Of(kind string) []chat.Message {
	var out []chat.Message
	for _, m := range w.Thread() {
		if m.Kind == kind {
			out = append(out, m)
		}
	}
	return out
}

// Replies returns the agent's replies.
func (w *World) Replies() []string {
	var out []string
	for _, m := range w.Thread() {
		if m.Author == chat.Agent {
			out = append(out, m.Text())
		}
	}
	return out
}

// Command sends a slash command and returns its result.
func (w *World) Command(line string) (string, bool) {
	w.T.Helper()
	w.Say(line)
	results := w.Of(chat.KindCommandResult)
	last := results[len(results)-1]
	var t chat.Text
	json.Unmarshal(last.Body, &t)
	return t.Text, !t.Failed
}

// Count runs a counting query.
func (w *World) Count(query string, args ...any) int {
	w.T.Helper()
	var n int
	if err := w.App.DB.QueryRowContext(w.Ctx, query, args...).Scan(&n); err != nil {
		w.T.Fatal(err)
	}
	return n
}
