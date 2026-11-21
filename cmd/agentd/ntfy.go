package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"time"

	"agentd/internal/assignments"
	"agentd/internal/integrations/ntfy"
	"agentd/internal/notify"
)

const ntfyKeychainAccount = "ntfy"

type ntfyConfig struct {
	TopicURL    string `json:"topic_url"`
	Token       string `json:"token,omitempty"`
	FullContent bool   `json:"full_content,omitempty"`
}

func loadNtfy() (*ntfy.Channel, bool) {
	raw, ok := keychainLoadHex("agentd", ntfyKeychainAccount)
	if !ok {
		return nil, false
	}
	var c ntfyConfig
	if err := json.Unmarshal(raw, &c); err != nil || c.TopicURL == "" {
		return nil, false
	}
	return &ntfy.Channel{HTTP: &http.Client{Timeout: 20 * time.Second}, TopicURL: c.TopicURL, Token: c.Token, FullContent: c.FullContent}, true
}

func ntfyCommand(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: agentd ntfy setup [-server URL] [-topic NAME] [-full] | test | off")
	}
	switch args[0] {
	case "setup":
		fs := flag.NewFlagSet("ntfy setup", flag.ContinueOnError)
		server := fs.String("server", "https://ntfy.sh", "ntfy server")
		topic := fs.String("topic", "", "topic name (default: a random unguessable one)")
		token := fs.String("token", "", "access token, for a server that requires one")
		full := fs.Bool("full", false, "include the notification's title and text (default: only that something needs attention)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *topic == "" {
			b := make([]byte, 16)
			rand.Read(b)
			*topic = "baseet-" + hex.EncodeToString(b)
		}
		base, err := url.Parse(*server)
		if err != nil || base.Scheme != "https" || base.Host == "" {
			return errors.New("-server must be an https URL")
		}
		cfg := ntfyConfig{TopicURL: base.JoinPath(*topic).String(), Token: *token, FullContent: *full}
		raw, _ := json.Marshal(cfg)
		if err := keychainStoreHex("agentd", ntfyKeychainAccount, raw); err != nil {
			return err
		}
		fmt.Printf("Saved to the login keychain. In the ntfy phone app, subscribe to:\n\n  %s\n\n", cfg.TopicURL)
		fmt.Println("Treat that topic name like a password: anyone who knows it can read what is sent to it.")
		fmt.Println("Then run `agentd ntfy test`, and restart `agentd serve` to start delivering.")
		return nil
	case "test":
		ch, ok := loadNtfy()
		if !ok {
			return errors.New("ntfy is not set up; run `agentd ntfy setup`")
		}
		ref, err := ch.Send(ctx, notify.Message{ID: "test", Assignment: "A test from agentd", Severity: assignments.SeverityAttention,
			Title: "Test notification", Body: "If you can read this, delivery works."})
		if err != nil {
			return err
		}
		fmt.Println("sent", ref)
		return nil
	case "off":
		out, err := exec.Command("/usr/bin/security", "delete-generic-password", "-s", "agentd", "-a", ntfyKeychainAccount).CombinedOutput()
		if err != nil {
			return fmt.Errorf("keychain: %v: %s", err, out)
		}
		fmt.Println("ntfy delivery turned off; restart `agentd serve` to apply.")
		return nil
	}
	return fmt.Errorf("unknown ntfy command %q", args[0])
}
