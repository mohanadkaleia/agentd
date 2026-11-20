package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"

	"agentd/internal/model"
)

const (
	ProviderName     = "anthropic"
	DefaultModel     = "claude-opus-5-5"
	defaultMaxTokens = 16000
)

type Config struct {
	APIKey    string
	Model     string
	MaxTokens int64
	Effort    string
	Options   []option.RequestOption
}

type Provider struct {
	client sdk.Client
	cfg    Config
}

func New(cfg Config) *Provider {
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.MaxTokens == 0 {
		cfg.MaxTokens = defaultMaxTokens
	}
	opts := cfg.Options
	if cfg.APIKey != "" {
		opts = append([]option.RequestOption{option.WithAPIKey(cfg.APIKey)}, opts...)
	}
	return &Provider{client: sdk.NewClient(opts...), cfg: cfg}
}

func (p *Provider) Complete(ctx context.Context, req model.Request) (model.Response, error) {
	params, err := p.params(req)
	if err != nil {
		return model.Response{}, err
	}
	resp, err := p.client.Beta.Messages.New(ctx, params)
	if err != nil {
		return model.Response{}, fmt.Errorf("anthropic: %w", err)
	}
	return toResponse(resp)
}

func (p *Provider) params(req model.Request) (sdk.BetaMessageNewParams, error) {
	modelID := req.Model
	if modelID == "" {
		modelID = p.cfg.Model
	}
	maxTokens := p.cfg.MaxTokens
	if req.MaxTokens > 0 {
		maxTokens = int64(req.MaxTokens)
	}
	system, messages, err := toMessages(req.Messages)
	if err != nil {
		return sdk.BetaMessageNewParams{}, err
	}
	params := sdk.BetaMessageNewParams{
		Model:     sdk.Model(modelID),
		MaxTokens: maxTokens,
		System:    system,
		Messages:  messages,
		Tools:     toTools(req.Tools),
		Betas:     []sdk.AnthropicBeta{sdk.AnthropicBetaServerSideFallback2026_07_01},
		Fallbacks: sdk.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()},
	}
	if p.cfg.Effort != "" {
		params.OutputConfig = sdk.BetaOutputConfigParam{Effort: sdk.BetaOutputConfigEffort(p.cfg.Effort)}
	}
	return params, nil
}

func toTools(defs []model.ToolDefinition) []sdk.BetaToolUnionParam {
	tools := make([]sdk.BetaToolUnionParam, 0, len(defs))
	for _, d := range defs {
		tool := sdk.BetaToolParam{
			Name:        d.Name,
			Description: sdk.String(d.Description),
			InputSchema: param.Override[sdk.BetaToolInputSchemaParam](d.InputSchema),
		}
		tools = append(tools, sdk.BetaToolUnionParam{OfTool: &tool})
	}
	return tools
}

func toMessages(msgs []model.Message) ([]sdk.BetaTextBlockParam, []sdk.BetaMessageParam, error) {
	var system []sdk.BetaTextBlockParam
	var out []sdk.BetaMessageParam
	var pendingUser []sdk.BetaContentBlockParamUnion
	flushUser := func() {
		if len(pendingUser) > 0 {
			out = append(out, sdk.NewBetaUserMessage(pendingUser...))
			pendingUser = nil
		}
	}
	for _, m := range msgs {
		switch m.Role {
		case model.RoleSystem:
			system = append(system, sdk.BetaTextBlockParam{Text: m.Content})
		case model.RoleUser:
			pendingUser = append(pendingUser, sdk.NewBetaTextBlock(m.Content))
		case model.RoleTool:
			pendingUser = append(pendingUser, sdk.NewBetaToolResultBlock(m.ToolCallID, m.Content, false))
		case model.RoleAssistant:
			flushUser()
			if m.Native != nil && m.Native.Provider == ProviderName {
				out = append(out, param.Override[sdk.BetaMessageParam](m.Native.Payload))
				continue
			}
			var blocks []sdk.BetaContentBlockParamUnion
			if m.Content != "" {
				blocks = append(blocks, sdk.NewBetaTextBlock(m.Content))
			}
			for _, c := range m.ToolCalls {
				blocks = append(blocks, sdk.NewBetaToolUseBlock(c.ID, objectArguments(c.Arguments), c.Name))
			}
			out = append(out, sdk.BetaMessageParam{Role: sdk.BetaMessageParamRoleAssistant, Content: blocks})
		default:
			return nil, nil, fmt.Errorf("anthropic: unsupported role %q", m.Role)
		}
	}
	flushUser()
	return system, out, nil
}

func objectArguments(args json.RawMessage) json.RawMessage {
	trimmed := strings.TrimSpace(string(args))
	if strings.HasPrefix(trimmed, "{") && json.Valid(args) {
		return args
	}
	wrapped, _ := json.Marshal(map[string]json.RawMessage{"invalid_arguments": validOrString(args)})
	return wrapped
}

func validOrString(raw json.RawMessage) json.RawMessage {
	if json.Valid(raw) {
		return raw
	}
	s, _ := json.Marshal(string(raw))
	return s
}

func toResponse(resp *sdk.BetaMessage) (model.Response, error) {
	native, err := json.Marshal(resp.ToParam())
	if err != nil {
		return model.Response{}, err
	}
	msg := model.Message{
		Role:   model.RoleAssistant,
		Native: &model.Native{Provider: ProviderName, Payload: native},
	}
	var text []string
	for _, block := range resp.Content {
		switch b := block.AsAny().(type) {
		case sdk.BetaTextBlock:
			text = append(text, b.Text)
		case sdk.BetaToolUseBlock:
			msg.ToolCalls = append(msg.ToolCalls, model.ToolCall{
				ID:        b.ID,
				Name:      b.Name,
				Arguments: json.RawMessage(b.JSON.Input.Raw()),
			})
		}
	}
	msg.Content = strings.Join(text, "\n")
	in, out := resp.Usage.InputTokens, resp.Usage.OutputTokens
	return model.Response{
		Model:      string(resp.Model),
		Message:    msg,
		StopReason: stopReason(resp.StopReason, len(msg.ToolCalls) > 0),
		Usage:      model.Usage{InputTokens: in, OutputTokens: out, CostMicros: CostMicros(string(resp.Model), in, out)},
	}, nil
}

func stopReason(r sdk.BetaStopReason, hasTools bool) model.StopReason {
	switch r {
	case sdk.BetaStopReasonRefusal:
		return model.StopRefusal
	case sdk.BetaStopReasonMaxTokens:
		return model.StopMaxTokens
	}
	if hasTools {
		return model.StopToolUse
	}
	return model.StopEnd
}
