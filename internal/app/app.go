package app

import (
	"database/sql"
	"time"

	"agentd/internal/gateway"
	"agentd/internal/model"
	"agentd/internal/projects"
	"agentd/internal/runtime"
	"agentd/internal/tasks"
	"agentd/internal/tools"
)

type App struct {
	DB       *sql.DB
	Projects *projects.Store
	Tasks    *tasks.Store
	Gateway  *gateway.Gateway
	Runtime  *runtime.Runtime
}

func New(db *sql.DB, provider model.Provider, modelID string, registry tools.Registry, now func() time.Time) *App {
	p := &projects.Store{DB: db, Now: now}
	t := &tasks.Store{DB: db, Now: now}
	g := &gateway.Gateway{DB: db, Tools: registry, Projects: p, Now: now, ApprovalTTL: 24 * time.Hour}
	return &App{
		DB:       db,
		Projects: p,
		Tasks:    t,
		Gateway:  g,
		Runtime: &runtime.Runtime{
			DB:          db,
			Tasks:       t,
			Projects:    p,
			Gateway:     g,
			Tools:       registry,
			Model:       provider,
			ModelID:     modelID,
			Now:         now,
			LeaseTTL:    30 * time.Second,
			MaxSteps:    20,
			BaseBackoff: 5 * time.Second,

			MaxRejectedCompletions: 2,
		},
	}
}
