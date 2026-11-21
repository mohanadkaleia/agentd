package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
)

func TestAPIRefusesWildcardBinds(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	token := filepath.Join(t.TempDir(), "token")
	for _, addr := range []string{":0", "0.0.0.0:0", "[::]:0", "[0::]:0"} {
		if stop, err := startAPI(context.Background(), nil, addr, token, log); err == nil {
			stop()
			t.Errorf("listen %q was accepted", addr)
		}
	}
	stop, err := startAPI(context.Background(), nil, "127.0.0.1:0", token, log)
	if err != nil {
		t.Fatalf("loopback refused: %v", err)
	}
	stop()
}

func TestShortEnvironmentTokenIsRejected(t *testing.T) {
	t.Setenv("AGENTD_API_TOKEN", "short")
	if _, err := loadOrCreateToken(filepath.Join(t.TempDir(), "unused")); err == nil {
		t.Fatal("a 5-character token was accepted")
	}
}
