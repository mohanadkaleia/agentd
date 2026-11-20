package demo

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agentd/internal/app"
	"agentd/internal/assignments"
	"agentd/internal/model"
	"agentd/internal/policy"
	"agentd/internal/scheduler"
	"agentd/internal/tasks"
)

const AirlineThread = "thread-xy123"

const (
	AirlineRefunds = "refunds@airline.example"
	AirlineClaims  = "claims@airline.example"
)

var Policy = policy.Policy{Tools: map[string]policy.Rule{
	"web_search":        {Mode: policy.Allow},
	"schedule_followup": {Mode: policy.Allow},
	"delegate":          {Mode: policy.Allow},
	"await_workers":     {Mode: policy.Allow},
	"send_email":        {Mode: policy.Allow, Resources: []string{AirlineRefunds}, Otherwise: policy.RequireApproval},
}, Limits: policy.Limits{
	MaxLLMSpendUSD: 3,
	ToolRates:      map[string]policy.RateLimit{"send_email": {Max: 5, Per: policy.Duration{Duration: 24 * time.Hour}}},
}}

func Seed(ctx context.Context, a *app.App) (assignmentID, taskID string, err error) {
	assignmentID, err = a.Assignments.CreateProject(ctx, assignments.NewProject{
		Name:       "airline-refund",
		Objective:  "Recover the refund for cancelled flight XY123.",
		Policy:     Policy,
		Completion: assignments.Completion{Criteria: []string{"Refund received or formally refused in writing"}},
	})
	if err != nil {
		return "", "", err
	}
	taskID, err = a.Tasks.Create(ctx, tasks.NewTask{
		AssignmentID: assignmentID,
		Title:        "Request refund",
		Objective:    "Research the airline refund policy, email the refunds desk, and escalate a formal claim.",
		Success:      tasks.Success{RequireSucceededTools: []string{"send_email"}},
	})
	if err != nil {
		return "", "", err
	}
	_, err = a.Scheduler.AddRoute(ctx, scheduler.Route{
		AssignmentID: assignmentID,
		Source:       "gmail",
		Kind:         "email.received",
		Match:        map[string]string{"thread_id": AirlineThread},
		Title:        "Handle airline reply",
		Objective:    "The airline replied on the refund thread. Read the reply and decide the next step.",
	})
	return assignmentID, taskID, err
}

func Script(req model.Request) model.Message {
	if objective := firstUserMessage(req.Messages); !strings.HasPrefix(objective, "Research the airline refund policy") {
		return model.Message{Content: "Acknowledged: " + firstLine(objective)}
	}
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
	case 3:
		return model.Message{ToolCalls: []model.ToolCall{{ID: "call_4", Name: "schedule_followup",
			Arguments: json.RawMessage(`{"after_hours":72,"reason":"No reply yet from the airline refunds desk"}`)}}}
	default:
		return model.Message{Content: "Refund requested and formal claim handled."}
	}
}

func firstUserMessage(msgs []model.Message) string {
	for _, m := range msgs {
		if m.Role == model.RoleUser {
			return m.Content
		}
	}
	return ""
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
