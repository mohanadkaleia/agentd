package mcp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"agentd/internal/model"
	"agentd/internal/tools"
)

const (
	maxToolsPerServer = 100
	maxDescription    = 2000
	maxSchemaBytes    = 16 << 10
	defaultPath       = "/usr/bin:/bin:/usr/sbin:/sbin"
)

var toolName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

type Server struct {
	Name    string
	config  ServerConfig
	log     *slog.Logger
	connect func(context.Context) (mcp.Transport, error)
	store   *store
	pinTTL  time.Duration

	mu       sync.Mutex
	session  *mcp.ClientSession
	listed   map[string]listed
	defs     map[string]model.ToolDefinition
	accepted map[string]string
	loadedAt time.Time
}

type Manager struct {
	Servers []*Server
}

// Start connects to every configured server and returns the tools they offer. A server
// that fails to start is logged and skipped; agentd starts without its tools. A tool is
// registered whatever its review status; it runs only once its definition is accepted.
func Start(ctx context.Context, c Config, dataDir string, db *sql.DB, log *slog.Logger) (*Manager, []tools.Tool) {
	names := make([]string, 0, len(c.Servers))
	for name := range c.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	m := &Manager{}
	st := newStore(db, time.Now)
	var registered []tools.Tool
	for _, name := range names {
		s := newServer(name, c.Servers[name], dataDir, st, log)
		found, err := s.start(ctx)
		if err != nil {
			log.Error("mcp server unavailable", "server", name, "err", err)
			continue
		}
		m.Servers = append(m.Servers, s)
		registered = append(registered, found...)
		log.Info("mcp server ready", "server", name, "tools", len(found))
	}
	return m, registered
}

func (m *Manager) Close() {
	for _, s := range m.Servers {
		s.close()
	}
}

func newServer(name string, c ServerConfig, dataDir string, st *store, log *slog.Logger) *Server {
	s := &Server{Name: name, config: c, log: log.With("mcp_server", name), store: st, pinTTL: 2 * time.Second}
	s.connect = func(context.Context) (mcp.Transport, error) {
		dir := c.Dir
		if dir == "" {
			dir = filepath.Join(dataDir, "mcp", name)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, err
			}
		}
		cmd := exec.Command(c.Command, c.Args...)
		cmd.Dir = dir
		cmd.Env = childEnv(c.Env, dir)
		cmd.Stderr = &lineLog{log: s.log}
		return &mcp.CommandTransport{Command: cmd, TerminateDuration: 5 * time.Second}, nil
	}
	return s
}

// childEnv never inherits agentd's environment, which may hold provider API keys.
func childEnv(configured map[string]string, dir string) []string {
	env := map[string]string{"PATH": defaultPath, "HOME": dir}
	for k, v := range configured {
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func (s *Server) start(ctx context.Context) ([]tools.Tool, error) {
	t, err := s.connect(ctx)
	if err != nil {
		return nil, err
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "agentd", Version: "1"}, &mcp.ClientOptions{
		Capabilities:   &mcp.ClientCapabilities{},
		MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true},
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			go func() {
				if err := s.refresh(context.Background()); err != nil {
					s.log.Error("mcp tool list refresh", "err", err)
				}
			}()
		},
	})
	cs, err := client.Connect(ctx, t, nil)
	if err != nil {
		return nil, fmt.Errorf("initialize: %w", err)
	}
	init := cs.InitializeResult()
	if init == nil || init.Capabilities == nil || init.Capabilities.Tools == nil {
		cs.Close()
		return nil, fmt.Errorf("server does not offer tools")
	}
	found, err := s.discover(ctx, cs)
	if err == nil {
		err = s.record(ctx, init.Instructions)
	}
	if err != nil {
		cs.Close()
		return nil, err
	}
	s.mu.Lock()
	s.session = cs
	s.mu.Unlock()
	go func() {
		err := cs.Wait()
		s.mu.Lock()
		dropped := s.session == cs
		if dropped {
			s.session = nil
		}
		s.mu.Unlock()
		if dropped {
			s.log.Warn("mcp server disconnected", "err", err)
		}
	}()
	return found, nil
}

func (s *Server) discover(ctx context.Context, cs *mcp.ClientSession) ([]tools.Tool, error) {
	var found []tools.Tool
	listed := map[string]listed{}
	defs := map[string]model.ToolDefinition{}
	for remote, err := range cs.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("tools/list: %w", err)
		}
		if !s.config.Tools.allows(remote.Name) {
			continue
		}
		if len(listed) == maxToolsPerServer {
			s.log.Warn("mcp server offers too many tools; ignoring the rest", "limit", maxToolsPerServer)
			break
		}
		t, l, err := s.wrap(remote)
		if err != nil {
			s.log.Warn("mcp tool skipped", "tool", remote.Name, "err", err)
			continue
		}
		listed[remote.Name], defs[remote.Name] = l, t.def
		found = append(found, t)
	}
	s.mu.Lock()
	s.listed, s.defs = listed, defs
	s.mu.Unlock()
	return found, nil
}

// refresh re-reads the tool list after the server announces a change. Tools that are new
// since startup are recorded for review and registered on the next restart.
func (s *Server) refresh(ctx context.Context) error {
	cs := s.live()
	if cs == nil {
		return nil
	}
	if _, err := s.discover(ctx, cs); err != nil {
		return err
	}
	return s.record(ctx, cs.InitializeResult().Instructions)
}

func (s *Server) record(ctx context.Context, instructions string) error {
	s.mu.Lock()
	listed := s.listed
	s.loadedAt = time.Time{}
	s.mu.Unlock()
	review, err := s.store.sync(ctx, s.Name, instructions, listed)
	if err != nil {
		return err
	}
	if len(review) > 0 {
		s.log.Warn("mcp tools await review; they will not run until accepted", "tools", review, "review", "agentd mcp review "+s.Name)
	}
	return s.store.notifyReview(ctx, s.Name, review, listed)
}

func (s *Server) wrap(remote *mcp.Tool) (*Tool, listed, error) {
	name := s.Name + "__" + remote.Name
	if !toolName.MatchString(name) {
		return nil, listed{}, fmt.Errorf("name %q cannot be offered to models (letters, digits, _ and -, at most 64)", name)
	}
	def, hash, err := definitionOf(remote)
	if err != nil {
		return nil, listed{}, err
	}
	if len(def.InputSchema) > maxSchemaBytes {
		return nil, listed{}, fmt.Errorf("input schema is %d bytes, over the %d byte limit", len(def.InputSchema), maxSchemaBytes)
	}
	var shape struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(def.InputSchema, &shape) != nil || shape.Type != "object" {
		return nil, listed{}, fmt.Errorf("input schema must be a JSON object schema")
	}
	schema, err := cleanSchema(def.InputSchema)
	if err != nil {
		return nil, listed{}, err
	}
	meta := s.config.Tools.Meta[remote.Name]
	effect := tools.EffectWrite
	if meta.Effect == "read" {
		if a := remote.Annotations; a != nil && a.DestructiveHint != nil && *a.DestructiveHint {
			return nil, listed{}, fmt.Errorf("declared a read, but the server marks it destructive")
		}
		effect = tools.EffectRead
	}
	var resource []string
	if meta.Resource != "" {
		resource = strings.Split(meta.Resource, ".")
	}
	return &Tool{server: s, remote: remote.Name, effect: effect, resource: resource, def: model.ToolDefinition{
		Name:        name,
		Description: clean(remote.Description, maxDescription),
		InputSchema: schema,
	}}, listed{def: def, hash: hash}, nil
}

func (s *Server) definition(remote string, fallback model.ToolDefinition) model.ToolDefinition {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.defs[remote]; ok {
		return d
	}
	return fallback
}

func (s *Server) listedHash(remote string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.listed[remote]
	return l.hash, ok
}

func (s *Server) pin(remote string) (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadedAt.IsZero() || time.Since(s.loadedAt) >= s.pinTTL {
		accepted, err := s.store.accepted(context.Background(), s.Name)
		if err != nil {
			return "", "cannot read its review status: " + err.Error()
		}
		s.accepted, s.loadedAt = accepted, time.Now()
	}
	review := "run: agentd mcp review " + s.Name
	current, offered := s.listed[remote]
	accepted := s.accepted[remote]
	switch {
	case !offered:
		return accepted, "the server no longer offers it"
	case accepted == "":
		return "", "not reviewed yet; " + review
	case accepted != current.hash:
		return accepted, "its definition changed since it was accepted; " + review
	}
	return accepted, ""
}

func (s *Server) live() *mcp.ClientSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.session
}

func (s *Server) timeout() time.Duration {
	if s.config.CallTimeout.Duration > 0 {
		return s.config.CallTimeout.Duration
	}
	return defaultCallTimeout
}

func (s *Server) close() {
	s.mu.Lock()
	cs := s.session
	s.session = nil
	s.mu.Unlock()
	if cs != nil {
		if err := cs.Close(); err != nil {
			s.log.Warn("mcp server close", "err", err)
		}
	}
}

type lineLog struct {
	log     *slog.Logger
	mu      sync.Mutex
	pending []byte
}

func (l *lineLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pending = append(l.pending, p...)
	for {
		i := bytes.IndexByte(l.pending, '\n')
		if i < 0 {
			break
		}
		l.log.Info("mcp server stderr", "line", truncate(string(l.pending[:i]), 1000))
		l.pending = l.pending[i+1:]
	}
	if len(l.pending) > 4096 {
		l.log.Info("mcp server stderr", "line", truncate(string(l.pending), 1000))
		l.pending = nil
	}
	return len(p), nil
}

func truncate(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + " [truncated]"
}
