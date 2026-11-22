package ollama

import "testing"

func TestBaseURLAcceptsOllamaHostFormats(t *testing.T) {
	for in, want := range map[string]string{
		"":                           "http://localhost:11434",
		"0.0.0.0":                    "http://127.0.0.1:11434",
		"0.0.0.0:11434":              "http://127.0.0.1:11434",
		"192.168.1.20":               "http://192.168.1.20:11434",
		"mini.local:8080":            "http://mini.local:8080",
		"http://localhost:11434/":    "http://localhost:11434",
		"https://ollama.example.com": "https://ollama.example.com",
	} {
		if got := baseURL(in); got != want {
			t.Errorf("baseURL(%q) = %q, want %q", in, got, want)
		}
	}
}
