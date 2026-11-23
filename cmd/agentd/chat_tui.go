package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	xansi "github.com/charmbracelet/x/ansi"

	"agentd/internal/chat"
	"agentd/internal/system"
)

// The new interactive chat (agentd chat -ui new). It draws inline: finished messages are
// printed into the terminal's own scrollback, and only the work indicator, the composer
// and the status line are redrawn. Everything from a model, a tool or a project still
// passes through sanitize before it reaches the terminal.

const (
	tuiMaxWidth    = 100
	tuiMaxRows     = 8
	tuiMenuRows    = 6
	tuiIdlePoll    = 400 * time.Millisecond
	tuiBusyPoll    = 150 * time.Millisecond
	tuiStatusEvery = time.Second
	tuiQuitWindow  = 2 * time.Second
	tuiPartialRows = 12
	tuiStartWait   = 8 * time.Second
)

const tuiDownHint = "No server is running, so nothing will answer. Press ctrl+s to start one in the background."

// serveCommand is this executable with the same global flags and the serve command; the
// chat runs it when the owner asks it to start the server.
var serveCommand []string

type tuiStatus struct {
	jobs, waiting int
	halted        bool
	alive         bool
	model         string
}

type tuiTheme struct {
	accent, muted, warn, bad, good, violet lipgloss.Style
	bold                                   lipgloss.Style
}

func newTheme(color, dark bool) tuiTheme {
	plain := lipgloss.NewStyle()
	if !color {
		return tuiTheme{accent: plain, muted: plain, warn: plain, bad: plain, good: plain, violet: plain, bold: plain}
	}
	pick := func(light, darkC string) lipgloss.Style {
		if dark {
			return plain.Foreground(lipgloss.Color(darkC))
		}
		return plain.Foreground(lipgloss.Color(light))
	}
	th := tuiTheme{
		accent: pick("#17695a", "#5cc4a6"),
		muted:  pick("#6b6b66", "#8c8c86"),
		warn:   pick("#9a5b00", "#e0a84a"),
		bad:    pick("#b3261e", "#f2867d"),
		good:   pick("#1f7a3d", "#7dcf8e"),
		violet: pick("#6247aa", "#b3a1f0"),
		bold:   plain.Bold(true),
	}
	return th
}

type tui struct {
	ctx   context.Context
	c     *chatClient
	theme tuiTheme
	in    textarea.Model
	spin  spinner.Model
	width int

	hist   *fileHistory
	histAt int
	stash  string
	menu   int

	turn     *chat.Message
	started  time.Time
	stopping bool
	seen     map[string]bool
	sent     map[string]bool

	// partial is reply text streamed so far for partialTask, shown until the stored reply arrives.
	partial     string
	partialTask string
	deltas      chan chat.Delta
	// open holds the cards still waiting for a decision, to notice when one is decided.
	open  map[int64]chat.Message
	focus bool
	// startServer starts a background server and says where its log is.
	startServer func() (string, error)
	starting    bool

	status     tuiStatus
	statusAt   time.Time
	hint       string
	quitArmed  time.Time
	warnedDown bool
	quitting   bool

	pending  []string
	printing bool
	// printed, when set, receives every block sent to the scrollback.
	printed func(string)
	// drawn, when set, receives each frame of the live region.
	drawn func(string)
}

type (
	tuiLoaded struct {
		away   []chat.Message
		last   int64
		status tuiStatus
		err    error
	}
	tuiPolled struct {
		msgs   []chat.Message
		tools  []toolCall
		cards  []chat.Message
		status *tuiStatus
		err    error
	}
	tuiDelta   chat.Delta
	tuiStarted struct {
		log string
		err error
	}
	tuiStartCheck struct{}
	tuiPosted     struct {
		m   chat.Message
		err error
	}
	tuiEdited struct {
		n    int
		text string
		err  error
	}
	tuiTick    struct{}
	tuiPrinted struct{}
)

func (c *chatClient) tui(ctx context.Context, in *os.File) error {
	t := newTUI(ctx, c, c.color && lipgloss.HasDarkBackground())
	go t.stream()
	_, err := tea.NewProgram(t, tea.WithContext(ctx), tea.WithInput(in), tea.WithOutput(c.out)).Run()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return nil
	}
	return err
}

func newTUI(ctx context.Context, c *chatClient, dark bool) *tui {
	t := &tui{ctx: ctx, c: c, theme: newTheme(c.color, dark), hist: loadHistory(), histAt: -1,
		seen: map[string]bool{}, sent: map[string]bool{}, open: map[int64]chat.Message{},
		deltas: make(chan chat.Delta, 256), startServer: startServer}
	in := textarea.New()
	in.ShowLineNumbers = false
	in.CharLimit = 0
	in.Placeholder = "Message Baseet"
	in.SetPromptFunc(2, func(line int) string {
		if line == 0 {
			return "> "
		}
		return "  "
	})
	in.FocusedStyle.CursorLine = lipgloss.NewStyle()
	in.FocusedStyle.Prompt = t.theme.accent
	in.FocusedStyle.Placeholder = t.theme.muted
	in.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("ctrl+j", "alt+enter"))
	in.Focus()
	t.in = in
	t.spin = spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(t.theme.accent))
	t.resize(c.width)
	return t
}

func (t *tui) resize(width int) {
	t.width = max(min(width, tuiMaxWidth), 30)
	t.c.width = t.width
	t.in.SetWidth(t.width - 4)
	t.fit()
}

// fit sizes the composer to its text, up to tuiMaxRows.
func (t *tui) fit() {
	per := max(t.width-6, 10)
	rows := 0
	for _, line := range strings.Split(t.in.Value(), "\n") {
		rows += max(1, (lipgloss.Width(line)+per)/per)
	}
	t.in.SetHeight(max(1, min(rows, tuiMaxRows)))
}

func (t *tui) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, t.spin.Tick, t.load, t.nextDelta)
}

func (t *tui) nextDelta() tea.Msg {
	select {
	case d := <-t.deltas:
		return tuiDelta(d)
	case <-t.ctx.Done():
		return nil
	}
}

// stream follows the server's event stream for reply text as it is written. Messages and
// cards still come from the database, so the chat works the same without it.
func (t *tui) stream() {
	for t.ctx.Err() == nil {
		t.streamOnce()
		select {
		case <-t.ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

func (t *tui) streamOnce() {
	l, alive, err := system.Alive(t.ctx, t.c.app.DB, time.Now())
	if err != nil || !alive || l.Listen == "" {
		return
	}
	token := os.Getenv("AGENTD_API_TOKEN")
	if token == "" && l.TokenFile != "" {
		body, _ := os.ReadFile(l.TokenFile)
		token = strings.TrimSpace(string(body))
	}
	if token == "" {
		return
	}
	url := fmt.Sprintf("http://%s/api/conversations/%s/stream?after=%d", l.Listen, t.c.conv, int64(math.MaxInt64))
	req, err := http.NewRequestWithContext(t.ctx, http.MethodGet, url, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := (&http.Client{Transport: &http.Transport{}}).Do(req)
	if err != nil {
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return
	}
	lines := bufio.NewScanner(res.Body)
	lines.Buffer(make([]byte, 64<<10), 1<<20)
	event := ""
	for lines.Scan() {
		line := lines.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: ") && (event == "delta" || event == "turn"):
			var d chat.Delta
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &d) != nil || d.TaskID == "" || (event == "turn" && !d.Restarted) {
				continue
			}
			select {
			case t.deltas <- d:
			case <-t.ctx.Done():
				return
			}
		}
	}
}

// startServer runs `agentd serve` in the background, detached from this terminal.
func startServer() (string, error) {
	if len(serveCommand) == 0 || stateDir() == "" {
		return "", errors.New("cannot start the server from here; run `agentd serve` in another terminal")
	}
	if err := os.MkdirAll(stateDir(), 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(stateDir(), "serve.log")
	log, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	defer log.Close()
	cmd := exec.Command(serveCommand[0], serveCommand[1:]...)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	return path, cmd.Process.Release()
}

func (t *tui) load() tea.Msg {
	var l tuiLoaded
	if l.status, l.err = t.c.status(t.ctx); l.err != nil {
		return l
	}
	if l.away, l.err = t.c.app.Chat.Away(t.ctx, t.c.conv, chatChannel); l.err != nil {
		return l
	}
	l.err = t.c.app.DB.QueryRowContext(t.ctx, `SELECT COALESCE(MAX(seq), 0) FROM conversation_messages WHERE conversation_id = ?`,
		t.c.conv).Scan(&l.last)
	return l
}

func (c *chatClient) status(ctx context.Context) (tuiStatus, error) {
	var s tuiStatus
	if err := c.app.DB.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM assignments WHERE builtin IS NULL AND status IN ('active', 'paused')),
		(SELECT COUNT(*) FROM approvals WHERE status = 'pending')
		+ (SELECT COUNT(*) FROM actions WHERE status = 'needs_reconciliation'),
		COALESCE((SELECT model FROM conversations WHERE id = ?), '')`, c.conv).Scan(&s.jobs, &s.waiting, &s.model); err != nil {
		return s, err
	}
	st, err := system.Get(ctx, c.app.DB)
	if err != nil {
		return s, err
	}
	s.halted = st.Halted
	s.alive, err = c.alive(ctx)
	return s, err
}

func (t *tui) poll(after int64, turn string, withStatus bool) tea.Cmd {
	cards := make([]chat.Message, 0, len(t.open))
	for _, m := range t.open {
		cards = append(cards, m)
	}
	return func() tea.Msg {
		var p tuiPolled
		if p.msgs, p.err = t.c.app.Chat.Messages(t.ctx, t.c.conv, after, 200); p.err != nil {
			return p
		}
		if turn != "" {
			if p.tools, p.err = t.c.toolCalls(t.ctx, turn); p.err != nil {
				return p
			}
		}
		if withStatus {
			st, err := t.c.status(t.ctx)
			if p.status, p.err = &st, err; err == nil && len(cards) > 0 {
				p.cards, p.err = cards, t.c.app.Chat.Live(t.ctx, cards)
			}
		}
		if len(p.msgs) > 0 {
			t.c.app.Chat.MarkRead(t.ctx, t.c.conv, chatChannel, p.msgs[len(p.msgs)-1].Seq)
		}
		return p
	}
}

func (t *tui) tick() tea.Cmd {
	every := tuiIdlePoll
	if t.turn != nil {
		every = tuiBusyPoll
	}
	return tea.Tick(every, func(time.Time) tea.Msg { return tuiTick{} })
}

func (t *tui) post(text string) tea.Cmd {
	return func() tea.Msg {
		id := make([]byte, 8)
		rand.Read(id)
		m, err := t.c.app.Chat.Post(t.ctx, chat.Post{ConversationID: t.c.conv, Channel: chatChannel,
			ClientMsgID: hex.EncodeToString(id), Text: text})
		return tuiPosted{m: m, err: err}
	}
}

// emit queues a block for the scrollback. Blocks are printed one at a time, in order.
func (t *tui) emit(s string) { t.pending = append(t.pending, s) }

func (t *tui) flush() tea.Cmd {
	if t.printing {
		return nil
	}
	if len(t.pending) == 0 {
		if t.quitting {
			return tea.Quit
		}
		return nil
	}
	// A line wider than the terminal would wrap and throw off the redraw below it.
	block := xansi.Wrap(strings.Join(t.pending, "\n"), t.width-1, "")
	t.pending = nil
	t.printing = true
	if t.printed != nil {
		t.printed(block)
	}
	return tea.Sequence(tea.Println(block), func() tea.Msg { return tuiPrinted{} })
}

func (t *tui) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := t.update(msg)
	return t, tea.Batch(cmd, t.flush())
}

func (t *tui) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		t.resize(msg.Width)
		return nil
	case tuiPrinted:
		t.printing = false
		return nil
	case spinner.TickMsg:
		var cmd tea.Cmd
		t.spin, cmd = t.spin.Update(msg)
		return cmd
	case tuiLoaded:
		return t.loaded(msg)
	case tuiTick:
		t.c.mu.Lock()
		after := t.c.after
		t.c.mu.Unlock()
		turn := ""
		if t.turn != nil {
			turn = t.turn.TurnTaskID
		}
		withStatus := time.Since(t.statusAt) >= tuiStatusEvery
		if withStatus {
			t.statusAt = time.Now()
		}
		return t.poll(after, turn, withStatus)
	case tuiPolled:
		t.polled(msg)
		return t.tick()
	case tuiPosted:
		if msg.err != nil {
			t.emit(t.theme.bad.Render("✗ " + sanitize(msg.err.Error())))
			return nil
		}
		t.sent[msg.m.ID] = true
		sent := msg.m
		t.turn, t.started, t.stopping, t.seen = &sent, time.Now(), false, map[string]bool{}
		return nil
	case tuiDelta:
		switch {
		case msg.Restarted && msg.TaskID == t.partialTask:
			t.partial = ""
		case !msg.Restarted && msg.TaskID != t.partialTask:
			t.partialTask, t.partial = msg.TaskID, msg.Text
		case !msg.Restarted:
			t.partial += msg.Text
		}
		return t.nextDelta
	case tuiStarted:
		if msg.err != nil {
			t.starting = false
			t.emit(t.theme.bad.Render("✗ " + t.c.wrap(sanitize(msg.err.Error()), 2)))
			return nil
		}
		t.emit(t.theme.muted.Render("· Starting agentd in the background. Its log is " + msg.log))
		return tea.Tick(tuiStartWait, func(time.Time) tea.Msg { return tuiStartCheck{} })
	case tuiStartCheck:
		if t.starting && !t.status.alive {
			t.emit(t.theme.bad.Render("✗ " + t.c.wrap("The server did not start. Its log says why; a common cause is that no model is set (agentd -model ... chat).", 2)))
		}
		t.starting = false
		return nil
	case tuiEdited:
		if msg.err != nil {
			t.emit(t.theme.bad.Render("✗ " + sanitize(msg.err.Error())))
			return nil
		}
		if msg.text == "" {
			t.emit(t.theme.muted.Render("· Nothing changed."))
			return nil
		}
		return t.post(fmt.Sprintf("/edit %d %s", msg.n, msg.text))
	case tea.KeyMsg:
		return t.key(msg)
	}
	var cmd tea.Cmd
	t.in, cmd = t.in.Update(msg)
	return cmd
}

func (t *tui) loaded(l tuiLoaded) tea.Cmd {
	if l.err != nil {
		t.emit(t.theme.bad.Render("✗ " + sanitize(l.err.Error())))
		t.quitting = true
		return nil
	}
	t.status, t.statusAt = l.status, time.Now()
	t.emit(t.theme.accent.Bold(true).Render("Baseet") + t.theme.muted.Render(" · "+plural(l.status.jobs, "job")+" · / for commands"))
	if !l.status.alive {
		t.emit(t.theme.warn.Render("· " + t.c.wrap(tuiDownHint, 2)))
		t.warnedDown = true
	}
	if len(l.away) > 0 {
		t.emit("\n" + t.theme.muted.Render(fmt.Sprintf("While you were away (%d):", len(l.away))))
		for _, m := range l.away {
			t.show(m)
			t.emit(t.render(m))
		}
	} else if l.last == 0 {
		t.emit(t.theme.muted.Render("  Tell me what you want handled, for example: chase my airline refund every Friday."))
	}
	t.c.mu.Lock()
	t.c.after = max(t.c.after, l.last)
	t.c.mu.Unlock()
	t.c.markRead(t.ctx)
	return t.tick()
}

func (t *tui) polled(p tuiPolled) {
	if p.err != nil {
		if t.ctx.Err() == nil {
			t.hint = "cannot read the conversation: " + sanitize(p.err.Error())
		}
		return
	}
	if p.status != nil {
		t.status = *p.status
		if t.status.alive {
			t.starting = false
		}
	}
	for _, m := range p.cards {
		if was, ok := t.open[m.Seq]; ok && m.Status != was.Status {
			delete(t.open, m.Seq)
			t.emit(t.render(m))
			t.c.mu.Lock()
			if t.c.last != nil && t.c.last.Seq == m.Seq {
				t.c.last = nil
			}
			t.c.mu.Unlock()
		}
	}
	for _, call := range p.tools {
		if !t.seen[call.id] {
			t.seen[call.id] = true
			t.emit(t.theme.muted.Render("  ⏵ " + sanitize(call.words())))
		}
	}
	for _, m := range p.msgs {
		t.show(m)
		if !t.sent[m.ID] {
			t.emit("\n" + t.render(m))
		}
		if m.TurnTaskID != "" && m.TurnTaskID == t.partialTask && m.Author != chat.Owner {
			t.partial, t.partialTask = "", ""
		}
		if t.turn != nil {
			if done, _ := settles(*t.turn, m); done {
				if t.turn.TurnTaskID == t.partialTask {
					t.partial, t.partialTask = "", ""
				}
				t.turn = nil
			}
		}
	}
	if t.focused() == nil {
		t.focus = false
	}
}

// show records a message as shown: how far the conversation has been read, which open
// card came last, and which cards still wait for a decision.
func (t *tui) show(m chat.Message) {
	t.c.track(m)
	if (m.Kind == chat.KindApproval || m.Kind == chat.KindProposal || m.Kind == chat.KindQuestion) && (m.Status == "pending" || m.Status == "open") {
		t.open[m.Seq] = m
	}
}

// focused is the card Tab gives the focus to: the newest one still waiting for a decision.
// The focus line names it, so it need not be the last thing shown.
func (t *tui) focused() *chat.Message {
	var newest *chat.Message
	for _, m := range t.open {
		if newest == nil || m.Seq > newest.Seq {
			card := m
			newest = &card
		}
	}
	return newest
}

// focusKey handles a key while a card has the focus. It reports whether the key was used;
// any other key gives the focus back to the composer and is handled there.
func (t *tui) focusKey(msg tea.KeyMsg) (tea.Cmd, bool) {
	card := t.focused()
	t.focus = false
	if card == nil || msg.Paste {
		return nil, false
	}
	k := msg.String()
	switch {
	case k == "esc" || k == "tab" || k == "ctrl+c":
		return nil, true
	case card.Kind == chat.KindQuestion:
		choices := card.Card().Choices
		if len(k) == 1 && k[0] >= '1' && int(k[0]-'0') <= len(choices) {
			answer := fmt.Sprintf("/answer %d %s", card.Card().N, choices[k[0]-'1'])
			t.emit("\n" + t.theme.muted.Render("> "+sanitize(answer)))
			return t.post(answer), true
		}
	case len(k) == 1 && strings.Contains("arde", k):
		if cmd, ok := t.decide(k, card); ok {
			return cmd, true
		}
	}
	return nil, false
}

func (t *tui) key(msg tea.KeyMsg) tea.Cmd {
	t.hint = ""
	if t.focus {
		if cmd, used := t.focusKey(msg); used {
			return cmd
		}
	}
	draft := t.in.Value()
	menu := t.matches()
	switch msg.String() {
	case "ctrl+s":
		if !t.status.alive && !t.starting {
			t.starting = true
			return func() tea.Msg {
				log, err := t.startServer()
				return tuiStarted{log: log, err: err}
			}
		}
		return nil
	case "ctrl+c":
		switch {
		case draft != "":
			t.clear()
		case t.running():
			return t.stop()
		case time.Since(t.quitArmed) < tuiQuitWindow:
			return t.quit()
		default:
			t.quitArmed = time.Now()
			t.hint = "press ctrl+c again to leave"
		}
		return nil
	case "ctrl+d":
		if draft == "" {
			return t.quit()
		}
	case "esc":
		if len(menu) > 0 {
			t.clear()
			return nil
		}
		if t.running() {
			return t.stop()
		}
		return nil
	case "tab":
		if len(menu) > 0 {
			t.complete(menu)
		} else if draft == "" && t.focused() != nil {
			t.focus = true
		}
		return nil
	case "up", "down":
		step := map[string]int{"up": -1, "down": 1}[msg.String()]
		if len(menu) > 0 {
			t.menu = (min(t.menu, len(menu)-1) + step + len(menu)) % len(menu)
			return nil
		}
		if t.history(step, draft) {
			return nil
		}
	case "enter":
		if strings.HasSuffix(draft, `\`) {
			t.in.SetValue(strings.TrimSuffix(draft, `\`) + "\n")
			t.fit()
			return nil
		}
		if len(menu) > 0 && "/"+menu[min(t.menu, len(menu)-1)].Name != strings.TrimSpace(draft) {
			t.complete(menu)
			return nil
		}
		return t.submit(strings.TrimSpace(draft))
	}
	var cmd tea.Cmd
	t.in, cmd = t.in.Update(msg)
	t.fit()
	if t.in.Value() != draft {
		t.menu, t.histAt = 0, -1
	}
	return cmd
}

func (t *tui) running() bool { return t.turn != nil && t.turn.TurnTaskID != "" }

func (t *tui) clear() {
	t.in.Reset()
	t.menu, t.histAt = 0, -1
	t.fit()
}

func (t *tui) stop() tea.Cmd {
	if t.stopping {
		return nil
	}
	t.stopping = true
	return func() tea.Msg {
		if err := t.c.app.Chat.Cancel(t.ctx, t.c.conv, "owner:"+chatChannel); err != nil && !errors.Is(err, chat.ErrNoTurn) {
			return tuiPosted{err: err}
		}
		return nil
	}
}

func (t *tui) quit() tea.Cmd {
	t.c.markRead(t.ctx)
	marker := filepath.Join(stateDir(), "chat_exit_noted")
	if _, err := os.Stat(marker); err != nil && stateDir() != "" {
		t.emit(t.theme.muted.Render("· Your projects keep running; leaving the chat stops nothing."))
		if os.MkdirAll(stateDir(), 0o700) == nil {
			os.WriteFile(marker, nil, 0o600)
		}
	}
	t.quitting = true
	return nil
}

// history moves through earlier input when the cursor is on the first or last line.
func (t *tui) history(step int, draft string) bool {
	if step < 0 && t.in.Line() == 0 && (draft == "" || t.histAt >= 0) && t.histAt+1 < t.hist.Len() {
		if t.histAt < 0 {
			t.stash = draft
		}
		t.histAt++
	} else if step > 0 && t.histAt >= 0 && t.in.Line() == t.in.LineCount()-1 {
		t.histAt--
	} else {
		return false
	}
	if t.histAt < 0 {
		t.in.SetValue(t.stash)
	} else {
		t.in.SetValue(t.hist.At(t.histAt))
	}
	t.in.CursorEnd()
	t.fit()
	return true
}

var tuiLocal = []chat.CommandHelp{
	{Name: "clear", Usage: "/clear", Help: "clear the screen"},
	{Name: "quit", Usage: "/quit", Help: "leave the chat; your work keeps running"},
}

// matches lists the slash commands the draft could become, while it is still one word.
func (t *tui) matches() []chat.CommandHelp {
	draft := t.in.Value()
	if !strings.HasPrefix(draft, "/") || strings.ContainsAny(draft, " \n\t") {
		return nil
	}
	var out []chat.CommandHelp
	for _, c := range append(chat.Commands(), tuiLocal...) {
		if strings.HasPrefix("/"+c.Name, draft) {
			out = append(out, c)
		}
	}
	return out
}

func (t *tui) complete(menu []chat.CommandHelp) {
	pick := menu[min(t.menu, len(menu)-1)]
	value := "/" + pick.Name
	if strings.Contains(pick.Usage, " ") {
		value += " "
	}
	t.in.SetValue(value)
	t.in.CursorEnd()
	t.menu = 0
}

func (t *tui) submit(text string) tea.Cmd {
	t.clear()
	if text == "" {
		return nil
	}
	t.hist.Add(text)
	switch text {
	case "/quit", "/exit":
		return t.quit()
	case "/clear":
		return tea.ClearScreen
	}
	t.c.mu.Lock()
	last := t.c.last
	t.c.mu.Unlock()
	if cmd, ok := t.decide(text, last); ok {
		return cmd
	}
	t.emit("\n" + t.theme.muted.Render("> "+strings.ReplaceAll(xansi.Wrap(sanitize(text), t.width-3, ""), "\n", "\n  ")))
	if !t.status.alive && !strings.HasPrefix(text, "/") && !t.warnedDown {
		t.warnedDown = true
		t.emit(t.theme.warn.Render("· " + t.c.wrap(tuiDownHint, 2)))
		t.emit(t.theme.muted.Render("· Your message is saved and will be answered when agentd starts."))
	}
	return t.post(text)
}

// decide handles the one-letter answers to the card shown last. A letter is only a
// decision when it is the whole message and the card is the last thing shown.
func (t *tui) decide(text string, last *chat.Message) (tea.Cmd, bool) {
	if len(text) != 1 {
		return nil, false
	}
	if last == nil || last.Kind == chat.KindQuestion {
		return nil, false
	}
	n := last.Card().N
	switch text {
	case "d":
		t.emit(t.theme.muted.Render(t.c.details(*last)))
		return nil, true
	case "a":
		return t.post(fmt.Sprintf("/approve %d", n)), true
	case "r":
		return t.post(fmt.Sprintf("/reject %d", n)), true
	case "e":
		if last.Kind != chat.KindProposal {
			return nil, false
		}
		run := &editorRun{card: last.Card()}
		return tea.Exec(run, func(error) tea.Msg { return tuiEdited{n: n, text: run.text, err: run.err} }), true
	}
	return nil, false
}

// editorRun opens a proposal in the owner's editor while the chat lets go of the terminal.
type editorRun struct {
	card chat.Card
	text string
	err  error
}

func (e *editorRun) Run() error {
	e.text, e.err = editInEditor(e.card)
	return nil
}
func (e *editorRun) SetStdin(io.Reader)  {}
func (e *editorRun) SetStdout(io.Writer) {}
func (e *editorRun) SetStderr(io.Writer) {}

func (t *tui) View() string {
	view := t.view()
	if t.drawn != nil {
		t.drawn(view)
	}
	return view
}

func (t *tui) view() string {
	if t.quitting {
		return ""
	}
	rows := []string{""}
	if t.partial != "" {
		lines := strings.Split(t.markdown(t.partial), "\n")
		if len(lines) > tuiPartialRows {
			lines = append([]string{t.theme.muted.Render("…")}, lines[len(lines)-tuiPartialRows:]...)
		}
		rows = append(rows, t.theme.accent.Render("●")+" "+strings.Join(lines, "\n  "), "")
	}
	if t.running() {
		state := "Working"
		if t.stopping {
			state = "Stopping"
		} else if !t.status.alive {
			state = "Saved; waiting for the server"
		}
		rows = append(rows, " "+t.spin.View()+" "+state+t.theme.muted.Render(
			fmt.Sprintf(" · %s · esc to stop", time.Since(t.started).Round(time.Second))), "")
	}
	if card := t.focused(); t.focus && card != nil {
		keys := "a approve · r reject · d details · esc back"
		switch {
		case card.Kind == chat.KindProposal:
			keys = "a approve · r reject · e edit · d details · esc back"
		case card.Kind == chat.KindQuestion && len(card.Card().Choices) > 0:
			keys = fmt.Sprintf("1 to %d to answer · esc back", len(card.Card().Choices))
		case card.Kind == chat.KindQuestion:
			keys = fmt.Sprintf("esc, then /answer %d <text>", card.Card().N)
		}
		rows = append(rows, xansi.Truncate(" "+t.theme.warn.Bold(true).Render("▸ "+t.cardName(*card))+t.theme.muted.Render("  "+keys), t.width-1, "…"))
	}
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1).Width(t.width - 2)
	box = box.BorderForeground(t.theme.muted.GetForeground())
	rows = append(rows, box.Render(t.in.View()))
	if menu := t.matches(); len(menu) > 0 {
		at := min(t.menu, len(menu)-1)
		first := max(0, min(at-tuiMenuRows+1, len(menu)-tuiMenuRows))
		for i := first; i < len(menu) && i < first+tuiMenuRows; i++ {
			line := fmt.Sprintf("  %-34s %s", menu[i].Usage, menu[i].Help)
			line = t.c.clip(line, t.width-1)
			if i == at {
				rows = append(rows, t.theme.accent.Render(line))
			} else {
				rows = append(rows, t.theme.muted.Render(line))
			}
		}
	}
	rows = append(rows, t.statusLine())
	return strings.Join(rows, "\n")
}

func (t *tui) statusLine() string {
	if t.hint != "" {
		return "  " + t.theme.warn.Render(t.c.clip(t.hint, t.width-3))
	}
	model := "model: automatic"
	if t.status.model != "" {
		model = "model: " + sanitize(t.status.model)
	}
	parts := []string{t.theme.muted.Render(model)}
	if t.status.waiting > 0 {
		parts = append(parts, t.theme.warn.Render(fmt.Sprintf("%d waiting on you", t.status.waiting)))
	}
	if t.status.halted {
		parts = append(parts, t.theme.bad.Render("emergency stop ON"))
	}
	switch {
	case t.status.alive:
	case t.starting:
		parts = append(parts, t.theme.warn.Render("starting the server"))
	default:
		parts = append(parts, t.theme.bad.Render("server not running · ctrl+s to start it"))
	}
	left := "  " + strings.Join(parts, t.theme.muted.Render(" · "))
	right := t.theme.muted.Render("/ commands · ctrl+j new line")
	if card := t.focused(); card != nil && !t.focus && t.in.Value() == "" {
		right = t.theme.warn.Render("tab to decide " + t.cardName(*card))
	}
	if gap := t.width - 1 - lipgloss.Width(left) - lipgloss.Width(right); gap >= 2 {
		return left + strings.Repeat(" ", gap) + right
	}
	return left
}

func remaining(d time.Duration) string {
	if d >= time.Hour {
		return fmt.Sprintf("%dh", int(d.Round(time.Hour).Hours()))
	}
	return fmt.Sprintf("%dm", max(int(d.Round(time.Minute).Minutes()), 0))
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// render draws one message for the scrollback.
func (t *tui) render(m chat.Message) string {
	switch m.Kind {
	case chat.KindApproval, chat.KindProposal, chat.KindQuestion, chat.KindNotification, chat.KindReport:
		return t.card(m)
	case chat.KindError:
		return t.theme.bad.Render("✗ " + t.c.wrap(sanitize(m.Text()), 2))
	case chat.KindNotice:
		return t.theme.muted.Render("· " + t.c.wrap(sanitize(m.Text()), 2))
	case chat.KindCommandResult:
		return "  " + strings.ReplaceAll(strings.TrimRight(sanitize(m.Text()), "\n"), "\n", "\n  ")
	}
	if m.Author == chat.Owner {
		from := ""
		if m.Channel != "" && m.Channel != chatChannel {
			from = " (" + sanitize(m.Channel) + ")"
		}
		return t.theme.muted.Render("> " + strings.ReplaceAll(xansi.Wrap(sanitize(m.Text()), t.width-3, ""), "\n", "\n  ") + from)
	}
	return t.theme.accent.Render("●") + " " + strings.ReplaceAll(t.markdown(m.Text()), "\n", "\n  ")
}

var (
	mdCode    = regexp.MustCompile("`([^`\n]+)`")
	mdBold    = regexp.MustCompile(`\*\*([^*\n]+)\*\*|__([^_\n]+)__`)
	mdItalic  = regexp.MustCompile(`(^|[\s(])[*_]([^*_\s][^*_\n]*)[*_]`)
	mdLink    = regexp.MustCompile(`\[([^\]\n]+)\]\(([^)\s]+)\)`)
	mdHeading = regexp.MustCompile(`^#{1,6}\s+(.*)$`)
	mdItem    = regexp.MustCompile(`^(\s*)([-*+]|\d+[.)])\s+(.*)$`)
	mdRule    = regexp.MustCompile(`^\s*([-*_]\s*){3,}$`)
	mdDivider = regexp.MustCompile(`^\s*\|?[\s:|-]+\|?\s*$`)
)

// markdown renders the Markdown a model usually writes (headings, lists, quotes, code,
// tables, emphasis and links) for the terminal. It is deliberately small: a full Markdown
// library doubled the size of the binary.
func (t *tui) markdown(text string) string {
	width := t.width - 3
	var out []string
	lines := strings.Split(strings.TrimSpace(sanitize(text)), "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(strings.ReplaceAll(lines[i], "\t", "    "), " ")
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "```"):
			for i++; i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```"); i++ {
				code := xansi.Hardwrap(strings.ReplaceAll(lines[i], "\t", "    "), width-2, true)
				for _, part := range strings.Split(code, "\n") {
					out = append(out, t.theme.muted.Render("│ ")+t.theme.accent.Render(part))
				}
			}
		case strings.HasPrefix(trimmed, "|") && i+1 < len(lines) && strings.Contains(lines[i+1], "-") && mdDivider.MatchString(lines[i+1]):
			var rows [][]string
			for ; i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "|"); i++ {
				if !mdDivider.MatchString(lines[i]) {
					rows = append(rows, strings.Split(strings.Trim(strings.TrimSpace(lines[i]), "|"), "|"))
				}
			}
			i--
			out = append(out, t.table(rows, width)...)
		case mdHeading.MatchString(trimmed):
			out = append(out, xansi.Wrap(t.theme.accent.Bold(true).Render(t.inline(mdHeading.FindStringSubmatch(trimmed)[1])), width, ""))
		case mdRule.MatchString(line):
			out = append(out, t.theme.muted.Render(strings.Repeat("─", min(width, 40))))
		case strings.HasPrefix(trimmed, ">"):
			quote := xansi.Wrap(t.inline(strings.TrimSpace(strings.TrimPrefix(trimmed, ">"))), width-2, "")
			out = append(out, t.theme.muted.Render("│ "+strings.ReplaceAll(quote, "\n", "\n│ ")))
		case mdItem.MatchString(line):
			m := mdItem.FindStringSubmatch(line)
			indent, marker := min(len(m[1]), 8), m[2]
			if len(marker) == 1 {
				marker = "•"
			}
			pad := strings.Repeat(" ", indent+lipgloss.Width(marker)+1)
			item := xansi.Wrap(t.inline(m[3]), max(width-len(pad), 10), "")
			out = append(out, strings.Repeat(" ", indent)+t.theme.accent.Render(marker)+" "+strings.ReplaceAll(item, "\n", "\n"+pad))
		default:
			out = append(out, xansi.Wrap(t.inline(trimmed), width, ""))
		}
	}
	return strings.Join(out, "\n")
}

// inline styles emphasis, code and links inside one line. Without colour the text keeps
// its backticks and loses only the emphasis marks.
func (t *tui) inline(s string) string {
	s = mdLink.ReplaceAllStringFunc(s, func(link string) string {
		m := mdLink.FindStringSubmatch(link)
		if m[1] == m[2] {
			return m[2]
		}
		return m[1] + " (" + m[2] + ")"
	})
	var spans []string
	s = mdCode.ReplaceAllStringFunc(s, func(code string) string {
		spans = append(spans, code)
		return fmt.Sprintf("\x00%d\x00", len(spans)-1)
	})
	s = mdBold.ReplaceAllStringFunc(s, func(b string) string {
		return t.theme.bold.Render(strings.Trim(b, "*_"))
	})
	s = mdItalic.ReplaceAllString(s, "$1$2")
	for i, code := range spans {
		shown := code
		if t.c.color {
			shown = t.theme.accent.Render(strings.Trim(code, "`"))
		}
		s = strings.Replace(s, fmt.Sprintf("\x00%d\x00", i), shown, 1)
	}
	return s
}

func (t *tui) table(rows [][]string, width int) []string {
	var widths []int
	for _, row := range rows {
		for i := range row {
			row[i] = t.inline(strings.TrimSpace(row[i]))
			if i == len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], lipgloss.Width(row[i]))
		}
	}
	var out []string
	for n, row := range rows {
		var cells []string
		for i, cell := range row {
			pad := strings.Repeat(" ", widths[i]-lipgloss.Width(cell))
			if n == 0 {
				cell = t.theme.bold.Render(cell)
			}
			cells = append(cells, cell+pad)
		}
		line := strings.TrimRight(strings.Join(cells, t.theme.muted.Render(" │ ")), " ")
		out = append(out, xansi.Hardwrap(line, width, true))
	}
	return out
}

var tuiKinds = map[string]string{
	chat.KindApproval: "Approval", chat.KindProposal: "Proposal", chat.KindQuestion: "Question",
	chat.KindNotification: "Notification", chat.KindReport: "Report",
}

func (t *tui) cardName(m chat.Message) string {
	if n := m.Card().N; n > 0 {
		return fmt.Sprintf("%s %d", tuiKinds[m.Kind], n)
	}
	return tuiKinds[m.Kind]
}

func (t *tui) card(m chat.Message) string {
	card := m.Card()
	tint := map[string]lipgloss.Style{chat.KindApproval: t.theme.warn, chat.KindProposal: t.theme.accent,
		chat.KindQuestion: t.theme.violet, chat.KindNotification: t.theme.muted, chat.KindReport: t.theme.good}[m.Kind]
	decidable := m.Kind == chat.KindApproval || m.Kind == chat.KindProposal || m.Kind == chat.KindQuestion
	open := decidable && (m.Status == "pending" || m.Status == "open")
	name := tuiKinds[m.Kind]
	if card.N > 0 {
		name += fmt.Sprintf(" %d", card.N)
	}
	title := sanitize(card.Title)
	if decidable && !open {
		return t.theme.muted.Render(t.c.clip(fmt.Sprintf("◦ %s · %s · %s", name, sanitize(m.Status), title), t.width-1))
	}
	head := tint.Bold(true).Render(name)
	if card.Assignment != "" {
		head += t.theme.muted.Render(" · " + sanitize(card.Assignment))
	}
	if card.ExpiresAt != nil && open {
		head += t.theme.muted.Render(" · expires in " + remaining(time.Until(*card.ExpiresAt)))
	}
	body := []string{head, t.theme.bold.Render(title)}
	if text := strings.TrimSpace(sanitize(card.Text)); text != "" {
		body = append(body, text)
	}
	for i, choice := range card.Choices {
		body = append(body, fmt.Sprintf("%d) %s", i+1, sanitize(choice)))
	}
	if open {
		keys := map[string]string{
			chat.KindApproval: "tab, then  a approve   r reject   d details",
			chat.KindProposal: "tab, then  a approve   r reject   e edit   d details",
			chat.KindQuestion: fmt.Sprintf("answer with /answer %d <text>", card.N),
		}[m.Kind]
		body = append(body, "", t.theme.muted.Render(keys))
	}
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(tint.GetForeground()).
		Padding(0, 1).Width(t.width - 3)
	return box.Render(strings.Join(body, "\n"))
}
