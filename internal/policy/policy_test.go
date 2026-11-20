package policy

import (
	"encoding/json"
	"testing"
	"time"
)

func TestEvaluate(t *testing.T) {
	p := Policy{Tools: map[string]Rule{
		"web_search": {Mode: Allow},
		"send_email": {Mode: Allow, Resources: []string{"ok@example.com"}, Otherwise: RequireApproval},
		"pay":        {Mode: Deny},
		"legacy":     {Mode: Allow, Resources: []string{"x"}},
		"typo":       {Mode: "alow"},
	}}
	cases := []struct {
		tool, resource string
		want           Mode
	}{
		{"web_search", "", Allow},
		{"send_email", "ok@example.com", Allow},
		{"send_email", "stranger@example.com", RequireApproval},
		{"pay", "", Deny},
		{"shell", "", Deny},
		{"legacy", "y", Deny},
		{"typo", "", Deny},
	}
	for _, c := range cases {
		if got := Evaluate(p, c.tool, c.resource); got.Mode != c.want {
			t.Errorf("Evaluate(%s, %q) = %s (%s), want %s", c.tool, c.resource, got.Mode, got.Reason, c.want)
		}
	}
}

func TestLimitsRoundTripAndValidate(t *testing.T) {
	var p Policy
	err := json.Unmarshal([]byte(`{"tools":{},"limits":{"max_llm_spend_usd":3,
		"tool_rate_limits":{"send_email":{"max":5,"per":"24h"}}}}`), &p)
	if err != nil {
		t.Fatal(err)
	}
	if p.Limits.MaxLLMSpendMicros() != 3_000_000 || p.Limits.ToolRates["send_email"].Per.Duration != 24*time.Hour {
		t.Fatalf("limits = %+v", p.Limits)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(p)
	var again Policy
	if err := json.Unmarshal(body, &again); err != nil || again.Limits.ToolRates["send_email"].Max != 5 {
		t.Fatalf("round trip failed: %s, %v", body, err)
	}

	if err := json.Unmarshal([]byte(`{"limits":{"tool_rate_limits":{"x":{"max":1,"per":86400}}}}`), &p); err == nil {
		t.Fatal("numeric duration was accepted")
	}
	bad := Policy{Limits: Limits{ToolRates: map[string]RateLimit{"x": {Max: 1}}}}
	if bad.Validate() == nil {
		t.Fatal("zero-length window was accepted")
	}
}
