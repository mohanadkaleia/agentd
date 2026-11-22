package scripted

import (
	"context"
	"time"

	"agentd/internal/model"
)

// Streaming is a scripted model that streams its text in chunks, for tests.
type Streaming struct {
	Script Provider
	Chunk  int
	Delay  time.Duration
	// Fail, when set, is returned after this many chunks have been emitted.
	Fail      error
	FailAfter int
}

func (s Streaming) Complete(ctx context.Context, req model.Request) (model.Response, error) {
	return s.Script.Complete(ctx, req)
}

func (s Streaming) Stream(ctx context.Context, req model.Request, emit func(model.Delta)) (model.Response, error) {
	resp, err := s.Script.Complete(ctx, req)
	if err != nil {
		return resp, err
	}
	chunk := max(s.Chunk, 1)
	text := []rune(resp.Message.Content)
	for i, n := 0, 0; i < len(text); i, n = i+chunk, n+1 {
		if s.Fail != nil && n == s.FailAfter {
			return model.Response{}, s.Fail
		}
		select {
		case <-ctx.Done():
			return model.Response{}, ctx.Err()
		case <-time.After(s.Delay):
		}
		emit(model.Delta{Text: string(text[i:min(i+chunk, len(text))])})
	}
	if s.Fail != nil && s.FailAfter <= 0 && len(text) == 0 {
		return model.Response{}, s.Fail
	}
	return resp, nil
}
