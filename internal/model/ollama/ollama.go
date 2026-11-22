package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"agentd/internal/model"
)

const (
	ProviderName   = "ollama"
	DefaultBaseURL = "http://localhost:11434"
)

type Config struct {
	BaseURL    string
	Model      string
	HTTPClient *http.Client
}

type Provider struct {
	cfg Config
}

func New(cfg Config) *Provider {
	cfg.BaseURL = baseURL(cfg.BaseURL)
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Minute}
	}
	return &Provider{cfg: cfg}
}

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []message `json:"messages"`
	Tools    []tool    `json:"tools,omitempty"`
	Stream   bool      `json:"stream"`
	Options  options   `json:"options,omitzero"`
}

type options struct {
	NumPredict int `json:"num_predict,omitempty"`
}

type message struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
	ToolName  string     `json:"tool_name,omitempty"`
}

type toolCall struct {
	ID       string   `json:"id,omitempty"`
	Function function `json:"function"`
}

type function struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type tool struct {
	Type     string         `json:"type"`
	Function toolDefinition `json:"function"`
}

type toolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type chatResponse struct {
	Message         message `json:"message"`
	Done            bool    `json:"done"`
	DoneReason      string  `json:"done_reason"`
	PromptEvalCount int64   `json:"prompt_eval_count"`
	EvalCount       int64   `json:"eval_count"`
}

func (p *Provider) Complete(ctx context.Context, req model.Request) (model.Response, error) {
	modelID, resp, err := p.post(ctx, req, false)
	if err != nil {
		return model.Response{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return model.Response{}, fmt.Errorf("ollama: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return model.Response{}, fmt.Errorf("ollama: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return model.Response{}, fmt.Errorf("ollama: decode response: %w", err)
	}
	result := toResponse(out)
	result.Model = modelID
	return result, nil
}

// Stream reads Ollama's streamed chat (one JSON object per line), showing text as it
// arrives, and returns the same Response as Complete.
func (p *Provider) Stream(ctx context.Context, req model.Request, emit func(model.Delta)) (model.Response, error) {
	modelID, resp, err := p.post(ctx, req, true)
	if err != nil {
		return model.Response{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return model.Response{}, fmt.Errorf("ollama: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var whole chatResponse
	var text strings.Builder
	dec := json.NewDecoder(resp.Body)
	for {
		var chunk chatResponse
		if err := dec.Decode(&chunk); err == io.EOF {
			break
		} else if err != nil {
			return model.Response{}, fmt.Errorf("ollama: decode stream: %w", err)
		}
		if chunk.Message.Content != "" {
			text.WriteString(chunk.Message.Content)
			emit(model.Delta{Text: chunk.Message.Content})
		}
		whole.Message.ToolCalls = append(whole.Message.ToolCalls, chunk.Message.ToolCalls...)
		if chunk.Done {
			whole.Done, whole.DoneReason = true, chunk.DoneReason
			whole.PromptEvalCount, whole.EvalCount = chunk.PromptEvalCount, chunk.EvalCount
			break
		}
	}
	if !whole.Done {
		return model.Response{}, fmt.Errorf("ollama: the stream ended before the reply was complete")
	}
	whole.Message.Content = text.String()
	result := toResponse(whole)
	result.Model = modelID
	return result, nil
}

func (p *Provider) post(ctx context.Context, req model.Request, stream bool) (string, *http.Response, error) {
	modelID := req.Model
	if modelID == "" {
		modelID = p.cfg.Model
	}
	body, err := json.Marshal(chatRequest{
		Model:    modelID,
		Messages: toMessages(req.Messages),
		Tools:    toTools(req.Tools),
		Stream:   stream,
		Options:  options{NumPredict: req.MaxTokens},
	})
	if err != nil {
		return "", nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.cfg.BaseURL, "/")+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := p.cfg.HTTPClient.Do(httpReq)
	if err != nil {
		return "", nil, fmt.Errorf("ollama: %w", err)
	}
	return modelID, resp, nil
}

func toTools(defs []model.ToolDefinition) []tool {
	tools := make([]tool, 0, len(defs))
	for _, d := range defs {
		tools = append(tools, tool{Type: "function", Function: toolDefinition{Name: d.Name, Description: d.Description, Parameters: d.InputSchema}})
	}
	return tools
}

func toMessages(msgs []model.Message) []message {
	toolNames := map[string]string{}
	out := make([]message, 0, len(msgs))
	for _, m := range msgs {
		msg := message{Role: string(m.Role), Content: m.Content}
		for _, c := range m.ToolCalls {
			toolNames[c.ID] = c.Name
			msg.ToolCalls = append(msg.ToolCalls, toolCall{ID: c.ID, Function: function{Name: c.Name, Arguments: objectArguments(c.Arguments)}})
		}
		if m.Role == model.RoleTool {
			msg.ToolName = toolNames[m.ToolCallID]
		}
		out = append(out, msg)
	}
	return out
}

func objectArguments(args json.RawMessage) json.RawMessage {
	if strings.HasPrefix(strings.TrimSpace(string(args)), "{") && json.Valid(args) {
		return args
	}
	if !json.Valid(args) {
		args, _ = json.Marshal(string(args))
	}
	wrapped, _ := json.Marshal(map[string]json.RawMessage{"invalid_arguments": args})
	return wrapped
}

func toResponse(r chatResponse) model.Response {
	msg := model.Message{Role: model.RoleAssistant, Content: r.Message.Content}
	for i, c := range r.Message.ToolCalls {
		id := c.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", i+1)
		}
		msg.ToolCalls = append(msg.ToolCalls, model.ToolCall{ID: id, Name: c.Function.Name, Arguments: c.Function.Arguments})
	}
	stop := model.StopEnd
	switch {
	case r.DoneReason == "length":
		stop = model.StopMaxTokens
	case len(msg.ToolCalls) > 0:
		stop = model.StopToolUse
	}
	return model.Response{
		Message:    msg,
		StopReason: stop,
		Usage:      model.Usage{InputTokens: r.PromptEvalCount, OutputTokens: r.EvalCount},
	}
}

// baseURL accepts OLLAMA_HOST as Ollama itself does: "0.0.0.0", "host:port" or a full URL.
func baseURL(host string) string {
	host = strings.TrimRight(strings.TrimSpace(host), "/")
	if host == "" {
		return DefaultBaseURL
	}
	bare := !strings.Contains(host, "://")
	if bare {
		host = "http://" + host
	}
	u, err := url.Parse(host)
	if err != nil || u.Host == "" {
		return DefaultBaseURL
	}
	hostname, port := u.Hostname(), u.Port()
	if hostname == "0.0.0.0" || hostname == "::" {
		hostname = "127.0.0.1"
	}
	switch {
	case port != "":
		u.Host = net.JoinHostPort(hostname, port)
	case bare:
		u.Host = net.JoinHostPort(hostname, "11434")
	default:
		u.Host = hostname
	}
	return u.String()
}
