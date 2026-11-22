# MCP servers

agentd can use the tools of any [Model Context Protocol](https://modelcontextprotocol.io) server
without new Go code: a local server it starts (stdio) or a hosted one it reaches over HTTPS.
Every MCP tool goes through the same gateway as a built-in tool: the action ledger, policy rules,
approvals bound to exact arguments, rate limits, audit and the emergency stop.

## Set up a server in five steps

1. **Write the config** (`chmod 600`), for example [`examples/mcp.json`](examples/mcp.json). Store
   secrets in the keychain and reference them: `security add-generic-password -s agentd -a slack-client-secret -w`.
2. **Check it:** `agentd mcp check -config ~/agentd-mcp.json` (every secret resolves).
3. **Sign in** to an OAuth server: `agentd mcp auth -config ~/agentd-mcp.json slack`.
4. **Install with it:** `deploy/install.sh --mcp-config ~/agentd-mcp.json --listen tailscale:7777 <model>`.
   Then `agentd mcp review` shows each tool's definition, and `agentd mcp accept slack <tool>...`
   accepts the ones you want. Nothing runs before that.
5. **Write the policy** for each assignment: one rule per tool, using the decision table below.

## Deciding about each tool

| Question | Config (`tools.meta.<tool>`) | Policy |
|---|---|---|
| Does it only read? | `"effect": "read"` (default is write) | usually `allow` |
| Which argument names the thing it touches? | `"resource": "channel_id"` | `"resources": ["C0123"]` |
| Is running it twice harmless? | `"idempotent": true` | |
| Can a write be found again afterwards? | `"reconcile": {...}` | |
| Should a person see each call? | | `"mode": "approval"`, or `"otherwise": "approval"` |
| How often at most? | | `"limits": {"tool_rate_limits": {...}}` |
| Its name has dots or is too long? | `"name": "search_threads"` | rules use the new name |

Server hints such as `readOnlyHint` are shown by `agentd mcp tools`, but never applied.

## What happens after a crash or an unclear error

| Tool | Outcome |
|---|---|
| Read | The call is failed; after a crash it simply runs again. |
| Idempotent write | The call is failed and the model may try again; a wrong declaration means a duplicate. |
| Write with `reconcile` | agentd looks for its marker: found means done, not found keeps the task blocked and is checked every minute, unless `miss_is_authoritative_after` has passed. |
| Plain write | The task blocks until you run `agentd resolve <action> happened|not-happened`. |
| Request never left (connection refused, 401/403, no free slot) | Not executed; the model is told. |

## Two worked examples (checked 2026-11-21)

Both providers run official hosted servers. Confirm current details in their docs before relying
on them; these products are new and change.

**Slack** ([docs](https://docs.slack.dev/ai/slack-mcp-server/)): `https://mcp.slack.com/mcp`,
Streamable HTTP, user-token OAuth with a confidential client. Slack does not support dynamic
client registration, so create a Slack app (internal or directory-published), add the redirect URL
`http://127.0.0.1:53682/callback` to it, and give agentd its client id and a keychain reference to
its secret; `redirect_port` makes agentd listen on that exact port during `agentd mcp auth`. Ask
only for the user scopes the tools need, such as `channels:history` to read a channel and
`chat:write` to post. Slack does not publish the tool names in its docs: run
`agentd mcp test -config FILE slack` to list them, then declare `meta` for the ones you use.

**Google Workspace** ([docs](https://developers.google.com/workspace/guides/configure-mcp-servers)):
one server per product, for example Gmail at `https://gmailmcp.googleapis.com/mcp/v1` with
`gmail.search_threads`, `gmail.get_thread` and `gmail.create_draft`. It was in Developer Preview
when checked. In a Google Cloud project, enable the product API and its MCP service
(`gmail.googleapis.com` and `gmailmcp.googleapis.com`), create a Desktop OAuth client (which
accepts agentd's loopback redirect), and request only the scopes you need. Google's tool names
contain dots, which models do not accept, so each one is named in `meta`.

## Webhooks and polling

See the reference below: a `poll` turns a read tool's new items into inbox events with no model call, and
a service that can call out can post to `POST /api/events` instead.

## Reference

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
