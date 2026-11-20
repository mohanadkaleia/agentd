package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agentd/internal/model"
	"agentd/internal/tasks"
	"agentd/internal/tools"
)

const maxFollowupHours = 24 * 30

type Followup struct {
	Tasks *tasks.Store
}

type followupArgs struct {
	AfterHours float64 `json:"after_hours"`
	Reason     string  `json:"reason"`
}

func (*Followup) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        "schedule_followup",
		Description: "Schedule a follow-up task in this project to run after a delay, e.g. to check for a reply.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"after_hours":{"type":"number","minimum":0.1,"maximum":720},"reason":{"type":"string"}},"required":["after_hours","reason"]}`),
	}
}

func (*Followup) Effect() tools.Effect { return tools.EffectWrite }

func (*Followup) Resource(args json.RawMessage) (string, error) {
	_, err := parseFollowup(args)
	return "", err
}

func parseFollowup(args json.RawMessage) (followupArgs, error) {
	var in followupArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return in, err
	}
	if in.AfterHours <= 0 || in.AfterHours > maxFollowupHours {
		return in, fmt.Errorf("after_hours must be between 0 and %d", maxFollowupHours)
	}
	if strings.TrimSpace(in.Reason) == "" {
		return in, fmt.Errorf("reason is required")
	}
	return in, nil
}

func (f *Followup) Execute(ctx context.Context, call tools.Invocation) (tools.Result, error) {
	in, err := parseFollowup(call.Arguments)
	if err != nil {
		return tools.Result{}, fmt.Errorf("%w: %v", tools.ErrNotExecuted, err)
	}
	runAt := f.Tasks.Now().Add(time.Duration(in.AfterHours * float64(time.Hour)))
	id, err := f.Tasks.Create(ctx, tasks.NewTask{
		ProjectID:    call.ProjectID,
		ParentTaskID: call.TaskID,
		OriginKey:    "followup:" + call.IdempotencyKey,
		NextRunAt:    runAt,
		Title:        "Follow-up: " + in.Reason,
		Objective:    "Follow up on earlier work in this project. Reason given when scheduled: " + in.Reason,
	})
	if err != nil {
		return tools.Result{}, err
	}
	return followupResult(id, runAt), nil
}

func (f *Followup) Reconcile(ctx context.Context, call tools.Invocation) (tools.Result, bool, error) {
	id, found, err := f.Tasks.FindByOrigin(ctx, "followup:"+call.IdempotencyKey)
	if err != nil || !found {
		return tools.Result{}, false, err
	}
	return tools.Result{Output: mustJSON(map[string]string{"status": "scheduled", "task_id": id}), ExternalID: id}, true, nil
}

func followupResult(id string, runAt time.Time) tools.Result {
	return tools.Result{
		Output:     mustJSON(map[string]string{"status": "scheduled", "task_id": id, "run_at": runAt.UTC().Format(time.RFC3339)}),
		ExternalID: id,
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
