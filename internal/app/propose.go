package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"agentd/internal/assignments"
	"agentd/internal/audit"
	"agentd/internal/chat"
	"agentd/internal/model"
	"agentd/internal/policy"
	"agentd/internal/scheduler"
	"agentd/internal/tasks"
	"agentd/internal/tools"
)

// ProposalCeilingUSD is the budget above which a proposal card warns the owner.
const ProposalCeilingUSD = 20

// A proposal is a front desk tool whose call is a draft: it always waits for the owner's
// approval (tools.OwnerConfirmed), and on approval applies the draft through the same code
// the CLI and API use, in one transaction that also records which action applied it, so a
// crash can be reconciled with certainty.
type proposal struct {
	app      *App
	name     string
	desc     string
	schema   string
	check    func(ctx context.Context, args json.RawMessage) (resource string, err error)
	describe func(args json.RawMessage) (title, text string)
	render   func(args json.RawMessage) string
	apply    func(ctx context.Context, tx *sql.Tx, args json.RawMessage, origin string) (string, error)
}

func (p *proposal) Definition() model.ToolDefinition {
	return model.ToolDefinition{Name: p.name, Description: p.desc, InputSchema: json.RawMessage(p.schema)}
}
func (p *proposal) Effect() tools.Effect { return tools.EffectWrite }
func (p *proposal) OnlyFor() string      { return chat.FrontDeskKey }
func (p *proposal) NeedsOwner()          {}

func (p *proposal) Resource(args json.RawMessage) (string, error) {
	return p.check(context.Background(), args)
}

func (p *proposal) Describe(args json.RawMessage) (string, string) {
	return p.describe(args)
}

func (p *proposal) Render(args json.RawMessage) string {
	if p.render == nil {
		return ""
	}
	return p.render(args)
}

func (p *proposal) Execute(ctx context.Context, call tools.Invocation) (tools.Result, error) {
	id, err := p.app.applyOnce(ctx, call.ActionID, func(tx *sql.Tx) (string, error) {
		return p.apply(ctx, tx, call.Arguments, call.IdempotencyKey)
	})
	if err != nil {
		return tools.Result{}, fmt.Errorf("%w: %v", tools.ErrNotExecuted, err)
	}
	out, _ := json.Marshal(map[string]string{"applied": p.name, "id": id})
	return tools.Result{Output: out, ExternalID: id}, nil
}

// Reconcile is authoritative: the change and its applied record commit together.
func (p *proposal) Reconcile(ctx context.Context, call tools.Invocation) (tools.Result, tools.Finding, error) {
	var id string
	err := p.app.DB.QueryRowContext(ctx, `SELECT object_id FROM proposal_applied WHERE action_id = ?`, call.ActionID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return tools.Result{}, tools.NotHappened, nil
	}
	if err != nil {
		return tools.Result{}, tools.Unknown, err
	}
	out, _ := json.Marshal(map[string]string{"applied": p.name, "id": id})
	return tools.Result{Output: out, ExternalID: id}, tools.Happened, nil
}

func (a *App) applyOnce(ctx context.Context, actionID string, apply func(*sql.Tx) (string, error)) (string, error) {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx, `SELECT object_id FROM proposal_applied WHERE action_id = ?`, actionID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if id, err = apply(tx); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO proposal_applied (action_id, object_id, applied_at) VALUES (?, ?, ?)`,
		actionID, id, a.Runtime.Now().UnixMilli()); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// pauser pauses an assignment at once: narrowing what runs needs no approval.
type pauser struct{ app *App }

func (p *pauser) Definition() model.ToolDefinition {
	return model.ToolDefinition{Name: chat.PauseTool, Description: "Pause a project or responsibility now: no new work starts until the owner resumes it.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"assignment_id":{"type":"string"},"reason":{"type":"string"}},"required":["assignment_id"]}`)}
}
func (p *pauser) Effect() tools.Effect { return tools.EffectWrite }
func (p *pauser) OnlyFor() string      { return chat.FrontDeskKey }
func (p *pauser) Resource(args json.RawMessage) (string, error) {
	var in target
	if err := json.Unmarshal(args, &in); err != nil {
		return "", err
	}
	return in.AssignmentID, p.app.ordinary(context.Background(), in.AssignmentID)
}
func (p *pauser) Execute(ctx context.Context, call tools.Invocation) (tools.Result, error) {
	var in struct {
		target
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(call.Arguments, &in); err != nil {
		return tools.Result{}, fmt.Errorf("%w: %v", tools.ErrNotExecuted, err)
	}
	reason := strings.TrimSpace("paused from chat: " + in.Reason)
	if err := p.app.Assignments.Pause(ctx, in.AssignmentID, assignments.PausedByUser, reason, "front-desk"); err != nil {
		return tools.Result{}, fmt.Errorf("%w: %v", tools.ErrNotExecuted, err)
	}
	return tools.Result{Output: json.RawMessage(`{"paused":true}`)}, nil
}
func (p *pauser) Reconcile(ctx context.Context, call tools.Invocation) (tools.Result, tools.Finding, error) {
	var in target
	json.Unmarshal(call.Arguments, &in)
	a, err := p.app.Assignments.Get(ctx, in.AssignmentID)
	if err != nil {
		return tools.Result{}, tools.Unknown, err
	}
	if a.Status == assignments.Paused {
		return tools.Result{Output: json.RawMessage(`{"paused":true}`)}, tools.Happened, nil
	}
	return tools.Result{}, tools.NotHappened, nil
}

type target struct {
	AssignmentID string `json:"assignment_id"`
}

// ordinary refuses built-in assignments and unknown ids as targets.
func (a *App) ordinary(ctx context.Context, id string) error {
	got, err := a.Assignments.Get(ctx, id)
	if errors.Is(err, assignments.ErrNotFound) {
		return fmt.Errorf("no project or responsibility %q; use list_assignments for ids", id)
	}
	if err != nil {
		return err
	}
	if got.Builtin != "" {
		return errors.New("built-in assignments cannot be changed from chat")
	}
	return nil
}

type assignmentProposal struct {
	Spec      Spec       `json:"spec"`
	FirstTask *firstTask `json:"first_task,omitempty"`
}

type firstTask struct {
	Title     string `json:"title"`
	Objective string `json:"objective"`
}

func decode[T any](args json.RawMessage) (T, error) {
	var v T
	dec := json.NewDecoder(strings.NewReader(string(args)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, fmt.Errorf("arguments do not match the schema: %v", err)
	}
	return v, nil
}

// decodeAssignment reads {"spec": {...}, "first_task": {...}}, and also a spec given at the top
// level, which smaller models tend to write.
func decodeAssignment(args json.RawMessage) (assignmentProposal, error) {
	in, err := decode[assignmentProposal](args)
	if err == nil {
		return in, nil
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal(args, &probe) != nil || probe["kind"] == nil {
		return in, err
	}
	first := probe["first_task"]
	delete(probe, "first_task")
	flat, _ := json.Marshal(probe)
	spec, serr := decode[Spec](flat)
	if serr != nil {
		return in, serr
	}
	in = assignmentProposal{Spec: spec}
	if first != nil {
		task, ferr := decode[firstTask](first)
		if ferr != nil {
			return in, ferr
		}
		in.FirstTask = &task
	}
	return in, nil
}

const specSchema = `{"type":"object","description":"The same JSON as an agentd create file.","additionalProperties":false,"properties":{
"kind":{"type":"string","enum":["project","responsibility"],"description":"project: finishes when its criteria are met; responsibility: ongoing"},
"name":{"type":"string","description":"short, lowercase-with-dashes"},
"objective":{"type":"string"},
"policy":{"type":"object","additionalProperties":false,"properties":{
  "tools":{"type":"object","description":"one entry per tool name from list_tools","additionalProperties":{"type":"object","additionalProperties":false,"properties":{
    "mode":{"type":"string","enum":["allow","approval","deny"]},
    "resources":{"type":"array","items":{"type":"string"},"description":"only these targets (recipients, hostnames, channel ids) get mode"},
    "otherwise":{"type":"string","enum":["allow","approval","deny"],"description":"mode for any other target"}},"required":["mode"]}},
  "limits":{"type":"object","additionalProperties":false,"properties":{
    "max_llm_spend_usd":{"type":"number"},
    "budget_period":{"type":"string","description":"like \"720h\"; required for a responsibility"}},"required":["max_llm_spend_usd"]}},
  "required":["tools","limits"]},
"completion":{"type":"object","description":"projects only","additionalProperties":false,"properties":{
  "criteria":{"type":"array","items":{"type":"object","additionalProperties":false,"properties":{
    "id":{"type":"string"},"description":{"type":"string"},
    "event":{"type":"object","description":"optional: an event that proves it","properties":{"source":{"type":"string"},"kind":{"type":"string"},"match":{"type":"object"}}}},
    "required":["description"]}}}},
"notifications":{"type":"object","description":"responsibilities only","additionalProperties":false,"properties":{
  "min_severity":{"type":"string","enum":["info","attention","action"]},"dedup_window":{"type":"string"},"max_per_day":{"type":"integer"}}},
"schedules":{"type":"array","items":{"type":"object","additionalProperties":false,"properties":{"every":{"type":"string"},"title":{"type":"string"},"objective":{"type":"string"}},"required":["every","title","objective"]}},
"routes":{"type":"array","items":{"type":"object","properties":{"source":{"type":"string"},"kind":{"type":"string"},"match":{"type":"object"},"title":{"type":"string"},"objective":{"type":"string"}},"required":["source","kind","title","objective"]}}},
"required":["kind","name","objective","policy"]}`

func (a *App) proposals() []tools.Tool {
	return []tools.Tool{
		&proposal{app: a, name: "propose_assignment",
			desc: "Draft a new project or responsibility for the owner to approve. Nothing exists until they approve this exact draft. " +
				"Include first_task for work that should start at once.",
			schema: `{"type":"object","properties":{"spec":` + specSchema + `,"first_task":{"type":"object","properties":{"title":{"type":"string"},"objective":{"type":"string"}},"required":["title","objective"]}},"required":["spec"]}`,
			check: func(ctx context.Context, args json.RawMessage) (string, error) {
				in, err := decodeAssignment(args)
				if err != nil {
					return "", err
				}
				return in.Spec.Name, a.checkSpec(in.Spec)
			},
			describe: a.describeAssignment,
			render: func(args json.RawMessage) string {
				in, err := decodeAssignment(args)
				if err != nil {
					return ""
				}
				md, _ := in.Spec.Markdown()
				return md
			},
			apply: func(ctx context.Context, tx *sql.Tx, args json.RawMessage, origin string) (string, error) {
				in, err := decodeAssignment(args)
				if err != nil {
					return "", err
				}
				id, err := a.CreateTx(ctx, tx, in.Spec)
				if err != nil {
					return "", err
				}
				first := in.FirstTask
				if first == nil && in.Spec.Kind == assignments.KindProject {
					first = &firstTask{Title: "Start: " + in.Spec.Name, Objective: in.Spec.Objective}
				}
				if first != nil {
					if _, err := a.Tasks.CreateTx(ctx, tx, tasks.NewTask{AssignmentID: id, OriginKey: origin + ":first",
						Title: first.Title, Objective: first.Objective}); err != nil {
						return "", err
					}
				}
				return id, nil
			}},
		&proposal{app: a, name: "propose_policy",
			desc:   "Draft a replacement policy (tools and limits) for an existing assignment. The owner sees what it widens before approving.",
			schema: `{"type":"object","properties":{"assignment_id":{"type":"string"},"policy":{"type":"object"}},"required":["assignment_id","policy"]}`,
			check: func(ctx context.Context, args json.RawMessage) (string, error) {
				in, err := decode[struct {
					target
					Policy policy.Policy `json:"policy"`
				}](args)
				if err != nil {
					return "", err
				}
				if err := a.ordinary(ctx, in.AssignmentID); err != nil {
					return "", err
				}
				return in.AssignmentID, a.checkPolicy(in.Policy)
			},
			describe: a.describePolicy,
			apply: func(ctx context.Context, tx *sql.Tx, args json.RawMessage, _ string) (string, error) {
				in, err := decode[struct {
					target
					Policy policy.Policy `json:"policy"`
				}](args)
				if err != nil {
					return "", err
				}
				_, err = a.Assignments.SetPolicyTx(ctx, tx, in.AssignmentID, in.Policy, "owner")
				return in.AssignmentID, err
			}},
		&proposal{app: a, name: "propose_schedule",
			desc:   "Draft a recurring check on an existing assignment: every (like \"24h\"), title, objective.",
			schema: `{"type":"object","properties":{"assignment_id":{"type":"string"},"schedule":{"type":"object","properties":{"every":{"type":"string"},"title":{"type":"string"},"objective":{"type":"string"},"skills":{"type":"array","items":{"type":"string"}}},"required":["every","title","objective"]}},"required":["assignment_id","schedule"]}`,
			check: func(ctx context.Context, args json.RawMessage) (string, error) {
				in, err := decode[struct {
					target
					Schedule ScheduleSpec `json:"schedule"`
				}](args)
				if err != nil {
					return "", err
				}
				if err := a.ordinary(ctx, in.AssignmentID); err != nil {
					return "", err
				}
				if in.Schedule.Every.Duration < 5*time.Minute || in.Schedule.Title == "" || in.Schedule.Objective == "" {
					return "", errors.New("a schedule needs every (5m or more), title and objective")
				}
				_, err = a.Skills.Resolve(in.Schedule.Skills)
				return in.AssignmentID, err
			},
			describe: func(args json.RawMessage) (string, string) {
				var in struct {
					target
					Schedule ScheduleSpec `json:"schedule"`
				}
				json.Unmarshal(args, &in)
				return "New schedule on " + a.nameOf(in.AssignmentID), fmt.Sprintf("Every %s: %s\nInstruction (written by the agent): %s",
					shortDuration(in.Schedule.Every.Duration), in.Schedule.Title, in.Schedule.Objective)
			},
			apply: func(ctx context.Context, tx *sql.Tx, args json.RawMessage, _ string) (string, error) {
				var in struct {
					target
					Schedule ScheduleSpec `json:"schedule"`
				}
				json.Unmarshal(args, &in)
				return a.Scheduler.AddScheduleTx(ctx, tx, scheduler.Schedule{AssignmentID: in.AssignmentID, Every: in.Schedule.Every.Duration,
					Skills: in.Schedule.Skills, Title: in.Schedule.Title, Objective: in.Schedule.Objective})
			}},
		&proposal{app: a, name: "propose_route",
			desc:   "Draft a route on an existing assignment: events of a source and kind (optionally matching payload fields) become tasks.",
			schema: `{"type":"object","properties":{"assignment_id":{"type":"string"},"route":{"type":"object","properties":{"source":{"type":"string"},"kind":{"type":"string"},"match":{"type":"object"},"coalesce_field":{"type":"string"},"title":{"type":"string"},"objective":{"type":"string"}},"required":["source","kind","title","objective"]}},"required":["assignment_id","route"]}`,
			check: func(ctx context.Context, args json.RawMessage) (string, error) {
				in, err := decode[struct {
					target
					Route RouteSpec `json:"route"`
				}](args)
				if err != nil {
					return "", err
				}
				if err := a.ordinary(ctx, in.AssignmentID); err != nil {
					return "", err
				}
				if in.Route.Source == "" || in.Route.Kind == "" || in.Route.Title == "" || in.Route.Objective == "" {
					return "", errors.New("a route needs source, kind, title and objective")
				}
				_, err = a.Skills.Resolve(in.Route.Skills)
				return in.AssignmentID, err
			},
			describe: func(args json.RawMessage) (string, string) {
				var in struct {
					target
					Route RouteSpec `json:"route"`
				}
				json.Unmarshal(args, &in)
				match, _ := json.Marshal(in.Route.Match)
				return "New route on " + a.nameOf(in.AssignmentID), fmt.Sprintf("On %s/%s events matching %s: %s\nInstruction (written by the agent): %s",
					in.Route.Source, in.Route.Kind, match, in.Route.Title, in.Route.Objective)
			},
			apply: func(ctx context.Context, tx *sql.Tx, args json.RawMessage, _ string) (string, error) {
				var in struct {
					target
					Route RouteSpec `json:"route"`
				}
				json.Unmarshal(args, &in)
				return a.Scheduler.AddRouteTx(ctx, tx, scheduler.Route{AssignmentID: in.AssignmentID, Source: in.Route.Source, Kind: in.Route.Kind,
					Match: in.Route.Match, CoalesceField: in.Route.CoalesceField, Skills: in.Route.Skills, Title: in.Route.Title, Objective: in.Route.Objective})
			}},
		&proposal{app: a, name: "propose_task",
			desc:   "Draft one extra task in an existing assignment, such as \"also check whether they charged me twice\".",
			schema: `{"type":"object","properties":{"assignment_id":{"type":"string"},"title":{"type":"string"},"objective":{"type":"string"}},"required":["assignment_id","title","objective"]}`,
			check: func(ctx context.Context, args json.RawMessage) (string, error) {
				in, err := decode[struct {
					target
					Title     string `json:"title"`
					Objective string `json:"objective"`
				}](args)
				if err != nil {
					return "", err
				}
				if in.Title == "" || in.Objective == "" {
					return "", errors.New("a task needs a title and an objective")
				}
				return in.AssignmentID, a.ordinary(ctx, in.AssignmentID)
			},
			describe: func(args json.RawMessage) (string, string) {
				var in struct {
					target
					Title     string `json:"title"`
					Objective string `json:"objective"`
				}
				json.Unmarshal(args, &in)
				return "New task in " + a.nameOf(in.AssignmentID) + ": " + in.Title,
					"Instruction (written by the agent; it becomes the task's objective): " + in.Objective
			},
			apply: func(ctx context.Context, tx *sql.Tx, args json.RawMessage, origin string) (string, error) {
				var in struct {
					target
					Title     string `json:"title"`
					Objective string `json:"objective"`
				}
				json.Unmarshal(args, &in)
				return a.Tasks.CreateTx(ctx, tx, tasks.NewTask{AssignmentID: in.AssignmentID, OriginKey: origin, Title: in.Title, Objective: in.Objective})
			}},
		&proposal{app: a, name: "propose_resume",
			desc:   "Ask the owner to resume a paused project or responsibility.",
			schema: `{"type":"object","properties":{"assignment_id":{"type":"string"}},"required":["assignment_id"]}`,
			check: func(ctx context.Context, args json.RawMessage) (string, error) {
				in, err := decode[target](args)
				if err != nil {
					return "", err
				}
				return in.AssignmentID, a.ordinary(ctx, in.AssignmentID)
			},
			describe: func(args json.RawMessage) (string, string) {
				var in target
				json.Unmarshal(args, &in)
				return "Resume " + a.nameOf(in.AssignmentID), "Work on it starts again."
			},
			apply: func(ctx context.Context, tx *sql.Tx, args json.RawMessage, _ string) (string, error) {
				var in target
				json.Unmarshal(args, &in)
				return in.AssignmentID, a.Assignments.ResumeTx(ctx, tx, in.AssignmentID, "owner")
			}},
		&pauser{app: a},
	}
}

func (a *App) nameOf(id string) string {
	got, err := a.Assignments.Get(context.Background(), id)
	if err != nil {
		return id
	}
	return fmt.Sprintf("%q (%s)", got.Name, id)
}

// checkSpec fails a draft that would fail later, so the model can fix it before the owner
// is asked: the spec and its policy are valid, every tool and skill it names exists, and it
// has a budget.
func (a *App) checkSpec(s Spec) error {
	if err := s.validate(); err != nil {
		return err
	}
	if s.Kind != assignments.KindProject && s.Kind != assignments.KindResponsibility {
		return errors.New(`kind must be "project" or "responsibility"`)
	}
	if err := a.checkPolicy(s.Policy); err != nil {
		return err
	}
	for _, names := range s.skillNames() {
		if _, err := a.Skills.Resolve(names); err != nil {
			return err
		}
	}
	if s.Kind == assignments.KindResponsibility {
		if s.Policy.Limits.BudgetPeriod.Duration <= 0 {
			return errors.New("a responsibility needs limits.budget_period, such as \"720h\"")
		}
		if s.Notifications == nil {
			return errors.New("a responsibility needs notifications: {min_severity, dedup_window, max_per_day}")
		}
		if err := s.Notifications.Validate(); err != nil {
			return err
		}
	}
	if s.Kind == assignments.KindProject && len(s.Completion.Criteria) == 0 {
		return errors.New("a project needs at least one completion criterion")
	}
	return nil
}

func (a *App) checkPolicy(p policy.Policy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if p.Limits.MaxLLMSpendUSD <= 0 {
		return errors.New("limits.max_llm_spend_usd is required")
	}
	for name := range p.Tools {
		tool, ok := a.Runtime.Tools[name]
		if !ok {
			return fmt.Errorf("no tool named %q; list_tools shows the real names", name)
		}
		if !tools.Permits(tool, "") {
			return fmt.Errorf("tool %q is only for the conversation itself", name)
		}
	}
	return a.ownerOnly(p)
}

// checkPolicy refuses what the policy package cannot judge alone: an owner-confirmed tool
// allowed outright, and a preferred model that is not configured (when this process knows
// the configured models).
func (a *App) checkStored(p policy.Policy) error {
	if err := a.ownerOnly(p); err != nil {
		return err
	}
	if a.Runtime.ModelID == "" {
		return nil
	}
	for _, m := range p.Models.Prefer {
		if _, err := a.Runtime.Choice(m); err != nil {
			return fmt.Errorf("models.prefer: %w", err)
		}
	}
	return nil
}

// ownerOnly refuses a policy that would let an owner-confirmed tool run without approval.
func (a *App) ownerOnly(p policy.Policy) error {
	for name, rule := range p.Tools {
		if tools.NeedsOwner(a.Runtime.Tools[name]) && (rule.Mode == policy.Allow || rule.Otherwise == policy.Allow) {
			return fmt.Errorf("%q always needs the owner's approval; its rule cannot be allow", name)
		}
	}
	return nil
}

func (a *App) describeAssignment(args json.RawMessage) (string, string) {
	in, _ := decodeAssignment(args)
	s := in.Spec
	var b strings.Builder
	fmt.Fprintf(&b, "Objective: %s\n", s.Objective)
	free, ask := rules(s.Policy)
	fmt.Fprintf(&b, "May do without asking: %s\n", list(free))
	fmt.Fprintf(&b, "Needs your approval: %s\n", list(ask))
	period := "in total"
	if p := s.Policy.Limits.BudgetPeriod.Duration; p > 0 {
		period = "per " + shortDuration(p)
	}
	fmt.Fprintf(&b, "Budget: $%.2f %s\n", s.Policy.Limits.MaxLLMSpendUSD, period)
	if s.Kind == assignments.KindProject {
		var done []string
		for _, c := range s.Completion.Criteria {
			how := "you confirm it"
			if c.Event != nil {
				how = "a " + c.Event.Source + "/" + c.Event.Kind + " event shows it"
			}
			done = append(done, c.Description+" ("+how+")")
		}
		fmt.Fprintf(&b, "Done when: %s\n", list(done))
	} else {
		b.WriteString("Done when: never; a responsibility runs until you revoke it\n")
	}
	var runs []string
	if in.FirstTask != nil {
		runs = append(runs, "now: "+in.FirstTask.Title)
	} else if s.Kind == assignments.KindProject {
		runs = append(runs, "now: start on the objective")
	}
	for _, sc := range s.Schedules {
		runs = append(runs, "every "+shortDuration(sc.Every.Duration)+": "+sc.Title)
	}
	for _, r := range s.Routes {
		runs = append(runs, "on "+r.Source+"/"+r.Kind+": "+r.Title)
	}
	fmt.Fprintf(&b, "Runs: %s\n", list(runs))
	if a.Runtime.ModelID != "" {
		if res, err := a.Runtime.Preview(context.Background(), s.Policy, ""); err == nil {
			where := "in the cloud: its content goes to " + strings.SplitN(res.Model, "/", 2)[0]
			if a.Runtime.IsLocal(res.Model) {
				where = "on this machine"
			}
			fmt.Fprintf(&b, "Model today: %s, %s\n", res.Model, where)
		} else {
			fmt.Fprintf(&b, "Model today: none allowed by its models policy\n")
		}
	}
	if in.FirstTask != nil {
		fmt.Fprintf(&b, "First task instruction (written by the agent): %s\n", in.FirstTask.Objective)
	}
	for _, w := range a.proposalWarnings(s, in.FirstTask != nil) {
		fmt.Fprintf(&b, "Warning: %s\n", w)
	}
	return fmt.Sprintf("New %s %q", s.Kind, s.Name), strings.TrimSpace(b.String())
}

// ownerOnlyTools only ever reach the owner, so allowing them without approval widens nothing.
var ownerOnlyTools = map[string]bool{"notify": true, "ask_owner": true}

func (a *App) proposalWarnings(s Spec, firstTask bool) []string {
	warnings := a.PolicyWarnings(s.Policy)
	for _, name := range sortedTools(s.Policy) {
		rule := s.Policy.Tools[name]
		if tool, ok := a.Runtime.Tools[name]; ok && tool.Effect() == tools.EffectWrite && !ownerOnlyTools[name] &&
			(rule.Mode == policy.Allow || rule.Otherwise == policy.Allow) {
			warnings = append(warnings, fmt.Sprintf("%s can act without asking you", name))
		}
	}
	if len(s.Policy.Tools) == 0 {
		warnings = append(warnings, "it has no tools, so it can only think and reply; it cannot read, send or tell you anything")
	} else if _, ok := s.Policy.Tools["notify"]; !ok {
		warnings = append(warnings, "it has no notify rule, so it cannot tell you anything")
	}
	if s.Policy.Limits.MaxLLMSpendUSD > ProposalCeilingUSD {
		warnings = append(warnings, fmt.Sprintf("budget is above your $%d ceiling for proposals", ProposalCeilingUSD))
	}
	if len(s.Schedules) == 0 && len(s.Routes) == 0 && !firstTask && s.Kind == assignments.KindResponsibility {
		warnings = append(warnings, "no schedule, route or first task: it would sit idle")
	}
	manual := 0
	for _, c := range s.Completion.Criteria {
		if c.Event == nil {
			manual++
		}
	}
	if manual > 0 && manual == len(s.Completion.Criteria) {
		warnings = append(warnings, "every completion criterion needs your confirm; nothing will finish it on its own")
	}
	return warnings
}

func (a *App) describePolicy(args json.RawMessage) (string, string) {
	var in struct {
		target
		Policy policy.Policy `json:"policy"`
	}
	json.Unmarshal(args, &in)
	current, err := a.Assignments.Get(context.Background(), in.AssignmentID)
	if err != nil {
		return "New policy for " + in.AssignmentID, "(assignment not found)"
	}
	var b strings.Builder
	rank := map[policy.Mode]int{policy.Deny: 0, policy.RequireApproval: 1, policy.Allow: 2}
	names := map[string]bool{}
	for n := range current.Policy.Tools {
		names[n] = true
	}
	for n := range in.Policy.Tools {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	for _, n := range sorted {
		before, had := current.Policy.Tools[n]
		after, has := in.Policy.Tools[n]
		switch {
		case !had:
			fmt.Fprintf(&b, "WIDENS  + %s: %s%s\n", n, after.Mode, resources(after))
		case !has:
			fmt.Fprintf(&b, "        - %s (removed)\n", n)
		case before.Mode != after.Mode || strings.Join(before.Resources, ",") != strings.Join(after.Resources, ",") || before.Otherwise != after.Otherwise:
			mark := "        "
			if rank[after.Mode] > rank[before.Mode] || rank[after.Otherwise] > rank[before.Otherwise] || len(after.Resources) > len(before.Resources) ||
				(len(before.Resources) > 0 && len(after.Resources) == 0) {
				mark = "WIDENS  "
			}
			fmt.Fprintf(&b, "%s~ %s: %s%s -> %s%s\n", mark, n, before.Mode, resources(before), after.Mode, resources(after))
		}
	}
	if before, after := current.Policy.Limits.MaxLLMSpendUSD, in.Policy.Limits.MaxLLMSpendUSD; before != after {
		mark := "        "
		if after > before {
			mark = "WIDENS  "
		}
		fmt.Fprintf(&b, "%s~ budget: $%.2f -> $%.2f\n", mark, before, after)
	}
	if b.Len() == 0 {
		b.WriteString("No change to tools or budget.")
	}
	return fmt.Sprintf("New policy for %q", current.Name), strings.TrimSpace(b.String())
}

func resources(r policy.Rule) string {
	if len(r.Resources) == 0 {
		return ""
	}
	out := " for " + strings.Join(r.Resources, ", ")
	if r.Otherwise != "" {
		out += " (otherwise " + string(r.Otherwise) + ")"
	}
	return out
}

func rules(p policy.Policy) (free, ask []string) {
	for _, name := range sortedTools(p) {
		r := p.Tools[name]
		switch {
		case r.Mode == policy.Allow:
			free = append(free, name+resources(r))
		case r.Mode == policy.RequireApproval || r.Otherwise == policy.RequireApproval:
			ask = append(ask, name+resources(r))
		}
	}
	return free, ask
}

func sortedTools(p policy.Policy) []string {
	names := make([]string, 0, len(p.Tools))
	for n := range p.Tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func list(items []string) string {
	if len(items) == 0 {
		return "nothing"
	}
	return strings.Join(items, "; ")
}

// EditProposal applies an owner-edited version of a pending proposal: the old approval is
// rejected as superseded and the owner's version takes effect, as `agentd create` or
// `agentd policy` would. No model is involved.
func (a *App) EditProposal(ctx context.Context, approvalID string, args json.RawMessage, by string) (string, error) {
	var toolName, actionID, status string
	if err := a.DB.QueryRowContext(ctx, `SELECT a.tool_name, a.id, ap.status FROM approvals ap JOIN actions a ON a.id = ap.action_id
		WHERE ap.id = ?`, approvalID).Scan(&toolName, &actionID, &status); err != nil {
		return "", err
	}
	p, ok := a.Runtime.Tools[toolName].(*proposal)
	if !ok {
		return "", fmt.Errorf("%s is not a proposal", approvalID)
	}
	if status != "pending" {
		return "", fmt.Errorf("proposal %s is %s", approvalID, status)
	}
	if !json.Valid(args) && p.name == "propose_assignment" {
		spec, err := ParseMarkdown(args)
		if err != nil {
			return "", err
		}
		var old assignmentProposal
		var current string
		if err := a.DB.QueryRowContext(ctx, `SELECT arguments_json FROM actions WHERE id = ?`, actionID).Scan(&current); err != nil {
			return "", err
		}
		old, _ = decodeAssignment(json.RawMessage(current))
		args, _ = json.Marshal(assignmentProposal{Spec: spec, FirstTask: old.FirstTask})
	}
	if _, err := p.check(ctx, args); err != nil {
		return "", err
	}
	if err := a.Gateway.Decide(ctx, approvalID, false, by); err != nil {
		return "", err
	}
	if err := audit.Record(ctx, a.DB, a.Runtime.Now(), "", by, "chat.proposal_superseded", approvalID,
		map[string]string{"action_id": actionID}); err != nil {
		return "", err
	}
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	id, err := p.apply(ctx, tx, args, "edit:"+approvalID)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	if p.name == "propose_assignment" {
		return "Created " + a.nameOf(id) + " from your edited version.", nil
	}
	return "Applied your edited " + strings.TrimPrefix(p.name, "propose_") + ".", nil
}
