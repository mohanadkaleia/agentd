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
	"net"
	"net/url"
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
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]Secret `json:"env,omitempty"`
	Dir         string            `json:"dir,omitempty"`
	URL         string            `json:"url,omitempty"`
	Auth        *Auth             `json:"auth,omitempty"`
	CallTimeout policy.Duration   `json:"call_timeout,omitzero"`
	Tools       ToolFilter        `json:"tools,omitzero"`

	MaxConcurrent int    `json:"max_concurrent,omitempty"`
	Disabled      bool   `json:"disabled,omitempty"`
	Polls         []Poll `json:"poll,omitempty"`
}

// Poll turns new items from a read tool into inbox events, with no model involved. It
// calls the tool every interval, emits items it has not seen before (none on the first
// poll), and maps only the named fields into the event payload.
type Poll struct {
	Name      string            `json:"name,omitempty"`
	Every     policy.Duration   `json:"every"`
	Tool      string            `json:"tool"`
	Args      map[string]any    `json:"args,omitempty"`
	Items     string            `json:"items"`
	ID        string            `json:"id"`
	Event     PollEvent         `json:"event"`
	Payload   map[string]string `json:"payload,omitempty"`
	Exclude   map[string]string `json:"exclude,omitempty"`
	MaxEvents int               `json:"max_events,omitempty"`
}

type PollEvent struct {
	Source string `json:"source"`
	Kind   string `json:"kind"`
}

func (p Poll) key() string {
	if p.Name != "" {
		return p.Name
	}
	return p.Tool
}

// Auth is how agentd authenticates to a remote server: "bearer" with a stored token, or
// "oauth", signed in once with `agentd mcp auth <server>`.
type Auth struct {
	Type         string   `json:"type"`
	Token        *Secret  `json:"token,omitempty"`
	ClientID     string   `json:"client_id,omitempty"`
	ClientSecret *Secret  `json:"client_secret,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
}

// Secret is a literal value or a reference: {"keychain": "service/account"} or {"file": "/abs/path"}.
type Secret struct {
	Literal  string
	Keychain string `json:"keychain,omitempty"`
	File     string `json:"file,omitempty"`
}

func (s *Secret) UnmarshalJSON(b []byte) error {
	if json.Unmarshal(b, &s.Literal) == nil {
		return nil
	}
	type ref Secret
	var r ref
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return fmt.Errorf(`a secret is a string or {"keychain": "service/account"} or {"file": "/path"}: %w`, err)
	}
	*s = Secret(r)
	if (s.Keychain == "") == (s.File == "") {
		return errors.New(`a secret reference names exactly one of "keychain" or "file"`)
	}
	return nil
}

func (s Secret) reference() bool { return s.Keychain != "" || s.File != "" }

type ToolFilter struct {
	Include []string            `json:"include,omitempty"`
	Exclude []string            `json:"exclude,omitempty"`
	Meta    map[string]ToolMeta `json:"meta,omitempty"`
}

// ToolMeta is what the owner declares about a tool, because MCP cannot say it with
// authority. Declaring a write as a read is unsafe: after a crash mid-call agentd runs a
// read again.
type ToolMeta struct {
	Effect     string         `json:"effect,omitempty"`
	Resource   string         `json:"resource,omitempty"`
	Idempotent bool           `json:"idempotent,omitempty"`
	Reconcile  *ReconcileRule `json:"reconcile,omitempty"`
}

// ReconcileRule lets agentd find out whether an interrupted write happened. agentd puts a
// marker derived from the action into one argument, and after a crash or an ambiguous
// error calls a read tool on the same server to look for it. A miss is not proof; only
// MissIsAuthoritativeAfter, when the owner knows the lookup is consistent, makes it one.
type ReconcileRule struct {
	Marker struct {
		Arg string `json:"arg"`
	} `json:"marker"`
	Lookup struct {
		Tool       string         `json:"tool"`
		Args       map[string]any `json:"args"`
		FoundWhen  string         `json:"found_when"`
		ExternalID string         `json:"external_id,omitempty"`
	} `json:"lookup"`
	MissIsAuthoritativeAfter policy.Duration `json:"miss_is_authoritative_after,omitzero"`
}

const defaultCallTimeout = 60 * time.Second

var (
	serverName = regexp.MustCompile(`^[a-z0-9]([a-z0-9_-]*[a-z0-9])?$`)
	envName    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	argPath    = regexp.MustCompile(`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$`)
	secretish  = regexp.MustCompile(`(?i)token|secret|key|password|passwd|credential|auth|cookie`)
	keychainID = regexp.MustCompile(`^[a-z0-9.-]+/[a-z0-9-]+$`)
)

func validatePoll(s ServerConfig, p Poll) error {
	switch {
	case p.Every.Duration < 10*time.Second:
		return errors.New(`every must be at least "10s"`)
	case !s.Tools.allows(p.Tool) || s.Tools.Meta[p.Tool].Effect != "read":
		return fmt.Errorf("tool %q must be included and declared \"effect\": \"read\": a poll calls it without the gateway", p.Tool)
	case !argPath.MatchString(p.Items) || !argPath.MatchString(p.ID):
		return errors.New(`items and id are paths into the result, like "messages" and "ts"`)
	case !serverName.MatchString(p.Event.Source) || p.Event.Kind == "":
		return errors.New("event.source (lowercase) and event.kind are required")
	case p.Name != "" && !serverName.MatchString(p.Name):
		return fmt.Errorf("poll name %q: lowercase letters, digits, - and _", p.Name)
	case p.MaxEvents < 0:
		return errors.New("max_events must be positive")
	}
	for field, path := range p.Payload {
		if !envName.MatchString(field) || !argPath.MatchString(path) {
			return fmt.Errorf("payload %q: %q must be a path into the item", field, path)
		}
	}
	for field := range p.Exclude {
		if _, ok := p.Payload[field]; !ok {
			return fmt.Errorf("exclude %q must name a payload field", field)
		}
	}
	return nil
}

func validateRecovery(s ServerConfig, tool string, m ToolMeta) error {
	if (m.Idempotent || m.Reconcile != nil) && m.Effect == "read" {
		return errors.New("idempotent and reconcile apply to writes; a read is always safe to run again")
	}
	if m.Idempotent && m.Reconcile != nil {
		return errors.New("choose idempotent or reconcile, not both")
	}
	r := m.Reconcile
	if r == nil {
		return nil
	}
	if !argPath.MatchString(r.Marker.Arg) {
		return errors.New(`reconcile.marker.arg must name an argument, like "metadata.client_ref"`)
	}
	if r.Marker.Arg == m.Resource {
		return errors.New("reconcile.marker.arg cannot be the policy resource argument")
	}
	if r.Lookup.Tool == tool || s.Tools.Meta[r.Lookup.Tool].Effect != "read" || !s.Tools.allows(r.Lookup.Tool) {
		return fmt.Errorf("reconcile.lookup.tool %q must be another tool of this server, included and declared \"effect\": \"read\"", r.Lookup.Tool)
	}
	if !argPath.MatchString(r.Lookup.FoundWhen) || (r.Lookup.ExternalID != "" && !argPath.MatchString(r.Lookup.ExternalID)) {
		return errors.New(`reconcile.lookup.found_when and external_id are paths into the lookup result, like "matches.0.id"`)
	}
	if !mentionsMarker(r.Lookup.Args) {
		return errors.New(`reconcile.lookup.args must pass "{{marker}}" somewhere`)
	}
	if r.MissIsAuthoritativeAfter.Duration < 0 {
		return errors.New("miss_is_authoritative_after must be positive")
	}
	return nil
}

func mentionsMarker(v any) bool {
	switch v := v.(type) {
	case string:
		return v == "{{marker}}"
	case map[string]any:
		for _, inner := range v {
			if mentionsMarker(inner) {
				return true
			}
		}
	case []any:
		for _, inner := range v {
			if mentionsMarker(inner) {
				return true
			}
		}
	}
	return false
}

func validateRemote(name string, s ServerConfig) error {
	u, err := url.Parse(s.URL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("server %q: invalid url %q", name, s.URL)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback(u.Hostname())) {
		return fmt.Errorf("server %q: url must be https (plain http only for localhost)", name)
	}
	if u.User != nil {
		return fmt.Errorf("server %q: put credentials in auth, not the url", name)
	}
	if s.Args != nil || s.Env != nil || s.Dir != "" {
		return fmt.Errorf("server %q: args, env and dir apply to local servers", name)
	}
	a := s.Auth
	if a == nil {
		return nil
	}
	switch a.Type {
	case "bearer":
		if a.Token == nil || !a.Token.reference() {
			return fmt.Errorf(`server %q: auth.token must reference a stored secret, like {"keychain": "agentd/%s-token"}`, name, name)
		}
		return a.Token.validate()
	case "oauth":
		if a.ClientSecret == nil {
			return nil
		}
		if !a.ClientSecret.reference() {
			return fmt.Errorf("server %q: auth.client_secret must reference a stored secret, not a literal", name)
		}
		if a.ClientID == "" {
			return fmt.Errorf("server %q: auth.client_secret needs auth.client_id", name)
		}
		return a.ClientSecret.validate()
	}
	return fmt.Errorf(`server %q: auth.type must be "bearer" or "oauth"`, name)
}

func (s Secret) validate() error {
	switch {
	case s.Keychain != "" && !keychainID.MatchString(s.Keychain):
		return fmt.Errorf("keychain reference %q must look like service/account (lowercase letters, digits, - and .)", s.Keychain)
	case s.File != "" && !filepath.IsAbs(s.File):
		return fmt.Errorf("file reference %q must be an absolute path", s.File)
	}
	return nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

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
		if (s.Command == "") == (s.URL == "") {
			return fmt.Errorf("server %q: set either command (a local server) or url (a remote one)", name)
		}
		if s.URL != "" {
			if err := validateRemote(name, s); err != nil {
				return err
			}
		} else if !filepath.IsAbs(s.Command) {
			return fmt.Errorf("server %q: command must be an absolute path, got %q", name, s.Command)
		} else if s.Auth != nil {
			return fmt.Errorf("server %q: auth applies to url servers; pass a local server's credentials through env", name)
		}
		if s.Dir != "" && !filepath.IsAbs(s.Dir) {
			return fmt.Errorf("server %q: dir must be an absolute path", name)
		}
		if len(s.Env) > 0 && mode&0o044 != 0 {
			return fmt.Errorf("server %q sets env, which may hold secrets: make the file readable by its owner only (chmod 600)", name)
		}
		for k, v := range s.Env {
			if !envName.MatchString(k) {
				return fmt.Errorf("server %q: invalid env name %q", name, k)
			}
			if !v.reference() && secretish.MatchString(k) {
				return fmt.Errorf(`server %q: env %s looks like a secret; store it and reference it, like {"keychain": "agentd/%s-%s"}`, name, k, name, strings.ToLower(strings.ReplaceAll(k, "_", "-")))
			}
			if err := v.validate(); err != nil {
				return fmt.Errorf("server %q env %s: %w", name, k, err)
			}
		}
		if s.CallTimeout.Duration < 0 {
			return errors.New("call_timeout must be positive")
		}
		for tool, m := range s.Tools.Meta {
			if !s.Tools.allows(tool) {
				return fmt.Errorf("server %q: meta for %q, which include/exclude leaves out", name, tool)
			}
			if m.Effect != "" && m.Effect != "read" && m.Effect != "write" {
				return fmt.Errorf("server %q tool %q: effect must be \"read\" or \"write\"", name, tool)
			}
			if m.Resource != "" && !argPath.MatchString(m.Resource) {
				return fmt.Errorf("server %q tool %q: resource must name an argument, like \"channel_id\" or \"message.channel\"", name, tool)
			}
			if err := validateRecovery(s, tool, m); err != nil {
				return fmt.Errorf("server %q tool %q: %w", name, tool, err)
			}
		}
		seen := map[string]bool{}
		for i, p := range s.Polls {
			if err := validatePoll(s, p); err != nil {
				return fmt.Errorf("server %q poll %d: %w", name, i, err)
			}
			if seen[p.key()] {
				return fmt.Errorf("server %q: two polls named %q; give them distinct names", name, p.key())
			}
			seen[p.key()] = true
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
