package chat

import (
	"context"
	"sync"
)

// hub wakes conversation streams in this process when something was written. Signals
// never block: a stream that is busy or stuck simply catches up from stored messages.
type hub struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

// Subscribe returns a channel that receives a signal after conversation changes.
func (s *Service) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.hub.mu.Lock()
	if s.hub.subs == nil {
		s.hub.subs = map[chan struct{}]struct{}{}
	}
	s.hub.subs[ch] = struct{}{}
	s.hub.mu.Unlock()
	return ch, func() {
		s.hub.mu.Lock()
		delete(s.hub.subs, ch)
		s.hub.mu.Unlock()
	}
}

func (s *Service) changed() {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	for ch := range s.hub.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Live fills in the current status of the cards among msgs.
func (s *Service) Live(ctx context.Context, msgs []Message) error {
	return s.live(ctx, msgs)
}

// Delta is streamed reply text for a turn. Restarted means text shown so far for that turn
// must be discarded.
type Delta struct {
	TaskID    string `json:"turn_task_id"`
	Text      string `json:"text,omitempty"`
	Restarted bool   `json:"restarted,omitempty"`
}

// deltaBuffer bounds what one subscriber may fall behind; beyond it the subscriber misses
// text and shows the stored reply when it arrives.
const deltaBuffer = 256

type deltas struct {
	mu   sync.Mutex
	subs map[chan Delta]struct{}
}

// Delta implements runtime.DeltaSink. It never blocks the turn.
func (s *Service) Delta(taskID, text string) { s.publish(Delta{TaskID: taskID, Text: text}) }

func (s *Service) Restarted(taskID string) { s.publish(Delta{TaskID: taskID, Restarted: true}) }

func (s *Service) publish(d Delta) {
	s.deltas.mu.Lock()
	defer s.deltas.mu.Unlock()
	for ch := range s.deltas.subs {
		select {
		case ch <- d:
		default:
		}
	}
}

// SubscribeDeltas returns streamed text for every turn in this process.
func (s *Service) SubscribeDeltas() (<-chan Delta, func()) {
	ch := make(chan Delta, deltaBuffer)
	s.deltas.mu.Lock()
	if s.deltas.subs == nil {
		s.deltas.subs = map[chan Delta]struct{}{}
	}
	s.deltas.subs[ch] = struct{}{}
	s.deltas.mu.Unlock()
	return ch, func() {
		s.deltas.mu.Lock()
		delete(s.deltas.subs, ch)
		s.deltas.mu.Unlock()
	}
}

// ConversationOf returns the conversation a turn belongs to, or "".
func (s *Service) ConversationOf(ctx context.Context, taskID string) string {
	var conv string
	s.DB.QueryRowContext(ctx, `SELECT conversation_id FROM conversation_messages WHERE turn_task_id = ? LIMIT 1`, taskID).Scan(&conv)
	return conv
}
