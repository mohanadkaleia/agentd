# agentd

A durable, model-agnostic personal agent runtime. Go + SQLite, one process.

## Try it

```bash
go build -o bin/agentd ./cmd/agentd
./bin/agentd demo                        # seed the airline-refund project
./bin/agentd -skills docs/examples/skills create docs/examples/portfolio-intelligence.json
./bin/agentd show <assignment-id>        # state, spend, recent activity, next expected action
./bin/agentd serve -demo-tools -tool-delay 5s   # kill -9 it while send_email is sleeping
./bin/agentd serve -demo-tools            # restart: the email is reconciled, not resent
./bin/agentd status
./bin/agentd approve <approval-id>
```

Feed it events and schedules:

```bash
./bin/agentd event -source gmail -kind email.received -id gm-001 -payload '{"thread_id":"thread-xy123"}'
./bin/agentd schedule -assignment <id> -every 24h -title "Check inbox" -objective "Look for airline replies."
```

Pick a model with `-model` (or `AGENTD_MODEL`):

```bash
./bin/agentd -model ollama/qwen2.5:7b serve      # local, free
./bin/agentd -model anthropic serve              # claude-opus-5-5; uses ANTHROPIC_API_KEY or `ant auth login`
./bin/agentd -model anthropic/claude-sonnet-5-5 -effort medium serve
```

`go test -race ./...` runs the leasing, transition, crash-recovery, adapter and neutrality suites.

## Layout

| Package | Role |
|---|---|
| `internal/model` | Provider-neutral inference + tool-calling types. `Message.Native` carries opaque provider state (e.g. Claude thinking blocks) that only the producing adapter replays. |
| `internal/model/{anthropic,ollama,scripted}` | Adapters. The only packages allowed to import a provider SDK; only `cmd/` may import an adapter (`internal/archtest` enforces both). |
| `internal/tasks` | Durable task queue: atomic claim, lease renew, versioned transitions. |
| `internal/runtime` | Agent loop. Checkpoints every model step and tool result; resumes the same run after a crash. |
| `internal/gateway` | The only path to side effects: action ledger, policy check, approvals, dispatch, reconciliation. |
| `internal/scheduler` | Event inbox (deduped by source + external id), deterministic routes that turn events into tasks, recurring schedules, and the `schedule_followup` tool. Every task it creates carries an origin key, so retries and restarts never double-create. |
| `internal/delegation` | `delegate` and `await_workers`. Workers are durable child tasks with a tool allowlist that is a subset of their parent's; depth ≤ 2, ≤ 3 active workers per parent. A waiting parent holds no worker slot and is woken when a worker finishes. |
| `internal/policy` | Fail-closed per-tool rules, scoped by resource, plus limits: a per-project LLM spend cap and per-tool rate limits (`{"send_email": {"max": 5, "per": "24h"}}`). |
| `internal/tools` | Tool interface + registry; `mock.go` holds the simulated web search and mailbox. |
| `migrations/` | Embedded SQL migrations, applied on open. |

## Gmail

One-time setup (about 10 minutes, in your browser):

1. In Google Cloud Console, create a project and enable the **Gmail API**.
2. Configure the OAuth consent screen (External, add yourself as a test user).
3. Create an OAuth client ID of type **Desktop app** and download its JSON.
4. Run `./bin/agentd gmail auth -client-secret ~/Downloads/client_secret_….json` and approve in the browser.

Scopes are `gmail.readonly` and `gmail.compose` only (no delete, no label changes). The client
secret and refresh token are stored in the login keychain, never on disk or in the model context.
`agentd serve` then registers four tools and checks the inbox every minute (`-gmail-poll`):

| Tool | Effect | Notes |
|---|---|---|
| `gmail_search`, `gmail_read` | read | Bodies are plain text, truncated, and labelled as untrusted. `resources` limits them to mail from, to or cc'd to those correspondents: `{"mode":"allow","resources":["@airline.com"]}`. Without `resources`, the whole mailbox is readable. |
| `gmail_draft` | write | Up to 10 recipients; nothing is sent. |
| `gmail_send` | write | Exactly one recipient, so policy can allow-list contacts: `{"mode":"allow","resources":["refunds@airline.com"],"otherwise":"approval"}`. |

New inbox mail becomes `gmail / email.received` events (payload: `message_id`, `thread_id`, `from`,
`subject`, `snippet`); routes decide which ones become tasks.

## Phone notifications

Notifications the agent raises can be pushed to your phone through the free [ntfy](https://ntfy.sh) app:

```bash
./bin/agentd ntfy setup        # prints a private topic; subscribe to it in the ntfy app
./bin/agentd ntfy test         # a test push
```

`agentd serve` then delivers pending notifications, retrying on failure. Pushes say only which
assignment needs attention; `ntfy setup -full` includes the title and text. `agentd ntfy off` stops it.

## HTTP API

```bash
./bin/agentd serve -listen 127.0.0.1:7777     # token is created in ./api-token (0600) on first run
curl -H "Authorization: Bearer $(cat api-token)" localhost:7777/api/assignments
```

Every request needs the bearer token. The default is loopback only; to reach it from your phone,
bind to the Mac mini's Tailscale address (e.g. `-listen 100.x.y.z:7777`), never `0.0.0.0`.

| Endpoint | |
|---|---|
| `GET/POST /api/assignments`, `GET /api/assignments/{id}` | list, create (same JSON as `agentd create`), overview |
| `POST /api/assignments/{id}/{pause,resume,complete,cancel,revoke}`, `PUT /api/assignments/{id}/policy` | lifecycle, policy |
| `GET /api/tasks?assignment=&status=`, `GET /api/tasks/{id}` | tasks, transcript |
| `GET /api/approvals`, `POST /api/approvals/{id}/{approve,reject}` | approvals inbox |
| `GET /api/notifications`, `POST /api/notifications/{id}/delivered` | notification outbox |
| `POST /api/events` | event intake for webhooks and source adapters |
| `GET /api/activity`, `GET /api/events/stream` | audit log, live SSE stream (resumes with `Last-Event-ID`) |
| `GET /api/usage` | spend per assignment, current period and lifetime |
| `GET /api/system`, `POST /api/system/emergency-stop`, `POST /api/system/resume` | emergency stop |

`agentd halt [reason]` and `agentd unhalt` do the same from the terminal.

## Architecture

See [ARCHITECTURE.md](ARCHITECTURE.md) for the domain model (Projects, Responsibilities, tasks,
skills, notifications), the invariants the code enforces, and how a responsibility runs.

## Run as a service (macOS)

```bash
deploy/set-anthropic-key.sh                 # optional: store the key in the login keychain
deploy/install.sh ollama/qwen2.5:7b         # or: deploy/install.sh anthropic
deploy/install.sh --listen tailscale:7777 ollama/qwen2.5:7b   # also serve the API on this Mac's Tailscale IP
deploy/install.sh --dry-run anthropic       # print the rendered plist only
deploy/uninstall.sh                         # stop and unregister; data is kept
```

`install.sh` builds a copy of the binary into `~/Library/Application Support/agentd/`, renders
`~/Library/LaunchAgents/local.agentd.plist`, and starts it. launchd restarts the process if it
exits; SIGTERM lets an in-flight tool call finish and record its result before the process exits.
The database lives next to the binary, logs in `~/Library/Logs/agentd/agentd.log`.

The design target is stricter: a dedicated non-admin macOS account running a LaunchDaemon from
`/Library/LaunchDaemons` with `UserName` set to that account, so the agent cannot read your own
files or keychain. That needs `sudo` and a new user (`sysadminctl -addUser agentd`); the same plist
works with the paths changed to that user's home.
