package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"agentd/internal/app"
	"agentd/internal/assignments"
	"agentd/internal/overview"
	"agentd/internal/policy"
	"agentd/internal/scheduler"
)

type assignmentFile struct {
	Kind          assignments.Kind                `json:"kind"`
	Name          string                          `json:"name"`
	Objective     string                          `json:"objective"`
	Policy        policy.Policy                   `json:"policy"`
	Completion    assignments.Completion          `json:"completion"`
	Notifications *assignments.NotificationPolicy `json:"notifications"`
	Routes        []routeFile                     `json:"routes"`
	Schedules     []scheduleFile                  `json:"schedules"`
}

type routeFile struct {
	Source        string            `json:"source"`
	Kind          string            `json:"kind"`
	Match         map[string]string `json:"match"`
	CoalesceField string            `json:"coalesce_field"`
	Skills        []string          `json:"skills"`
	Title         string            `json:"title"`
	Objective     string            `json:"objective"`
}

type scheduleFile struct {
	Every     policy.Duration `json:"every"`
	Skills    []string        `json:"skills"`
	Title     string          `json:"title"`
	Objective string          `json:"objective"`
}

func createFromFile(ctx context.Context, a *app.App, path string) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var f assignmentFile
	if err := json.Unmarshal(body, &f); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	var id string
	switch f.Kind {
	case assignments.KindProject:
		id, err = a.Assignments.CreateProject(ctx, assignments.NewProject{
			Name: f.Name, Objective: f.Objective, Policy: f.Policy, Completion: f.Completion,
		})
	case assignments.KindResponsibility:
		if f.Notifications == nil {
			return "", fmt.Errorf("a responsibility needs a notifications policy")
		}
		id, err = a.Assignments.CreateResponsibility(ctx, assignments.NewResponsibility{
			Name: f.Name, Objective: f.Objective, Policy: f.Policy, Notifications: *f.Notifications,
		})
	default:
		return "", fmt.Errorf("kind must be %q or %q", assignments.KindProject, assignments.KindResponsibility)
	}
	if err != nil {
		return "", err
	}
	for _, r := range f.Routes {
		if _, err := a.Scheduler.AddRoute(ctx, scheduler.Route{
			AssignmentID: id, Source: r.Source, Kind: r.Kind, Match: r.Match, CoalesceField: r.CoalesceField,
			Skills: r.Skills, Title: r.Title, Objective: r.Objective,
		}); err != nil {
			return id, err
		}
	}
	for _, s := range f.Schedules {
		if _, err := a.Scheduler.AddSchedule(ctx, scheduler.Schedule{
			AssignmentID: id, Every: s.Every.Duration, Skills: s.Skills, Title: s.Title, Objective: s.Objective,
		}); err != nil {
			return id, err
		}
	}
	return id, nil
}

func show(ctx context.Context, a *app.App, id string) error {
	o, err := overview.Get(ctx, a.Runtime, id)
	if err != nil {
		return err
	}
	as := o.Assignment
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()
	fmt.Fprintf(w, "%s\t%s (%s)\n", as.ID, as.Name, as.Kind)
	fmt.Fprintf(w, "objective\t%s\n", as.Objective)
	fmt.Fprintf(w, "status\t%s %s\n", as.Status, as.StatusReason)
	limit := "unlimited"
	if o.LimitMicros > 0 {
		limit = fmt.Sprintf("$%.2f", float64(o.LimitMicros)/1e6)
		if p := as.Policy.Limits.BudgetPeriod.Duration; p > 0 {
			limit += " per " + p.String()
		}
	}
	fmt.Fprintf(w, "spend\t$%.4f of %s\n", float64(o.SpentMicros)/1e6, limit)
	fmt.Fprintf(w, "state\t%s\n", overview.StateSummary(as.State))
	fmt.Fprintf(w, "next\t%s\n", o.Next)
	fmt.Fprintf(w, "waiting\t%d approvals, %d undelivered notifications\n", o.PendingAsk, o.Undelivered)
	fmt.Fprintf(w, "\nOPEN TASKS\n")
	for _, t := range o.OpenTasks {
		fmt.Fprintf(w, "%s\t%s\t%s\n", t.ID, t.Status, t.Title)
	}
	fmt.Fprintf(w, "\nRECENT ACTIVITY\n")
	for _, r := range o.Recent {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.At.Local().Format("01-02 15:04"), r.Actor, r.Operation, r.ObjectID)
	}
	return nil
}

func notifications(ctx context.Context, a *app.App) error {
	rows, err := a.DB.QueryContext(ctx, `SELECT n.id, n.severity, a.name, n.title, n.body FROM notifications n
		JOIN assignments a ON a.id = n.assignment_id WHERE n.status = 'pending' ORDER BY n.created_at`)
	if err != nil {
		return err
	}
	defer rows.Close()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()
	for rows.Next() {
		var id, severity, name, title, body string
		if err := rows.Scan(&id, &severity, &name, &title, &body); err != nil {
			return err
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", id, severity, name, title, body)
	}
	return rows.Err()
}
