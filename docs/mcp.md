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

See the README: a `poll` turns a read tool's new items into inbox events with no model call, and
a service that can call out can post to `POST /api/events` instead.
