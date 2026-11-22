package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"agentd/internal/audit"
	"agentd/internal/policy"
)

// ConversationModel is the owner's choice of model for the conversation a turn belongs to.
// It implements runtime.ModelChooser.
func (s *Service) ConversationModel(ctx context.Context, taskID string) string {
	var model string
	s.DB.QueryRowContext(ctx, `SELECT COALESCE(c.model, '') FROM conversations c WHERE c.id = (
		SELECT conversation_id FROM conversation_messages WHERE turn_task_id = ? LIMIT 1)`, taskID).Scan(&model)
	return model
}

// SetConversationModel records the owner's choice of model for a conversation, from its
// next turn. The conversation is rebuilt from stored messages each turn, so no provider's
// private data crosses to another.
func (s *Service) SetConversationModel(ctx context.Context, conv, value, by string) (string, error) {
	choice, err := s.Runtime.Choice(value)
	if err != nil {
		return "", err
	}
	desk, err := s.FrontDesk(ctx)
	if err != nil {
		return "", err
	}
	a, err := s.Assignments.Get(ctx, desk)
	if err != nil {
		return "", err
	}
	warning := ""
	if choice != "" {
		if strings.Contains(s.Runtime.Describe(choice), "not mapped") {
			return "", fmt.Errorf("tier %q is not mapped to a model", choice)
		}
		if a.Policy.Models.LocalOnly && !strings.HasSuffix(s.Runtime.Describe(choice), "(local)") {
			return "", errors.New("the conversation is limited to local models by the front desk's policy")
		}
		if s.Runtime.Egress(s.Runtime.ModelID, choice) {
			warning = " This sends the conversation, and what agentd tells it about your work, to a cloud provider."
		}
	}
	res, err := s.DB.ExecContext(ctx, `UPDATE conversations SET model = NULLIF(?, '') WHERE id = ?`, choice, conv)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", ErrNotFound
	}
	if err := audit.Record(ctx, s.DB, s.Now(), desk, by, "chat.model_set", conv, map[string]string{"model": choice}); err != nil {
		return "", err
	}
	if choice == "" {
		return "This conversation chooses its model automatically again (the fast tier).", nil
	}
	return "This conversation uses " + s.Runtime.Describe(choice) + " from its next reply." + warning, nil
}

func (s *Service) cmdModel(ctx context.Context, c call) (string, error) {
	by := "owner:" + c.channel
	switch {
	case len(c.args) == 0:
		var b strings.Builder
		var current string
		s.DB.QueryRowContext(ctx, `SELECT COALESCE(model, '') FROM conversations WHERE id = ?`, c.conv).Scan(&current)
		if current == "" {
			fmt.Fprintf(&b, "This conversation: automatic (fast tier: %s)\n", s.Runtime.Describe("fast"))
		} else {
			fmt.Fprintf(&b, "This conversation: %s (your choice)\n", s.Runtime.Describe(current))
		}
		b.WriteString("Models:\n")
		for i, m := range s.Runtime.Catalogue() {
			where := "cloud"
			if m.Local {
				where = "local"
			}
			line := "  " + m.ID
			if m.Alias != "" {
				line += " (" + m.Alias + ")"
			}
			line += ", " + where
			if i == 0 {
				line += ", default"
			}
			b.WriteString(line + "\n")
		}
		for _, tier := range policy.Tiers {
			fmt.Fprintf(&b, "  tier %s: %s\n", tier, s.Runtime.Describe(tier))
		}
		b.WriteString("Change it with /model <model or tier>, /model auto, or /model <task-id> <model or tier>.")
		return b.String(), nil
	case len(c.args) == 1:
		return s.SetConversationModel(ctx, c.conv, c.args[0], by)
	case len(c.args) == 2 && strings.HasPrefix(c.args[0], "tsk_"):
		return s.Runtime.SetTaskModel(ctx, c.args[0], c.args[1], by)
	}
	return "", errors.New("usage: /model, /model <model or tier or auto>, or /model <task-id> <model or tier or auto>")
}
