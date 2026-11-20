package ollama

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"agentd/internal/model"
)

func TestCompleteTranslatesToolTurn(t *testing.T) {
	var got chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("path = %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		io.WriteString(w, `{"message":{"role":"assistant","content":"",
			"tool_calls":[{"function":{"name":"send_email","arguments":{"to":"a@b.c","subject":"s","body":"b"}}}]},
			"done":true,"done_reason":"stop","prompt_eval_count":50,"eval_count":12}`)
	}))
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL, Model: "qwen2.5:7b"})
	resp, err := p.Complete(context.Background(), model.Request{
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: "sys"},
			{Role: model.RoleUser, Content: "go"},
			{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "x1", Name: "web_search", Arguments: json.RawMessage(`{"query":"q"}`)}},
				Native: &model.Native{Provider: "anthropic", Payload: json.RawMessage(`{"ignored":true}`)}},
			{Role: model.RoleTool, ToolCallID: "x1", Content: `{"results":[]}`},
		},
		Tools: []model.ToolDefinition{{Name: "web_search", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if got.Model != "qwen2.5:7b" || got.Stream || len(got.Tools) != 1 || got.Tools[0].Type != "function" {
		t.Fatalf("request = %+v", got)
	}
	if got.Messages[3].Role != "tool" || got.Messages[3].ToolName != "web_search" {
		t.Fatalf("tool message = %+v, want tool_name web_search", got.Messages[3])
	}
	if resp.StopReason != model.StopToolUse || len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("response = %+v", resp)
	}
	call := resp.Message.ToolCalls[0]
	if call.ID != "call_1" || call.Name != "send_email" || !json.Valid(call.Arguments) {
		t.Fatalf("tool call = %+v", call)
	}
	if resp.Usage.InputTokens != 50 || resp.Usage.OutputTokens != 12 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

func TestCompleteSurfacesHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"model not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()
	_, err := New(Config{BaseURL: srv.URL, Model: "nope"}).Complete(context.Background(),
		model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: "x"}}})
	if err == nil {
		t.Fatal("expected error for 404")
	}
}
