# agentd

A personal AI agent that keeps working in the background: it takes on a job, uses tools, waits
for replies, and picks up where it left off after a crash. It asks you before anything risky.

One Go program and one SQLite file. Works with a local model (Ollama) or Claude.

## Try it in 2 minutes

You need [Go 1.26+](https://go.dev/dl/). No model or API key is needed for the demo.

```bash
git clone https://github.com/mohanadkaleia/agentd.git
cd agentd
go build -o bin/agentd ./cmd/agentd

./bin/agentd demo                                   # create a sample job: get an airline refund
./bin/agentd serve -demo-tools -listen 127.0.0.1:7777
```

Leave `serve` running and open a second terminal in the same folder:

```bash
./bin/agentd status                     # the agent sent one email and waits for your OK on a second
./bin/agentd approve <approval-id>      # copy the apr_... id from status
./bin/agentd status                     # the task is completed and a follow-up is scheduled
```

Or use the web app: open http://127.0.0.1:7777 and paste the token from the `api-token` file.

The demo uses a scripted model and a fake mailbox, so nothing leaves your machine. Everything
lives in `agentd.db` in the current folder; delete it to start over.

## Use a real model

```bash
ollama pull qwen2.5:7b                              # needs https://ollama.com installed
./bin/agentd -model ollama/qwen2.5:7b serve         # local and free

export ANTHROPIC_API_KEY=...
./bin/agentd -model anthropic serve                 # Claude
```

## Give it a job

A job is a JSON file: what to do, which tools it may use, and what needs your approval.

```bash
./bin/agentd -skills docs/examples/skills create docs/examples/portfolio-intelligence.json
./bin/agentd show <assignment-id>
```

## Run it as a background service (macOS)

```bash
deploy/install.sh ollama/qwen2.5:7b     # starts at login and restarts if it crashes
deploy/uninstall.sh                     # stop it; your data is kept
```

## Learn more

| | |
|---|---|
| [docs/guide.md](docs/guide.md) | Every feature: Gmail, schedules, approvals, phone notifications, web app, HTTP API |
| [docs/mcp.md](docs/mcp.md) | Connect Slack, Google Workspace or any MCP server |
| [docs/isolation.md](docs/isolation.md) | Run it under its own macOS account |
| [ARCHITECTURE.md](ARCHITECTURE.md) | How it works and the safety rules the code enforces |

Run the tests with `go test -race ./...`.
