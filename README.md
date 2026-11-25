# agentd

A personal AI agent that keeps working in the background: it takes on a job, uses tools, waits
for replies, and picks up where it left off after a crash. It asks you before anything risky.

One Go program and one SQLite file. Works with a local model (Ollama) or Claude.

## Try it on your laptop

Works on macOS and Linux (on Windows, use WSL). You need `git`, [Go 1.26+](https://go.dev/dl/)
and a model. Pick the model by your laptop:

| Your laptop | Model | What it can do |
|---|---|---|
| 8–16 GB memory | `ollama/qwen2.5:7b` (5 GB download) | chat and run jobs; give it jobs as files (below) |
| 32 GB+ memory | `ollama/qwen3.5:27b` (17 GB download) | also drafts jobs from what you say, slowly |
| any | Claude (`anthropic`) | everything, fastest; needs an API key and costs money |

1. Build it (takes a few seconds):

   ```bash
   git clone https://github.com/mohanadkaleia/agentd.git
   cd agentd
   go build -o bin/agentd ./cmd/agentd
   ```

2. Get the model. For Ollama: install it from [ollama.com](https://ollama.com), open the app (or
   run `ollama serve`), then:

   ```bash
   ollama pull qwen2.5:7b
   ```

   For Claude: `export ANTHROPIC_API_KEY=...` in the terminal you use for step 3.

3. Run it:

   ```bash
   ./bin/agentd
   ```

   That is the whole start. agentd opens the chat, asks once which model to use (it lists
   what Ollama has pulled, and Claude when a key is set), and starts its own server in the
   background. Later runs start the server the same way without asking. Leaving the chat
   stops nothing; your jobs keep running.

   To type `agentd` from any folder, put it on your `PATH`, for example
   `ln -s "$PWD/bin/agentd" ~/.local/bin/agentd`. Away from a folder that holds an
   `agentd.db`, agentd keeps its data in `~/.local/share/agentd/`.

   To run the server yourself instead (a second machine, a service, other flags):
   `./bin/agentd -model ollama/qwen2.5:7b serve -listen 127.0.0.1:7777`. The chat never
   starts a second server next to one that is running.

**If something is off**

| You see | Do this |
|---|---|
| `address already in use` | another program has port 7777: use `-listen 127.0.0.1:7788` (and that port in the web address) |
| `I could not reach the model (... connection refused)` | Ollama is not running: open the app or run `ollama serve`, then send the message again |
| `model '...' not found` | run `ollama pull` with the exact name you passed to `-model` |
| Ollama runs on another computer | start agentd with `OLLAMA_HOST=192.168.1.20 ./bin/agentd ...`; that computer runs `OLLAMA_HOST=0.0.0.0 ollama serve` |

Several models at once, with small work on the fast one:
`./bin/agentd -models small=ollama/qwen2.5:7b,big=ollama/qwen3.5:27b -tier fast=small -tier default=big serve -listen 127.0.0.1:7777`.
See [Choosing a model](docs/guide.md#choosing-a-model).

## Talk to it

```bash
./bin/agentd
```

Say what you want, for example *"Every morning, read https://go.dev/blog and tell me if there is a
new post."* agentd drafts the job and shows it as a **proposal**: what it may do without asking,
what needs your approval, its budget, and when it runs. Nothing happens until you approve it:
press **Enter** on *Yes, approve* (or type `/approve 1`). Then ask *"how is it going?"* whenever you like.

The same conversation is in the web app: open http://127.0.0.1:7777 and paste the token that
`cat api-token` prints (the file sits next to `agentd.db`). Type `/help` for commands that always
work, even without a model.

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

Everything lives in one file: `agentd.db` in the current folder when there is one, else
`~/.local/share/agentd/agentd.db`. The chat's first box shows which; delete it to start over.

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
