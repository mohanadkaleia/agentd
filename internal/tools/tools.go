package tools

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"agentd/internal/model"
)

type Effect string

const (
	EffectRead  Effect = "read"
	EffectWrite Effect = "write"
)

type Finding int

const (
	Unknown Finding = iota
	Happened
	NotHappened
)

var (
	ErrNotExecuted = errors.New("tool call was not executed")
	ErrNotReady    = errors.New("tool cannot complete yet")
)

type Invocation struct {
	ActionID       string
	AssignmentID   string
	TaskID         string
	RunID          string
	IdempotencyKey string
	Name           string
	Arguments      json.RawMessage
	Scope          []string
	DispatchedAt   time.Time
	Now            time.Time
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
	Reconcile(ctx context.Context, call Invocation) (Result, Finding, error)
}

// Pinned tools run only under the definition the owner accepted. Pin returns that
// definition's hash, or why the tool cannot run now.
type Pinned interface {
	Tool
	Pin() (hash, unavailable string)
}

func Ready(t Tool) bool {
	if p, ok := t.(Pinned); ok {
		_, unavailable := p.Pin()
		return unavailable == ""
	}
	return t != nil
}

type Untargeted interface {
	Tool
	NoResource() bool
}

type Scoped interface {
	Tool
	FiltersToScope()
}

// Restricted tools run only for the built-in assignment they name, whatever any policy says.
type Restricted interface {
	Tool
	OnlyFor() string
}

// OwnerConfirmed tools always need the owner's approval: the gateway asks for it whatever the
// policy says, and a policy that allows one outright is refused.
type OwnerConfirmed interface {
	Tool
	NeedsOwner()
}

func NeedsOwner(t Tool) bool {
	_, ok := t.(OwnerConfirmed)
	return ok
}

// Describer tools explain a call in plain words for the owner deciding on it, with warnings.
type Describer interface {
	Tool
	Describe(args json.RawMessage) (title, text string)
}

// RateLimited tools have a rate limit that applies when the policy sets none.
type RateLimited interface {
	Tool
	DefaultRate() (max int, per time.Duration)
}

// Rendered tools can show a call's arguments as a readable file for the owner. The
// rendering is for reading only: approvals bind the canonical JSON arguments.
type Rendered interface {
	Tool
	Render(args json.RawMessage) string
}

// Permits reports whether a tool may run for an assignment with this built-in key ("" for none).
func Permits(t Tool, builtin string) bool {
	r, ok := t.(Restricted)
	return !ok || r.OnlyFor() == builtin
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
