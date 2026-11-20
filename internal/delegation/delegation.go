package delegation

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"agentd/internal/model"
	"agentd/internal/tasks"
	"agentd/internal/tools"
)

type Limits struct {
	MaxDepth         int
	MaxActiveWorkers int
	MaxWorkerSteps   int
}

var DefaultLimits = Limits{MaxDepth: 2, MaxActiveWorkers: 3, MaxWorkerSteps: 10}

type Delegate struct {
	Tasks  *tasks.Store
	Tools  tools.Registry
	Limits Limits
}

type delegateArgs struct {
	Title         string   `json:"title"`
	Objective     string   `json:"objective"`
	AllowedTools  []string `json:"allowed_tools"`
	MaxSteps      int      `json:"max_steps"`
	DeadlineHours float64  `json:"deadline_hours"`
}

func (*Delegate) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name: "delegate",
		Description: "Start a helper agent on a narrow sub-task. It may only use the tools you list, which must be tools you have. " +
			"Returns a task_id; call await_workers to collect results.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"title":{"type":"string"},
			"objective":{"type":"string"},
			"allowed_tools":{"type":"array","items":{"type":"string"},"minItems":1},
			"max_steps":{"type":"integer","minimum":1},
			"deadline_hours":{"type":"number","minimum":0.1}},
			"required":["title","objective","allowed_tools"]}`),
	}
}

func (*Delegate) Effect() tools.Effect { return tools.EffectWrite }

func (*Delegate) Resource(args json.RawMessage) (string, error) {
	_, err := parseDelegate(args)
	return "", err
}

func parseDelegate(args json.RawMessage) (delegateArgs, error) {
	var in delegateArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return in, err
	}
	if strings.TrimSpace(in.Title) == "" || strings.TrimSpace(in.Objective) == "" {
		return in, fmt.Errorf("title and objective are required")
	}
	if len(in.AllowedTools) == 0 {
		return in, fmt.Errorf("allowed_tools must list at least one tool")
	}
	if in.MaxSteps < 0 || in.DeadlineHours < 0 {
		return in, fmt.Errorf("max_steps and deadline_hours must be positive")
	}
	return in, nil
}

func (d *Delegate) Execute(ctx context.Context, call tools.Invocation) (tools.Result, error) {
	in, err := parseDelegate(call.Arguments)
	if err != nil {
		return tools.Result{}, refuse(err.Error())
	}
	parent, err := d.Tasks.Get(ctx, call.TaskID)
	if err != nil {
		return tools.Result{}, err
	}
	if parent.Depth+1 > d.Limits.MaxDepth {
		return tools.Result{}, refuse(fmt.Sprintf("delegation depth limit of %d reached", d.Limits.MaxDepth))
	}
	for _, name := range in.AllowedTools {
		if _, ok := d.Tools.Lookup(name); !ok {
			return tools.Result{}, refuse(fmt.Sprintf("unknown tool %q", name))
		}
		if !parent.Allows(name) {
			return tools.Result{}, refuse(fmt.Sprintf("cannot grant %q: this task does not have it", name))
		}
	}
	children, err := d.Tasks.Children(ctx, parent.ID)
	if err != nil {
		return tools.Result{}, err
	}
	active := 0
	for _, c := range children {
		if !tasks.Terminal(c.Status) {
			active++
		}
	}
	if active >= d.Limits.MaxActiveWorkers {
		return tools.Result{}, refuse(fmt.Sprintf("%d workers already active; await them before delegating more", active))
	}

	steps := d.Limits.MaxWorkerSteps
	if in.MaxSteps > 0 && in.MaxSteps < steps {
		steps = in.MaxSteps
	}
	var deadline time.Time
	if in.DeadlineHours > 0 {
		deadline = d.Tasks.Now().Add(time.Duration(in.DeadlineHours * float64(time.Hour)))
	}
	id, err := d.Tasks.Create(ctx, tasks.NewTask{
		AssignmentID: parent.AssignmentID,
		ParentTaskID: parent.ID,
		OriginKey:    "delegate:" + call.IdempotencyKey,
		Title:        in.Title,
		Objective: "You are a helper agent working on one part of a larger task. Do only this, then reply with your findings.\n\n" +
			in.Objective,
		Depth:        parent.Depth + 1,
		AllowedTools: slices.Compact(slices.Sorted(slices.Values(in.AllowedTools))),
		MaxSteps:     steps,
		DeadlineAt:   deadline,
	})
	if err != nil {
		return tools.Result{}, err
	}
	return tools.Result{Output: mustJSON(map[string]string{"status": "started", "task_id": id}), ExternalID: id}, nil
}

func (d *Delegate) Reconcile(ctx context.Context, call tools.Invocation) (tools.Result, bool, error) {
	id, found, err := d.Tasks.FindByOrigin(ctx, "delegate:"+call.IdempotencyKey)
	if err != nil || !found {
		return tools.Result{}, false, err
	}
	return tools.Result{Output: mustJSON(map[string]string{"status": "started", "task_id": id}), ExternalID: id}, true, nil
}

type Await struct {
	Tasks *tasks.Store
}

type workerResult struct {
	TaskID string          `json:"task_id"`
	Title  string          `json:"title"`
	Status string          `json:"status"`
	Result json.RawMessage `json:"result,omitempty"`
}

func (*Await) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        "await_workers",
		Description: "Wait until the given helper agents finish (all of yours if task_ids is empty), then return their results.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"task_ids":{"type":"array","items":{"type":"string"}}}}`),
	}
}

func (*Await) Effect() tools.Effect { return tools.EffectRead }

func (*Await) Resource(args json.RawMessage) (string, error) {
	var in struct {
		TaskIDs []string `json:"task_ids"`
	}
	return "", json.Unmarshal(args, &in)
}

func (a *Await) Execute(ctx context.Context, call tools.Invocation) (tools.Result, error) {
	var in struct {
		TaskIDs []string `json:"task_ids"`
	}
	if err := json.Unmarshal(call.Arguments, &in); err != nil {
		return tools.Result{}, refuse(err.Error())
	}
	children, err := a.Tasks.Children(ctx, call.TaskID)
	if err != nil {
		return tools.Result{}, err
	}
	byID := map[string]tasks.Task{}
	for _, c := range children {
		byID[c.ID] = c
	}
	wanted := in.TaskIDs
	if len(wanted) == 0 {
		for _, c := range children {
			wanted = append(wanted, c.ID)
		}
	}
	if len(wanted) == 0 {
		return tools.Result{}, refuse("this task has no workers")
	}
	results := make([]workerResult, 0, len(wanted))
	for _, id := range wanted {
		c, ok := byID[id]
		if !ok {
			return tools.Result{}, refuse(fmt.Sprintf("%s is not a worker of this task", id))
		}
		if !tasks.Terminal(c.Status) {
			return tools.Result{}, tools.ErrNotReady
		}
		results = append(results, workerResult{TaskID: c.ID, Title: c.Title, Status: c.Status, Result: c.Result})
	}
	return tools.Result{Output: mustJSON(map[string]any{"workers": results})}, nil
}

func (*Await) Reconcile(context.Context, tools.Invocation) (tools.Result, bool, error) {
	return tools.Result{}, false, nil
}

func refuse(reason string) error {
	return fmt.Errorf("%w: %s", tools.ErrNotExecuted, reason)
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
