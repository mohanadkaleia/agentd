package mcp

import (
	"fmt"
	"testing"

	"agentd/internal/gateway"
	"agentd/internal/integrations/mcp/mcptest"
	"agentd/internal/policy"
)

// For every way a write can fail and every recovery the owner can declare, the action ends
// in one of four states and the service never holds two effects for one action.
func TestEveryFailurePointEndsInAKnownStateWithoutDuplicates(t *testing.T) {
	recon := reconciled(0).Meta["post"].Reconcile
	recon.Lookup.Tool = "find_message"
	configs := map[string]ToolFilter{
		"plain":      {Meta: map[string]ToolMeta{"find_message": {Effect: "read"}}},
		"idempotent": {Meta: map[string]ToolMeta{"post_message": {Idempotent: true}, "find_message": {Effect: "read"}}},
		"reconciled": {Meta: map[string]ToolMeta{"post_message": {Reconcile: recon}, "find_message": {Effect: "read"}}},
	}
	final := map[string]bool{gateway.Succeeded: true, gateway.Failed: true, gateway.Denied: true, gateway.NeedsReconciliation: true}
	for name, filter := range configs {
		for _, mode := range []string{mcptest.Normal, mcptest.FailBefore, mcptest.ApplyThenFail} {
			t.Run(fmt.Sprintf("%s/%s", name, mode), func(t *testing.T) {
				chat := mcptest.NewChat()
				w := newWorldWith(t, &fake{srv: chat.Server}, filter,
					policy.Policy{Tools: map[string]policy.Rule{"fake__post_message": {Mode: policy.Allow}}}, true)
				chat.Next(mode)
				_, status := w.handle("fake__post_message", `{"channel_id":"C1","text":"hi"}`)
				if !final[status] {
					t.Fatalf("status = %q, want a final or blocked state", status)
				}
				posts := len(chat.Posts("C1"))
				if posts > 1 || chat.DuplicateRefs() != 0 {
					t.Fatalf("%d posts for one action", posts)
				}
				if status == gateway.Succeeded && posts != 1 || status == gateway.Failed && name != "idempotent" && posts != 0 {
					t.Fatalf("status %s with %d posts: the ledger disagrees with the world", status, posts)
				}
				if name == "reconciled" && mode == mcptest.ApplyThenFail && status != gateway.Succeeded {
					t.Fatalf("reconciled write that landed = %s, want succeeded through the lookup", status)
				}
				if name == "plain" && mode != mcptest.Normal && status != gateway.NeedsReconciliation {
					t.Fatalf("plain write with an unclear outcome = %s, want needs_reconciliation", status)
				}
			})
		}
	}
}
