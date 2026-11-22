// Package mcp registers the tools of Model Context Protocol servers as agentd tools.
//
// It uses the official Go SDK (github.com/modelcontextprotocol/go-sdk) rather than a
// hand-written client: remote servers need the Streamable HTTP transport, sessions and
// OAuth, which are easy to get subtly wrong. Only this package may import the SDK
// (internal/archtest).
//
// Each remote tool is registered as <server>__<tool>. Server names never contain a
// double underscore and no built-in or other integration tool name does, so an MCP
// tool can never shadow another tool. Installing a server grants nothing: a tool runs
// only when an assignment's policy has a rule for it, like every other tool.
package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"agentd/internal/policy"
)

type Config struct {
	Servers map[string]ServerConfig `json:"servers"`
}

type ServerConfig struct {
	Command     string            `json:"command"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Dir         string            `json:"dir,omitempty"`
	CallTimeout policy.Duration   `json:"call_timeout,omitzero"`
	Tools       ToolFilter        `json:"tools,omitzero"`
}

type ToolFilter struct {
	Include []string `json:"include,omitempty"`
	Exclude []string `json:"exclude,omitempty"`
}

const defaultCallTimeout = 60 * time.Second

var (
	serverName = regexp.MustCompile(`^[a-z0-9]([a-z0-9_-]*[a-z0-9])?$`)
	envName    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

func Load(path string) (Config, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Config{}, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.validate(info.Mode().Perm()); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func (c Config) validate(mode os.FileMode) error {
	if mode&0o022 != 0 {
		return fmt.Errorf("must not be writable by group or others (mode %o): it names programs agentd runs", mode)
	}
	for name, s := range c.Servers {
		if !serverName.MatchString(name) || strings.Contains(name, "__") {
			return fmt.Errorf("server name %q: use lowercase letters, digits, - and single _ (not first or last)", name)
		}
		if !filepath.IsAbs(s.Command) {
			return fmt.Errorf("server %q: command must be an absolute path, got %q", name, s.Command)
		}
		if s.Dir != "" && !filepath.IsAbs(s.Dir) {
			return fmt.Errorf("server %q: dir must be an absolute path", name)
		}
		if len(s.Env) > 0 && mode&0o044 != 0 {
			return fmt.Errorf("server %q sets env, which may hold secrets: make the file readable by its owner only (chmod 600)", name)
		}
		for k := range s.Env {
			if !envName.MatchString(k) {
				return fmt.Errorf("server %q: invalid env name %q", name, k)
			}
		}
		if s.CallTimeout.Duration < 0 {
			return errors.New("call_timeout must be positive")
		}
	}
	return nil
}

func (f ToolFilter) allows(tool string) bool {
	if len(f.Include) > 0 && !contains(f.Include, tool) {
		return false
	}
	return !contains(f.Exclude, tool)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
