# agentd

A personal AI agent that keeps working in the background: it takes on a job, uses tools, waits
for replies, and picks up where it left off after a crash. It asks you before anything risky.

One Go program and one SQLite file. Works with a local model (Ollama) or Claude.

## Try it locally

You need [Go 1.26+](https://go.dev/dl/) and a model: [Ollama](https://ollama.com) (free, runs on
your machine) or an Anthropic API key.

1. Build it:

   ```bash
   git clone https://github.com/mohanadkaleia/agentd.git
   cd agentd
   go build -o bin/agentd ./cmd/agentd
   ```

2. Give it a job. A job is a short Markdown file: a header that says which tools it may use and
   what needs your approval, then what to do in plain words. Start from
   [`docs/examples/first-job.md`](docs/examples/first-job.md), which reads the Go blog once a day
   and summarizes the newest post (or just ask for a job in the chat):

   ```bash
   ./bin/agentd create docs/examples/first-job.md
   ```

3. Start it with a model:

   ```bash
   ollama pull qwen2.5:7b
   ./bin/agentd -model ollama/qwen2.5:7b serve -listen 127.0.0.1:7777

   # or Claude
   export ANTHROPIC_API_KEY=...
   ./bin/agentd -model anthropic serve -listen 127.0.0.1:7777
   ```

4. Talk to it from a second terminal: `./bin/agentd chat` (ask "what are you working on?"), or
   watch with `./bin/agentd status`, or open the web app at http://127.0.0.1:7777 and paste the
   token from the `api-token` file.

Everything lives in `agentd.db` in the current folder; delete it to start over.

## Run it as a background service (macOS)

```bash
deploy/install.sh ollama/qwen2.5:7b     # starts at login and restarts if it crashes
deploy/uninstall.sh                     # stop it; your data is kept
```

## Learn more

| | |
|---|---|
| [docs/chat.md](docs/chat.md) | Talking to agentd in the terminal |
| [docs/guide.md](docs/guide.md) | Every feature: Gmail, schedules, approvals, phone notifications, web app, HTTP API |
| [docs/mcp.md](docs/mcp.md) | Connect Slack, Google Workspace or any MCP server |
| [docs/isolation.md](docs/isolation.md) | Run it under its own macOS account |
| [ARCHITECTURE.md](ARCHITECTURE.md) | How it works and the safety rules the code enforces |

Run the tests with `go test -race ./...`.
