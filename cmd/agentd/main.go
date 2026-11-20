package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"agentd/internal/app"
	"agentd/internal/demo"
	"agentd/internal/model/scripted"
	"agentd/internal/store"
	"agentd/internal/tools"
)

const usage = `usage: agentd [-db path] <command> [args]

commands:
  serve [-workers N] [-tool-delay D]   run the supervisor until interrupted
  demo                                 seed the airline-refund demo project
  status                               show projects, tasks, actions, approvals
  approve <approval-id>                approve a pending action
  reject <approval-id>                 reject a pending action
  pause <project-id>                   stop new work on a project
  resume <project-id>                  resume a paused project
`

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "agentd:", err)
		os.Exit(1)
	}
}

func run() error {
	global := flag.NewFlagSet("agentd", flag.ContinueOnError)
	global.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	dbPath := global.String("db", envOr("AGENTD_DB", "agentd.db"), "SQLite database path")
	if err := global.Parse(os.Args[1:]); err != nil {
		return err
	}
	args := global.Args()
	if len(args) == 0 {
		global.Usage()
		return fmt.Errorf("missing command")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := store.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	cmd := flag.NewFlagSet(args[0], flag.ContinueOnError)
	workers := cmd.Int("workers", 3, "concurrent workers")
	toolDelay := cmd.Duration("tool-delay", 0, "pause inside send_email after sending, to test crash recovery")
	if err := cmd.Parse(args[1:]); err != nil {
		return err
	}

	mailbox, err := tools.NewMailbox(ctx, db, *toolDelay)
	if err != nil {
		return err
	}
	a := app.New(db, scripted.Provider(demo.Script), "scripted-demo", tools.NewRegistry(tools.WebSearch{}, mailbox), time.Now)

	switch args[0] {
	case "serve":
		log := slog.New(slog.NewTextHandler(os.Stderr, nil))
		host, _ := os.Hostname()
		log.Info("agentd serving", "db", *dbPath, "workers", *workers)
		a.Runtime.Serve(ctx, fmt.Sprintf("%s-%d", host, os.Getpid()), *workers, time.Second, log)
		log.Info("agentd stopped")
		return nil
	case "demo":
		projectID, taskID, err := demo.Seed(ctx, a)
		if err != nil {
			return err
		}
		fmt.Printf("project %s\ntask    %s\n", projectID, taskID)
		return nil
	case "status":
		return status(ctx, a)
	case "approve", "reject":
		if cmd.NArg() != 1 {
			return fmt.Errorf("%s needs an approval id", args[0])
		}
		return a.Gateway.Decide(ctx, cmd.Arg(0), args[0] == "approve", "cli")
	case "pause", "resume":
		if cmd.NArg() != 1 {
			return fmt.Errorf("%s needs a project id", args[0])
		}
		status := map[string]string{"pause": "paused", "resume": "active"}[args[0]]
		return a.Projects.SetStatus(ctx, cmd.Arg(0), status)
	default:
		global.Usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func status(ctx context.Context, a *app.App) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()
	sections := []struct {
		title string
		query string
	}{
		{"PROJECTS", `SELECT id, name, status FROM projects ORDER BY created_at`},
		{"TASKS", `SELECT id, title, status, attempt_count, COALESCE(result_json, '') FROM tasks ORDER BY created_at`},
		{"ACTIONS", `SELECT id, tool_name, resource, status FROM actions ORDER BY created_at`},
		{"APPROVALS", `SELECT ap.id, a.tool_name, a.resource, ap.status FROM approvals ap JOIN actions a ON a.id = ap.action_id ORDER BY ap.requested_at`},
		{"MOCK MAILBOX", `SELECT recipient, subject, message_id FROM mock_mailbox ORDER BY sent_at`},
	}
	for _, s := range sections {
		fmt.Fprintf(w, "\n%s\n", s.title)
		rows, err := a.DB.QueryContext(ctx, s.query)
		if err != nil {
			return err
		}
		cols, _ := rows.Columns()
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return err
			}
			for i, v := range vals {
				if i > 0 {
					fmt.Fprint(w, "\t")
				}
				fmt.Fprint(w, v)
			}
			fmt.Fprintln(w)
		}
		rows.Close()
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
