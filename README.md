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

2. Start it with a model:

   ```bash
   ollama pull qwen2.5:7b
   ./bin/agentd -model ollama/qwen2.5:7b serve -listen 127.0.0.1:7777

   # or several models, with small work on the fast one
   ./bin/agentd -models small=ollama/qwen2.5:7b,big=ollama/qwen3.5:27b -tier fast=small -tier default=big serve -listen 127.0.0.1:7777

   # or Claude
   export ANTHROPIC_API_KEY=...
   ./bin/agentd -model anthropic serve -listen 127.0.0.1:7777
   ```

## Talk to it

In a second terminal, in the same folder:

```bash
./bin/agentd chat
```

Say what you want, for example *"Every morning, read https://go.dev/blog and tell me if there is a
new post."* agentd drafts the job and shows it as a **proposal**: what it may do without asking,
what needs your approval, its budget, and when it runs. Nothing happens until you approve it:
type `a` (or `/approve 1`). Then ask *"how is it going?"* whenever you like.

The same conversation is in the web app: open http://127.0.0.1:7777 and paste the token from the
`api-token` file. Type `/help` for commands that always work, even without a model.

Drafting a job needs a capable model. In our tests `qwen3.5:27b` drafted, corrected and confirmed a
job (slowly, on a Mac mini), while `qwen2.5:7b` usually wrote the draft as text instead; agentd
then says plainly that nothing was created. You can also write the job yourself:

## Or give it a job file

A job is a short Markdown file: a header that says which tools it may use and what needs your
approval, then what to do in plain words.
[`docs/examples/first-job.md`](docs/examples/first-job.md) reads the Go blog once a day and
summarizes the newest post:

```bash
./bin/agentd create docs/examples/first-job.md
```

Everything lives in `agentd.db` in the current folder; delete it to start over.

## Run it as a background service (macOS)

```bash
deploy/install.sh ollama/qwen2.5:7b     # starts at login and restarts if it crashes
deploy/uninstall.sh                     # stop it; your data is kept
```

## Learn more

| | |
|---|---|
| [docs/chat.md](docs/chat.md) | Talking to agentd: terminal, web app, phone, and what it can and cannot do from chat |
| [docs/guide.md](docs/guide.md) | Every feature: Gmail, schedules, approvals, phone notifications, web app, HTTP API |
| [docs/mcp.md](docs/mcp.md) | Connect Slack, Google Workspace or any MCP server |
| [docs/isolation.md](docs/isolation.md) | Run it under its own macOS account |
| [ARCHITECTURE.md](ARCHITECTURE.md) | How it works and the safety rules the code enforces |

Run the tests with `go test -race ./...`.
