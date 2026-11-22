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
./bin/agentd -model anthropic -fallback-models ollama/qwen2.5:7b serve   # use Ollama when Claude is down
```

Each run is pinned to one model for its whole conversation. A fallback model is tried only when the
main one fails before the run's first reply. An assignment's policy can restrict which models it may
use: `"models": {"local_only": true}` keeps its data on the Mac, and `"models": {"allowed": [...]}`
lists model specs by name.

`go test -race ./...` runs the leasing, transition, crash-recovery, adapter and neutrality suites.
`internal/e2e` replays a week of an airline-refund project against a fake Gmail and a fake clock
(a crash mid-send, a prompt-injection email, a follow-up, an approval that is not yet the outcome,
and the bank posting the refund) and logs its metrics with `go test -v ./internal/e2e`. To add a
scenario, script the agent's choices in `decide` and drive time with `hours`.

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

## Obligations

A responsibility can declare what must keep happening, and Baseet checks it from evidence instead
of trusting its schedules:

```json
"obligations": [
  {"description": "Daily brief delivered", "every": "24h", "grace": "2h", "evidence": {"delivered": "brief:"}},
  {"description": "Gmail events keep arriving", "every": "6h", "evidence": {"event": {"source": "gmail", "kind": "email.received"}}}
]
```

Evidence is one of `delivered` (a delivered notification whose dedup key starts with the prefix),
`tool` (a successful action of that tool) or `event` (a received event). When one is overdue,
housekeeping starts one catch-up task per missed interval (`on_miss: "task"`, the default) or
notifies you (`"notify"`, the default for event feeds), and escalates after three misses.
`agentd show` lists each obligation's health.

## Project completion

A project's `completion.criteria` decide when it is done. Each criterion is verified by a matching
event or by you, never by the model:

```json
"completion": {"criteria": [
  {"id": "refund", "description": "Refund posted to my card",
   "event": {"source": "bank", "kind": "transaction.posted", "match": {"merchant": "Airline"}}},
  {"id": "receipt", "description": "Airline confirmed in writing"}
]}
```

`agentd confirm <project-id> receipt` records your confirmation; `agentd show` lists each criterion's
status. Once all are verified and no action is unresolved, the project completes itself and queues a
notification. Plain-text criteria (`"criteria": ["..."]`) still work and need `agentd confirm`.

## Holdings

`agentd serve -holdings ~/holdings.json` registers `portfolio_positions`, which reads a snapshot you
keep up to date (see [`docs/examples/holdings.json`](docs/examples/holdings.json)). Quantities and
prices are decimal strings and the arithmetic is exact. Holdings of one symbol across accounts are
combined, foreign currencies need an `fx` rate, and anything without a price or rate is listed with
a warning instead of a guessed value. A snapshot older than three days is marked incomplete. The
tool only reads; there is no trading code anywhere in agentd.

## Reading the web

`agentd serve` registers `web_read`, which fetches one public page and returns its title, readable
text and links, marked as untrusted. It is not a browser: no JavaScript, cookies, forms or logins.
Every connection is checked against the address it actually reaches, so localhost, private,
link-local, cloud-metadata and Tailscale addresses are refused, including through redirects. Pages
are capped at 2 MB and 20,000 characters of text. Policy resources are hostnames:
`"web_read": {"mode": "allow", "resources": ["www.sec.gov", "investor.example.com"], "otherwise": "approval"}`.

## MCP servers

`agentd serve -mcp-config ~/agentd-mcp.json` starts the [MCP](https://modelcontextprotocol.io)
servers listed in the file and registers each of their tools as `<server>__<tool>`:

```json
{"servers": {"chat": {"command": "/opt/agentd/mcp/chat-server", "args": ["--stdio"],
  "env": {"CHAT_TOKEN": {"keychain": "agentd/chat-token"}}, "call_timeout": "60s",
  "tools": {"include": ["post_message", "read_channel"],
            "meta": {"read_channel": {"effect": "read", "resource": "channel_id"},
                     "post_message": {"effect": "write", "resource": "channel_id"}}}}}}
```

Installing a server grants nothing. Every tool starts unreviewed and does not run until you accept
its exact definition:

```bash
./bin/agentd mcp review          # new or changed tools: description (hidden characters shown), schema, server hints
./bin/agentd mcp accept chat post_message read_channel
```

If a server later changes a tool's description, schema or hints, that tool stops (calls to it
wait) until you review it again, and assignments using it get one notification; a call approved
under the old definition never runs under the new one. A tool also runs only when an assignment's
policy has a rule for it, and the model is only shown tools its policy can allow:

```json
"chat__read_channel": {"mode": "allow", "resources": ["C0123ALERTS"], "otherwise": "deny"},
"chat__post_message": {"mode": "allow", "resources": ["C0123ALERTS"], "otherwise": "approval"}
```

`meta` is yours to declare; agentd never trusts what a server says about its own tools. A tool is
a write unless you mark it `"effect": "read"`. Mark only true reads: after a crash mid-call agentd
runs a read again. An interrupted write, or a write the server reports as failed, blocks its task
until you settle it with `agentd resolve`. `resource` names the argument policy matches on, exactly
as the tool takes it (a channel id, not `#alerts`); a call without it, or with a list of more than
one value, is refused before anything is sent.

A write that is interrupted blocks until you settle it with `agentd resolve`, unless you give agentd
a way to know. `"idempotent": true` says running it twice is harmless (setting a status, adding a
label), so after an unclear outcome the model may simply try again; a wrong declaration means a
duplicate. `"reconcile"` lets agentd check: it puts a marker derived from the action in one argument
and, after a crash or an unclear error, looks the marker up with a read tool of the same server:

```json
"post_message": {"effect": "write", "resource": "channel_id", "reconcile": {
  "marker": {"arg": "metadata.client_ref"},
  "lookup": {"tool": "find_message", "args": {"client_ref": "{{marker}}"}, "found_when": "matches.0", "external_id": "matches.0.id"}}}
```

A hit settles the action as done; a miss is not proof (indexes lag), so the task stays blocked and
agentd keeps checking every minute, unless you add `"miss_is_authoritative_after": "10m"` for a
lookup you know is consistent. The model cannot set the marker argument itself, and lookups are not
agent actions: they need no policy rule, count against no rate limit, and stop during an emergency
stop. `agentd mcp tools <server>` counts how often you resolved each tool by hand, to show which
tools deserve one of these. A `resources` list only gates which value was asked
for, not what comes back, so a search tool with no resource argument reads the whole account. Rules
name exact tools; `chat__*` is rejected. `agentd serve` logs rules that can never apply, and the
command center shows them on the assignment. The server runs
as agentd's user with only `PATH`, `HOME` (its own directory under the data dir) and the `env` you
list; it never sees agentd's environment, and it is never offered model access, prompts to you, or
your files. Results are capped at 20,000 characters and marked untrusted. Commands must be absolute
paths; pin versions rather than `npx some-server@latest`, which runs new code on every start. The
file must not be writable by others, and must be `chmod 600` when it sets `env`.

agentd keeps servers running: one that crashes, drops its connection or fails to start is retried
after 1 s, then 2 s, doubling up to 5 minutes; after five failures within an hour it stays down and
you get one notification. A call that runs past `call_timeout` (default 60 s) restarts a local server,
and `max_concurrent` (default 3) caps calls in flight per server. While a server is down its tools
keep their names but are not offered, and calls to them wait, with no model calls and no spend.
Config changes need a restart: `agentd mcp check` first, then `deploy/install.sh --mcp-config FILE ...`
(which also puts each local server's stderr in `~/Library/Logs/agentd/mcp-<server>.log`).

```bash
./bin/agentd mcp list               # state of every server: ready, needs_review, needs_auth, failed, disabled
./bin/agentd mcp tools chat         # each tool: review status, declared effect and resource, calls, manual resolves
./bin/agentd mcp test -config ~/agentd-mcp.json chat     # connect and list, calling nothing
./bin/agentd mcp restart chat       # the running agentd reconnects it and forgets its failures
```

The command center's Tools page shows the same, with a review screen for new or changed tools.

MCP gives the agent things to do, not news of things that happened. To wake an assignment when
something changes, a server can be polled with no model involved:

```json
"chat": {"command": "...", "tools": {"meta": {"read_channel": {"effect": "read"}}},
  "poll": [{"every": "2m", "tool": "read_channel", "args": {"channel_id": "C0123ALERTS", "limit": 50},
            "items": "messages", "id": "ts", "event": {"source": "chat", "kind": "message.posted"},
            "payload": {"channel": "channel", "user": "user", "text": "text"},
            "exclude": {"user": "U_BOT"}, "max_events": 20}]}
```

Each poll calls the read tool, and every item it has not seen becomes an inbox event (`source`,
`kind`, an external id from `id`, and only the `payload` fields you map, as strings), which routes
turn into tasks like any other event. The first poll only records what is there (no backfill);
after an outage at most `max_events` arrive per poll, and a route's `coalesce` groups a burst into one
task. Polls only run a tool you declared a read and accepted, and pause during an emergency stop or
while the server is down or needs sign-in. `exclude` drops the assignment's own posts. A poll that
fails three times in a row notifies the assignments routing that source, saying the feed is broken
rather than quiet; `agentd mcp list` shows each poll's last success.

A service that can call out can instead post to `POST /api/events` (over Tailscale, with the bearer
token) with its own `external_id`, which needs no polling at all:

```bash
curl -X POST http://100.x.y.z:7777/api/events -H "Authorization: Bearer $(cat api-token)" \
  -d '{"source": "chat", "kind": "message.posted", "external_id": "C0123:1700000000.1234", "payload": {"channel": "C0123"}}'
```

Keep the API off public interfaces (agentd refuses to bind it to every interface); a relay that
receives a public webhook should forward only what the route needs.

Secrets are never written in the file. An `env` value whose name looks secret (`TOKEN`, `KEY`,
`SECRET`, `PASSWORD`, ...) must reference one: `{"keychain": "agentd/chat-token"}` reads the login
keychain item with service `agentd` and account `chat-token` (store it with
`security add-generic-password -s agentd -a chat-token -w`, which prompts for the value), and
`{"file": "/abs/path"}` reads an owner-only file. Under the separate-account install the keychain
form reads `$AGENTD_SECRETS_DIR/chat-token` instead.

Hosted servers are reached over HTTPS instead of started (`http` is accepted only for localhost):

```json
{"servers": {"workspace": {"url": "https://mcp.example.com/mcp", "auth": {"type": "oauth", "scopes": ["read"]}},
             "notes": {"url": "https://notes.example.net/mcp",
                       "auth": {"type": "bearer", "token": {"keychain": "agentd/notes-token"}}}}}
```

For `oauth`, sign in once in your browser with `agentd mcp auth -config ~/agentd-mcp.json workspace`.
agentd discovers the server's authorization server, registers itself if the server allows that (or
uses `client_id` and a referenced `client_secret`), asks for only the scopes you list, and stores the
token in the keychain; `agentd serve` refreshes it, one refresh at a time. If the server later
rejects agentd, its tools stop being offered, calls to them wait, and you get one notification; sign
in again and the running agentd picks it up within seconds, no restart. Redirects to another host
are refused, so a token is only ever sent to the configured server, and a request that never left
agentd (connection refused, or refused credentials) counts as not executed.

## Phone notifications

Notifications the agent raises can be pushed to your phone through the free [ntfy](https://ntfy.sh) app:

```bash
./bin/agentd ntfy setup        # prints a private topic; subscribe to it in the ntfy app
./bin/agentd ntfy test         # a test push
```

`agentd serve` then delivers pending notifications, retrying on failure. Pushes say only which
assignment needs attention; `ntfy setup -full` includes the title and text. `agentd ntfy off` stops it.

## Command center

With `-listen` set, agentd also serves a small web app at the same address: assignments with their
next action, spend, criteria and obligations; the approvals inbox showing the exact arguments and
their hash; actions with an unknown outcome to settle; undelivered notifications; a live activity
feed; task transcripts; and the emergency stop. It is Vue's runtime build (vendored, no build step,
no CDN) under a strict Content-Security-Policy. Sign in by pasting the API token once: the browser
gets an HttpOnly, SameSite=Strict session cookie (12 hours, kept in memory, so a restart signs you
out), every change also needs a CSRF header, and cross-origin requests are refused. Over Tailscale,
open `http://<tailscale-ip>:7777/` on your phone.

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
| `POST /api/assignments/{id}/criteria/{criterion}/confirm` | owner confirms a completion criterion |
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

For a stronger boundary, `deploy/provision-user.sh` runs agentd as its own non-admin account under a
LaunchDaemon, so it cannot read your files and you cannot read its secrets without `sudo`. It only
prints its plan until you add `--apply`; see [docs/isolation.md](docs/isolation.md) for the threat
model, migration, verification and rollback.
