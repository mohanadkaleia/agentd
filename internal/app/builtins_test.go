package app_test

import (
	"strings"
	"testing"

	"agentd/internal/app"
	"agentd/internal/model/scripted"
	"agentd/internal/testutil"
	"agentd/internal/tools"
)

func TestBuiltinNamesCannotCollideWithMCPTools(t *testing.T) {
	a := app.New(testutil.Open(t, testutil.DBPath(t)), scripted.Provider(nil), "scripted", tools.Registry{}, testutil.NewClock().Now)
	for name := range a.Runtime.Tools {
		if strings.Contains(name, "__") {
			t.Errorf("built-in tool %q contains \"__\", which is reserved for <server>__<tool> MCP names", name)
		}
	}
}
