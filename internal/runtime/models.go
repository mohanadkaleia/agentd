package runtime

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"agentd/internal/audit"
	"agentd/internal/model"
	"agentd/internal/policy"
)

type Candidate struct {
	ID       string
	Provider model.Provider
	Local    bool
}

var errNoModel = errors.New("no configured model is allowed by this assignment's model policy")

func (r *Runtime) candidates(p policy.Policy) []Candidate {
	all := append([]Candidate{{ID: r.ModelID, Provider: r.Model, Local: r.ModelLocal}}, r.Fallbacks...)
	var out []Candidate
	for _, c := range all {
		if p.Models.LocalOnly && !c.Local {
			continue
		}
		if len(p.Models.Allowed) > 0 && !slices.Contains(p.Models.Allowed, c.ID) {
			continue
		}
		out = append(out, c)
	}
	return out
}

func (r *Runtime) firstModel(p policy.Policy) (string, error) {
	c := r.candidates(p)
	if len(c) == 0 {
		return "", errNoModel
	}
	return c[0].ID, nil
}

func (r *Runtime) pinned(ctx context.Context, rn *run, assignmentID string, p policy.Policy, msgs []model.Message) (model.Provider, error) {
	cands := r.candidates(p)
	for _, c := range cands {
		if c.ID == rn.Model {
			return c.Provider, nil
		}
	}
	if len(cands) == 0 {
		return nil, errNoModel
	}
	if carriesNative(msgs) {
		return nil, fmt.Errorf("this run is pinned to model %q, which is no longer configured or allowed, and its conversation holds that provider's private data", rn.Model)
	}
	if err := r.repin(ctx, rn, assignmentID, cands[0].ID, "pinned model no longer configured or allowed"); err != nil {
		return nil, err
	}
	return cands[0].Provider, nil
}

func (r *Runtime) fallback(ctx context.Context, rn *run, assignmentID string, p policy.Policy, msgs []model.Message, cause error) (bool, error) {
	for _, m := range msgs {
		if m.Role == model.RoleAssistant {
			return false, nil
		}
	}
	cands := r.candidates(p)
	for i, c := range cands {
		if c.ID == rn.Model && i+1 < len(cands) {
			return true, r.repin(ctx, rn, assignmentID, cands[i+1].ID, "fallback after error: "+cause.Error())
		}
	}
	return false, nil
}

func (r *Runtime) repin(ctx context.Context, rn *run, assignmentID, to, reason string) error {
	now := r.Now()
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET model_id = ? WHERE id = ?`, to, rn.ID); err != nil {
		return err
	}
	if err := audit.Record(ctx, tx, now, assignmentID, "runtime", "run.model_switched", rn.ID,
		map[string]string{"from": rn.Model, "to": to, "reason": reason}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	rn.Model = to
	return nil
}

func carriesNative(msgs []model.Message) bool {
	for _, m := range msgs {
		if m.Native != nil {
			return true
		}
	}
	return false
}
