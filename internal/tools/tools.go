package tools

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	"agentd/internal/model"
)

type Effect string

const (
	EffectRead  Effect = "read"
	EffectWrite Effect = "write"
)

var (
	ErrNotExecuted = errors.New("tool call was not executed")
	ErrNotReady    = errors.New("tool cannot complete yet")
)

type Invocation struct {
	ActionID       string
	ProjectID      string
	TaskID         string
	RunID          string
	IdempotencyKey string
	Name           string
	Arguments      json.RawMessage
}

type Result struct {
	Output     json.RawMessage
	ExternalID string
}

type Tool interface {
	Definition() model.ToolDefinition
	Effect() Effect
	Resource(args json.RawMessage) (string, error)
	Execute(ctx context.Context, call Invocation) (Result, error)
	Reconcile(ctx context.Context, call Invocation) (Result, bool, error)
}

type Registry map[string]Tool

func NewRegistry(ts ...Tool) Registry {
	r := Registry{}
	for _, t := range ts {
		r[t.Definition().Name] = t
	}
	return r
}

func (r Registry) Lookup(name string) (Tool, bool) {
	t, ok := r[name]
	return t, ok
}

func (r Registry) Definitions(allowed func(name string) bool) []model.ToolDefinition {
	defs := make([]model.ToolDefinition, 0, len(r))
	for name, t := range r {
		if allowed(name) {
			defs = append(defs, t.Definition())
		}
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	return defs
}
