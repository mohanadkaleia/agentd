package chat

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"agentd/internal/audit"
	"agentd/internal/policy"
)

// A channel is a messaging service on the owner's phone. Adapters (internal/integrations/*)
// only move messages; who counts as the owner, what may be done from the phone and how much
// is sent out are decided here.

const (
	LevelMinimal = "minimal"
	LevelSummary = "summary"
	LevelFull    = "full"

	pairingTTL     = 10 * time.Minute
	channelTextMax = 3500
)

var ErrUnknownChannel = errors.New("no such channel")

// ChannelState is a channel's pairing and settings.
type ChannelState struct {
	Name     string     `json:"name"`
	Paired   bool       `json:"paired"`
	SenderID string     `json:"-"`
	ChatID   string     `json:"-"`
	Level    string     `json:"level"`
	Widen    bool       `json:"widen"`
	Decide   bool       `json:"decide"`
	PairedAt *time.Time `json:"paired_at,omitempty"`
	Offset   int64      `json:"-"`
}

// Inbound is one update from a channel, already reduced to what the rules need.
type Inbound struct {
	SenderID  string
	ChatID    string
	Private   bool
	MessageID string
	Text      string
	Forwarded bool
	Edited    bool
	// Action is a button press: its payload and the provider's id for acknowledging it.
	Action   string
	ActionID string
}

// Reply is what the adapter sends back at once (a refusal, a pairing confirmation, the
// answer to a button press). Empty means nothing.
type Reply struct {
	ChatID string
	Text   string
	// Ack is shown as a button-press acknowledgement instead of a message.
	Ack bool
}

// Outbound is a message for the paired chat, already reduced to the channel's level.
type Outbound struct {
	Seq     int64
	ChatID  string
	Text    string
	Buttons []Button
}

type Button struct {
	Label  string
	Action string
}

const refusal = "This bot only talks to its owner. If that is you, run `agentd channel pair` on your Mac and send the code here."

// Pair starts pairing: it returns a one-time code to send to the bot from the owner's phone.
func (s *Service) Pair(ctx context.Context, channel string) (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000_000))
	if err != nil {
		return "", err
	}
	code := fmt.Sprintf("%09d", n.Int64())
	now := s.Now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO channels (name, created_at) VALUES (?, ?) ON CONFLICT (name) DO NOTHING`,
		channel, now.UnixMilli()); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM channel_codes WHERE channel = ? AND used_at IS NULL`, channel); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO channel_codes (code_hash, channel, expires_at) VALUES (?, ?, ?)`,
		hashCode(code), channel, now.Add(pairingTTL).UnixMilli()); err != nil {
		return "", err
	}
	if err := audit.Record(ctx, tx, now, "", "owner", "channel.pairing_started", channel, nil); err != nil {
		return "", err
	}
	return code, tx.Commit()
}

func hashCode(code string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(code)))
	return hex.EncodeToString(sum[:])
}

// Revoke unpairs a channel at once.
func (s *Service) Revoke(ctx context.Context, channel string) error {
	now := s.Now()
	res, err := s.DB.ExecContext(ctx, `UPDATE channels SET sender_id = NULL, chat_id = NULL, revoked_at = ? WHERE name = ?`,
		now.UnixMilli(), channel)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrUnknownChannel
	}
	return audit.Record(ctx, s.DB, now, "", "owner", "channel.revoked", channel, nil)
}

// Configure sets what a channel may receive and do.
func (s *Service) Configure(ctx context.Context, channel, level string, widen, decide bool) error {
	if level != LevelMinimal && level != LevelSummary && level != LevelFull {
		return fmt.Errorf("level must be minimal, summary or full")
	}
	res, err := s.DB.ExecContext(ctx, `UPDATE channels SET level = ?, widen = ?, decide = ? WHERE name = ?`, level, widen, decide, channel)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrUnknownChannel
	}
	return audit.Record(ctx, s.DB, s.Now(), "", "owner", "channel.configured", channel,
		map[string]any{"level": level, "widen": widen, "decide": decide})
}

func (s *Service) Channels(ctx context.Context) ([]ChannelState, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT name FROM channels ORDER BY name`)
	if err != nil {
		return nil, err
	}
	names, err := strings1(rows)
	if err != nil {
		return nil, err
	}
	out := []ChannelState{}
	for _, n := range names {
		c, err := s.Channel(ctx, n)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func (s *Service) Channel(ctx context.Context, name string) (ChannelState, error) {
	c := ChannelState{Name: name}
	var sender, chat sql.NullString
	var paired sql.NullInt64
	err := s.DB.QueryRowContext(ctx, `SELECT sender_id, chat_id, level, widen, decide, paired_at, update_offset FROM channels WHERE name = ?`, name).
		Scan(&sender, &chat, &c.Level, &c.Widen, &c.Decide, &paired, &c.Offset)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrUnknownChannel
	}
	if err != nil {
		return c, err
	}
	c.SenderID, c.ChatID, c.Paired = sender.String, chat.String, sender.Valid
	if paired.Valid && c.Paired {
		at := time.UnixMilli(paired.Int64)
		c.PairedAt = &at
	}
	return c, nil
}

// SetOffset records the provider's update cursor once an update has been handled.
func (s *Service) SetOffset(ctx context.Context, channel string, offset int64) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE channels SET update_offset = max(update_offset, ?) WHERE name = ?`, offset, channel)
	return err
}

// ignore audits a refused update, at most once a minute per reason, so a stranger cannot
// fill the log.
func (s *Service) ignore(ctx context.Context, channel, why string) {
	key := channel + ":" + why
	if last, ok := s.ignored.Load(key); ok && s.Now().Sub(last.(time.Time)) < time.Minute {
		return
	}
	s.ignored.Store(key, s.Now())
	audit.Record(ctx, s.DB, s.Now(), "", "channel", "channel.ignored", channel, map[string]string{"why": why})
}

var widening = map[string]bool{"resume": true, "unhalt": true, "edit": true, "model": true}
var deciding = map[string]bool{"approve": true, "reject": true, "answer": true}

// Inbound handles one update. Only the paired sender in a direct chat is heard; anything
// else gets the fixed refusal (when it could be a pairing attempt) or is ignored.
func (s *Service) Inbound(ctx context.Context, channel string, in Inbound) (Reply, error) {
	c, err := s.Channel(ctx, channel)
	if errors.Is(err, ErrUnknownChannel) {
		return Reply{ChatID: in.ChatID, Text: refusal}, nil
	}
	if err != nil {
		return Reply{}, err
	}
	if !in.Private {
		s.ignore(ctx, channel, "not a direct chat")
		return Reply{}, nil
	}
	if !c.Paired || in.SenderID != c.SenderID {
		if in.Action == "" && !in.Edited && !in.Forwarded {
			if ok, err := s.tryPair(ctx, channel, in); err != nil || ok {
				if ok {
					return Reply{ChatID: in.ChatID, Text: "Paired. You can talk to agentd here now. Send /help for commands."}, err
				}
				return Reply{}, err
			}
		}
		s.ignore(ctx, channel, "not the paired sender")
		if in.Action != "" {
			return Reply{ChatID: in.ChatID, Text: "Refused.", Ack: true}, nil
		}
		return Reply{ChatID: in.ChatID, Text: refusal}, nil
	}
	if in.Forwarded || in.Edited {
		s.ignore(ctx, channel, "forwarded or edited")
		return Reply{}, nil
	}
	if in.Action != "" {
		text, err := s.action(ctx, c, in.Action)
		return Reply{ChatID: in.ChatID, Text: text, Ack: true}, err
	}
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return Reply{}, nil
	}
	var code int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM channel_codes WHERE code_hash = ? AND channel = ?`,
		hashCode(text), channel).Scan(&code); err != nil {
		return Reply{}, err
	}
	if code > 0 {
		return Reply{}, nil
	}
	if strings.HasPrefix(text, "/") {
		if refusal := s.allowed(ctx, c, text); refusal != "" {
			return Reply{ChatID: in.ChatID, Text: refusal}, nil
		}
	}
	conv, err := s.Default(ctx)
	if err != nil {
		return Reply{}, err
	}
	_, err = s.Post(ctx, Post{ConversationID: conv, Text: text, Channel: channel, ClientMsgID: channel + ":" + in.ChatID + ":" + in.MessageID})
	return Reply{}, err
}

func (s *Service) tryPair(ctx context.Context, channel string, in Inbound) (bool, error) {
	now := s.Now()
	conv, err := s.Default(ctx)
	if err != nil {
		return false, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE channel_codes SET used_at = ? WHERE code_hash = ? AND channel = ? AND used_at IS NULL AND expires_at > ?`,
		now.UnixMilli(), hashCode(in.Text), channel, now.UnixMilli())
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	var start int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM conversation_messages WHERE conversation_id = ?`, conv).Scan(&start); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE channels SET sender_id = ?, chat_id = ?, paired_at = ?, revoked_at = NULL WHERE name = ?`,
		in.SenderID, in.ChatID, now.UnixMilli(), channel); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_reads (conversation_id, channel, seen_seq, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (conversation_id, channel) DO UPDATE SET seen_seq = excluded.seen_seq, updated_at = excluded.updated_at`,
		conv, channel, start, now.UnixMilli()); err != nil {
		return false, err
	}
	if err := audit.Record(ctx, tx, now, "", "owner", "channel.paired", channel, map[string]string{"sender": in.SenderID}); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// allowed checks a slash command against what this channel may do. Stopping is always
// allowed; anything that widens authority needs the widen setting.
func (s *Service) allowed(ctx context.Context, c ChannelState, line string) string {
	fields := strings.Fields(strings.TrimPrefix(line, "/"))
	if len(fields) == 0 {
		return ""
	}
	name := strings.ToLower(fields[0])
	pointer := "Do that from the command center or the terminal, or allow it with: agentd channel set " + c.Name + " -widen"
	switch {
	case widening[name] && !c.Widen:
		return "From this phone /" + name + " is turned off. " + pointer
	case deciding[name] && !c.Decide:
		return "Decisions from this phone are turned off."
	case (name == "approve" || name == "reject") && len(fields) > 1 && !c.Widen:
		conv, err := s.Default(ctx)
		if err == nil {
			if card, err := s.findCard(ctx, conv, fields[1]); err == nil && card.Kind == KindProposal && name == "approve" {
				return "Approving a proposal from this phone is turned off. " + pointer
			}
		}
	}
	return ""
}

// action handles a button press. Buttons carry only an id and a hash prefix; everything
// else is looked up and checked again.
func (s *Service) action(ctx context.Context, c ChannelState, payload string) (string, error) {
	parts := strings.Split(payload, ":")
	conv, err := s.Default(ctx)
	if err != nil {
		return "", err
	}
	switch {
	case len(parts) == 4 && parts[0] == "d" && (parts[1] == "a" || parts[1] == "r"):
		if !c.Decide {
			return "Decisions from this phone are turned off.", nil
		}
		card, err := s.findCard(ctx, conv, parts[2])
		if err != nil {
			return "That item is not in the conversation.", nil
		}
		if !strings.HasPrefix(card.Card().ArgumentsHash, parts[3]) || len(parts[3]) < 8 {
			return "That button no longer matches the item.", nil
		}
		if card.Kind == KindProposal && parts[1] == "a" && !c.Widen {
			return "Approving a proposal from this phone is turned off.", nil
		}
		out, err := s.Decide(ctx, conv, card.RefID, parts[1] == "a", c.Name+":"+c.SenderID)
		if err != nil {
			return err.Error(), nil
		}
		return out, nil
	case len(parts) == 3 && parts[0] == "q":
		if !c.Decide {
			return "Decisions from this phone are turned off.", nil
		}
		card, err := s.findCard(ctx, conv, parts[1])
		if err != nil || card.Kind != KindQuestion {
			return "That question is not in the conversation.", nil
		}
		choices := card.Card().Choices
		var i int
		if _, err := fmt.Sscan(parts[2], &i); err != nil || i < 0 || i >= len(choices) {
			return "That choice is not on the question.", nil
		}
		out, err := s.answer(ctx, card.RefID, choices[i], c.Name+":"+c.SenderID)
		if err != nil {
			return err.Error(), nil
		}
		return out, nil
	}
	return "Unknown button.", nil
}

// Outbound returns what the paired chat has not been sent yet, at the channel's level.
// The owner's own messages are not echoed.
func (s *Service) Outbound(ctx context.Context, channel string) ([]Outbound, error) {
	c, err := s.Channel(ctx, channel)
	if err != nil || !c.Paired {
		return nil, err
	}
	conv, err := s.Default(ctx)
	if err != nil {
		return nil, err
	}
	after, err := s.LastRead(ctx, conv, channel)
	if err != nil {
		return nil, err
	}
	msgs, err := s.Messages(ctx, conv, after, 20)
	if err != nil {
		return nil, err
	}
	var out []Outbound
	for _, m := range msgs {
		o := Outbound{Seq: m.Seq, ChatID: c.ChatID}
		if m.Author != Owner {
			level, err := s.levelFor(ctx, c, m)
			if err != nil {
				return nil, err
			}
			o.Text, o.Buttons = renderForChannel(m, level, c)
		}
		out = append(out, o)
	}
	return out, nil
}

// Delivered records that everything up to seq has been sent (or skipped) on this channel.
func (s *Service) Delivered(ctx context.Context, channel string, seq int64) error {
	conv, err := s.Default(ctx)
	if err != nil {
		return err
	}
	return s.MarkRead(ctx, conv, channel, seq)
}

// levelFor lowers the level for content that must stay on this machine: anything about an
// assignment with models.local_only, and replies from a turn whose model was local while
// such an assignment exists (that model may have read its details).
func (s *Service) levelFor(ctx context.Context, c ChannelState, m Message) (string, error) {
	if card := m.Card(); card.AssignmentID != "" {
		var raw string
		if err := s.DB.QueryRowContext(ctx, `SELECT policy_json FROM assignments WHERE id = ?`, card.AssignmentID).Scan(&raw); err == nil {
			var p policy.Policy
			json.Unmarshal([]byte(raw), &p)
			if p.Models.LocalOnly {
				return LevelMinimal, nil
			}
		}
	}
	if m.Author == Agent && m.TurnTaskID != "" {
		var private int
		if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM assignments WHERE status IN ('active', 'paused')
			AND json_extract(policy_json, '$.models.local_only') = 1`).Scan(&private); err != nil {
			return "", err
		}
		var model string
		s.DB.QueryRowContext(ctx, `SELECT model_id FROM runs WHERE task_id = ? ORDER BY started_at DESC LIMIT 1`, m.TurnTaskID).Scan(&model)
		if private > 0 && s.Runtime.IsLocal(model) {
			return LevelMinimal, nil
		}
	}
	return c.Level, nil
}

// renderForChannel writes a message as plain text for a third-party service. Text from models, tools
// and projects is defused so it cannot form mentions, links or commands there.
func renderForChannel(m Message, level string, c ChannelState) (string, []Button) {
	card := m.Card()
	open := "Open the command center to see it."
	switch m.Kind {
	case KindApproval, KindProposal:
		what := "Approval"
		if m.Kind == KindProposal {
			what = "Proposal"
		}
		if level == LevelMinimal {
			return fmt.Sprintf("%s #%d is waiting for you. %s", what, card.N, open), nil
		}
		text := fmt.Sprintf("%s #%d: %s", what, card.N, defuse(card.Title))
		if card.Text != "" && level == LevelFull {
			text += "\n" + defuse(card.Text)
		}
		if level != LevelFull {
			return clip(text + "\nOpen the command center to review it before deciding."), nil
		}
		args, _ := json.MarshalIndent(card.Arguments, "", "  ")
		text += "\nExact arguments:\n" + defuse(string(args))
		hash := card.ArgumentsHash
		if len(hash) > 8 {
			hash = hash[:8]
		}
		var buttons []Button
		if m.Kind == KindApproval || c.Widen {
			buttons = append(buttons, Button{Label: "Approve", Action: fmt.Sprintf("d:a:%d:%s", card.N, hash)})
		} else {
			text += "\nApprove it from the command center or the terminal."
		}
		buttons = append(buttons, Button{Label: "Reject", Action: fmt.Sprintf("d:r:%d:%s", card.N, hash)})
		return clip(text), buttons
	case KindQuestion:
		if level == LevelMinimal {
			return fmt.Sprintf("Question #%d is waiting for your answer. %s", card.N, open), nil
		}
		text := fmt.Sprintf("Question #%d: %s", card.N, defuse(card.Title))
		if card.Text != "" {
			text += "\n" + defuse(card.Text)
		}
		var buttons []Button
		for i, choice := range card.Choices {
			buttons = append(buttons, Button{Label: clipLabel(defuse(choice)), Action: fmt.Sprintf("q:%d:%d", card.N, i)})
		}
		return clip(text + fmt.Sprintf("\nAnswer with: /answer %d <your answer>", card.N)), buttons
	case KindNotification, KindReport:
		if level == LevelMinimal {
			return "agentd has news for you. " + open, nil
		}
		text := defuse(card.Title)
		if level == LevelFull && card.Text != "" {
			text += "\n" + defuse(card.Text)
		}
		return clip(text), nil
	}
	if m.Author == Agent && level == LevelMinimal {
		return "agentd replied. " + open, nil
	}
	return clip(defuse(m.Text())), nil
}

// defuse breaks the patterns messaging apps turn into mentions, links and bot commands, by
// putting an invisible separator inside them. Messages are sent without any markup mode,
// so nothing else in the text is interpreted.
func defuse(text string) string {
	r := strings.NewReplacer("@", "@⁠", "://", ":⁠//", "www.", "www⁠.", "/", "/⁠", "#", "#⁠")
	return r.Replace(text)
}

func clip(text string) string {
	if len([]rune(text)) <= channelTextMax {
		return text
	}
	return string([]rune(text)[:channelTextMax]) + "\n[cut; the rest is in the command center]"
}

func clipLabel(text string) string {
	if len([]rune(text)) <= 40 {
		return text
	}
	return string([]rune(text)[:39]) + "…"
}

// Covered reports whether a paired channel already brings conversation cards to the
// owner's phone, so a notification push would only repeat them.
func (s *Service) Covered(ctx context.Context) (bool, error) {
	for _, c := range s.liveChannels() {
		state, err := s.Channel(ctx, c)
		if err != nil && !errors.Is(err, ErrUnknownChannel) {
			return false, err
		}
		if state.Paired {
			return true, nil
		}
	}
	return false, nil
}

// Running marks a channel adapter as running in this process (or stopped), so pushes are
// skipped only while something actually delivers to the phone.
func (s *Service) Running(channel string, running bool) {
	if running {
		s.running.Store(channel, true)
	} else {
		s.running.Delete(channel)
	}
}

func (s *Service) liveChannels() []string {
	var out []string
	s.running.Range(func(k, _ any) bool {
		out = append(out, k.(string))
		return true
	})
	return out
}
