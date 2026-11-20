package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"agentd/internal/app"
	"agentd/internal/demo"
	"agentd/internal/model"
	"agentd/internal/model/anthropic"
	"agentd/internal/model/ollama"
	"agentd/internal/model/scripted"
	"agentd/internal/policy"
	"agentd/internal/scheduler"
	"agentd/internal/store"
	"agentd/internal/tools"
)

const usage = `usage: agentd [-db path] [-model spec] [-effort level] <command> [args]

models:
  scripted                  deterministic demo model (default)
  anthropic[/model-id]      Claude via the Anthropic API (default claude-opus-5-5)
  ollama/model-id           local model via Ollama, e.g. ollama/qwen2.5:7b

commands:
  serve [-workers N] [-tool-delay D]   run the supervisor until interrupted
  demo                                 seed the airline-refund demo project
  status                               show projects, tasks, actions, approvals
  approve <approval-id>                approve a pending action
  reject <approval-id>                 reject a pending action
  policy <project-id> <policy.json>    replace a project's policy and limits
  pause <project-id>                   stop new work on a project
  resume <project-id>                  resume a paused project
  event -source S -kind K [-id X] [-payload JSON]
                                       push an external event into the inbox
  route -project P -source S -kind K -title T -objective O [-match JSON]
                                       turn matching events into tasks
  schedule -project P -every D -title T -objective O
                                       create a recurring task
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
	modelSpec := global.String("model", envOr("AGENTD_MODEL", "scripted"), "model provider and id")
	effort := global.String("effort", "high", "reasoning effort for Anthropic models")
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
	project := cmd.String("project", "", "project id")
	source := cmd.String("source", "", "event source, e.g. gmail")
	kind := cmd.String("kind", "", "event kind, e.g. email.received")
	externalID := cmd.String("id", "", "external id used to drop duplicate events")
	payload := cmd.String("payload", "{}", "event payload JSON")
	match := cmd.String("match", "{}", "route filter: JSON object of payload field values")
	title := cmd.String("title", "", "task title")
	objective := cmd.String("objective", "", "task objective")
	every := cmd.Duration("every", 0, "schedule interval, e.g. 24h")
	if err := cmd.Parse(args[1:]); err != nil {
		return err
	}

	mailbox, err := tools.NewMailbox(ctx, db, *toolDelay)
	if err != nil {
		return err
	}
	provider, err := newProvider(*modelSpec, *effort)
	if err != nil {
		return err
	}
	a := app.New(db, provider, *modelSpec, tools.NewRegistry(tools.WebSearch{}, mailbox), time.Now)

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
	case "policy":
		if cmd.NArg() != 2 {
			return fmt.Errorf("policy needs a project id and a JSON file")
		}
		body, err := os.ReadFile(cmd.Arg(1))
		if err != nil {
			return err
		}
		var p policy.Policy
		if err := json.Unmarshal(body, &p); err != nil {
			return fmt.Errorf("%s: %w", cmd.Arg(1), err)
		}
		version, err := a.Projects.SetPolicy(ctx, cmd.Arg(0), p, "cli")
		if err != nil {
			return err
		}
		fmt.Println("policy version", version)
		return nil
	case "pause", "resume":
		if cmd.NArg() != 1 {
			return fmt.Errorf("%s needs a project id", args[0])
		}
		status := map[string]string{"pause": "paused", "resume": "active"}[args[0]]
		return a.Projects.SetStatus(ctx, cmd.Arg(0), status, "", "cli")
	case "event":
		id, dup, err := a.Scheduler.Ingest(ctx, scheduler.Event{Source: *source, Kind: *kind, ExternalID: *externalID, Payload: json.RawMessage(*payload)})
		if err != nil {
			return err
		}
		fmt.Printf("event %s duplicate=%v\n", id, dup)
		return nil
	case "route":
		var filter map[string]string
		if err := json.Unmarshal([]byte(*match), &filter); err != nil {
			return fmt.Errorf("-match: %w", err)
		}
		id, err := a.Scheduler.AddRoute(ctx, scheduler.Route{ProjectID: *project, Source: *source, Kind: *kind, Match: filter, Title: *title, Objective: *objective})
		if err != nil {
			return err
		}
		fmt.Println("route", id)
		return nil
	case "schedule":
		id, err := a.Scheduler.AddSchedule(ctx, scheduler.Schedule{ProjectID: *project, Title: *title, Objective: *objective, Every: *every})
		if err != nil {
			return err
		}
		fmt.Println("schedule", id)
		return nil
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
		{"PROJECTS", `SELECT p.id, p.name, p.status, COALESCE(p.status_reason, ''),
			printf('spent $%.4f / $%s', COALESCE((SELECT SUM(cost_micros) FROM model_usage u WHERE u.project_id = p.id), 0) / 1e6,
				COALESCE(json_extract(p.policy_json, '$.limits.max_llm_spend_usd'), 'unlimited'))
			FROM projects p ORDER BY p.created_at`},
		{"TASKS", `SELECT id, title, status, attempt_count,
			COALESCE(datetime(next_run_at / 1000, 'unixepoch', 'localtime'), '-'), COALESCE(result_json, '') FROM tasks ORDER BY created_at`},
		{"EVENTS", `SELECT id, source, kind, COALESCE(external_id, ''), CASE WHEN processed_at IS NULL THEN 'pending' ELSE 'routed:' || routed_count END FROM events ORDER BY received_at`},
		{"SCHEDULES", `SELECT id, title, interval_ms / 1000 || 's', datetime(next_fire_at / 1000, 'unixepoch', 'localtime') FROM schedules ORDER BY created_at`},
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

func newProvider(spec, effort string) (model.Provider, error) {
	name, id, _ := strings.Cut(spec, "/")
	switch name {
	case "scripted":
		return scripted.Provider(demo.Script), nil
	case "anthropic":
		return anthropic.New(anthropic.Config{Model: id, Effort: effort}), nil
	case "ollama":
		if id == "" {
			return nil, fmt.Errorf("ollama needs a model id, e.g. ollama/qwen2.5:7b")
		}
		return ollama.New(ollama.Config{Model: id, BaseURL: os.Getenv("OLLAMA_HOST")}), nil
	}
	return nil, fmt.Errorf("unknown model provider %q", name)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
