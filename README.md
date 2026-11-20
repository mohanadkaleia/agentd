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

`go test -race ./...` runs the leasing, transition and crash-recovery suites.

## Layout

| Package | Role |
|---|---|
| `internal/model` | Provider-neutral inference + tool-calling types. Adapters live in subpackages (`model/scripted`; `model/anthropic`, `model/ollama` next). |
| `internal/tasks` | Durable task queue: atomic claim, lease renew, versioned transitions. |
| `internal/runtime` | Agent loop. Checkpoints every model step and tool result; resumes the same run after a crash. |
| `internal/gateway` | The only path to side effects: action ledger, policy check, approvals, dispatch, reconciliation. |
| `internal/policy` | Fail-closed per-tool rules, scoped by resource. |
| `internal/tools` | Tool interface + registry; `mock.go` holds the simulated web search and mailbox. |
| `migrations/` | Embedded SQL migrations, applied on open. |

## Invariants

1. Every tool call is persisted as an action, keyed `run:step:call`, before it runs.
2. Policy is evaluated by code on every dispatch; no rule means deny.
3. An approval binds to the action's exact argument hash and expires.
4. A crash mid-dispatch is reconciled with the provider before anything is retried; an unknowable outcome blocks the task.
5. A task completes only when its success criteria have recorded evidence.
