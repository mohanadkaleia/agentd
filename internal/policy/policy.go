package policy

import (
	"fmt"
	"slices"
)

type Mode string

const (
	Allow           Mode = "allow"
	RequireApproval Mode = "approval"
	Deny            Mode = "deny"
)

type Rule struct {
	Mode      Mode     `json:"mode"`
	Resources []string `json:"resources,omitempty"`
	Otherwise Mode     `json:"otherwise,omitempty"`
}

type Policy struct {
	Tools map[string]Rule `json:"tools"`
}

type Decision struct {
	Mode   Mode   `json:"mode"`
	Reason string `json:"reason"`
}

func Evaluate(p Policy, tool, resource string) Decision {
	rule, ok := p.Tools[tool]
	if !ok {
		return Decision{Deny, fmt.Sprintf("no policy rule for tool %q", tool)}
	}
	mode, reason := rule.Mode, fmt.Sprintf("tool %q rule", tool)
	if len(rule.Resources) > 0 && !slices.Contains(rule.Resources, resource) {
		mode, reason = rule.Otherwise, fmt.Sprintf("resource %q is not pre-approved for %q", resource, tool)
	}
	switch mode {
	case Allow, RequireApproval, Deny:
		return Decision{mode, reason}
	case "":
		return Decision{Deny, reason}
	default:
		return Decision{Deny, fmt.Sprintf("unknown policy mode %q", mode)}
	}
}
