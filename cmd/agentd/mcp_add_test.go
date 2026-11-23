package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentd/internal/integrations/mcp"
	"agentd/internal/integrations/mcp/mcptest"
	"agentd/internal/testutil"
)

// The test binary doubles as a local MCP server, the way `npx some-server` would be one.
func TestMain(m *testing.M) {
	if os.Getenv("AGENTD_FAKE_MCP") == "1" {
		mcptest.ServeStdio()
		return
	}
	os.Exit(m.Run())
}

func scriptedConsole(answers, secret string) (mcpConsole, *bytes.Buffer) {
	var out bytes.Buffer
	return mcpConsole{in: bufio.NewReader(strings.NewReader(answers)), out: &out, interactive: true,
		secret: func(string) (string, error) { return secret, nil }}, &out
}

func TestMCPAddStoresSecretsReviewsToolsAndRemoveUndoesIt(t *testing.T) {
	ctx := context.Background()
	db := testutil.Open(t, testutil.DBPath(t))
	dir := t.TempDir()
	secrets := filepath.Join(dir, "secrets")
	os.Mkdir(secrets, 0o700)
	t.Setenv("AGENTD_SECRETS_DIR", secrets)
	path := defaultMCPConfig(dir)

	o, err := parseMCPAdd([]string{"chat", "-env", "AGENTD_FAKE_MCP=1", "-env", "CHAT_TOKEN", "--", os.Args[0], "-test.run=^$"})
	if err != nil {
		t.Fatal(err)
	}
	// Tools come in name order: delete_channel, find_message, post_message, read_channel.
	console, out := scriptedConsole("s\nd\nr\nw\nr\n", `s3cret "quoted"`)
	if err := mcpAdd(ctx, db, dir, path, o, console); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	if strings.Contains(out.String(), "s3cret") {
		t.Fatalf("the secret was printed:\n%s", out)
	}
	info, _ := os.Stat(path)
	body, _ := os.ReadFile(path)
	if info.Mode().Perm() != 0o600 || strings.Contains(string(body), "s3cret") || !strings.Contains(string(body), `"keychain": "agentd/chat-chat-token"`) {
		t.Fatalf("config mode %o:\n%s", info.Mode().Perm(), body)
	}
	if stored, _ := os.ReadFile(filepath.Join(secrets, "chat-chat-token")); string(stored) != `s3cret "quoted"` {
		t.Fatalf("stored secret = %q", stored)
	}
	cfg, err := mcp.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Servers["chat"]
	if !filepath.IsAbs(s.Command) || s.Env["AGENTD_FAKE_MCP"].Literal != "1" {
		t.Fatalf("server = %+v", s)
	}
	if s.Tools.Meta["find_message"].Effect != "read" || s.Tools.Meta["read_channel"].Effect != "read" || s.Tools.Meta["post_message"].Effect != "" {
		t.Fatalf("declared effects = %+v", s.Tools.Meta)
	}
	status := map[string]string{}
	pins, _ := mcp.ListPins(ctx, db, "chat")
	for _, p := range pins {
		status[p.Tool] = p.Status
	}
	if status["delete_channel"] != mcp.StatusNew || status["find_message"] != mcp.StatusAccepted ||
		status["post_message"] != mcp.StatusAccepted || status["read_channel"] != mcp.StatusAccepted {
		t.Fatalf("tool statuses = %v (a skipped tool must stay unaccepted)\n%s", status, out)
	}
	if !strings.Contains(out.String(), "1 tool(s) are not accepted") {
		t.Fatalf("output does not say what is left to review:\n%s", out)
	}

	if err := mcpAdd(ctx, db, dir, path, o, console); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("adding the same name twice = %v", err)
	}
	quiet, _ := scriptedConsole("", "")
	quiet.interactive = false
	if err := mcpRemove(ctx, db, path, "chat", false, quiet); err == nil {
		t.Fatal("remove without a terminal and without -yes must refuse")
	}
	if err := mcpRemove(ctx, db, path, "chat", true, quiet); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := mcp.Load(path); len(cfg.Servers) != 0 {
		t.Fatalf("servers after remove = %v", cfg.Servers)
	}
	if _, err := os.Stat(filepath.Join(secrets, "chat-chat-token")); !os.IsNotExist(err) {
		t.Fatal("remove left the stored secret behind")
	}
}

func TestMCPAddSavesNothingWhenTheServerDoesNotAnswer(t *testing.T) {
	ctx := context.Background()
	db := testutil.Open(t, testutil.DBPath(t))
	dir := t.TempDir()
	secrets := filepath.Join(dir, "secrets")
	os.Mkdir(secrets, 0o700)
	t.Setenv("AGENTD_SECRETS_DIR", secrets)
	o, _ := parseMCPAdd([]string{"broken", "-env", "API_KEY", "--", "/usr/bin/false"})
	console, _ := scriptedConsole("", "value")
	err := mcpAdd(ctx, db, dir, defaultMCPConfig(dir), o, console)
	if err == nil || !strings.Contains(err.Error(), "nothing was saved") {
		t.Fatalf("add of a server that exits = %v", err)
	}
	if _, err := os.Stat(defaultMCPConfig(dir)); !os.IsNotExist(err) {
		t.Fatal("a config was written for a server that never answered")
	}
	if left, _ := os.ReadDir(secrets); len(left) != 0 {
		t.Fatalf("a secret was left behind: %v", left)
	}
}

func TestMCPAddArguments(t *testing.T) {
	o, err := parseMCPAdd([]string{"notes", "https://mcp.example.com/mcp", "-oauth", "-client-id", "abc", "-scopes", "a,b", "-redirect-port", "53682"})
	if err != nil || o.url != "https://mcp.example.com/mcp" || !o.oauth || o.clientID != "abc" || len(o.scopes) != 2 || o.redirectPort != 53682 {
		t.Fatalf("hosted server: %+v, %v", o, err)
	}
	o, err = parseMCPAdd([]string{"gh", "--", "npx", "-y", "pkg", "-env", "X"})
	if err != nil || strings.Join(o.command, " ") != "npx -y pkg -env X" || len(o.env) != 0 {
		t.Fatalf("flags after -- belong to the server's command: %+v, %v", o, err)
	}
	for _, bad := range [][]string{
		{}, {"name"}, {"name", "https://x.example", "--", "cmd"}, {"name", "https://x.example", "-env", "K"},
		{"name", "-token", "--", "cmd"}, {"name", "https://x.example", "-token", "-oauth"}, {"name", "https://x.example", "-wat"},
	} {
		if _, err := parseMCPAdd(bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	dir := t.TempDir()
	db := testutil.Open(t, testutil.DBPath(t))
	console, _ := scriptedConsole("", "")
	o, _ = parseMCPAdd([]string{"plain", "http://example.com/mcp"})
	if err := mcpAdd(context.Background(), db, dir, defaultMCPConfig(dir), o, console); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("a plain http server on another host = %v", err)
	}
}

func TestMCPPresetsNeedOnlyTheOwnersClient(t *testing.T) {
	if _, err := parseMCPAdd([]string{"slack"}); err == nil || !strings.Contains(err.Error(), "-client-id") {
		t.Fatalf("a preset without a client id = %v, want the setup steps", err)
	}
	o, err := parseMCPAdd([]string{"gmail", "-client-id", "abc", "-client-secret"})
	if err != nil || o.preset == nil || !o.oauth || o.url != "https://gmailmcp.googleapis.com/mcp/v1" {
		t.Fatalf("gmail preset: %+v, %v", o, err)
	}
	for name, p := range mcpPresets {
		if err := (mcp.Config{Servers: map[string]mcp.ServerConfig{name: p.server}}).Check(); err != nil {
			t.Errorf("preset %s is not a valid server: %v", name, err)
		}
		for tool, m := range p.server.Tools.Meta {
			if !mcp.ValidToolName(name, m.Name) {
				t.Errorf("preset %s names %s as %q, which models cannot use", name, tool, m.Name)
			}
		}
	}
	// A name that is not a preset still needs a URL or a command.
	if _, err := parseMCPAdd([]string{"notion"}); err == nil {
		t.Fatal("an unknown name alone was accepted")
	}
	var out bytes.Buffer
	mcpPresetList(&out)
	if !strings.Contains(out.String(), "slack:") || !strings.Contains(out.String(), "gmail:") {
		t.Fatalf("presets:\n%s", out.String())
	}
}

func TestMCPConfigChangeIsNoticedOnlyWhenValid(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db := testutil.Open(t, testutil.DBPath(t))
	path := filepath.Join(t.TempDir(), "mcp.json")
	changed := make(chan struct{})
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	go watchMCPConfig(ctx, db, path, fileSignature(path), 10*time.Millisecond, time.Second, quiet, func() { close(changed) })
	os.WriteFile(path, []byte(`{"servers":{"Bad Name":{"command":"/bin/cat"}}}`), 0o600)
	select {
	case <-changed:
		t.Fatal("an invalid config triggered a reload")
	case <-time.After(150 * time.Millisecond):
	}
	if err := mcp.Save(path, mcp.Config{Servers: map[string]mcp.ServerConfig{"chat": {Command: "/bin/cat"}}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("a valid change was not noticed")
	}
}
