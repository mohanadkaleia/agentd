package assignments

import (
	"context"
	"encoding/json"
	"fmt"

	"agentd/internal/model"
	"agentd/internal/tools"
)

type StateTool struct {
	Assignments *Store
}

func (*StateTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name: "update_state",
		Description: "Replace this assignment's working state: a small JSON object of facts you want to remember between tasks " +
			"(baselines, open issues, what was last checked). It is shown to you at the start of every task.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"state":{"type":"object"}},"required":["state"]}`),
	}
}

func (*StateTool) Effect() tools.Effect { return tools.EffectWrite }

const maxStateBytes = 16 << 10

func (*StateTool) Resource(args json.RawMessage) (string, error) {
	_, err := parseState(args)
	return "", err
}

func parseState(args json.RawMessage) (json.RawMessage, error) {
	var in struct {
		State json.RawMessage `json:"state"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, err
	}
	if len(in.State) == 0 || in.State[0] != '{' {
		return nil, fmt.Errorf("state must be a JSON object")
	}
	if len(in.State) > maxStateBytes {
		return nil, fmt.Errorf("state is larger than %d bytes; keep it to a summary", maxStateBytes)
	}
	return in.State, nil
}

func (t *StateTool) Execute(ctx context.Context, call tools.Invocation) (tools.Result, error) {
	state, err := parseState(call.Arguments)
	if err != nil {
		return tools.Result{}, fmt.Errorf("%w: %v", tools.ErrNotExecuted, err)
	}
	if err := t.Assignments.SetState(ctx, call.AssignmentID, state, "agent"); err != nil {
		return tools.Result{}, err
	}
	return tools.Result{Output: json.RawMessage(`{"status":"saved"}`)}, nil
}

func (*StateTool) Reconcile(context.Context, tools.Invocation) (tools.Result, tools.Finding, error) {
	return tools.Result{}, tools.NotHappened, nil
}
