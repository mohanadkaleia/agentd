package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agentd/internal/app"
	"agentd/internal/assignments"
	"agentd/internal/chat"
	"agentd/internal/model"
	"agentd/internal/policy"
	"agentd/internal/runtime"
	"agentd/internal/scheduler"
	"agentd/internal/tasks"
	"agentd/internal/testutil"
	"agentd/internal/tools"
)

// named is a scripted model that records which model answered.
type named struct {
	id     string
	mu     *sync.Mutex
	used   *[]string
	fail   *int
	native bool
	cost   int64
}

func (n named) Complete(_ context.Context, req model.Request) (model.Response, error) {
	n.mu.Lock()
	*n.used = append(*n.used, n.id)
	failing := *n.fail > 0
	if failing {
		*n.fail--
	}
	n.mu.Unlock()
	if failing {
		return model.Response{}, errors.New("overloaded")
	}
	msg := model.Message{Content: "answered by " + n.id}
	if n.native {
		msg.Native = &model.Native{Provider: n.id, Payload: json.RawMessage(`{}`)}
	}
	return model.Response{Message: msg, StopReason: model.StopEnd, Usage: model.Usage{CostMicros: n.cost}}, nil
}

type catalogue struct {
	t     *testing.T
	ctx   context.Context
	clock *testutil.Clock
	app   *app.App
	mu    sync.Mutex
	used  []string
	fail  int
}

// newCatalogue configures three models: a local fast one (default), a local strong one and
// a cloud one, with tiers fast, default and strong mapped to them.
func newCatalogue(t *testing.T) *catalogue {
	c := &catalogue{t: t, ctx: context.Background(), clock: testutil.NewClock()}
	db := testutil.Open(t, testutil.DBPath(t))
	mk := func(id string, native bool) named {
		return named{id: id, mu: &c.mu, used: &c.used, fail: &c.fail, native: native, cost: 1000}
	}
	c.app = app.New(db, mk("ollama/small", false), "ollama/small", tools.Registry{}, c.clock.Now)
	rt := c.app.Runtime
	rt.ModelLocal, rt.ModelAlias = true, "small"
	rt.Fallbacks = []runtime.Candidate{
		{ID: "ollama/big", Alias: "big", Provider: mk("ollama/big", false), Local: true},
		{ID: "anthropic/claude", Alias: "claude", Provider: mk("anthropic/claude", true), Local: false},
	}
	rt.Tiers = map[string]string{"fast": "small", "default": "big", "strong": "claude"}
	return c
}

func (c *catalogue) project(p policy.Policy) string {
	c.t.Helper()
	id, err := c.app.Assignments.CreateProject(c.ctx, assignments.NewProject{Name: fmt.Sprint("p", rand.Int()), Objective: "o", Policy: p})
	if err != nil {
		c.t.Fatal(err)
	}
	return id
}

func (c *catalogue) run(assignment string, nt tasks.NewTask) (string, string) {
	c.t.Helper()
	nt.AssignmentID, nt.Title, nt.Objective = assignment, "t", "o"
	id, err := c.app.Tasks.Create(c.ctx, nt)
	if err != nil {
		c.t.Fatal(err)
	}
	return id, c.drain(id)
}

func (c *catalogue) drain(task string) string {
	c.t.Helper()
	for range 10 {
		c.app.Runtime.RunOnce(c.ctx, "w")
		c.clock.Advance(time.Minute)
	}
	var model, chosen string
	c.app.DB.QueryRow(`SELECT r.model_id, COALESCE((SELECT payload_json FROM audit_log WHERE operation = 'run.model_selected' AND object_id = r.id), '')
		FROM runs r WHERE r.task_id = ? ORDER BY r.started_at DESC LIMIT 1`, task).Scan(&model, &chosen)
	return model + " " + chosen
}

func TestWithNoChoicesEveryRunUsesTheDefault(t *testing.T) {
	c := newCatalogue(t)
	p := c.project(policy.Policy{})
	if _, got := c.run(p, tasks.NewTask{}); !strings.HasPrefix(got, "ollama/small ") || !strings.Contains(got, `"source":"default"`) {
		t.Fatalf("run = %s", got)
	}
}

func TestEachSourceWinsOverTheOnesBelow(t *testing.T) {
	c := newCatalogue(t)
	conv, _ := c.app.Chat.Default(c.ctx)
	c.app.Runtime.Chooser = fixedChooser{"claude"}
	for _, tc := range []struct {
		name, want, source string
		task               tasks.NewTask
		policy             policy.Policy
		override           string
		chooser            bool
	}{
		{"default", "ollama/small", "default", tasks.NewTask{}, policy.Policy{}, "", false},
		{"tier", "ollama/big", "tier:default", tasks.NewTask{ModelTier: "default"}, policy.Policy{}, "", false},
		{"assignment tier", "anthropic/claude", "tier:strong", tasks.NewTask{}, policy.Policy{Models: policy.ModelPolicy{Tier: "strong"}}, "", false},
		{"prefer over tier", "ollama/big", "assignment_prefer", tasks.NewTask{ModelTier: "strong"}, policy.Policy{Models: policy.ModelPolicy{Prefer: []string{"big"}}}, "", false},
		{"conversation over prefer", "anthropic/claude", "conversation", tasks.NewTask{}, policy.Policy{Models: policy.ModelPolicy{Prefer: []string{"big"}}}, "", true},
		{"override over all", "ollama/small", "task_override", tasks.NewTask{ModelTier: "strong"}, policy.Policy{Models: policy.ModelPolicy{Prefer: []string{"big"}}}, "fast", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.chooser {
				c.app.Runtime.Chooser = fixedChooser{"claude"}
			} else {
				c.app.Runtime.Chooser = nil
			}
			p := c.project(tc.policy)
			tc.task.AssignmentID, tc.task.Title, tc.task.Objective = p, "t", "o"
			id, _ := c.app.Tasks.Create(c.ctx, tc.task)
			if tc.override != "" {
				if _, err := c.app.Runtime.SetTaskModel(c.ctx, id, tc.override, "owner"); err != nil {
					t.Fatal(err)
				}
			}
			got := c.drain(id)
			if !strings.HasPrefix(got, tc.want+" ") || !strings.Contains(got, `"source":"`+tc.source+`"`) {
				t.Fatalf("run = %s, want %s from %s", got, tc.want, tc.source)
			}
		})
	}
	_ = conv
}

type fixedChooser struct{ model string }

func (f fixedChooser) ConversationModel(context.Context, string) string { return f.model }

func TestForbiddenChoicesAreSkippedWithAReason(t *testing.T) {
	c := newCatalogue(t)
	p := c.project(policy.Policy{Models: policy.ModelPolicy{LocalOnly: true, Prefer: []string{"claude"}, Tier: "strong"}})
	_, got := c.run(p, tasks.NewTask{})
	if !strings.HasPrefix(got, "ollama/small ") || !strings.Contains(got, "assignment_prefer anthropic/claude: not allowed") ||
		!strings.Contains(got, "tier:strong anthropic/claude: not allowed") {
		t.Fatalf("run = %s", got)
	}
	none := c.project(policy.Policy{Models: policy.ModelPolicy{Allowed: []string{"nothing/configured"}}})
	id, _ := c.run(none, tasks.NewTask{})
	task, _ := c.app.Tasks.Get(c.ctx, id)
	if task.Status != tasks.Blocked || !strings.Contains(string(task.Result), "model policy") {
		t.Fatalf("with nothing allowed: %s %s", task.Status, task.Result)
	}
}

func TestChoicesAreCheckedWhenSet(t *testing.T) {
	c := newCatalogue(t)
	p := c.project(policy.Policy{Models: policy.ModelPolicy{LocalOnly: true}})
	id, _ := c.app.Tasks.Create(c.ctx, tasks.NewTask{AssignmentID: p, Title: "t", Objective: "o"})
	if _, err := c.app.Runtime.SetTaskModel(c.ctx, id, "gpt-9", "owner"); !errors.Is(err, runtime.ErrUnknownModel) {
		t.Fatalf("unknown model = %v", err)
	}
	if _, err := c.app.Runtime.SetTaskModel(c.ctx, id, "claude", "owner"); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("cloud model under local_only = %v", err)
	}
	open := c.project(policy.Policy{})
	id2, _ := c.app.Tasks.Create(c.ctx, tasks.NewTask{AssignmentID: open, Title: "t", Objective: "o"})
	msg, err := c.app.Runtime.SetTaskModel(c.ctx, id2, "claude", "owner")
	if err != nil || !strings.Contains(msg, "sends the task's content to anthropic") {
		t.Fatalf("local to cloud = %q, %v", msg, err)
	}
	conv, _ := c.app.Chat.Default(c.ctx)
	if _, err := c.app.Chat.SetConversationModel(c.ctx, conv, "nope", "owner"); !errors.Is(err, runtime.ErrUnknownModel) {
		t.Fatalf("conversation unknown model = %v", err)
	}
	if msg, err := c.app.Chat.SetConversationModel(c.ctx, conv, "strong", "owner"); err != nil || !strings.Contains(msg, "cloud provider") {
		t.Fatalf("conversation to the cloud = %q, %v", msg, err)
	}
}

func TestChatTurnsAreFastAndScheduledWorkTakesItsTier(t *testing.T) {
	c := newCatalogue(t)
	c.app.Runtime.Chooser = c.app.Chat
	conv, _ := c.app.Chat.Default(c.ctx)
	m, err := c.app.Chat.Post(c.ctx, chat.Post{ConversationID: conv, Text: "hello", Channel: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.drain(m.TurnTaskID); !strings.HasPrefix(got, "ollama/small ") || !strings.Contains(got, "tier:fast") {
		t.Fatalf("turn = %s", got)
	}
	p := c.project(policy.Policy{})
	if _, err := c.app.Scheduler.AddSchedule(c.ctx, schedule(p, "strong")); err != nil {
		t.Fatal(err)
	}
	c.app.Scheduler.Tick(c.ctx)
	var task string
	c.app.DB.QueryRow(`SELECT id FROM tasks WHERE assignment_id = ?`, p).Scan(&task)
	if got := c.drain(task); !strings.HasPrefix(got, "anthropic/claude ") {
		t.Fatalf("scheduled strong task = %s", got)
	}
	delete(c.app.Runtime.Tiers, "default")
	q := c.project(policy.Policy{})
	if _, got := c.run(q, tasks.NewTask{ModelTier: "default"}); !strings.HasPrefix(got, "ollama/small ") || !strings.Contains(got, "tier:default default: not configured") {
		t.Fatalf("unmapped tier = %s", got)
	}
}

func TestAFailedRunEscalatesOneTier(t *testing.T) {
	c := newCatalogue(t)
	p := c.project(policy.Policy{Tools: map[string]policy.Rule{"update_state": {Mode: policy.Allow}}})
	calls := 0
	c.app.Runtime.Model = scriptedNative(func() model.Message {
		calls++
		if calls == 1 {
			return model.Message{ToolCalls: []model.ToolCall{{ID: "x", Name: "update_state", Arguments: json.RawMessage(`{"state":{}}`)}}}
		}
		return model.Message{Content: "FAIL"}
	})
	id, _ := c.app.Tasks.Create(c.ctx, tasks.NewTask{AssignmentID: p, Title: "t", Objective: "o", ModelTier: "fast"})
	c.drain(id)
	task, _ := c.app.Tasks.Get(c.ctx, id)
	c.mu.Lock()
	used := slices.Clone(c.used)
	c.mu.Unlock()
	if task.ModelTier != "default" || task.Status != tasks.Completed || len(used) != 1 || used[0] != "ollama/big" {
		t.Fatalf("after a failed run: tier %q, status %s, later calls on %v", task.ModelTier, task.Status, used)
	}
	owned, _ := c.app.Tasks.Create(c.ctx, tasks.NewTask{AssignmentID: p, Title: "t", Objective: "o", ModelTier: "fast"})
	c.app.Runtime.SetTaskModel(c.ctx, owned, "small", "owner")
	calls = 0
	c.drain(owned)
	if got, _ := c.app.Tasks.Get(c.ctx, owned); got.ModelTier != "fast" {
		t.Fatalf("an owner's choice escalated to %q", got.ModelTier)
	}
}

func TestALostLeaseDoesNotEscalate(t *testing.T) {
	c := newCatalogue(t)
	p := c.project(policy.Policy{})
	id, _ := c.app.Tasks.Create(c.ctx, tasks.NewTask{AssignmentID: p, Title: "t", Objective: "o", ModelTier: "fast"})
	lease, _ := c.app.Tasks.ClaimReady(c.ctx, "dead", 30*time.Second)
	_ = lease
	c.clock.Advance(time.Minute)
	c.drain(id)
	if got, _ := c.app.Tasks.Get(c.ctx, id); got.ModelTier != "fast" {
		t.Fatalf("a crash escalated the tier to %q", got.ModelTier)
	}
}

func TestAnOverrideMidRunWaitsForTheNextRunWhenTheTranscriptIsPrivate(t *testing.T) {
	c := newCatalogue(t)
	p := c.project(policy.Policy{Models: policy.ModelPolicy{Tier: "strong"}})
	id, _ := c.app.Tasks.Create(c.ctx, tasks.NewTask{AssignmentID: p, Title: "t", Objective: "o"})
	steps := 0
	c.app.Runtime.Fallbacks[1].Provider = scriptedNative(func() model.Message {
		steps++
		if steps == 1 {
			c.app.Runtime.SetTaskModel(c.ctx, id, "small", "owner")
			return model.Message{ToolCalls: []model.ToolCall{{ID: "x", Name: "update_state", Arguments: json.RawMessage(`{"state":{}}`)}},
				Native: &model.Native{Provider: "anthropic", Payload: json.RawMessage(`{}`)}}
		}
		return model.Message{Content: "done", Native: &model.Native{Provider: "anthropic", Payload: json.RawMessage(`{}`)}}
	})
	c.drain(id)
	var switched int
	c.app.DB.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE operation = 'run.model_switched'`).Scan(&switched)
	if switched != 0 {
		t.Fatal("a run holding provider-private data switched model")
	}
	msg, _ := c.app.Runtime.SetTaskModel(c.ctx, id, "big", "owner")
	if !strings.Contains(msg, "next run") {
		t.Fatalf("message = %q, want it to say the next run", msg)
	}
}

type scriptedNative func() model.Message

func (s scriptedNative) Complete(context.Context, model.Request) (model.Response, error) {
	m := s()
	if m.Content == "FAIL" {
		return model.Response{}, errors.New("overloaded")
	}
	stop := model.StopEnd
	if len(m.ToolCalls) > 0 {
		stop = model.StopToolUse
	}
	return model.Response{Message: m, StopReason: stop}, nil
}

func TestAnOverrideMidRunSwitchesAtTheNextStepWithoutPrivateData(t *testing.T) {
	c := newCatalogue(t)
	p := c.project(policy.Policy{Tools: map[string]policy.Rule{"update_state": {Mode: policy.Allow}}})
	id, _ := c.app.Tasks.Create(c.ctx, tasks.NewTask{AssignmentID: p, Title: "t", Objective: "o"})
	steps := 0
	c.app.Runtime.Model = scriptedNative(func() model.Message {
		steps++
		if steps == 1 {
			c.app.Runtime.SetTaskModel(c.ctx, id, "big", "owner")
			return model.Message{ToolCalls: []model.ToolCall{{ID: "x", Name: "update_state", Arguments: json.RawMessage(`{"state":{}}`)}}}
		}
		return model.Message{Content: "small again?"}
	})
	c.drain(id)
	var switched int
	c.app.DB.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE operation = 'run.model_switched'`).Scan(&switched)
	c.mu.Lock()
	used := slices.Clone(c.used)
	c.mu.Unlock()
	if switched != 1 || len(used) != 1 || used[0] != "ollama/big" || steps != 1 {
		t.Fatalf("switches %d, later steps on %v, first model steps %d", switched, used, steps)
	}
}

func TestSpendIsAttributedToTheModelUsed(t *testing.T) {
	c := newCatalogue(t)
	p := c.project(policy.Policy{Models: policy.ModelPolicy{Tier: "default"}})
	c.run(p, tasks.NewTask{})
	a, _ := c.app.Assignments.Get(c.ctx, p)
	spend, err := c.app.Runtime.SpendByModel(c.ctx, a)
	if err != nil || spend["ollama/big"] != 1000 || len(spend) != 1 {
		t.Fatalf("spend = %v, %v", spend, err)
	}
}

// TestNoChoiceEverLeavesThePolicy tries random combinations of every source and limit.
func TestNoChoiceEverLeavesThePolicy(t *testing.T) {
	c := newCatalogue(t)
	values := []string{"", "small", "big", "claude", "fast", "default", "strong", "ollama/small", "anthropic/claude", "missing"}
	ids := []string{"ollama/small", "ollama/big", "anthropic/claude"}
	rng := rand.New(rand.NewSource(7))
	for range 300 {
		p := policy.Policy{Models: policy.ModelPolicy{LocalOnly: rng.Intn(2) == 0, Tier: []string{"", "fast", "default", "strong"}[rng.Intn(4)]}}
		if rng.Intn(3) == 0 {
			p.Models.Allowed = []string{ids[rng.Intn(3)]}
		}
		if v := values[rng.Intn(len(values))]; v != "" {
			p.Models.Prefer = []string{v}
		}
		c.app.Runtime.Chooser = fixedChooser{values[rng.Intn(len(values))]}
		res, err := c.app.Runtime.Preview(c.ctx, p, []string{"", "fast", "default", "strong"}[rng.Intn(4)])
		if err != nil {
			continue
		}
		local := res.Model != "anthropic/claude"
		if p.Models.LocalOnly && !local {
			t.Fatalf("local_only chose %s (%+v)", res.Model, p.Models)
		}
		if len(p.Models.Allowed) > 0 && !slices.Contains(p.Models.Allowed, res.Model) {
			t.Fatalf("allowed %v chose %s", p.Models.Allowed, res.Model)
		}
	}
}

func schedule(assignment, tier string) scheduler.Schedule {
	return scheduler.Schedule{AssignmentID: assignment, Every: 24 * time.Hour, Title: "Daily", Objective: "Check.", Tier: tier}
}
