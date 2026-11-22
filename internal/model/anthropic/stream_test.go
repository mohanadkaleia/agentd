package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"

	"agentd/internal/model"
)

const sameTurn = `{
  "id": "msg_1", "type": "message", "role": "assistant", "model": "claude-opus-5-5",
  "content": [
    {"type": "text", "text": "Checking both."},
    {"type": "text", "text": "Second block."},
    {"type": "tool_use", "id": "toolu_1", "name": "web_search", "input": {"query": "refund"}}
  ],
  "stop_reason": "tool_use", "stop_sequence": null,
  "usage": {"input_tokens": 120, "output_tokens": 40}
}`

var sameTurnEvents = []string{
	`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":120,"output_tokens":1}}}`,
	`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Check"}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ing both."}}`,
	`{"type":"content_block_stop","index":0}`,
	`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
	`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Second block."}}`,
	`{"type":"content_block_stop","index":1}`,
	`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"web_search","input":{}}}`,
	`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"query\": "}}`,
	`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"refund\"}"}}`,
	`{"type":"content_block_stop","index":2}`,
	`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":40}}`,
	`{"type":"message_stop"}`,
}

func TestStreamReturnsWhatCompleteReturns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] != true {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, sameTurn)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range sameTurnEvents {
			var head struct{ Type string }
			json.Unmarshal([]byte(ev), &head)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", head.Type, ev)
		}
	}))
	defer srv.Close()
	p := New(Config{Options: []option.RequestOption{option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0)}})
	req := model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: "hi"}}}
	whole, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	var deltas []string
	streamed, err := p.Stream(context.Background(), req, func(d model.Delta) { deltas = append(deltas, d.Text) })
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(deltas, ""); got != whole.Message.Content || len(deltas) < 3 {
		t.Fatalf("deltas %q join to %q, want %q", deltas, got, whole.Message.Content)
	}
	if streamed.Message.Content != whole.Message.Content || streamed.StopReason != whole.StopReason ||
		streamed.Usage != whole.Usage || streamed.Model != whole.Model {
		t.Fatalf("stream %+v\ncomplete %+v", streamed, whole)
	}
	if len(streamed.Message.ToolCalls) != 1 || streamed.Message.ToolCalls[0].ID != "toolu_1" || streamed.Message.ToolCalls[0].Name != "web_search" {
		t.Fatalf("tool calls = %+v", streamed.Message.ToolCalls)
	}
	var a, b any
	json.Unmarshal(streamed.Message.ToolCalls[0].Arguments, &a)
	json.Unmarshal(whole.Message.ToolCalls[0].Arguments, &b)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("arguments %s vs %s", streamed.Message.ToolCalls[0].Arguments, whole.Message.ToolCalls[0].Arguments)
	}
	var na, nb any
	json.Unmarshal(streamed.Message.Native.Payload, &na)
	json.Unmarshal(whole.Message.Native.Payload, &nb)
	if !reflect.DeepEqual(na, nb) {
		t.Fatalf("native payload differs:\n%s\n%s", streamed.Message.Native.Payload, whole.Message.Native.Payload)
	}
}
