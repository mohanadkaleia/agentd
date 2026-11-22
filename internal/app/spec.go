package app

import (
	"context"
	"database/sql"
	"fmt"

	"agentd/internal/assignments"
	"agentd/internal/policy"
	"agentd/internal/scheduler"
	"agentd/internal/watchdog"
)

type Spec struct {
	Kind          assignments.Kind                `json:"kind"`
	Name          string                          `json:"name"`
	Objective     string                          `json:"objective"`
	Policy        policy.Policy                   `json:"policy"`
	Completion    assignments.Completion          `json:"completion"`
	Notifications *assignments.NotificationPolicy `json:"notifications"`
	Routes        []RouteSpec                     `json:"routes"`
	Schedules     []ScheduleSpec                  `json:"schedules"`
	Obligations   []ObligationSpec                `json:"obligations"`
}

type ObligationSpec struct {
	Description string            `json:"description"`
	Every       policy.Duration   `json:"every"`
	Grace       policy.Duration   `json:"grace"`
	Evidence    watchdog.Evidence `json:"evidence"`
	OnMiss      string            `json:"on_miss"`
	Skills      []string          `json:"skills"`
}

type RouteSpec struct {
	Source        string            `json:"source"`
	Kind          string            `json:"kind"`
	Match         map[string]string `json:"match"`
	CoalesceField string            `json:"coalesce_field"`
	Skills        []string          `json:"skills"`
	Title         string            `json:"title"`
	Objective     string            `json:"objective"`
}

type ScheduleSpec struct {
	Every     policy.Duration `json:"every"`
	Skills    []string        `json:"skills"`
	Title     string          `json:"title"`
	Objective string          `json:"objective"`
}

func (s Spec) skillNames() [][]string {
	var out [][]string
	for _, r := range s.Routes {
		out = append(out, r.Skills)
	}
	for _, sc := range s.Schedules {
		out = append(out, sc.Skills)
	}
	for _, o := range s.Obligations {
		out = append(out, o.Skills)
	}
	return out
}

func (s Spec) validate() error {
	if s.Name == "" || s.Objective == "" {
		return fmt.Errorf("%w: name and objective are required", assignments.ErrInvalid)
	}
	for _, r := range s.Routes {
		if r.Source == "" || r.Kind == "" || r.Title == "" || r.Objective == "" {
			return fmt.Errorf("%w: every route needs source, kind, title and objective", assignments.ErrInvalid)
		}
	}
	for _, sc := range s.Schedules {
		if sc.Every.Duration <= 0 || sc.Title == "" || sc.Objective == "" {
			return fmt.Errorf("%w: every schedule needs a positive interval, title and objective", assignments.ErrInvalid)
		}
	}
	return nil
}

func (a *App) Create(ctx context.Context, s Spec) (string, error) {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	id, err := a.CreateTx(ctx, tx, s)
	if err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// CreateTx creates an assignment with its routes, schedules and obligations in the caller's
// transaction, so either all of it exists or none of it does.
func (a *App) CreateTx(ctx context.Context, tx *sql.Tx, s Spec) (string, error) {
	if err := s.validate(); err != nil {
		return "", err
	}
	for _, names := range s.skillNames() {
		if _, err := a.Skills.Resolve(names); err != nil {
			return "", fmt.Errorf("%w: %v", assignments.ErrInvalid, err)
		}
	}
	var id string
	var err error
	switch s.Kind {
	case assignments.KindProject:
		id, err = a.Assignments.CreateProjectTx(ctx, tx, assignments.NewProject{
			Name: s.Name, Objective: s.Objective, Policy: s.Policy, Completion: s.Completion,
		})
	case assignments.KindResponsibility:
		if s.Notifications == nil {
			return "", fmt.Errorf("%w: a responsibility needs a notifications policy", assignments.ErrInvalid)
		}
		id, err = a.Assignments.CreateResponsibilityTx(ctx, tx, assignments.NewResponsibility{
			Name: s.Name, Objective: s.Objective, Policy: s.Policy, Notifications: *s.Notifications,
		})
	default:
		return "", fmt.Errorf("%w: kind must be %q or %q", assignments.ErrInvalid, assignments.KindProject, assignments.KindResponsibility)
	}
	if err != nil {
		return "", err
	}
	for _, r := range s.Routes {
		if _, err := a.Scheduler.AddRouteTx(ctx, tx, scheduler.Route{
			AssignmentID: id, Source: r.Source, Kind: r.Kind, Match: r.Match, CoalesceField: r.CoalesceField,
			Skills: r.Skills, Title: r.Title, Objective: r.Objective,
		}); err != nil {
			return "", err
		}
	}
	for _, sc := range s.Schedules {
		if _, err := a.Scheduler.AddScheduleTx(ctx, tx, scheduler.Schedule{
			AssignmentID: id, Every: sc.Every.Duration, Skills: sc.Skills, Title: sc.Title, Objective: sc.Objective,
		}); err != nil {
			return "", err
		}
	}
	for _, o := range s.Obligations {
		if _, err := a.Watchdog.AddTx(ctx, tx, watchdog.Obligation{
			AssignmentID: id, Description: o.Description, Every: o.Every.Duration, Grace: o.Grace.Duration,
			Evidence: o.Evidence, OnMiss: o.OnMiss, Skills: o.Skills,
		}); err != nil {
			return "", err
		}
	}
	return id, nil
}
