package model

import (
	"context"
	"encoding/json"
)

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	Native     *Native    `json:"native,omitempty"`
}

type Native struct {
	Provider string          `json:"provider"`
	Payload  json.RawMessage `json:"payload"`
}

type StopReason string

const (
	StopEnd       StopReason = "end"
	StopToolUse   StopReason = "tool_use"
	StopMaxTokens StopReason = "max_tokens"
	StopRefusal   StopReason = "refusal"
)

type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type Request struct {
	Model     string
	Messages  []Message
	Tools     []ToolDefinition
	MaxTokens int
}

type Usage struct {
	InputTokens  int64
	OutputTokens int64
}

type Response struct {
	Message    Message
	Usage      Usage
	StopReason StopReason
}

type Provider interface {
	Complete(ctx context.Context, req Request) (Response, error)
}
