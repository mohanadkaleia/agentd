package scripted

import (
	"context"

	"agentd/internal/model"
)

type Provider func(req model.Request) model.Message

func (p Provider) Complete(_ context.Context, req model.Request) (model.Response, error) {
	msg := p(req)
	msg.Role = model.RoleAssistant
	return model.Response{Message: msg, StopReason: "end_turn"}, nil
}
