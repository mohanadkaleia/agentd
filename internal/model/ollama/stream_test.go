package ollama

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"agentd/internal/model"
)

const whole = `{"message":{"role":"assistant","content":"Checking both refunds.","tool_calls":[{"function":{"name":"web_search","arguments":{"query":"refund"}}}]},"done":true,"done_reason":"stop","prompt_eval_count":50,"eval_count":12}`

var chunks = []string{
	`{"message":{"role":"assistant","content":"Checking "},"done":false}`,
	`{"message":{"role":"assistant","content":"both "},"done":false}`,
	`{"message":{"role":"assistant","content":"refunds."},"done":false}`,
	`{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"web_search","arguments":{"query":"refund"}}}]},"done":false}`,
	`{"message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":50,"eval_count":12}`,
}

func TestStreamReturnsWhatCompleteReturns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		json.NewDecoder(r.Body).Decode(&req)
		if !req.Stream {
			fmt.Fprint(w, whole)
			return
		}
		for _, c := range chunks {
			fmt.Fprintln(w, c)
		}
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "qwen2.5:7b"})
	req := model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: "hi"}}}
	want, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	var deltas []string
	got, err := p.Stream(context.Background(), req, func(d model.Delta) { deltas = append(deltas, d.Text) })
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stream %+v\ncomplete %+v", got, want)
	}
	if strings.Join(deltas, "") != want.Message.Content || len(deltas) != 3 {
		t.Fatalf("deltas = %q", deltas)
	}
}

func TestStreamThatStopsEarlyIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, chunks[0])
	}))
	defer srv.Close()
	_, err := New(Config{BaseURL: srv.URL, Model: "m"}).Stream(context.Background(),
		model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: "x"}}}, func(model.Delta) {})
	if err == nil || !strings.Contains(err.Error(), "before the reply was complete") {
		t.Fatalf("err = %v", err)
	}
}

// TestStreamAgainstLocalOllama runs only when AGENTD_OLLAMA_TEST names a pulled model.
func TestStreamAgainstLocalOllama(t *testing.T) {
	name := os.Getenv("AGENTD_OLLAMA_TEST")
	if name == "" {
		t.Skip("set AGENTD_OLLAMA_TEST=<model> to run against a local Ollama")
	}
	p := New(Config{Model: name})
	var deltas []string
	resp, err := p.Stream(context.Background(), model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: "Say hello in five words."}}},
		func(d model.Delta) { deltas = append(deltas, d.Text) })
	if err != nil {
		t.Fatal(err)
	}
	if len(deltas) < 2 || strings.Join(deltas, "") != resp.Message.Content || resp.Usage.OutputTokens == 0 {
		t.Fatalf("%d deltas, reply %q, usage %+v", len(deltas), resp.Message.Content, resp.Usage)
	}
}
