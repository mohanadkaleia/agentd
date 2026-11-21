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
)

func createFromFile(ctx context.Context, a *app.App, path string) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var spec app.Spec
	if err := json.Unmarshal(body, &spec); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	return a.Create(ctx, spec)
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
	spend := fmt.Sprintf("$%.4f of %s", float64(o.SpentMicros)/1e6, limit)
	if o.ReservedMicros > 0 {
		spend += fmt.Sprintf(" (+$%.4f held for in-flight calls)", float64(o.ReservedMicros)/1e6)
	}
	fmt.Fprintf(w, "spend\t%s\n", spend)
	fmt.Fprintf(w, "state\t%s\n", overview.StateSummary(as.State))
	fmt.Fprintf(w, "next\t%s\n", o.Next)
	fmt.Fprintf(w, "waiting\t%d approvals, %d undelivered notifications\n", o.PendingAsk, o.Undelivered)
	if len(o.Criteria) > 0 {
		fmt.Fprintf(w, "\nCOMPLETION CRITERIA\n")
		for _, c := range o.Criteria {
			how := "pending: " + criterionSource(c.Criterion)
			if c.Verified {
				how = "verified by " + c.Kind
				if c.EventID != "" {
					how += " " + c.EventID
				}
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", c.Criterion.ID, c.Criterion.Description, how)
		}
	}
	if len(o.Obligations) > 0 {
		fmt.Fprintf(w, "\nOBLIGATIONS\n")
		for _, ob := range o.Obligations {
			state := "ok, next due " + ob.DueAt.Local().Format("01-02 15:04")
			if ob.Overdue {
				state = fmt.Sprintf("OVERDUE by %d interval(s)", ob.Missed)
			}
			fmt.Fprintf(w, "%s\tevery %s\t%s\n", ob.Description, ob.Every, state)
		}
	}
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
	rows, err := a.DB.QueryContext(ctx, `SELECT n.id, n.severity, a.name, n.title, n.body,
		CASE WHEN n.failed_at IS NOT NULL THEN 'delivery failed: ' || COALESCE(n.last_error, '')
		     WHEN n.attempts > 0 THEN 'retrying: ' || COALESCE(n.last_error, '')
		     ELSE 'waiting' END
		FROM notifications n
		JOIN assignments a ON a.id = n.assignment_id WHERE n.status = 'pending' ORDER BY n.created_at`)
	if err != nil {
		return err
	}
	defer rows.Close()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()
	for rows.Next() {
		var id, severity, name, title, body, delivery string
		if err := rows.Scan(&id, &severity, &name, &title, &body, &delivery); err != nil {
			return err
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", id, severity, name, title, body, delivery)
	}
	return rows.Err()
}

func criterionSource(c assignments.Criterion) string {
	if c.Event == nil {
		return "needs `agentd confirm`"
	}
	return "waiting for a " + c.Event.Source + "/" + c.Event.Kind + " event"
}
