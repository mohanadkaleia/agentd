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
