package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"agentd/internal/assignments"
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
}

func New(db *sql.DB, provider model.Provider, modelID string, registry tools.Registry, now func() time.Time) *App {
	a := &assignments.Store{DB: db, Now: now}
	t := &tasks.Store{DB: db, Now: now}
	outbox := &notify.Outbox{DB: db, Assignments: a, Now: now}
	evaluator := &outcomes.Evaluator{DB: db, Assignments: a, Outbox: outbox, Now: now}
	dog := &watchdog.Watchdog{DB: db, Tasks: t, Outbox: outbox, Now: now, EscalateAfter: 3}
	deliverer := &notify.Deliverer{DB: db, Now: now, MaxAttempts: 8, Claim: 2 * time.Minute}
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
	rt.Housekeeping = func(ctx context.Context) error {
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
		if err := rt.ResumeWithinBudget(ctx); err != nil {
			return err
		}
		if err := sch.Tick(ctx); err != nil {
			return err
		}
		if err := evaluator.Tick(ctx); err != nil {
			return err
		}
		if err := dog.Tick(ctx); err != nil {
			return err
		}
		return deliverer.DeliverDue(ctx)
	}
	return &App{
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
	}
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
