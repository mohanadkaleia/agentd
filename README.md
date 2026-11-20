# agentd

A durable, model-agnostic personal agent runtime. Go + SQLite, one process.

## Try it

```bash
go build -o bin/agentd ./cmd/agentd
./bin/agentd demo                        # seed the airline-refund project
./bin/agentd serve -tool-delay 5s        # kill -9 it while send_email is sleeping
./bin/agentd serve                       # restart: the email is reconciled, not resent
./bin/agentd status
./bin/agentd approve <approval-id>
```

Feed it events and schedules:

```bash
./bin/agentd event -source gmail -kind email.received -id gm-001 -payload '{"thread_id":"thread-xy123"}'
./bin/agentd schedule -project <id> -every 24h -title "Check inbox" -objective "Look for airline replies."
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

## Invariants

1. Every tool call is persisted as an action, keyed `run:step:call`, before it runs.
2. Policy is evaluated by code on every dispatch; no rule means deny.
3. An approval binds to the action's exact argument hash and expires.
4. A crash mid-dispatch is reconciled with the provider before anything is retried; an unknowable outcome blocks the task.
5. A task completes only when its success criteria have recorded evidence.
6. Each event, schedule slot, follow-up and delegation creates at most one task, even across crashes.
7. A task's tool allowlist is enforced by the gateway, and a worker can never be granted a tool its parent lacks.
8. Every model call's cost is recorded with the step it produced. When a project's spend reaches its cap the project pauses; raising the cap and resuming continues the same run.
9. A rate-limited tool reserves its slot in the same transaction that marks the action dispatching, so concurrent workers cannot overshoot.
