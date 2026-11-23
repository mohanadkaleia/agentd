// Package mcptest is an in-process MCP server for tests: a small chat service whose
// failures, outages and tool definitions a test controls, with a record of every effect
// so tests can assert exactly-once behaviour.
package mcptest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Failure modes for the next post_message call.
const (
	Normal        = ""
	FailBefore    = "fail-before"
	ApplyThenFail = "apply-then-fail"
)

type Message struct {
	ID        string `json:"id"`
	Channel   string `json:"channel"`
	User      string `json:"user"`
	Text      string `json:"text"`
	TS        string `json:"ts"`
	ClientRef string `json:"client_ref,omitempty"`
}

type Chat struct {
	Server *mcp.Server

	mu       sync.Mutex
	messages []Message
	calls    map[string]int
	mode     string
	offline  bool
	sessions []*mcp.ServerSession
	seq      int
}

var object = map[string]any{"type": "object"}

func NewChat() *Chat {
	c := &Chat{Server: mcp.NewServer(&mcp.Implementation{Name: "chat", Version: "1"}, nil), calls: map[string]int{}}
	c.add("read_channel", "Recent messages in one channel.", c.readChannel)
	c.add("post_message", "Post a message to one channel.", c.postMessage)
	c.add("find_message", "Find a message by client_ref.", c.findMessage)
	c.add("delete_channel", "Delete a channel and every message in it.", c.deleteChannel)
	return c
}

func (c *Chat) add(name, description string, h func(map[string]any) (any, error)) {
	c.Server.AddTool(&mcp.Tool{Name: name, Description: description, InputSchema: object},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args map[string]any
			json.Unmarshal(req.Params.Arguments, &args)
			c.mu.Lock()
			c.calls[name]++
			c.mu.Unlock()
			out, err := h(args)
			if err != nil {
				return nil, err
			}
			b, _ := json.Marshal(out)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil
		})
}

func str(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

func (c *Chat) readChannel(args map[string]any) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []Message{}
	for _, m := range c.messages {
		if m.Channel == str(args, "channel_id") {
			out = append(out, m)
		}
	}
	return map[string]any{"messages": out}, nil
}

func (c *Chat) postMessage(args map[string]any) (any, error) {
	ref := ""
	if meta, ok := args["metadata"].(map[string]any); ok {
		ref, _ = meta["client_ref"].(string)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	mode := c.mode
	c.mode = Normal
	if mode == FailBefore {
		return nil, errors.New("connection reset")
	}
	m := c.append(str(args, "channel_id"), "agentd", str(args, "text"), ref)
	if mode == ApplyThenFail {
		return nil, errors.New("connection reset after the write")
	}
	return map[string]string{"id": m.ID}, nil
}

func (c *Chat) findMessage(args map[string]any) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	matches := []Message{}
	for _, m := range c.messages {
		if m.ClientRef != "" && m.ClientRef == str(args, "client_ref") {
			matches = append(matches, m)
		}
	}
	return map[string]any{"matches": matches}, nil
}

func (c *Chat) deleteChannel(args map[string]any) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	kept := c.messages[:0]
	for _, m := range c.messages {
		if m.Channel != str(args, "channel_id") {
			kept = append(kept, m)
		}
	}
	c.messages = kept
	return map[string]string{"deleted": str(args, "channel_id")}, nil
}

func (c *Chat) append(channel, user, text, ref string) Message {
	c.seq++
	m := Message{ID: fmt.Sprintf("m%d", c.seq), Channel: channel, User: user, Text: text, TS: fmt.Sprintf("%d", 1000+c.seq), ClientRef: ref}
	c.messages = append(c.messages, m)
	return m
}

// Say posts a message from someone else, as the outside world would.
func (c *Chat) Say(channel, user, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.append(channel, user, text, "")
}

// Next makes the next post_message fail in the given mode.
func (c *Chat) Next(mode string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mode = mode
}

// Posts returns agentd's own messages in a channel.
func (c *Chat) Posts(channel string) []Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Message
	for _, m := range c.messages {
		if m.Channel == channel && m.User == "agentd" {
			out = append(out, m)
		}
	}
	return out
}

// DuplicateRefs counts client references that appear on more than one message.
func (c *Chat) DuplicateRefs() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	seen, dups := map[string]int{}, 0
	for _, m := range c.messages {
		if m.ClientRef == "" {
			continue
		}
		seen[m.ClientRef]++
		if seen[m.ClientRef] == 2 {
			dups++
		}
	}
	return dups
}

func (c *Chat) Calls(tool string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[tool]
}

// Redefine replaces a tool's description, as a server update would.
func (c *Chat) Redefine(tool, description string) {
	handlers := map[string]func(map[string]any) (any, error){
		"read_channel": c.readChannel, "post_message": c.postMessage, "find_message": c.findMessage, "delete_channel": c.deleteChannel,
	}
	c.Server.RemoveTools(tool)
	c.add(tool, description, handlers[tool])
}

// ServeStdio runs the chat server on standard input and output until they close, for
// tests that start an MCP server as a child process.
func ServeStdio() {
	NewChat().Server.Run(context.Background(), &mcp.StdioTransport{})
}

// Offline drops every session and refuses new ones until Online.
func (c *Chat) Offline() {
	c.mu.Lock()
	c.offline = true
	sessions := c.sessions
	c.sessions = nil
	c.mu.Unlock()
	for _, ss := range sessions {
		ss.Close()
	}
}

func (c *Chat) Online() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offline = false
}

// Dial connects a client to this server in-process; it fails while the server is offline.
func (c *Chat) Dial(ctx context.Context, _ string) (mcp.Transport, error) {
	c.mu.Lock()
	offline := c.offline
	c.mu.Unlock()
	if offline {
		return nil, errors.New("connection refused")
	}
	client, server := mcp.NewInMemoryTransports()
	ss, err := c.Server.Connect(ctx, server, nil)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.sessions = append(c.sessions, ss)
	c.mu.Unlock()
	return client, nil
}
