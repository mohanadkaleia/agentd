package demo

import (
	"context"
	"encoding/json"
	"fmt"

	"agentd/internal/app"
	"agentd/internal/model"
	"agentd/internal/policy"
	"agentd/internal/tasks"
)

const (
	AirlineRefunds = "refunds@airline.example"
	AirlineClaims  = "claims@airline.example"
)

var Policy = policy.Policy{Tools: map[string]policy.Rule{
	"web_search": {Mode: policy.Allow},
	"send_email": {Mode: policy.Allow, Resources: []string{AirlineRefunds}, Otherwise: policy.RequireApproval},
}}

func Seed(ctx context.Context, a *app.App) (projectID, taskID string, err error) {
	projectID, err = a.Projects.Create(ctx, "airline-refund", "Recover the refund for cancelled flight XY123.", Policy)
	if err != nil {
		return "", "", err
	}
	taskID, err = a.Tasks.Create(ctx, tasks.NewTask{
		ProjectID: projectID,
		Title:     "Request refund",
		Objective: "Research the airline refund policy, email the refunds desk, and escalate a formal claim.",
		Success:   tasks.Success{RequireSucceededTools: []string{"send_email"}},
	})
	return projectID, taskID, err
}

func Script(req model.Request) model.Message {
	done := 0
	for _, m := range req.Messages {
		if m.Role == model.RoleTool {
			done++
		}
	}
	call := func(name string, args map[string]string) model.Message {
		body, _ := json.Marshal(args)
		return model.Message{ToolCalls: []model.ToolCall{{ID: fmt.Sprintf("call_%d", done+1), Name: name, Arguments: body}}}
	}
	switch done {
	case 0:
		return call("web_search", map[string]string{"query": "XY airline cancelled flight refund policy"})
	case 1:
		return call("send_email", map[string]string{
			"to": AirlineRefunds, "subject": "Refund request: flight XY123",
			"body": "Flight XY123 was cancelled. Please refund the fare to the original payment method.",
		})
	case 2:
		return call("send_email", map[string]string{
			"to": AirlineClaims, "subject": "Formal claim: flight XY123",
			"body": "I am filing a formal claim for the cancelled flight XY123.",
		})
	default:
		return model.Message{Content: "Refund requested and formal claim handled."}
	}
}
