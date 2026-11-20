package app

import (
	"context"
	"database/sql"
	"time"

	"agentd/internal/gateway"
	"agentd/internal/model"
	"agentd/internal/projects"
	"agentd/internal/runtime"
	"agentd/internal/scheduler"
	"agentd/internal/tasks"
	"agentd/internal/tools"
)

type App struct {
	DB        *sql.DB
	Projects  *projects.Store
	Tasks     *tasks.Store
	Gateway   *gateway.Gateway
	Scheduler *scheduler.Scheduler
	Runtime   *runtime.Runtime
}

func New(db *sql.DB, provider model.Provider, modelID string, registry tools.Registry, now func() time.Time) *App {
	p := &projects.Store{DB: db, Now: now}
	t := &tasks.Store{DB: db, Now: now}
	all := tools.Registry{}
	for name, tool := range registry {
		all[name] = tool
	}
	followup := &scheduler.Followup{Tasks: t}
	all[followup.Definition().Name] = followup
	g := &gateway.Gateway{DB: db, Tools: all, Projects: p, Now: now, ApprovalTTL: 24 * time.Hour}
	sch := &scheduler.Scheduler{DB: db, Tasks: t, Now: now}
	return &App{
		DB:        db,
		Projects:  p,
		Tasks:     t,
		Gateway:   g,
		Scheduler: sch,
		Runtime: &runtime.Runtime{
			DB:          db,
			Tasks:       t,
			Projects:    p,
			Gateway:     g,
			Tools:       all,
			Model:       provider,
			ModelID:     modelID,
			Now:         now,
			LeaseTTL:    30 * time.Second,
			MaxSteps:    20,
			BaseBackoff: 5 * time.Second,

			MaxRejectedCompletions: 2,
			Housekeeping: func(ctx context.Context) error {
				if _, err := g.ExpireApprovals(ctx); err != nil {
					return err
				}
				return sch.Tick(ctx)
			},
		},
	}
}
