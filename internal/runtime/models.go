package runtime

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"agentd/internal/audit"
	"agentd/internal/model"
	"agentd/internal/policy"
	"agentd/internal/tasks"
)

type Candidate struct {
	ID       string
	Alias    string
	Provider model.Provider
	Local    bool
}

// A ModelChooser holds owner choices of model kept outside the task: the conversation a
// turn belongs to.
type ModelChooser interface {
	ConversationModel(ctx context.Context, taskID string) string
}

var errNoModel = errors.New("no configured model is allowed by this assignment's model policy")

func (r *Runtime) candidates(p policy.Policy) []Candidate {
	var out []Candidate
	for _, c := range r.Catalogue() {
		if permitted(p, c) {
			out = append(out, c)
		}
	}
	return out
}

// IsLocal reports whether a configured model keeps data on this machine.
func (r *Runtime) IsLocal(id string) bool {
	return r.local(id)
}

func (r *Runtime) local(id string) bool {
	for _, c := range r.Catalogue() {
		if c.ID == id {
			return c.Local
		}
	}
	return false
}

// Catalogue is every configured model, the default first.
func (r *Runtime) Catalogue() []Candidate {
	return append([]Candidate{{ID: r.ModelID, Alias: r.ModelAlias, Provider: r.Model, Local: r.ModelLocal}}, r.Fallbacks...)
}

func (r *Runtime) find(value string) (Candidate, bool) {
	for _, c := range r.Catalogue() {
		if c.ID == value || (c.Alias != "" && c.Alias == value) {
			return c, true
		}
	}
	return Candidate{}, false
}

// expand turns a choice (a model id, an alias or a tier) into the model it names, if any.
func (r *Runtime) expand(value string) (Candidate, bool) {
	if slices.Contains(policy.Tiers, value) {
		target, ok := r.Tiers[value]
		if !ok {
			return Candidate{}, false
		}
		value = target
	}
	return r.find(value)
}

var ErrUnknownModel = errors.New("no such model is configured; agentd models lists them")

// Choice checks an owner's choice of model: a configured id or alias, a tier, or "auto"
// (no choice, returned as ""). It is checked when set, never first at run time.
func (r *Runtime) Choice(value string) (string, error) {
	value = strings.TrimSpace(value)
	switch {
	case value == "auto" || value == "":
		return "", nil
	case slices.Contains(policy.Tiers, value):
		return value, nil
	}
	if _, ok := r.find(value); !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownModel, value)
	}
	return value, nil
}

// permitted applies the assignment's model limits to one candidate.
func permitted(p policy.Policy, c Candidate) bool {
	if p.Models.LocalOnly && !c.Local {
		return false
	}
	return len(p.Models.Allowed) == 0 || slices.Contains(p.Models.Allowed, c.ID)
}

// Resolution is the model chosen for a run and why.
type Resolution struct {
	Model   string   `json:"model"`
	Source  string   `json:"source"`
	Skipped []string `json:"skipped,omitempty"`
}

// resolveModel picks a run's model from the most specific source with an opinion: the
// owner's choice for the task, for the conversation, the assignment's preferences, the
// task's tier, then the default. Every candidate passes the assignment's model limits
// first, so no source can pick a model the policy forbids. There is no model call in it.
func (r *Runtime) resolveModel(ctx context.Context, t tasks.Task, p policy.Policy) (Resolution, error) {
	type source struct{ name, value string }
	var sources []source
	if t.ModelOverride != "" {
		sources = append(sources, source{"task_override", t.ModelOverride})
	}
	if r.Chooser != nil {
		if v := r.Chooser.ConversationModel(ctx, t.ID); v != "" {
			sources = append(sources, source{"conversation", v})
		}
	}
	for _, v := range p.Models.Prefer {
		sources = append(sources, source{"assignment_prefer", v})
	}
	tier := t.ModelTier
	if tier == "" {
		tier = p.Models.Tier
	}
	if tier != "" {
		sources = append(sources, source{"tier:" + tier, tier})
	}
	var res Resolution
	for _, s := range sources {
		c, ok := r.expand(s.value)
		switch {
		case !ok:
			res.Skipped = append(res.Skipped, fmt.Sprintf("%s %s: not configured", s.name, s.value))
		case !permitted(p, c):
			res.Skipped = append(res.Skipped, fmt.Sprintf("%s %s: not allowed by this assignment's models policy", s.name, c.ID))
		default:
			res.Model, res.Source = c.ID, s.name
			return res, nil
		}
	}
	cands := r.candidates(p)
	if len(cands) == 0 {
		return res, errNoModel
	}
	res.Model, res.Source = cands[0].ID, "default"
	return res, nil
}

// nextTier is the tier a failed run escalates to.
var nextTier = map[string]string{"fast": "default", "default": "strong", "strong": "strong"}

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

// rechoose applies, at a step boundary, a choice of model made since the run's model was
// resolved (an owner's override or an escalation). A transcript holding provider-private
// data stays on its model; the change then takes effect on the task's next run.
func (r *Runtime) rechoose(ctx context.Context, lease tasks.Lease, taskID string, rn *run, p policy.Policy, msgs []model.Message) error {
	t, err := r.Tasks.Get(ctx, taskID)
	if err != nil {
		return err
	}
	choice, err := r.resolveModel(ctx, t, p)
	if err != nil || choice.Model == rn.Requested {
		return nil
	}
	if carriesNative(msgs) && choice.Model != rn.Model {
		return nil
	}
	now := r.Now()
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := tasks.HoldsLease(ctx, tx, lease, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET model_id = ?, requested_model = ? WHERE id = ?`, choice.Model, choice.Model, rn.ID); err != nil {
		return err
	}
	if err := audit.Record(ctx, tx, now, t.AssignmentID, "runtime", "run.model_switched", rn.ID,
		map[string]any{"from": rn.Model, "to": choice.Model, "reason": "new choice: " + choice.Source, "skipped": choice.Skipped}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	rn.Model, rn.Requested = choice.Model, choice.Model
	return nil
}

// escalate asks for one tier stronger after a run failed because of its model, unless the
// owner chose the task's model.
func (r *Runtime) escalate(ctx context.Context, taskID string, p policy.Policy) error {
	t, err := r.Tasks.Get(ctx, taskID)
	if err != nil || t.ModelOverride != "" {
		return err
	}
	from := t.ModelTier
	if from == "" {
		from = p.Models.Tier
	}
	if from == "" {
		from = "default"
	}
	to := nextTier[from]
	if to == from {
		return nil
	}
	now := r.Now()
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET model_tier = ? WHERE id = ? AND model_override IS NULL`, to, taskID); err != nil {
		return err
	}
	if err := audit.Record(ctx, tx, now, t.AssignmentID, "runtime", "task.model_escalated", taskID,
		map[string]string{"from": from, "to": to, "reason": "the model failed"}); err != nil {
		return err
	}
	return tx.Commit()
}

// SetTaskModel records the owner's choice of model for a task (an id, alias, tier, or
// "auto" to clear it). The choice is checked against the configured models and the
// assignment's model limits now; the message says when it takes effect and warns when the
// task's content would leave this machine.
func (r *Runtime) SetTaskModel(ctx context.Context, taskID, value, by string) (string, error) {
	choice, err := r.Choice(value)
	if err != nil {
		return "", err
	}
	t, err := r.Tasks.Get(ctx, taskID)
	if err != nil {
		return "", err
	}
	a, err := r.Assignments.Get(ctx, t.AssignmentID)
	if err != nil {
		return "", err
	}
	current, _ := r.resolveModel(ctx, t, a.Policy)
	warning := ""
	if choice != "" {
		c, ok := r.expand(choice)
		if !ok {
			return "", fmt.Errorf("tier %q is not mapped to a model; agentd models shows the tiers", choice)
		}
		if !permitted(a.Policy, c) {
			return "", fmt.Errorf("%s is not allowed by this assignment's models policy", c.ID)
		}
		if !c.Local && r.local(current.Model) {
			warning = fmt.Sprintf(" This sends the task's content to %s instead of keeping it on this machine.", provider(c.ID))
		}
	}
	now := r.Now()
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET model_override = NULLIF(?, ''), updated_at = ? WHERE id = ?`, choice, now.UnixMilli(), taskID); err != nil {
		return "", err
	}
	if err := audit.Record(ctx, tx, now, t.AssignmentID, by, "task.model_set", taskID, map[string]string{"model": choice}); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	if choice == "" {
		return "The task's model is automatic again." + warning, nil
	}
	msgs, _ := r.Transcript(ctx, taskID)
	when := "It applies from the task's next step."
	if carriesNative(msgs) {
		when = "The task's current run holds provider-private data, so it finishes on its model; the choice applies from its next run."
	}
	return fmt.Sprintf("Task %s will use %s. %s%s", taskID, choice, when, warning), nil
}

// Describe explains a choice for the owner: the model it names and whether it is local.
func (r *Runtime) Describe(choice string) string {
	c, ok := r.expand(choice)
	if !ok {
		return choice + " (not mapped)"
	}
	where := "cloud"
	if c.Local {
		where = "local"
	}
	if c.ID == choice {
		return fmt.Sprintf("%s (%s)", c.ID, where)
	}
	return fmt.Sprintf("%s = %s (%s)", choice, c.ID, where)
}

// Egress reports whether choosing a model moves work from a local model to a cloud one.
func (r *Runtime) Egress(from, to string) bool {
	c, ok := r.expand(to)
	return ok && !c.Local && r.local(from)
}

func provider(id string) string {
	name, _, _ := strings.Cut(id, "/")
	return name
}

// Preview resolves the model a new task with this tier would run on under a policy.
func (r *Runtime) Preview(ctx context.Context, p policy.Policy, tier string) (Resolution, error) {
	return r.resolveModel(ctx, tasks.Task{ModelTier: tier}, p)
}
