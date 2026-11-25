package telegram_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agentd/internal/app"
	"agentd/internal/assignments"
	"agentd/internal/chat"
	"agentd/internal/integrations/telegram"
	"agentd/internal/model"
	"agentd/internal/policy"
	"agentd/internal/system"
	"agentd/internal/tasks"
	"agentd/internal/testutil"
	"agentd/internal/tools"
)

const token = "123456:SECRET-bot-token-value"

// fakeTelegram serves scripted updates and records what the bot sends.
type fakeTelegram struct {
	mu       sync.Mutex
	updates  []map[string]any
	next     int64
	sent     []map[string]any
	answered []map[string]any
	fail     bool
}

func (f *fakeTelegram) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(r.URL.Path, "/bot"+token+"/") {
		http.Error(w, "bad token", 401)
		return
	}
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	if f.fail {
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "description": "Bad Gateway"})
		return
	}
	switch strings.TrimPrefix(r.URL.Path, "/bot"+token+"/") {
	case "getUpdates":
		offset := int64(body["offset"].(float64))
		var out []map[string]any
		for _, u := range f.updates {
			if int64(u["update_id"].(int64)) >= offset {
				out = append(out, u)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": out})
	case "sendMessage":
		f.sent = append(f.sent, body)
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": len(f.sent)}})
	case "answerCallbackQuery":
		f.answered = append(f.answered, body)
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
	}
}

const owner, stranger = 1001, 2002

func (f *fakeTelegram) add(u map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	u["update_id"] = f.next
	f.updates = append(f.updates, u)
}

func text(from int64, chatType, body string, msgID int64) map[string]any {
	return map[string]any{"message": map[string]any{"message_id": msgID, "from": map[string]any{"id": from},
		"chat": map[string]any{"id": from, "type": chatType}, "text": body}}
}

func press(from int64, data string) map[string]any {
	return map[string]any{"callback_query": map[string]any{"id": fmt.Sprint("cb", from, data), "from": map[string]any{"id": from},
		"data": data, "message": map[string]any{"message_id": 1, "chat": map[string]any{"id": from, "type": "private"}}}}
}

func (f *fakeTelegram) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, s := range f.sent {
		out = append(out, s["text"].(string))
	}
	return out
}

type world struct {
	t      *testing.T
	ctx    context.Context
	clock  *testutil.Clock
	app    *app.App
	tg     *fakeTelegram
	bot    *telegram.Bot
	logs   *bytes.Buffer
	calls  int
	sender *sender
}

type sender struct {
	mu   sync.Mutex
	sent int
}

func (s *sender) Definition() model.ToolDefinition {
	return model.ToolDefinition{Name: "send", Description: "Send.", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (s *sender) Effect() tools.Effect                     { return tools.EffectWrite }
func (s *sender) Resource(json.RawMessage) (string, error) { return "refunds@airline.example", nil }
func (s *sender) Execute(context.Context, tools.Invocation) (tools.Result, error) {
	s.mu.Lock()
	s.sent++
	s.mu.Unlock()
	return tools.Result{Output: json.RawMessage(`{}`)}, nil
}
func (s *sender) Reconcile(context.Context, tools.Invocation) (tools.Result, tools.Finding, error) {
	return tools.Result{}, tools.Unknown, nil
}

func newWorld(t *testing.T) *world {
	w := &world{t: t, ctx: context.Background(), clock: testutil.NewClock(), tg: &fakeTelegram{}, logs: &bytes.Buffer{}, sender: &sender{}}
	db := testutil.Open(t, testutil.DBPath(t))
	reply := func(req model.Request) model.Message {
		w.calls++
		if strings.HasPrefix(req.Messages[0].Content, "You are agentd, the owner's personal agent") {
			return model.Message{Content: "Hi @everyone, see https://evil.example/x and #tag /start"}
		}
		if req.Messages[len(req.Messages)-1].Role == model.RoleTool {
			return model.Message{Content: "done"}
		}
		return model.Message{ToolCalls: []model.ToolCall{{ID: "s", Name: "send", Arguments: json.RawMessage(`{"to":"refunds@airline.example"}`)}}}
	}
	w.app = app.New(db, providerFunc(reply), "cloud", tools.NewRegistry(w.sender), w.clock.Now)
	srv := httptest.NewServer(w.tg)
	t.Cleanup(srv.Close)
	w.bot = &telegram.Bot{Token: token, BaseURL: srv.URL, Chat: w.app.Chat, Log: slog.New(slog.NewTextHandler(w.logs, nil))}
	w.app.Chat.Project(w.ctx)
	return w
}

type providerFunc func(model.Request) model.Message

func (p providerFunc) Complete(_ context.Context, req model.Request) (model.Response, error) {
	m := p(req)
	stop := model.StopEnd
	if len(m.ToolCalls) > 0 {
		stop = model.StopToolUse
	}
	return model.Response{Message: m, StopReason: stop}, nil
}

func (w *world) receive() {
	w.t.Helper()
	if err := w.bot.ReceiveOnce(w.ctx, 0); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) send() {
	w.t.Helper()
	if err := w.bot.SendPending(w.ctx); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) turns() {
	for {
		worked, _ := w.app.Runtime.RunOnceInteractive(w.ctx, "i")
		if !worked {
			return
		}
	}
}

func (w *world) pair() {
	w.t.Helper()
	code, err := w.app.Chat.Pair(w.ctx, telegram.Name)
	if err != nil {
		w.t.Fatal(err)
	}
	w.tg.add(text(owner, "private", code, 1))
	w.receive()
}

func (w *world) stored() int {
	var n int
	w.app.DB.QueryRow(`SELECT COUNT(*) FROM conversation_messages WHERE author = 'owner'`).Scan(&n)
	return n
}

func TestStrangersGetAFixedRefusalAndNothingElse(t *testing.T) {
	w := newWorld(t)
	w.tg.add(text(stranger, "private", "hello, what do you know?", 1))
	w.receive()
	w.turns()
	if got := w.tg.texts(); len(got) != 1 || !strings.Contains(got[0], "only talks to its owner") {
		t.Fatalf("sent = %q", got)
	}
	if w.stored() != 0 || w.calls != 0 {
		t.Fatalf("stored %d messages, %d model calls", w.stored(), w.calls)
	}
}

func TestPairingCodeWorksOnceAndExpires(t *testing.T) {
	w := newWorld(t)
	code, _ := w.app.Chat.Pair(w.ctx, telegram.Name)
	w.tg.add(text(stranger, "private", "000000000", 1))
	w.receive()
	if c, _ := w.app.Chat.Channel(w.ctx, telegram.Name); c.Paired {
		t.Fatal("a wrong code paired")
	}
	w.tg.add(text(owner, "private", code, 2))
	w.receive()
	if c, _ := w.app.Chat.Channel(w.ctx, telegram.Name); !c.Paired || c.SenderID != fmt.Sprint(owner) {
		t.Fatalf("pairing = %+v", c)
	}
	w.app.Chat.Revoke(w.ctx, telegram.Name)
	w.tg.add(text(stranger, "private", code, 3))
	w.receive()
	if c, _ := w.app.Chat.Channel(w.ctx, telegram.Name); c.Paired {
		t.Fatal("a used code paired again")
	}
	late, _ := w.app.Chat.Pair(w.ctx, telegram.Name)
	w.clock.Advance(11 * time.Minute)
	w.tg.add(text(owner, "private", late, 4))
	w.receive()
	if c, _ := w.app.Chat.Channel(w.ctx, telegram.Name); c.Paired {
		t.Fatal("an expired code paired")
	}
}

func TestOnlyDirectFreshMessagesFromTheOwnerCount(t *testing.T) {
	w := newWorld(t)
	w.pair()
	group := text(owner, "group", "from a group", 5)
	forwarded := text(owner, "private", "forwarded", 6)
	forwarded["message"].(map[string]any)["forward_origin"] = map[string]any{"type": "user"}
	edited := map[string]any{"edited_message": text(owner, "private", "edited", 7)["message"]}
	for _, u := range []map[string]any{group, forwarded, edited} {
		w.tg.add(u)
	}
	w.receive()
	if w.stored() != 0 {
		t.Fatalf("stored %d of a group, forwarded and edited message", w.stored())
	}
	w.app.Chat.Revoke(w.ctx, telegram.Name)
	w.tg.add(text(owner, "private", "after revoke", 8))
	w.receive()
	if w.stored() != 0 {
		t.Fatal("a revoked phone was heard")
	}
}

func TestSameUpdateTwiceMakesOneMessageAndOneTurn(t *testing.T) {
	w := newWorld(t)
	w.pair()
	msg := text(owner, "private", "how is it going?", 9)
	w.tg.add(msg)
	w.receive()
	w.app.DB.Exec(`UPDATE channels SET update_offset = 0`)
	w.receive()
	var turns int
	w.app.DB.QueryRow(`SELECT COUNT(*) FROM tasks WHERE priority = ?`, tasks.PriorityInteractive).Scan(&turns)
	if w.stored() != 1 || turns != 1 {
		t.Fatalf("stored %d, turns %d", w.stored(), turns)
	}
	w.turns()
	w.send()
	w.send()
	var replies int
	for _, s := range w.tg.texts() {
		if strings.HasPrefix(s, "Hi @") {
			replies++
		}
	}
	if replies != 1 {
		t.Fatalf("the reply was sent %d times", replies)
	}
}

func (w *world) pendingApproval() {
	w.t.Helper()
	project, _ := w.app.Assignments.CreateProject(w.ctx, assignments.NewProject{Name: "refund", Objective: "o",
		Policy: policy.Policy{Tools: map[string]policy.Rule{"send": {Mode: policy.RequireApproval}}}})
	w.app.Tasks.Create(w.ctx, tasks.NewTask{AssignmentID: project, Title: "t", Objective: "o"})
	w.app.Runtime.RunOnce(w.ctx, "w")
	w.app.Chat.Project(w.ctx)
}

func (w *world) lastButtons() []map[string]any {
	w.tg.mu.Lock()
	defer w.tg.mu.Unlock()
	for i := len(w.tg.sent) - 1; i >= 0; i-- {
		if m, ok := w.tg.sent[i]["reply_markup"].(map[string]any); ok {
			var out []map[string]any
			for _, b := range m["inline_keyboard"].([]any)[0].([]any) {
				out = append(out, b.(map[string]any))
			}
			return out
		}
	}
	return nil
}

func TestApproveByButtonOnlyFromTheOwner(t *testing.T) {
	w := newWorld(t)
	w.pair()
	w.app.Chat.Configure(w.ctx, telegram.Name, chat.LevelFull, false, true)
	w.pendingApproval()
	w.send()
	buttons := w.lastButtons()
	if len(buttons) != 2 || buttons[0]["text"] != "Approve" {
		t.Fatalf("buttons = %v", buttons)
	}
	approve := buttons[0]["callback_data"].(string)
	if strings.Contains(approve, "refunds@") || len(approve) > 64 {
		t.Fatalf("button payload %q carries arguments or is too long", approve)
	}
	forged := approve[:strings.LastIndex(approve, ":")+1] + "deadbeef"
	w.tg.add(press(owner, forged))
	w.tg.add(press(stranger, approve))
	w.receive()
	var status string
	w.app.DB.QueryRow(`SELECT status FROM approvals`).Scan(&status)
	if status != "pending" {
		t.Fatalf("a stranger's button press decided the approval: %s", status)
	}
	w.tg.add(press(owner, approve))
	w.receive()
	w.app.Runtime.RunOnce(w.ctx, "w")
	var actor string
	w.app.DB.QueryRow(`SELECT actor FROM audit_log WHERE operation = 'approval.approved'`).Scan(&actor)
	if actor != fmt.Sprintf("owner:telegram:%d", owner) || w.sender.sent != 1 {
		t.Fatalf("approved by %q, sent %d times", actor, w.sender.sent)
	}
}

func TestSummaryLevelSendsReviewNotButtons(t *testing.T) {
	w := newWorld(t)
	w.pair()
	w.pendingApproval()
	w.send()
	got := w.tg.texts()
	if !strings.Contains(got[len(got)-1], "review it before deciding") || w.lastButtons() != nil || strings.Contains(got[len(got)-1], "Exact arguments") {
		t.Fatalf("summary approval = %q, buttons %v", got[len(got)-1], w.lastButtons())
	}
}

func TestWideningFromThePhoneIsOffByDefault(t *testing.T) {
	w := newWorld(t)
	w.pair()
	system.Halt(w.ctx, w.app.DB, w.clock.Now(), "owner", "test")
	w.tg.add(text(owner, "private", "/unhalt", 10))
	w.receive()
	got := w.tg.texts()
	if st, _ := system.Get(w.ctx, w.app.DB); !st.Halted || !strings.Contains(got[len(got)-1], "command center or the terminal") {
		t.Fatalf("unhalt from the phone: halted=%v, reply %q", st.Halted, got[len(got)-1])
	}
	w.tg.add(text(owner, "private", "/halt again", 11))
	w.tg.add(text(owner, "private", "/status", 12))
	w.receive()
	w.send()
	if w.calls != 0 {
		t.Fatalf("%d model calls while halted", w.calls)
	}
	if all := strings.Join(w.tg.texts(), "\n"); !strings.Contains(all, "EMERGENCY STOP") {
		t.Fatalf("/status during a stop did not answer: %s", all)
	}
}

func TestLocalOnlyWorkSendsOnlyMinimalContent(t *testing.T) {
	w := newWorld(t)
	w.pair()
	w.app.Chat.Configure(w.ctx, telegram.Name, chat.LevelFull, true, true)
	project, _ := w.app.Assignments.CreateProject(w.ctx, assignments.NewProject{Name: "medical-claim", Objective: "o",
		Policy: policy.Policy{Tools: map[string]policy.Rule{"send": {Mode: policy.RequireApproval}}, Models: policy.ModelPolicy{LocalOnly: true}}})
	w.app.Runtime.ModelLocal = true
	w.app.Tasks.Create(w.ctx, tasks.NewTask{AssignmentID: project, Title: "t", Objective: "o"})
	w.app.Runtime.RunOnce(w.ctx, "w")
	w.app.Chat.Project(w.ctx)
	w.tg.add(text(owner, "private", "what is going on?", 13))
	w.receive()
	w.turns()
	w.send()
	for _, s := range w.tg.texts() {
		if strings.Contains(s, "refunds@") || strings.Contains(s, "medical") || strings.Contains(s, "Hi @") {
			t.Fatalf("local-only content left the machine: %q", s)
		}
	}
	if w.lastButtons() != nil {
		t.Fatal("a local-only approval was offered by button")
	}
}

func TestMarkupInRepliesIsInert(t *testing.T) {
	w := newWorld(t)
	w.pair()
	w.tg.add(text(owner, "private", "hi", 14))
	w.receive()
	w.turns()
	w.send()
	var reply string
	for _, s := range w.tg.texts() {
		if strings.HasPrefix(s, "Hi @") {
			reply = s
		}
	}
	for _, raw := range []string{"@everyone", "https://", "#tag", "/start"} {
		if strings.Contains(reply, raw) {
			t.Fatalf("%q reached Telegram as %q", raw, reply)
		}
	}
	w.tg.mu.Lock()
	defer w.tg.mu.Unlock()
	for _, s := range w.tg.sent {
		if s["parse_mode"] != nil {
			t.Fatal("a message was sent with a parse mode")
		}
	}
}

func TestTheTokenNeverLeaks(t *testing.T) {
	w := newWorld(t)
	w.pair()
	w.tg.fail = true
	if err := w.bot.ReceiveOnce(w.ctx, 0); err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("error = %v", err)
	}
	w.bot.BaseURL = "http://127.0.0.1:1"
	if err := w.bot.SendPending(w.ctx); err != nil && strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("network error leaks the token: %v", err)
	}
	if err := w.bot.ReceiveOnce(w.ctx, 0); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("network error = %v", err)
	}
	var dump strings.Builder
	for _, table := range []string{"audit_log", "channels", "conversation_messages", "channel_codes", "system_state"} {
		rows, _ := w.app.DB.Query(`SELECT * FROM ` + table)
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			rows.Scan(ptrs...)
			fmt.Fprintln(&dump, vals...)
		}
		rows.Close()
	}
	if strings.Contains(dump.String(), "SECRET") || strings.Contains(w.logs.String(), "SECRET") {
		t.Fatal("the bot token is stored or logged")
	}
}

var _ = io.Discard
