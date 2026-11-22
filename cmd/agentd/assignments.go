package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"agentd/internal/app"
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
	return overview.Write(os.Stdout, o)
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
