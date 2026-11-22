package policy

import (
	"encoding/json"
	"fmt"
	"regexp"
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
	Models ModelPolicy     `json:"models,omitzero"`
}

type ModelPolicy struct {
	Allowed   []string `json:"allowed,omitempty"`
	LocalOnly bool     `json:"local_only,omitempty"`
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

var toolName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (p Policy) Validate() error {
	for tool, rule := range p.Tools {
		if !toolName.MatchString(tool) {
			return fmt.Errorf("tool rule %q: rules name one tool exactly; there are no wildcards, because a server update could add a tool a pattern would silently allow", tool)
		}
		if !validMode(rule.Mode) {
			return fmt.Errorf("tool rule %q: mode %q is not one of allow, approval, deny", tool, rule.Mode)
		}
		if rule.Otherwise != "" && !validMode(rule.Otherwise) {
			return fmt.Errorf("tool rule %q: otherwise %q is not one of allow, approval, deny", tool, rule.Otherwise)
		}
	}
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

func validMode(m Mode) bool {
	return m == Allow || m == RequireApproval || m == Deny
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

func (p Policy) Offers(tool string) bool {
	rule, ok := p.Tools[tool]
	permits := func(m Mode) bool { return m == Allow || m == RequireApproval }
	return ok && (permits(rule.Mode) || len(rule.Resources) > 0 && permits(rule.Otherwise))
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
