package policy

import "testing"

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
