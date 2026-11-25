package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"golang.org/x/term"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"text/tabwriter"
	"time"

	"agentd/internal/app"
	"agentd/internal/ask"
	"agentd/internal/assignments"
	"agentd/internal/integrations/gmail"
	"agentd/internal/integrations/mcp"
	"agentd/internal/integrations/portfolio"
	"agentd/internal/integrations/web"
	"agentd/internal/model"
	"agentd/internal/model/anthropic"
	"agentd/internal/model/ollama"
	"agentd/internal/runtime"
	"agentd/internal/scheduler"
	"agentd/internal/skills"
	"agentd/internal/store"
	"agentd/internal/system"
	"agentd/internal/tools"
)

// help is what `agentd help` prints; usage, below, lists every command.
const help = `agentd runs your standing jobs in the background and asks before it acts.

  agentd                       open the chat; starts the server when none is running
  agentd chat "message"        send one message, print the reply, exit

Server
  agentd start | stop | restart   the background server; jobs resume where they stopped
  agentd logs [-f] [-n 40]        what the background server wrote
  agentd status                   the server, jobs, tasks and what waits on you

Jobs
  agentd create job.md            create a job from a file (docs/guide.md); or ask in the chat
  agentd show | pause | resume <job-id>
  agentd approve | reject <approval-id>
  agentd halt [reason] | unhalt   emergency stop: nothing runs until lifted

Connections
  agentd models                   models in use and whether their credentials are present
  agentd mcp add <name> <url>     connect an MCP server (agentd mcp presets lists ready ones)
  agentd gmail auth | ntfy setup | channel pair telegram

  agentd help all                 every command with its flags
  agentd version
`

const usage = `Talk to agentd: agentd

Run alone in a terminal, agentd opens the chat and starts its server in the background
when none is running.

usage: agentd [-db path] [-model spec | -models a=spec,b=spec] [-tier fast=a] [-fallback-models specs] [-effort level] <command> [args]

models:
  anthropic[/model-id]      Claude via the Anthropic API (default claude-opus-5-5)
  ollama/model-id           local model via Ollama, e.g. ollama/qwen2.5:7b

commands:
  chat                                 talk to agentd in this terminal; also what agentd alone does
  chat "message" | chat - | ... | chat send one message (or standard input), print the reply, exit
       [-new] [-c id] [-history [-n 50]] [-json] [-no-wait] [-timeout 5m]
  serve [-workers N] [-listen ADDR] [-mcp-config FILE]
                                       run the supervisor (and the HTTP API when -listen is set)
  gmail auth -client-secret FILE       connect a Gmail account (stored in the login keychain)
  gmail status                         show the connected Gmail account
  ntfy setup [-full] | test | off      deliver notifications to your phone through the ntfy app
  mcp add <name> <url> | -- <command>  connect an MCP server: stores secrets, signs in, reviews its tools
  mcp remove <name>                    disconnect an MCP server and delete the secrets add stored
  mcp list                             MCP servers: state, transport, tools by review status, scopes, last error
  mcp tools <server>                   a server's tools: status, declared effect and resource, calls, manual resolves
  mcp review [server]                  show MCP tools that are new or changed since you accepted them
  mcp accept <server> <tool>... | -all <server>
                                       accept the definitions shown; only accepted tools run
  mcp auth | test | check [-config FILE] [server]
                                       sign in (OAuth), connect and list without calling, or validate the config
  mcp restart <server>                 ask the running agentd to reconnect a server and forget its failures
  halt [reason]                        emergency stop: no task starts, no tool runs, no model call is made
  unhalt                               lift the emergency stop
  create [-dry-run] <job.md|job.json>  create a project or responsibility, with its routes and schedules
  export <assignment-id> [-format md]  write an assignment back as a job file (md or json)
  show <assignment-id>                 state, spend, open tasks, recent activity, next expected action
  start | stop | restart               the background server (the chat starts it by itself)
  logs [-f] [-n 40]                    what the background server wrote
  status [-all]                        the server, your jobs and what waits on you; -all lists every record
  help [all] | version
  retry <task-id> [note]               continue a blocked task in its existing run
  resolve <action-id> happened|not-happened [note]
                                       settle an action whose outcome could not be determined
  channel pair|list|set|revoke telegram talk to agentd from a Telegram bot on your phone (docs/chat.md)
  models                               configured models, their aliases and tiers, and whether credentials are present
  task model <task-id> <model|tier|auto>
                                       choose the model for one task (checked against its models policy)
  tell <task|assignment> "text"        send the owner's note to running work, word for word
  answer <question-id> "text"          answer a question a task asked
  approve <approval-id>                approve a pending action
  reject <approval-id>                 reject a pending action
  policy <assignment-id> <policy.json> replace an assignment's policy and limits
  pause | resume <assignment-id>       stop or restart new work
  confirm <project-id> <criterion-id> [note]
                                       record that you verified a completion criterion yourself
  complete | cancel <project-id>       end a project by hand
  revoke <responsibility-id>           end a responsibility
  notifications                        list notifications waiting for delivery
  event -source S -kind K [-id X] [-payload JSON]
                                       push an external event into the inbox
  route -assignment A -source S -kind K -title T -objective O [-match JSON] [-coalesce FIELD]
                                       turn matching events into tasks
  schedule -assignment A -every D -title T -objective O
                                       create a recurring task
`

func main() {
	if err := run(); err != nil {
		var exit *exitError
		if errors.As(err, &exit) {
			if exit.msg != "" {
				fmt.Fprintln(os.Stderr, "agentd:", exit.msg)
			}
			os.Exit(exit.code)
		}
		fmt.Fprintln(os.Stderr, "agentd:", err)
		os.Exit(1)
	}
}

// exitError ends the process with a specific status, for commands scripts depend on.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

func run() error {
	syscall.Umask(0o077)
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-version", "-v":
			fmt.Println(version())
			return nil
		case "help", "--help", "-help", "-h":
			if len(os.Args) > 2 && os.Args[2] == "all" {
				fmt.Print(usage)
			} else {
				fmt.Print(help)
			}
			return nil
		}
	}
	global := flag.NewFlagSet("agentd", flag.ContinueOnError)
	global.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	dbPath := global.String("db", defaultDB(), "SQLite database path (default: agentd.db here if it exists, else the one in your home directory)")
	modelSpec := global.String("model", envOr("AGENTD_MODEL", ""), "model provider and id, needed by serve")
	effort := global.String("effort", "high", "reasoning effort for Anthropic models")
	fallbackSpecs := global.String("fallback-models", envOr("AGENTD_FALLBACK_MODELS", ""), "comma-separated models to try when the main one fails before a run's first reply")
	modelList := global.String("models", envOr("AGENTD_MODELS", ""), "configured models, comma-separated, alias=spec or spec; the first is the default")
	tiers := tierFlags{}
	global.Var(tiers, "tier", "map a tier to a model, e.g. -tier fast=ollama/qwen2.5:7b (repeatable; or AGENTD_TIERS)")
	skillsDir := global.String("skills", envOr("AGENTD_SKILLS", "skills"), "directory of skill .md files")
	if err := global.Parse(os.Args[1:]); err != nil {
		return err
	}
	if env := os.Getenv("AGENTD_TIERS"); env != "" && len(tiers) == 0 {
		if err := tiers.Set(env); err != nil {
			return err
		}
	}
	entries, err := parseCatalogue(*modelList, *modelSpec, *fallbackSpecs)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		*modelSpec = entries[0].spec
	}
	args := global.Args()
	if len(args) == 0 && (term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) || piped(os.Stdin)) {
		args = []string{"chat"}
	}
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, help)
		return &exitError{code: 2}
	}
	if !knownCommand(args[0]) {
		msg := fmt.Sprintf("unknown command %q.", args[0])
		if near := nearestCommand(args[0]); near != "" {
			msg += fmt.Sprintf(" Did you mean %q?", near)
		}
		return &exitError{code: 2, msg: msg + " Run `agentd help` for the list."}
	}
	if exe, err := os.Executable(); err == nil {
		flags := os.Args[1 : len(os.Args)-len(global.Args())]
		serveCommand = append(append([]string{exe}, flags...), "serve")
		serveHasModel = *modelSpec != ""
	}
	dbFile = *dbPath

	signals := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if args[0] == "chat" {
		signals = signals[1:]
	}
	ctx, stop := signal.NotifyContext(context.Background(), signals...)
	defer stop()
	if args[0] == "gmail" {
		return gmailCommand(ctx, args[1:])
	}
	if args[0] == "ntfy" {
		return ntfyCommand(ctx, args[1:])
	}
	db, err := store.Open(ctx, *dbPath)
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("%w\nThe database belongs to another account. Under the separate-account install, run: "+
			"sudo -u <agentd-user> agentd -db %s %s", err, *dbPath, strings.Join(args, " "))
	}
	if err != nil {
		return err
	}
	defer db.Close()
	if args[0] == "chat" {
		if *modelSpec != "" {
			// A wrong model name should stop here, not in a server started later.
			if _, err := newProvider(*modelSpec, *effort); err != nil {
				return &exitError{code: 2, msg: err.Error()}
			}
		}
		return chatCommand(ctx, db, args[1:], os.Stdin, os.Stdout)
	}
	if args[0] == "start" || args[0] == "stop" || args[0] == "restart" || args[0] == "logs" {
		return serverCommand(ctx, db, args[0], args[1:], os.Stdout)
	}

	if args[0] == "mcp" {
		return mcpCommand(ctx, db, filepath.Dir(*dbPath), args[1:])
	}
	if args[0] == "channel" {
		return channelCommand(ctx, db, args[1:])
	}
	cmd := flag.NewFlagSet(args[0], flag.ContinueOnError)
	workers := cmd.Int("workers", 3, "concurrent workers")
	listen := cmd.String("listen", "", "HTTP API address, e.g. 127.0.0.1:7777 (off when empty)")
	tokenFile := cmd.String("token-file", filepath.Join(filepath.Dir(*dbPath), "api-token"), "file holding the API bearer token; created on first use")
	gmailPoll := cmd.Duration("gmail-poll", time.Minute, "how often to check Gmail for new mail (0 disables)")
	holdings := cmd.String("holdings", envOr("AGENTD_HOLDINGS", ""), "JSON holdings snapshot that portfolio_positions reads (off when empty)")
	mcpConfig := cmd.String("mcp-config", envOr("AGENTD_MCP_CONFIG", ""), "JSON file listing MCP servers whose tools to register (off when empty)")
	mcpLogs := cmd.String("mcp-log-dir", envOr("AGENTD_MCP_LOG_DIR", ""), "directory for one stderr log per local MCP server (the main log when empty)")
	assignment := cmd.String("assignment", "", "assignment id")
	coalesce := cmd.String("coalesce", "", "route: payload field whose value groups events into one open task")
	source := cmd.String("source", "", "event source, e.g. gmail")
	kind := cmd.String("kind", "", "event kind, e.g. email.received")
	externalID := cmd.String("id", "", "external id used to drop duplicate events")
	payload := cmd.String("payload", "{}", "event payload JSON")
	match := cmd.String("match", "{}", "route filter: JSON object of payload field values")
	title := cmd.String("title", "", "task title")
	objective := cmd.String("objective", "", "task objective")
	every := cmd.Duration("every", 0, "schedule interval, e.g. 24h")
	dryRun := cmd.Bool("dry-run", false, "create: check the job file and print it normalised, creating nothing")
	format := cmd.String("format", "md", "export: md or json")
	all := cmd.Bool("all", false, "status: list every assignment, task, event, schedule, action and approval")
	if err := cmd.Parse(args[1:]); err != nil {
		return err
	}

	var provider model.Provider
	if args[0] == "serve" || *modelSpec != "" {
		if provider, err = newProvider(*modelSpec, *effort); err != nil {
			return err
		}
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	var registered []tools.Tool
	var account *gmail.Account
	var servers *mcp.Manager
	// watched is the MCP config as it was when serve read it.
	watched := ""
	if args[0] == "serve" {
		registered = append(registered, &web.Reader{Now: time.Now, Timeout: 30 * time.Second})
		if *holdings != "" {
			registered = append(registered, &portfolio.Tool{Path: *holdings, Now: time.Now, MaxAge: 72 * time.Hour})
		}
		var extra []tools.Tool
		account, extra = gmailTools(ctx, log)
		registered = append(registered, extra...)
		named := *mcpConfig != ""
		if !named {
			*mcpConfig = defaultMCPConfig(filepath.Dir(*dbPath))
		}
		if abs, err := filepath.Abs(*mcpConfig); err == nil {
			*mcpConfig = abs
		}
		watched = fileSignature(*mcpConfig)
		if watched != "" || named {
			cfg, err := mcp.Load(*mcpConfig)
			if err != nil {
				return err
			}
			var found []tools.Tool
			servers, found = mcp.Start(ctx, cfg, mcp.Options{DataDir: filepath.Dir(*dbPath), LogDir: *mcpLogs, DB: db, Vault: mcpVault(), Log: log})
			defer servers.Close()
			servers.Supervise(ctx)
			registered = append(registered, found...)
		}
	}
	registry, err := uniqueRegistry(registered)
	if err != nil {
		return err
	}
	a := app.New(db, provider, *modelSpec, registry, time.Now)
	a.Chat.PromptDir = filepath.Dir(*dbPath)
	if err := configureModels(a, entries, tiers, *effort); err != nil {
		return err
	}
	if args[0] != "serve" {
		adoptCatalogue(ctx, a, db)
	}
	if err := skills.LoadDir(a.Skills, *skillsDir); err != nil {
		return fmt.Errorf("loading skills: %w", err)
	}

	switch args[0] {
	case "serve":
		// Two servers on one database would run every task twice.
		if l, alive, err := system.Alive(ctx, db, time.Now()); err != nil {
			return err
		} else if alive && l.PID != os.Getpid() {
			if proc, err := os.FindProcess(l.PID); err == nil && proc.Signal(syscall.Signal(0)) == nil {
				return fmt.Errorf("agentd is already serving this database (pid %d). `agentd restart` restarts it; `agentd stop` stops it", l.PID)
			}
		}
		host, _ := os.Hostname()
		log.Info("agentd serving", "db", *dbPath, "workers", *workers)
		stopAPI, err := startAPI(ctx, a, *listen, *tokenFile, log)
		if err != nil {
			return err
		}
		if ch, ok := loadNtfy(); ok {
			a.Deliverer.Channel = ch
			log.Info("notifications deliver via ntfy")
		}
		if _, err := a.Chat.ChatAssignment(ctx); err != nil {
			return err
		}
		logPolicyWarnings(ctx, a, log)
		watchGmail(ctx, a, account, *gmailPoll, log)
		if servers != nil {
			servers.Poll(ctx, a.Scheduler)
		}
		startTelegram(ctx, a, log)
		models, tierMap := catalogueInfo(a.Runtime)
		tokenPath := ""
		if *listen != "" && os.Getenv("AGENTD_API_TOKEN") == "" {
			tokenPath, _ = filepath.Abs(*tokenFile)
		}
		dir, _ := os.Getwd()
		if err := system.Launched(ctx, db, system.Launch{Args: os.Args[1:], Dir: dir}, time.Now()); err != nil {
			log.Error("recording how serve was started", "err", err)
		}
		go beat(ctx, db, system.Liveness{Exe: buildExe, Build: buildMark(), Started: time.Now(), Listen: *listen, TokenFile: tokenPath, MCPConfig: *mcpConfig, Models: models, Tiers: tierMap}, log)
		// A changed MCP config is loaded by starting again in place: tools are registered
		// once, at start, so that no tool can appear or change under a running task.
		serving, stopServing := context.WithCancel(ctx)
		defer stopServing()
		var reload atomic.Bool
		go watchMCPConfig(serving, db, *mcpConfig, watched, 2*time.Second, 2*time.Minute, log, func() {
			reload.Store(true)
			stopServing()
		})
		a.Runtime.Serve(serving, fmt.Sprintf("%s-%d", host, os.Getpid()), *workers, time.Second, log)
		if err := system.Stopped(context.WithoutCancel(ctx), db); err != nil {
			log.Error("liveness", "err", err)
		}
		stopAPI()
		if reload.Load() && ctx.Err() == nil {
			if servers != nil {
				servers.Close()
			}
			db.Close()
			log.Info("agentd starting again to load the changed MCP config")
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			return syscall.Exec(exe, os.Args, os.Environ())
		}
		log.Info("agentd stopped")
		return nil
	case "halt":
		reason := strings.Join(cmd.Args(), " ")
		if err := system.Halt(ctx, db, time.Now(), "cli", reason); err != nil {
			return err
		}
		fmt.Println("halted: no task will start, no tool will run, no model call will be made until `agentd unhalt`")
		return nil
	case "unhalt":
		return system.Resume(ctx, db, time.Now(), "cli")
	case "tell":
		if cmd.NArg() < 2 {
			return fmt.Errorf("usage: agentd tell <task-id|assignment> \"text\"")
		}
		task, when, err := a.Runtime.Tell(ctx, runtime.Note{Target: cmd.Arg(0), Text: strings.Join(cmd.Args()[1:], " "), By: "cli"})
		if err != nil {
			return err
		}
		fmt.Println("sent to", task+".", when)
		return nil
	case "answer":
		if cmd.NArg() < 2 {
			return fmt.Errorf("usage: agentd answer <question-id> \"text\"")
		}
		q, err := ask.Answer(ctx, db, time.Now(), cmd.Arg(0), strings.Join(cmd.Args()[1:], " "), "cli")
		if err != nil {
			return err
		}
		fmt.Printf("answered %q; its task continues\n", q.Question)
		return nil
	case "models":
		return printModels(a)
	case "task":
		if cmd.NArg() != 3 || cmd.Arg(0) != "model" {
			return fmt.Errorf("usage: agentd task model <task-id> <model|alias|tier|auto>")
		}
		msg, err := a.Runtime.SetTaskModel(ctx, cmd.Arg(1), cmd.Arg(2), "cli")
		if err != nil {
			return err
		}
		fmt.Println(msg)
		return nil
	case "status":
		if *all {
			return status(ctx, a)
		}
		fmt.Println(serverLine(ctx, db))
		summary, err := a.Chat.Command(ctx, "", "cli", "/status")
		if err != nil {
			return err
		}
		fmt.Print("\n" + sanitize(summary))
		fmt.Println("\nEvery task, event, action and approval: agentd status -all")
		return nil
	case "confirm":
		if cmd.NArg() < 2 {
			return fmt.Errorf("confirm needs a project id and a criterion id")
		}
		return a.Outcomes.Confirm(ctx, cmd.Arg(0), cmd.Arg(1), "cli", strings.Join(cmd.Args()[2:], " "))
	case "retry":
		if cmd.NArg() < 1 {
			return fmt.Errorf("retry needs a task id")
		}
		return a.Runtime.Retry(ctx, cmd.Arg(0), "cli", strings.Join(cmd.Args()[1:], " "))
	case "resolve":
		if cmd.NArg() < 2 || (cmd.Arg(1) != "happened" && cmd.Arg(1) != "not-happened") {
			return fmt.Errorf("usage: resolve <action-id> happened|not-happened [note]")
		}
		return a.Gateway.Resolve(ctx, cmd.Arg(0), cmd.Arg(1) == "happened", "cli", strings.Join(cmd.Args()[2:], " "))
	case "approve", "reject":
		if cmd.NArg() != 1 {
			return fmt.Errorf("%s needs an approval id", args[0])
		}
		return a.Gateway.Decide(ctx, cmd.Arg(0), args[0] == "approve", "cli")
	case "create":
		if cmd.NArg() != 1 {
			return fmt.Errorf("create needs a job file (.md or .json)")
		}
		id, err := createFromFile(ctx, a, cmd.Arg(0), *dryRun)
		if err != nil || *dryRun {
			return err
		}
		fmt.Println(id)
		return nil
	case "export":
		if cmd.NArg() != 1 {
			return fmt.Errorf("export needs an assignment id")
		}
		return export(ctx, a, cmd.Arg(0), *format)
	case "show":
		if cmd.NArg() != 1 {
			return fmt.Errorf("show needs an assignment id")
		}
		return show(ctx, a, cmd.Arg(0))
	case "notifications":
		return notifications(ctx, a)
	case "policy":
		if cmd.NArg() != 2 {
			return fmt.Errorf("policy needs an assignment id and a JSON file")
		}
		body, err := os.ReadFile(cmd.Arg(1))
		if err != nil {
			return err
		}
		p, err := app.ParsePolicy(cmd.Arg(1), body)
		if err != nil {
			return fmt.Errorf("%s: %w", cmd.Arg(1), err)
		}
		version, err := a.Assignments.SetPolicy(ctx, cmd.Arg(0), p, "cli")
		if err != nil {
			return err
		}
		fmt.Println("policy version", version)
		return nil
	case "pause", "resume", "complete", "cancel", "revoke":
		if cmd.NArg() != 1 {
			return fmt.Errorf("%s needs an assignment id", args[0])
		}
		id := cmd.Arg(0)
		switch args[0] {
		case "pause":
			return a.Assignments.Pause(ctx, id, assignments.PausedByUser, "", "cli")
		case "resume":
			return a.Assignments.Resume(ctx, id, "cli")
		case "complete":
			return a.Assignments.Complete(ctx, id, "cli")
		case "cancel":
			return a.Assignments.Cancel(ctx, id, "cli")
		}
		return a.Assignments.Revoke(ctx, id, "cli")
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
		id, err := a.Scheduler.AddRoute(ctx, scheduler.Route{AssignmentID: *assignment, Source: *source, Kind: *kind, Match: filter,
			CoalesceField: *coalesce, Title: *title, Objective: *objective})
		if err != nil {
			return err
		}
		fmt.Println("route", id)
		return nil
	case "schedule":
		id, err := a.Scheduler.AddSchedule(ctx, scheduler.Schedule{AssignmentID: *assignment, Title: *title, Objective: *objective, Every: *every})
		if err != nil {
			return err
		}
		fmt.Println("schedule", id)
		return nil
	default:
		return &exitError{code: 2, msg: fmt.Sprintf("unknown command %q. Run `agentd help` for the list.", args[0])}
	}
}

// commands is every command name, to tell a typing mistake from a real command.
var commands = strings.Fields(`chat serve start stop restart logs gmail ntfy mcp halt unhalt create export show status
	retry resolve channel models task tell answer approve reject policy pause resume confirm complete cancel revoke
	notifications event route schedule`)

func knownCommand(name string) bool {
	for _, c := range commands {
		if c == name {
			return true
		}
	}
	return false
}

// nearestCommand is the command a mistyped name most likely meant, if any is close.
func nearestCommand(name string) string {
	best, bestD := "", max(1, len(name)/3)+1
	for _, c := range commands {
		if strings.HasPrefix(c, name) && len(name) >= 2 {
			return c
		}
		if d := editDistance(name, c); d < bestD {
			best, bestD = c, d
		}
	}
	return best
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := []int{i}
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur = append(cur, min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost))
		}
		prev = cur
	}
	return prev[len(b)]
}

// piped reports whether f is a pipe or a file, so that `echo ... | agentd` sends a message.
func piped(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && (st.Mode()&os.ModeNamedPipe != 0 || st.Mode().IsRegular())
}

func status(ctx context.Context, a *app.App) error {
	fmt.Println(serverLine(ctx, a.DB))
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()
	sections := []struct {
		title string
		query string
	}{
		{"ASSIGNMENTS (spend vs budget: agentd show <id>)", `SELECT p.id, p.kind || CASE WHEN p.builtin IS NULL THEN '' ELSE ' (built in)' END, p.name, p.status,
			COALESCE(p.status_reason, '') FROM assignments p ORDER BY p.created_at`},
		{"TASKS", `SELECT id, title, status, attempt_count,
			COALESCE(datetime(next_run_at / 1000, 'unixepoch', 'localtime'), '-'), COALESCE(result_json, '') FROM tasks ORDER BY created_at`},
		{"EVENTS", `SELECT id, source, kind, COALESCE(external_id, ''), CASE WHEN processed_at IS NULL THEN 'pending' ELSE 'routed:' || routed_count END FROM events ORDER BY received_at`},
		{"SCHEDULES", `SELECT id, title, interval_ms / 1000 || 's', datetime(next_fire_at / 1000, 'unixepoch', 'localtime') FROM schedules ORDER BY created_at`},
		{"ACTIONS", `SELECT id, tool_name, resource, status FROM actions ORDER BY created_at`},
		{"MCP SERVERS", `SELECT server, state, transport, last_error FROM mcp_servers ORDER BY server`},
		{"CHANNELS", `SELECT name, CASE WHEN sender_id IS NULL THEN 'not paired' ELSE 'paired' END, level,
			CASE WHEN widen = 1 THEN 'widen on' ELSE 'widen off' END FROM channels ORDER BY name`},
		{"APPROVALS", `SELECT ap.id, a.tool_name, a.resource, ap.status FROM approvals ap JOIN actions a ON a.id = ap.action_id ORDER BY ap.requested_at`},
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

// beat records that this process is serving, so `agentd chat` can tell a message will be answered.
func beat(ctx context.Context, db *sql.DB, l system.Liveness, log *slog.Logger) {
	l.PID = os.Getpid()
	for {
		l.At = time.Now()
		if err := system.Beat(ctx, db, l); err != nil && ctx.Err() == nil {
			log.Error("liveness", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func isLocal(spec string) bool {
	name, _, _ := strings.Cut(spec, "/")
	return name == "ollama"
}

func newProvider(spec, effort string) (model.Provider, error) {
	if spec == "" {
		return nil, fmt.Errorf("choose a model with -model, e.g. -model ollama/qwen2.5:7b or -model anthropic")
	}
	name, id, _ := strings.Cut(spec, "/")
	switch name {
	case "anthropic":
		cfg := anthropic.Config{Model: id, Effort: effort}
		if os.Getenv("ANTHROPIC_API_KEY") == "" {
			cfg.APIKey = keychainSecret("agentd", "anthropic-api-key")
		}
		return anthropic.New(cfg), nil
	case "ollama":
		if id == "" {
			return nil, fmt.Errorf("ollama needs a model id, e.g. ollama/qwen2.5:7b")
		}
		return ollama.New(ollama.Config{Model: id, BaseURL: os.Getenv("OLLAMA_HOST")}), nil
	}
	return nil, fmt.Errorf("unknown model provider %q", name)
}

// defaultDB is the database agentd uses when none is named: the login service's when it
// is installed, else agentd.db in this directory when there is one, and otherwise one in
// the home directory, so that agentd run from anywhere opens the same conversation.
func defaultDB() string {
	if v := os.Getenv("AGENTD_DB"); v != "" {
		return v
	}
	// One agent per account: when agentd is installed as a login service, its database is
	// the one every command uses.
	if serviceInstalled() {
		return serviceDB()
	}
	if _, err := os.Stat("agentd.db"); err == nil {
		return "agentd.db"
	}
	dir := os.Getenv("XDG_DATA_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "agentd.db"
		}
		dir = filepath.Join(home, ".local", "share")
	}
	dir = filepath.Join(dir, "agentd")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "agentd.db"
	}
	return filepath.Join(dir, "agentd.db")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func uniqueRegistry(registered []tools.Tool) (tools.Registry, error) {
	r := tools.Registry{}
	for _, t := range registered {
		name := t.Definition().Name
		if _, taken := r[name]; taken {
			return nil, fmt.Errorf("two tools are named %q", name)
		}
		r[name] = t
	}
	return r, nil
}

func logPolicyWarnings(ctx context.Context, a *app.App, log *slog.Logger) {
	rows, err := a.DB.QueryContext(ctx, `SELECT id FROM assignments WHERE status IN ('active', 'paused')`)
	if err != nil {
		log.Error("policy warnings", "err", err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		asg, err := a.Assignments.Get(ctx, id)
		if err != nil {
			continue
		}
		for _, w := range a.PolicyWarnings(asg.Policy) {
			log.Warn("policy", "assignment", id, "warning", w)
		}
	}
}
