# Talking to agentd

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
- **Enter** sends. End a line with `\` or paste several lines to keep going.
- **Ctrl-C** while agentd is working stops that turn. **Ctrl-C** or **Ctrl-D** at the prompt
  leaves. Leaving stops nothing: your projects keep running.
- An approval card shows `(d)etails (a)pprove (r)eject`. Typing just `d`, `a` or `r` acts on it
  only when that card is the last thing shown; otherwise use `/approve <n>` or `/reject <n>`.
- `/quit` and `/clear` are handled by the terminal; every other `/command` (see `/help`) is run by
  agentd itself, never by a model, so it works when the model is down or the chat budget is used up.
- Input history is kept in `~/.local/state/agentd/chat_history` (or `$XDG_STATE_HOME/agentd`),
  mode 0600. `AGENTD_CHAT_HISTORY=off` keeps it in memory only.
- Text from models, tools and projects is printed with every control character removed, so it
  cannot move the cursor, change colours, or fake a prompt. `NO_COLOR` turns colour off.

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
