# Guide

Everything agentd can do, one feature per section. Start with the [README](../README.md) to install it.

## The conversation

Talking to agentd is the main way to use it: in the terminal (`agentd chat`), in the web app, or
from a Telegram bot on your phone. See [chat.md](chat.md) for the commands, the cards, what agentd
can and cannot do from chat, the chat's budget, the terminal client's flags, exit codes
and JSON output, and phone setup.

- **Cards** bring everything that needs you into the conversation: approvals, proposals,
  questions from tasks, notifications, blocked tasks, actions with an unknown outcome, completed
  projects, and the emergency stop. Each decision card has a fixed number for `/approve <n>`.
- **Notes**: `/tell <task or job> <text>` (or `agentd tell`) gives running work your words; it
  reads them at its next step.
- **Questions**: a job whose policy allows `ask_owner` can ask you something and wait, using no
  model calls while it waits. Answer on the card, with `/answer <n> <text>`, or with
  `agentd answer <question-id> "text"`.
- **Personality**: who agentd says it is and how it talks comes from
  [`internal/chat/prompts/identity.md`](../internal/chat/prompts/identity.md). To change it, put
  your own `identity.md` next to the database (`~/.local/share/agentd/identity.md` by default).
  It applies from the next reply, with no rebuild or restart. The safety rules in
  [`prompts/rules.md`](../internal/chat/prompts/rules.md) are always sent and cannot be replaced.

## Choosing a model

Configure several models and name tiers, then let each piece of work use the right one:

```bash
agentd -models small=ollama/qwen2.5:7b,big=ollama/qwen3.5:27b,claude=anthropic \
       -tier fast=small -tier default=big -tier strong=claude serve
agentd models                      # what is configured, local or cloud, credentials present or not
```

`-model` and `-fallback-models` still work: the first model is the default, the others follow.
Each time a run opens, its model comes from the most specific choice:

| # | Choice | Set with |
|---|---|---|
| 1 | the owner's choice for this task | `agentd task model <task> <model or tier>`, `/model <task> ...`, the task page |
| 2 | the owner's choice for this conversation (chat replies) | `/model <model or tier>`, `agentd chat -model ...`, the chat header |
| 3 | the job's preference | `models: {prefer: [big]}` in the job file |
| 4 | the task's tier | chat replies are `fast`; `tier: strong` on a schedule, route or obligation; `models: {tier: ...}` for the whole job |
| 5 | the default | the first configured model |

The job's models policy (`allowed`, `local_only`) is always the limit: a choice it forbids is
skipped, and why is recorded with the run (the task page shows it). A run whose model fails after
it has started answering moves one tier up for its next attempt, unless you chose its model. A
change applies from the task's next step, or from its next run when its transcript holds a
provider's private data (Claude's thinking). Moving work from a local model to a cloud one says so
when you choose it; from a paired phone it needs the `widen` setting. `/usage` shows spend per model.

## Job files

A job is a Markdown file: a header between two `---` lines, then the objective in plain words.

```markdown
---
kind: project
name: airline-refund
policy:
  tools:
    gmail_search: allow
    gmail_send:
      mode: approval
      resources: [refunds@airline.example]
    ask_owner: allow
  limits:
    max_llm_spend_usd: 5
completion:
  criteria:
    - id: refund
      description: Refund posted to my card
---

Get the airline to refund my cancelled flight LH123. Be polite but firm.
```

The header uses exactly the keys of the JSON form (`kind`, `name`, `policy`, `completion`,
`notifications`, `routes`, `schedules`, `obligations`); a tool rule may be just a mode
(`notify: allow`). It is strict: an unknown or repeated key, an unknown mode, anchors, aliases,
tags or a second document are errors with the line number. JSON files still work, and now also
refuse unknown keys.

```bash
agentd create -dry-run job.md     # check it and print warnings; creates nothing
agentd create job.md
agentd export <id> > job.md       # the assignment as it is now (-format json for JSON)
agentd policy <id> policy.md      # a file whose header holds only policy
```

Examples: [first-job.md](examples/first-job.md), [airline-refund.md](examples/airline-refund.md),
[portfolio-intelligence.md](examples/portfolio-intelligence.md) (with its JSON twin).

## Events, schedules and models

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

## Tests

`go test -race ./...` runs the leasing, transition, crash-recovery, adapter and neutrality suites.
`internal/e2e` replays a week of an airline-refund project against a fake Gmail and a fake clock
(a crash mid-send, a prompt-injection email, a follow-up, an approval that is not yet the outcome,
and the bank posting the refund) and logs its metrics with `go test -v ./internal/e2e`. To add a
scenario, script the agent's choices in `decide` and drive time with `hours`.

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

A responsibility can declare what must keep happening, and agentd checks it from evidence instead
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
keep up to date (see [`docs/examples/holdings.json`](examples/holdings.json)). Quantities and
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
prints its plan until you add `--apply`; see [docs/isolation.md](isolation.md) for the threat
model, migration, verification and rollback.
