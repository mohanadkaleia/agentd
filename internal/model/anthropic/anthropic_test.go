package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"

	"agentd/internal/model"
)

const toolTurn = `{
  "id": "msg_1", "type": "message", "role": "assistant", "model": "claude-opus-5-5",
  "content": [
    {"type": "thinking", "thinking": "", "signature": "sig-abc"},
    {"type": "text", "text": "Checking both."},
    {"type": "tool_use", "id": "toolu_1", "name": "web_search", "input": {"query": "refund"}},
    {"type": "tool_use", "id": "toolu_2", "name": "web_search", "input": {"query": "claims"}}
  ],
  "stop_reason": "tool_use", "stop_sequence": null,
  "usage": {"input_tokens": 120, "output_tokens": 40}
}`

const finalTurn = `{
  "id": "msg_2", "type": "message", "role": "assistant", "model": "claude-opus-5-5",
  "content": [{"type": "text", "text": "Done."}],
  "stop_reason": "end_turn", "stop_sequence": null,
  "usage": {"input_tokens": 200, "output_tokens": 5}
}`

type fakeAPI struct {
	replies  []string
	requests []map[string]any
	headers  []http.Header
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	json.Unmarshal(body, &req)
	f.requests = append(f.requests, req)
	f.headers = append(f.headers, r.Header.Clone())
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, f.replies[0])
	f.replies = f.replies[1:]
}

func newTestProvider(t *testing.T, api *fakeAPI) *Provider {
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	return New(Config{Options: []option.RequestOption{
		option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0),
	}})
}

func TestRoundTripPreservesNativeBlocksAndMergesToolResults(t *testing.T) {
	api := &fakeAPI{replies: []string{toolTurn, finalTurn}}
	p := newTestProvider(t, api)
	ctx := context.Background()
	tools := []model.ToolDefinition{{
		Name: "web_search", Description: "search",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
	}}
	msgs := []model.Message{
		{Role: model.RoleSystem, Content: "be careful"},
		{Role: model.RoleUser, Content: "get my refund"},
	}

	first, err := p.Complete(ctx, model.Request{Messages: msgs, Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	if first.StopReason != model.StopToolUse || len(first.Message.ToolCalls) != 2 {
		t.Fatalf("first response = %+v", first)
	}
	if got := string(first.Message.ToolCalls[0].Arguments); got != `{"query": "refund"}` && got != `{"query":"refund"}` {
		t.Fatalf("tool arguments = %s", got)
	}
	if first.Usage.InputTokens != 120 {
		t.Fatalf("usage = %+v", first.Usage)
	}

	persisted, _ := json.Marshal(first.Message)
	var restored model.Message
	json.Unmarshal(persisted, &restored)
	msgs = append(msgs, restored,
		model.Message{Role: model.RoleTool, ToolCallID: "toolu_1", Content: `{"results":["a"]}`},
		model.Message{Role: model.RoleTool, ToolCallID: "toolu_2", Content: `{"error":"denied"}`},
	)
	second, err := p.Complete(ctx, model.Request{Messages: msgs, Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	if second.StopReason != model.StopEnd || second.Message.Content != "Done." {
		t.Fatalf("second response = %+v", second)
	}

	req := api.requests[1]
	if req["model"] != DefaultModel || req["fallbacks"] != "default" {
		t.Fatalf("model=%v fallbacks=%v", req["model"], req["fallbacks"])
	}
	if !strings.Contains(api.headers[1].Get("anthropic-beta"), "server-side-fallback-2026-07-01") {
		t.Fatalf("beta header = %q", api.headers[1].Get("anthropic-beta"))
	}
	system := req["system"].([]any)
	if system[0].(map[string]any)["text"] != "be careful" {
		t.Fatalf("system = %v", system)
	}
	messages := req["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages = %d, want user/assistant/user", len(messages))
	}
	assistant := messages[1].(map[string]any)["content"].([]any)
	if b := assistant[0].(map[string]any); b["type"] != "thinking" || b["signature"] != "sig-abc" {
		t.Fatalf("thinking block not replayed verbatim: %v", b)
	}
	results := messages[2].(map[string]any)["content"].([]any)
	if len(results) != 2 || results[0].(map[string]any)["type"] != "tool_result" {
		t.Fatalf("tool results not merged into one user turn: %v", results)
	}
}

func TestForeignAssistantMessagesAreRebuilt(t *testing.T) {
	_, msgs, err := toMessages([]model.Message{
		{Role: model.RoleUser, Content: "hi"},
		{Role: model.RoleAssistant, Native: &model.Native{Provider: "ollama", Payload: json.RawMessage(`{}`)},
			ToolCalls: []model.ToolCall{{ID: "c1", Name: "web_search", Arguments: json.RawMessage(`"not an object"`)}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(msgs[1])
	if !strings.Contains(string(body), `"tool_use"`) || !strings.Contains(string(body), `"invalid_arguments":"not an object"`) {
		t.Fatalf("rebuilt assistant = %s", body)
	}
}

func TestRefusalMapsToNeutralStop(t *testing.T) {
	api := &fakeAPI{replies: []string{`{"id":"m","type":"message","role":"assistant","model":"claude-opus-5-5",
		"content":[],"stop_reason":"refusal","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":0}}`}}
	resp, err := newTestProvider(t, api).Complete(context.Background(), model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != model.StopRefusal {
		t.Fatalf("stop = %s, want refusal", resp.StopReason)
	}
}
