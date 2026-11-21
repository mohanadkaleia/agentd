package policy

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"
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
	Tools  map[string]Rule `json:"tools"`
	Limits Limits          `json:"limits,omitzero"`
}

type Limits struct {
	MaxLLMSpendUSD float64              `json:"max_llm_spend_usd,omitempty"`
	BudgetPeriod   Duration             `json:"budget_period,omitzero"`
	ToolRates      map[string]RateLimit `json:"tool_rate_limits,omitempty"`
}

type RateLimit struct {
	Max int      `json:"max"`
	Per Duration `json:"per"`
}

type Duration struct {
	time.Duration
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"24h\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (l Limits) MaxLLMSpendMicros() int64 {
	return int64(l.MaxLLMSpendUSD * 1e6)
}

func (p Policy) Validate() error {
	if p.Limits.MaxLLMSpendUSD < 0 {
		return fmt.Errorf("max_llm_spend_usd must not be negative")
	}
	if p.Limits.BudgetPeriod.Duration < 0 {
		return fmt.Errorf("budget_period must not be negative")
	}
	for tool, r := range p.Limits.ToolRates {
		if r.Max < 0 || r.Per.Duration <= 0 {
			return fmt.Errorf("rate limit for %q needs max >= 0 and a positive window", tool)
		}
	}
	return nil
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
	return decide(mode, reason)
}

func EvaluateScope(p Policy, tool string) (Decision, []string) {
	rule, ok := p.Tools[tool]
	if !ok {
		return Decision{Deny, fmt.Sprintf("no policy rule for tool %q", tool)}, nil
	}
	return decide(rule.Mode, fmt.Sprintf("tool %q rule", tool)), rule.Resources
}

func decide(mode Mode, reason string) Decision {
	switch mode {
	case Allow, RequireApproval, Deny:
		return Decision{mode, reason}
	case "":
		return Decision{Deny, reason}
	default:
		return Decision{Deny, fmt.Sprintf("unknown policy mode %q", mode)}
	}
}
