package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/term"

	"agentd/internal/app"
	"agentd/internal/chat"
	"agentd/internal/system"
	"agentd/internal/tools"
)

const (
	exitNotRunning = 2
	exitTurnFailed = 3
	exitTimeout    = 4
	maxAttachment  = 64 << 10
	chatChannel    = "cli"
)

const notRunningHint = "agentd is not running, so nothing will answer. Start it with `agentd serve`, " +
	"or restart the installed service: launchctl kickstart -k gui/$(id -u)/local.agentd"

type chatClient struct {
	app     *app.App
	conv    string
	out     io.Writer
	json    bool
	color   bool
	width   int
	timeout time.Duration
	// interrupt delivers Ctrl-C while a turn runs.
	interrupt chan os.Signal

	mu    sync.Mutex
	after int64
	last  *chat.Message
}

func chatCommand(ctx context.Context, db *sql.DB, args []string, stdin *os.File, stdout *os.File) error {
	tty := term.IsTerminal(int(stdin.Fd())) && term.IsTerminal(int(stdout.Fd()))
	width := 80
	if w, _, err := term.GetSize(int(stdout.Fd())); err == nil && w > 20 {
		width = w
	}
	return runChat(ctx, db, args, stdin, stdout, tty, term.IsTerminal(int(stdin.Fd())), width)
}

func runChat(ctx context.Context, db *sql.DB, args []string, stdin io.Reader, stdout io.Writer, tty, stdinTTY bool, width int) error {
	f := flag.NewFlagSet("chat", flag.ContinueOnError)
	newConv := f.Bool("new", false, "start a new conversation")
	conv := f.String("c", "", "conversation id")
	history := f.Bool("history", false, "print recent history and exit")
	n := f.Int("n", 50, "messages to print with -history")
	asJSON := f.Bool("json", false, "one JSON object per message or event per line")
	noWait := f.Bool("no-wait", false, "store the message and exit without waiting for the reply")
	timeout := f.Duration("timeout", 0, "give up waiting after this long (default: none interactive, 5m one-shot)")
	modelChoice := f.String("model", "", "choose the model for this conversation: a model, alias, tier, or auto")
	ui := f.String("ui", "new", "interactive interface: new, or classic for the plain line prompt")
	if err := f.Parse(args); err != nil {
		return &exitError{code: 1, msg: err.Error()}
	}
	if *ui != "classic" && *ui != "new" {
		return &exitError{code: 1, msg: "-ui is classic or new"}
	}
	a := app.New(db, nil, "", tools.Registry{}, time.Now)
	// Commands like /model run here, not in serve, so they need serve's models.
	adoptCatalogue(ctx, a, db)
	c := &chatClient{app: a, out: stdout, json: *asJSON, width: width, timeout: *timeout,
		color: tty && os.Getenv("NO_COLOR") == "" && !*asJSON}
	var err error
	switch {
	case *newConv:
		c.conv, err = a.Chat.NewConversation(ctx, "")
	case *conv != "":
		c.conv = *conv
	default:
		c.conv, err = a.Chat.Default(ctx)
	}
	if err != nil {
		return err
	}
	if *modelChoice != "" {
		msg, err := a.Chat.SetConversationModel(ctx, c.conv, *modelChoice, "owner:"+chatChannel)
		if err != nil {
			return &exitError{code: 1, msg: err.Error()}
		}
		fmt.Fprintln(stdout, sanitize(msg))
	}
	if *history {
		return c.history(ctx, *n)
	}
	message := strings.TrimSpace(strings.Join(f.Args(), " "))
	if message == "-" || (message == "" && !stdinTTY) {
		body, err := readCapped(stdin)
		if err != nil {
			return &exitError{code: 1, msg: err.Error()}
		}
		message = strings.TrimSpace(body)
	} else if message != "" && !stdinTTY && stdin != nil {
		body, err := readAttachment(stdin, 500*time.Millisecond)
		if err != nil {
			return &exitError{code: 1, msg: err.Error()}
		}
		if strings.TrimSpace(body) != "" {
			message += "\n\nAttached (from my standard input):\n```\n" + strings.TrimRight(body, "\n") + "\n```"
		}
	}
	if message == "" && *modelChoice != "" && !tty {
		return nil
	}
	if message == "" {
		if !tty || *asJSON {
			return &exitError{code: 1, msg: "no message: pass one, pipe one in, or run `agentd chat` in a terminal"}
		}
		if in, ok := stdin.(*os.File); ok && *ui == "new" && os.Getenv("TERM") != "dumb" {
			return c.tui(ctx, in)
		}
		return c.interactive(ctx, stdin)
	}
	return c.oneShot(ctx, message, *noWait)
}

// readAttachment reads standard input that comes with a message argument. Input that has
// not started within wait is ignored, so a caller whose standard input is an idle pipe
// (a job runner, a script without redirection) is not left waiting for an end that never comes.
func readAttachment(r io.Reader, wait time.Duration) (string, error) {
	first := make(chan error, 1)
	var head [1]byte
	var n int
	go func() {
		var err error
		n, err = r.Read(head[:])
		first <- err
	}()
	select {
	case err := <-first:
		if n == 0 {
			if errors.Is(err, io.EOF) {
				return "", nil
			}
			return "", err
		}
		rest, err := readCapped(r)
		if err != nil {
			return "", err
		}
		if len(rest)+1 > maxAttachment {
			return "", fmt.Errorf("standard input is larger than %d KB; send a shorter excerpt", maxAttachment>>10)
		}
		return string(head[:1]) + rest, nil
	case <-time.After(wait):
		return "", nil
	}
}

func readCapped(r io.Reader) (string, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxAttachment+1))
	if err != nil {
		return "", err
	}
	if len(body) > maxAttachment {
		return "", fmt.Errorf("standard input is larger than %d KB; send a shorter excerpt", maxAttachment>>10)
	}
	return string(body), nil
}

func (c *chatClient) alive(ctx context.Context) (bool, error) {
	_, ok, err := system.Alive(ctx, c.app.DB, time.Now())
	return ok, err
}

func (c *chatClient) oneShot(ctx context.Context, text string, noWait bool) error {
	alive, err := c.alive(ctx)
	if err != nil {
		return err
	}
	if !alive && !noWait {
		return &exitError{code: exitNotRunning, msg: notRunningHint + "\nTo store the message anyway: agentd chat -no-wait ..."}
	}
	m, err := c.post(ctx, chat.Post{Text: text})
	if err != nil {
		return err
	}
	if noWait {
		c.emit(m)
		return nil
	}
	timeout := c.timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	c.interrupt = make(chan os.Signal, 1)
	signal.Notify(c.interrupt, os.Interrupt)
	defer signal.Stop(c.interrupt)
	return c.outcomeError(c.follow(ctx, m, timeout))
}

func (c *chatClient) post(ctx context.Context, p chat.Post) (chat.Message, error) {
	id := make([]byte, 8)
	rand.Read(id)
	p.ConversationID, p.Channel, p.ClientMsgID = c.conv, chatChannel, hex.EncodeToString(id)
	m, err := c.app.Chat.Post(ctx, p)
	if err != nil {
		return m, err
	}
	c.mu.Lock()
	c.after = max(c.after, m.Seq)
	c.mu.Unlock()
	return m, nil
}

type outcome int

const (
	answered outcome = iota
	failed
	refused
	waiting
	cancelled
	timedOut
)

func (c *chatClient) outcomeError(o outcome, err error) error {
	switch {
	case err != nil:
		return err
	case o == refused:
		return &exitError{code: 1}
	case o == failed || o == waiting || o == cancelled:
		return &exitError{code: exitTurnFailed}
	case o == timedOut:
		return &exitError{code: exitTimeout, msg: "timed out waiting for a reply; it will still arrive in the conversation"}
	}
	return nil
}

// follow prints what happens after the owner's message until its answer arrives: tool
// activity, cards, and the reply, a failure, a notice that replies are paused, or the result
// of a slash command.
func (c *chatClient) follow(ctx context.Context, m chat.Message, timeout time.Duration) (outcome, error) {
	var deadline <-chan time.Time
	if timeout > 0 {
		deadline = time.After(timeout)
	}
	direct := m.TurnTaskID == ""
	seen := map[string]bool{}
	stopping := false
	for {
		msgs, err := c.app.Chat.Messages(ctx, c.conv, c.after, 200)
		if err != nil {
			return failed, err
		}
		done, result := false, answered
		for _, msg := range msgs {
			c.show(msg)
			if done {
				continue
			}
			done, result = settles(m, msg)
		}
		if done {
			return result, nil
		}
		if !direct {
			if err := c.activity(ctx, m.TurnTaskID, seen); err != nil {
				return failed, err
			}
		}
		select {
		case <-ctx.Done():
			return failed, ctx.Err()
		case <-deadline:
			return timedOut, nil
		case <-c.interrupt:
			if direct || stopping {
				return cancelled, nil
			}
			stopping = true
			if err := c.app.Chat.Cancel(ctx, c.conv, "owner:"+chatChannel); err != nil && !errors.Is(err, chat.ErrNoTurn) {
				return failed, err
			}
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// settles reports whether msg ends the wait that began with the owner's message sent, and how.
func settles(sent, msg chat.Message) (bool, outcome) {
	direct := sent.TurnTaskID == ""
	switch {
	case direct && msg.Kind == chat.KindCommandResult:
		if failedText(msg) {
			return true, refused
		}
		return true, answered
	case !direct && msg.TurnTaskID == sent.TurnTaskID && msg.Author == chat.Agent:
		return true, answered
	case !direct && (msg.Kind == chat.KindProposal || msg.Kind == chat.KindApproval || msg.Kind == chat.KindQuestion) && msg.Card().TaskID == sent.TurnTaskID:
		return true, answered
	case !direct && msg.TurnTaskID == sent.TurnTaskID && msg.Kind == chat.KindError:
		return true, failed
	case !direct && msg.TurnTaskID == sent.TurnTaskID && msg.Kind == chat.KindNotice && msg.RefKind == "":
		return true, cancelled
	case msg.Kind == chat.KindNotice && msg.RefKind == "wait":
		return true, waiting
	}
	return false, answered
}

func failedText(m chat.Message) bool {
	var t chat.Text
	json.Unmarshal(m.Body, &t)
	return t.Failed
}

var toolWords = map[string]string{
	"list_assignments": "looking at your projects", "get_assignment": "reading a project", "get_task": "reading a task",
	"list_waiting_on_owner": "checking what waits on you", "recent_activity": "reading recent activity",
	"get_usage": "checking spend", "list_tools": "checking available tools",
	"propose_assignment": "drafting a job", "propose_task": "drafting a task", "pause_assignment": "pausing a job",
	"web_read": "reading a web page", "web_search": "searching the web", "gmail_search": "searching your mail",
	"gmail_read": "reading an email", "gmail_draft": "drafting an email", "gmail_send": "sending an email",
	"send_email": "sending an email", "notify": "sending you a notification", "update_state": "saving its notes",
	"portfolio_positions": "reading your holdings", "ask_owner": "asking you a question",
	"propose_policy": "drafting a policy change", "propose_resume": "proposing to resume a job",
	"propose_route": "drafting an event rule", "propose_schedule": "drafting a schedule",
}

type toolCall struct{ id, tool, status string }

func (t toolCall) words() string {
	if words := toolWords[t.tool]; words != "" {
		return words
	}
	if server, tool, ok := strings.Cut(t.tool, "__"); ok {
		return "using " + strings.ReplaceAll(tool, "_", " ") + " on " + server
	}
	return "using " + strings.ReplaceAll(t.tool, "_", " ")
}

func (c *chatClient) toolCalls(ctx context.Context, turn string) ([]toolCall, error) {
	rows, err := c.app.DB.QueryContext(ctx, `SELECT id, tool_name, status FROM actions WHERE task_id = ? ORDER BY created_at, id`, turn)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []toolCall
	for rows.Next() {
		var t toolCall
		if err := rows.Scan(&t.id, &t.tool, &t.status); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// activity prints one line per tool call the turn makes.
func (c *chatClient) activity(ctx context.Context, turn string, seen map[string]bool) error {
	calls, err := c.toolCalls(ctx, turn)
	if err != nil {
		return err
	}
	for _, t := range calls {
		if seen[t.id] {
			continue
		}
		seen[t.id] = true
		if c.json {
			c.record(map[string]string{"type": "tool", "turn_task_id": turn, "tool": t.tool, "status": t.status})
			continue
		}
		c.print(c.dim("  · " + sanitize(t.words())))
	}
	return nil
}

func (c *chatClient) history(ctx context.Context, n int) error {
	var last int64
	if err := c.app.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM conversation_messages WHERE conversation_id = ?`,
		c.conv).Scan(&last); err != nil {
		return err
	}
	msgs, err := c.app.Chat.Messages(ctx, c.conv, max(last-int64(n), 0), n)
	if err != nil {
		return err
	}
	for _, m := range msgs {
		c.show(m)
	}
	return nil
}

// show prints one message, as text or as a JSON line.
func (c *chatClient) show(m chat.Message) {
	c.track(m)
	if c.json {
		c.emit(m)
		return
	}
	c.print(c.render(m))
}

// track remembers how far the conversation has been shown and which open card came last.
func (c *chatClient) track(m chat.Message) {
	c.mu.Lock()
	c.after = max(c.after, m.Seq)
	if (m.Kind == chat.KindApproval || m.Kind == chat.KindProposal || m.Kind == chat.KindQuestion) && (m.Status == "pending" || m.Status == "open") {
		shown := m
		c.last = &shown
	} else if m.Author != chat.Owner {
		c.last = nil
	}
	c.mu.Unlock()
}

func (c *chatClient) render(m chat.Message) string {
	switch m.Kind {
	case chat.KindApproval, chat.KindProposal, chat.KindQuestion, chat.KindNotification, chat.KindReport:
		return c.card(m)
	case chat.KindError:
		return c.bold("! ") + c.wrap(sanitize(m.Text()), 2)
	case chat.KindNotice:
		return c.dim("· " + c.wrap(sanitize(m.Text()), 2))
	case chat.KindCommandResult:
		return strings.TrimRight(sanitize(m.Text()), "\n")
	}
	if m.Author == chat.Owner {
		return c.dim("you: ") + c.wrap(sanitize(m.Text()), 5)
	}
	return c.wrap(sanitize(m.Text()), 0)
}

func (c *chatClient) card(m chat.Message) string {
	card := m.Card()
	var b strings.Builder
	switch m.Kind {
	case chat.KindApproval:
		expires := ""
		if card.ExpiresAt != nil && m.Status == "pending" {
			expires = "  expires in " + time.Until(*card.ExpiresAt).Round(time.Minute).String()
		}
		fmt.Fprintf(&b, "[%d] %s  %s%s\n", card.N, c.bold("APPROVAL"), c.clip(sanitize(card.Title), c.width-20), expires)
		if card.Assignment != "" {
			fmt.Fprintf(&b, "    for %s\n", sanitize(card.Assignment))
		}
		if m.Status == "pending" {
			fmt.Fprintf(&b, "    (d)etails  (a)pprove  (r)eject   or  /approve %d", card.N)
		} else {
			fmt.Fprintf(&b, "    %s", m.Status)
		}
	case chat.KindProposal:
		fmt.Fprintf(&b, "[%d] %s  %s\n    %s\n", card.N, c.bold("PROPOSAL"), sanitize(card.Title), c.wrap(sanitize(card.Text), 4))
		if m.Status == "pending" {
			fmt.Fprintf(&b, "    (d)etails  (a)pprove  (r)eject  (e)dit   or  /approve %d", card.N)
		} else {
			fmt.Fprintf(&b, "    %s", m.Status)
		}
	case chat.KindQuestion:
		fmt.Fprintf(&b, "[%d] %s  %s asks: %s", card.N, c.bold("QUESTION"), sanitize(card.Assignment), c.wrap(sanitize(card.Title), 4))
		if card.Text != "" {
			fmt.Fprintf(&b, "\n    %s", c.wrap(sanitize(card.Text), 4))
		}
		for i, choice := range card.Choices {
			fmt.Fprintf(&b, "\n    %d) %s", i+1, sanitize(choice))
		}
		if m.Status == "open" {
			fmt.Fprintf(&b, "\n    answer with /answer %d <text>", card.N)
		} else {
			fmt.Fprintf(&b, "\n    %s", m.Status)
		}
	case chat.KindNotification:
		fmt.Fprintf(&b, "%s  %s: %s\n    %s", c.bold("NOTIFICATION"), sanitize(card.Assignment), sanitize(card.Title),
			c.wrap(sanitize(card.Text), 4))
	default:
		fmt.Fprintf(&b, "%s  %s", c.bold("REPORT"), sanitize(card.Title))
		if card.Text != "" {
			fmt.Fprintf(&b, "\n    %s", c.wrap(sanitize(card.Text), 4))
		}
	}
	return b.String()
}

func (c *chatClient) details(m chat.Message) string {
	card := m.Card()
	args, _ := json.MarshalIndent(json.RawMessage(card.Arguments), "    ", "  ")
	return fmt.Sprintf("[%d] %s %s\n    arguments:\n    %s\n    sha256: %s", card.N, sanitize(card.Tool), sanitize(card.Resource),
		sanitize(string(args)), card.ArgumentsHash)
}

// emit writes one message as a JSON line, with text fields made safe to print.
func (c *chatClient) emit(m chat.Message) {
	kind := "message"
	if m.Kind == chat.KindApproval || m.Kind == chat.KindProposal || m.Kind == chat.KindQuestion || m.Kind == chat.KindNotification || m.Kind == chat.KindReport {
		kind = "card"
	}
	var body map[string]any
	json.Unmarshal(m.Body, &body)
	for k, v := range body {
		if s, ok := v.(string); ok {
			body[k] = sanitize(s)
		}
	}
	m.Body, _ = json.Marshal(body)
	c.record(map[string]any{"type": kind, "message": m})
}

func (c *chatClient) record(v any) {
	b, _ := json.Marshal(v)
	c.print(string(b))
}

func (c *chatClient) print(s string) {
	fmt.Fprintln(c.out, s)
}

func (c *chatClient) bold(s string) string {
	if !c.color {
		return s
	}
	return "\x1b[1m" + s + "\x1b[0m"
}

func (c *chatClient) dim(s string) string {
	if !c.color {
		return s
	}
	return "\x1b[2m" + s + "\x1b[0m"
}

func (c *chatClient) clip(s string, n int) string {
	if n < 10 || len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// wrap breaks text to the terminal width, indenting continuation lines.
func (c *chatClient) wrap(s string, indent int) string {
	limit := c.width - indent
	if limit < 20 {
		return s
	}
	pad := strings.Repeat(" ", indent)
	var out []string
	for _, para := range strings.Split(s, "\n") {
		line := ""
		for _, word := range strings.Fields(para) {
			if line != "" && len([]rune(line))+1+len([]rune(word)) > limit {
				out = append(out, line)
				line = word
				continue
			}
			if line != "" {
				line += " "
			}
			line += word
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n"+pad)
}

// sanitize keeps printable text, newlines and tabs. Escape sequences and other control
// characters are dropped, so text from a model, tool or project cannot move the cursor,
// recolour or hide text, or forge a prompt.
func sanitize(s string) string {
	var b strings.Builder
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r == 0x1b:
			i = skipEscape(rs, i)
		case r == 0x9b || r == 0x9d:
			for i+1 < len(rs) && !(rs[i+1] >= 0x40 && rs[i+1] <= 0x7e) && rs[i+1] != 0x07 {
				i++
			}
			i++
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		case !unicode.IsPrint(r) && !unicode.IsSpace(r):
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// skipEscape returns the index of the last rune of the escape sequence starting at i.
func skipEscape(rs []rune, i int) int {
	if i+1 >= len(rs) {
		return i
	}
	switch rs[i+1] {
	case '[':
		j := i + 2
		for j < len(rs) && !(rs[j] >= 0x40 && rs[j] <= 0x7e) {
			j++
		}
		return min(j, len(rs)-1)
	case ']', 'P', '^', '_':
		j := i + 2
		for j < len(rs) {
			if rs[j] == 0x07 {
				return j
			}
			if rs[j] == 0x1b && j+1 < len(rs) && rs[j+1] == '\\' {
				return j + 1
			}
			j++
		}
		return len(rs) - 1
	}
	return i + 1
}

// stateDir is where the chat keeps its input history, under the user's state directory.
func stateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "agentd")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state", "agentd")
}

// fileHistory is the interactive input history, kept across sessions unless
// AGENTD_CHAT_HISTORY=off.
type fileHistory struct {
	path  string
	lines []string
}

const historyLimit = 500

func loadHistory() *fileHistory {
	h := &fileHistory{}
	if os.Getenv("AGENTD_CHAT_HISTORY") == "off" || stateDir() == "" {
		return h
	}
	h.path = filepath.Join(stateDir(), "chat_history")
	if f, err := os.Open(h.path); err == nil {
		s := bufio.NewScanner(f)
		for s.Scan() {
			h.lines = append(h.lines, s.Text())
		}
		f.Close()
	}
	if len(h.lines) > historyLimit {
		h.lines = h.lines[len(h.lines)-historyLimit:]
	}
	return h
}

func (h *fileHistory) Add(line string) {
	if strings.TrimSpace(line) == "" || strings.Contains(line, "\n") {
		return
	}
	h.lines = append(h.lines, line)
	if h.path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(h.path), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(h.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}

func (h *fileHistory) Len() int { return len(h.lines) }

// At returns the idx-th most recent line.
func (h *fileHistory) At(idx int) string { return h.lines[len(h.lines)-1-idx] }
