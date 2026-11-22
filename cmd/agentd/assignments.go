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

func readJob(path string) (app.Spec, error) {
	info, err := os.Stat(path)
	if err != nil {
		return app.Spec{}, err
	}
	if info.Size() > app.MaxJobFile {
		return app.Spec{}, fmt.Errorf("%s is larger than %d KB", path, app.MaxJobFile>>10)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return app.Spec{}, err
	}
	spec, err := app.ParseJob(path, body)
	if err != nil {
		return spec, fmt.Errorf("%s: %w", path, err)
	}
	return spec, nil
}

func createFromFile(ctx context.Context, a *app.App, path string, dryRun bool) (string, error) {
	spec, err := readJob(path)
	if err != nil {
		return "", err
	}
	if !dryRun {
		return a.Create(ctx, spec)
	}
	if err := a.Check(spec); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	for _, w := range a.PolicyWarnings(spec.Policy) {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	normal, _ := json.MarshalIndent(spec, "", "  ")
	fmt.Println(string(normal))
	return "", nil
}

func export(ctx context.Context, a *app.App, id, format string) error {
	spec, err := a.Export(ctx, id)
	if err != nil {
		return err
	}
	switch format {
	case "json":
		out, _ := json.MarshalIndent(spec, "", "  ")
		fmt.Println(string(out))
	case "md":
		out, err := spec.Markdown()
		if err != nil {
			return err
		}
		fmt.Print(out)
	default:
		return fmt.Errorf("-format must be md or json")
	}
	return nil
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
