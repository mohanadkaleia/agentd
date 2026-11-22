package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"agentd/internal/ask"
	"agentd/internal/assignments"
	"agentd/internal/chat"
	"agentd/internal/delegation"
	"agentd/internal/gateway"
	"agentd/internal/model"
	"agentd/internal/notify"
	"agentd/internal/outcomes"
	"agentd/internal/policy"
	"agentd/internal/runtime"
	"agentd/internal/scheduler"
	"agentd/internal/skills"
	"agentd/internal/tasks"
	"agentd/internal/tools"
	"agentd/internal/watchdog"
)

type App struct {
	DB          *sql.DB
	Assignments *assignments.Store
	Tasks       *tasks.Store
	Gateway     *gateway.Gateway
	Scheduler   *scheduler.Scheduler
	Outbox      *notify.Outbox
	Deliverer   *notify.Deliverer
	Outcomes    *outcomes.Evaluator
	Watchdog    *watchdog.Watchdog
	Skills      skills.Registry
	Runtime     *runtime.Runtime
	Chat        *chat.Service
}

func New(db *sql.DB, provider model.Provider, modelID string, registry tools.Registry, now func() time.Time) *App {
	a := &assignments.Store{DB: db, Now: now}
	t := &tasks.Store{DB: db, Now: now}
	outbox := &notify.Outbox{DB: db, Assignments: a, Now: now}
	evaluator := &outcomes.Evaluator{DB: db, Assignments: a, Outbox: outbox, Now: now}
	dog := &watchdog.Watchdog{DB: db, Tasks: t, Outbox: outbox, Now: now, EscalateAfter: 3}
	deliverer := &notify.Deliverer{DB: db, Now: now, MaxAttempts: 8, Claim: 2 * time.Minute}
	questions := &ask.Tool{DB: db, Outbox: outbox, Now: now}
	all := tools.Registry{}
	for name, tool := range registry {
		all[name] = tool
	}
	for _, tool := range []tools.Tool{
		&scheduler.Followup{Tasks: t},
		&delegation.Delegate{Tasks: t, Tools: all, Limits: delegation.DefaultLimits},
		&delegation.Await{Tasks: t},
		&notify.Tool{Outbox: outbox},
		&assignments.StateTool{Assignments: a},
		&scheduler.TaskEvents{DB: db, Now: now},
		questions,
	} {
		all[tool.Definition().Name] = tool
	}
	g := &gateway.Gateway{DB: db, Tools: all, Assignments: a, Tasks: t, Now: now, ApprovalTTL: 24 * time.Hour, RecheckEvery: time.Minute}
	sch := &scheduler.Scheduler{DB: db, Tasks: t, Now: now}
	sk := skills.Registry{}
	rt := &runtime.Runtime{
		DB:          db,
		Tasks:       t,
		Assignments: a,
		Gateway:     g,
		Tools:       all,
		Skills:      sk,
		Model:       provider,
		ModelID:     modelID,
		Now:         now,
		LeaseTTL:    30 * time.Second,
		MaxSteps:    20,
		BaseBackoff: 5 * time.Second,
		ToolWait:    24 * time.Hour,
		Outbox:      outbox,

		MaxRejectedCompletions: 2,
	}
	conversations := &chat.Service{DB: db, Tasks: t, Assignments: a, Runtime: rt, Now: now, Wake: rt.Wake}
	reads := &chat.Reads{DB: db, Runtime: rt, Now: now}
	for _, tool := range append(reads.Tools(), conversations.Forward()) {
		all[tool.Definition().Name] = tool
	}
	t.OnRelease = conversations.Released
	rt.Opener = conversations
	recheck := func(ctx context.Context) error {
		if _, err := g.ExpireApprovals(ctx); err != nil {
			return err
		}
		confirmed, err := g.Recheck(ctx)
		if err != nil {
			return err
		}
		for _, settled := range confirmed {
			note := "the provider confirmed the action took effect"
			if !settled.Happened {
				note = "the provider confirmed the action did not take effect; decide whether to try again"
			}
			err := rt.Retry(ctx, settled.TaskID, "gateway", note)
			if err != nil && !errors.Is(err, runtime.ErrNotBlocked) && !errors.Is(err, runtime.ErrUnresolvedWork) {
				return err
			}
		}
		return nil
	}
	steps := []struct {
		name string
		run  func(context.Context) error
	}{
		{"approvals and reconciliation", recheck},
		{"budget resume", rt.ResumeWithinBudget},
		{"scheduler", sch.Tick},
		{"completion criteria", evaluator.Tick},
		{"obligations", dog.Tick},
		{"questions", questions.Tick},
		{"conversation", conversations.Tick},
		{"conversation cards", conversations.Project},
		{"notification delivery", deliverer.DeliverDue},
	}
	// Each step runs even when an earlier one fails, so a broken step cannot stop
	// schedules, the watchdog, cards or delivery.
	rt.Housekeeping = func(ctx context.Context) error {
		var errs []error
		for _, step := range steps {
			if err := step.run(ctx); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", step.name, err))
			}
		}
		return errors.Join(errs...)
	}
	app := &App{
		DB:          db,
		Assignments: a,
		Tasks:       t,
		Gateway:     g,
		Scheduler:   sch,
		Outbox:      outbox,
		Deliverer:   deliverer,
		Outcomes:    evaluator,
		Watchdog:    dog,
		Skills:      sk,
		Runtime:     rt,
		Chat:        conversations,
	}
	for _, tool := range app.proposals() {
		all[tool.Definition().Name] = tool
	}
	a.Check = app.ownerOnly
	conversations.Edit = app.EditProposal
	return app
}

func (a *App) PolicyWarnings(p policy.Policy) []string {
	names := make([]string, 0, len(p.Tools))
	for name := range p.Tools {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []string
	for _, name := range names {
		tool, ok := a.Runtime.Tools[name]
		if !ok {
			out = append(out, fmt.Sprintf("the rule for %q has no effect: no tool by that name is registered", name))
			continue
		}
		if t, ok := tool.(tools.Untargeted); ok && t.NoResource() && len(p.Tools[name].Resources) > 0 {
			out = append(out, fmt.Sprintf("the rule for %q lists resources, but the tool declares no resource argument, so its otherwise mode always applies", name))
		}
	}
	return out
}
