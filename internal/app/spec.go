package app

import (
	"context"
	"fmt"

	"agentd/internal/assignments"
	"agentd/internal/policy"
	"agentd/internal/scheduler"
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
	if err := s.validate(); err != nil {
		return "", err
	}
	var id string
	var err error
	switch s.Kind {
	case assignments.KindProject:
		id, err = a.Assignments.CreateProject(ctx, assignments.NewProject{
			Name: s.Name, Objective: s.Objective, Policy: s.Policy, Completion: s.Completion,
		})
	case assignments.KindResponsibility:
		if s.Notifications == nil {
			return "", fmt.Errorf("%w: a responsibility needs a notifications policy", assignments.ErrInvalid)
		}
		id, err = a.Assignments.CreateResponsibility(ctx, assignments.NewResponsibility{
			Name: s.Name, Objective: s.Objective, Policy: s.Policy, Notifications: *s.Notifications,
		})
	default:
		return "", fmt.Errorf("%w: kind must be %q or %q", assignments.ErrInvalid, assignments.KindProject, assignments.KindResponsibility)
	}
	if err != nil {
		return "", err
	}
	for _, r := range s.Routes {
		if _, err := a.Scheduler.AddRoute(ctx, scheduler.Route{
			AssignmentID: id, Source: r.Source, Kind: r.Kind, Match: r.Match, CoalesceField: r.CoalesceField,
			Skills: r.Skills, Title: r.Title, Objective: r.Objective,
		}); err != nil {
			return id, err
		}
	}
	for _, sc := range s.Schedules {
		if _, err := a.Scheduler.AddSchedule(ctx, scheduler.Schedule{
			AssignmentID: id, Every: sc.Every.Duration, Skills: sc.Skills, Title: sc.Title, Objective: sc.Objective,
		}); err != nil {
			return id, err
		}
	}
	return id, nil
}
