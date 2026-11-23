# Talking to agentd

## What agentd can and cannot do from chat

| From the conversation, agentd can | It cannot |
|---|---|
| Answer from its records ("how is the refund going?") | Approve or reject anything: only you can, with a button, `a`/`r` or `/approve` |
| Draft a new job, a policy change, a schedule, a route or a task, for you to approve | Make a draft take effect without your approval, or let itself skip approvals |
| Pass your own words, unchanged, to running work | Write instructions to a task in its own words |
| Pause a job | Resume a job, lift the emergency stop, complete or cancel a project, revoke a responsibility, confirm a completion criterion, accept an MCP tool |

Slash commands are run by agentd itself, never by a model, so they work when the model is down or
the chat budget is used up:

| Command | |
|---|---|
| `/status`, `/show <job>`, `/waiting`, `/usage` | what is going on, what waits on you, spend |
| `/approve <n>`, `/reject <n>` | decide a card (the number shown on it) |
| `/answer <n> <text>` | answer a question a task asked you |
| `/tell <task or job> <text>` | send your words to running work |
| `/edit <n> <job file or JSON>` | replace a proposal with your own version, which takes effect at once |
| `/pause <job>`, `/resume <job>`, `/retry <task> [note]` | as the CLI commands |
| `/halt [reason]`, `/unhalt` | emergency stop |
| `/model [task] [model or tier or auto]` | see or choose the model for this conversation or a task |
| `/new`, `/help` | new conversation, this list |

The conversation is answered by a built-in assignment, the **front desk** (`agentd status` shows it
as built in). It has its own budget, 5 USD per 30 days, so talking never spends a project's money;
change it with `agentd policy <front-desk-id> policy.md`. By default it may only read agentd's
records, draft proposals, pass on your messages and pause; giving it more tools (mail, web, MCP)
means text it reads in projects could reach those tools, so do that deliberately.

`agentd chat` is a conversation with agentd in your terminal. It needs `agentd serve` running
(directly or as the installed service): the terminal stores your message, and the running agentd
answers it.

```bash
agentd chat                          # interactive
agentd chat "how is the refund going?"   # one message, print the reply, exit
echo "what is waiting on me?" | agentd chat
agentd chat "summarise this" < notes.txt  # the file is attached to the message (64 KB at most)
agentd chat -history -n 20           # recent history
```

| Flag | |
|---|---|
| `-new`, `-c <id>` | start a new conversation, or use a specific one (default: the most recent) |
| `-json` | one JSON object per line (see below) |
| `-no-wait` | store the message and exit; it is answered when agentd runs |
| `-timeout 5m` | stop waiting (default: none interactive, 5 minutes one-shot) |

Text you pipe in is your own words to agentd, so it is treated as an instruction. Do not pipe in an
email or a web page and expect it to be treated as data.

## In the interactive session

- On start: one line of status, then what arrived since you were last here, decisions first.
  `agentd` with no command opens this session too.
- The input box stays at the bottom. **Enter** sends. **Ctrl-J**, **Alt-Enter** or a line ending
  in `\` adds a line; pasted text keeps its lines. **Up** and **Down** recall earlier input.
- Type `/` to see every command with what it does; **Tab** or **Enter** completes the one selected.
- The line under the box always shows the model, how many items wait on you, the emergency stop
  when it is on, and whether a server is running. With no server, **Ctrl-S** starts `agentd serve`
  in the background with the flags this chat was started with; its log is
  `~/.local/state/agentd/serve.log`.
- Replies are written to the screen as the model produces them when the server has `-listen` set
  (the chat reads the API token file the server uses); otherwise each reply appears when complete.
- **Esc** or **Ctrl-C** while agentd is working stops that turn. **Ctrl-C** twice, or **Ctrl-D**,
  at an empty box leaves. Leaving stops nothing: your projects keep running.
- An open approval, proposal or question is a box. When it is the last thing shown, **Tab** gives
  it the focus and then one key decides: `a` approve, `r` reject, `d` details, `e` edit, or a
  number for a question's choice. **Esc** gives the focus back. Without the focus a letter is only
  a decision when it is the whole message, so a sentence can never approve anything. Otherwise use
  `/approve <n>` or `/reject <n>`. A decided card is repeated as one line with its outcome.
- `agentd chat -ui classic` is the earlier plain line prompt; it is also used when `TERM=dumb`.
- Ask for new work in plain words ("get me a refund for flight LH123"). agentd drafts it and shows a
  proposal card: what it may do without asking, what needs your approval, its budget, and when it is
  done. Nothing exists until you approve that exact draft. `e` opens the draft in `$EDITOR`; saving
  replaces the draft with your version, which takes effect at once.
- `/tell <task or assignment> <text>` sends your words to running work; `agentd tell` does the same
  from the shell. When a task asks you something, a QUESTION card appears: answer with
  `/answer <n> <text>`, or `agentd answer <question-id> "text"`.
- `/quit` and `/clear` are handled by the terminal; every other `/command` (see `/help`) is run by
  agentd itself, never by a model, so it works when the model is down or the chat budget is used up.
- Input history is kept in `~/.local/state/agentd/chat_history` (or `$XDG_STATE_HOME/agentd`),
  mode 0600. `AGENTD_CHAT_HISTORY=off` keeps it in memory only.
- Text from models, tools and projects is printed with every control character removed, so it
  cannot move the cursor, change colours, or fake a prompt. `NO_COLOR` turns colour off.

## In the web app

The command center opens on the same conversation: http://<address>:7777 after signing in. Cards
have Approve, Reject and Edit buttons, questions have choice buttons and a reply box, the Stop
button cancels a running turn, and a task page has a "Message this task" box. Replies are shown as
plain text with light formatting; nothing in a reply can load an image or open a link by itself.

The web app shows replies as they are written. The terminal shows each reply whole, when it is
finished.

## On your phone (Telegram)

agentd can talk to you through a Telegram bot. It only reaches out to Telegram (long polling), so
your agentd stays private. Telegram can read what is sent, so choose how much goes there.

1. Create a bot with [@BotFather](https://t.me/BotFather) and copy its token.
2. Store the token on the Mac: `security add-generic-password -s agentd -a telegram-bot-token -w`
3. Restart agentd (`agentd serve`, or reinstall the service).
4. Run `agentd channel pair telegram` and send the printed code to your bot within 10 minutes.

Only that Telegram account, in a direct chat with the bot, is heard. Everyone else gets a fixed
refusal; group messages, forwarded messages and edits are ignored. `agentd channel revoke telegram`
unpairs at once (for a lost phone).

| Setting | Default | |
|---|---|---|
| `-level minimal` | | only "something is waiting, open the command center" |
| `-level summary` | yes | titles, the tool and recipient, questions; approvals say "open to review" |
| `-level full` | | bodies and exact arguments, and Approve / Reject buttons |
| `-no-decide` | | no approving, rejecting or answering from the phone |
| `-widen` | off | allow approving proposals, `/resume` and `/unhalt` from the phone |

`agentd channel set telegram -level full` changes it. Deciding needs the exact arguments, so
approve-in-place needs `full`. Anything about a project with `models.local_only` is only ever
sent at `minimal`. `/halt` always works from a paired phone. While the bot runs, notifications
reach the phone through it, so ntfy does not push them a second time.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | reply received, or message stored with `-no-wait` |
| 1 | usage or local error, or a slash command refused |
| 2 | agentd is not running |
| 3 | the turn failed, was stopped, or is waiting (emergency stop, paused, out of budget) |
| 4 | timed out waiting; the reply still arrives in the conversation |

## JSON lines (`-json`)

Every line is one object with a `type`:

| `type` | Fields |
|---|---|
| `message` | `message`: a conversation message (`id`, `conversation_id`, `seq`, `author` owner/agent/system, `kind` text/error/notice/command_result, `body.text`, `turn_task_id`, `created_at`) |
| `card` | `message`: a card (`kind` approval/notification/report, `status` for approvals, `body` with `n`, `title`, `text`, `tool`, `resource`, `arguments`, `arguments_sha256`, `expires_at`, `actions`) |
| `tool` | `turn_task_id`, `tool`, `status`: one tool call a turn made |

Strings are cleaned of control characters before they are written.

## Under the separate-account install

The database belongs to the agent account, so run the chat as that account:

```bash
sudo -u baseet "/Users/baseet/Library/Application Support/agentd/bin/agentd" \
    -db "/Users/baseet/Library/Application Support/agentd/agentd.db" chat
```

## How it works

Your message and the turn that answers it are written in one transaction; the running agentd
picks the turn up within 200 ms. See the Conversation section of [ARCHITECTURE.md](../ARCHITECTURE.md).
