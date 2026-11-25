package chat

import (
	"strings"
	"testing"
)

func TestModelTroubleSaysWhatToDo(t *testing.T) {
	for cause, want := range map[string]string{
		`ollama: Post "http://127.0.0.1:11434/api/chat": dial tcp 127.0.0.1:11434: connect: connection refused`: "ollama serve",
		`ollama: 404 Not Found: {"error":"model 'x' not found"}`:                                                "ollama pull",
		`anthropic: 401 Unauthorized: invalid x-api-key`:                                                        "agentd models",
		`anthropic: 429 Too Many Requests`:                                                                      "Wait a minute",
		`something else`:                                                                                        "(something else)",
	} {
		if got := modelTrouble(cause); !strings.Contains(got, want) || !strings.Contains(got, cause) {
			t.Fatalf("modelTrouble(%q) = %q, want %q and the cause", cause, got, want)
		}
	}
}
