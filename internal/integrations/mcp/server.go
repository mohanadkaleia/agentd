package mcp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
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
	vault   Vault
	stderr  io.Writer
	probe   bool
	pinTTL  time.Duration

	poll    time.Duration
	backoff [2]time.Duration
	sem     chan struct{}
	dropped chan struct{}

	mu          sync.Mutex
	session     *mcp.ClientSession
	state       string
	stateError  string
	failures    []time.Time
	authProblem string
	failedWith  string
	listed      map[string]listed
	defs        map[string]model.ToolDefinition
	accepted    map[string]string
	loadedAt    time.Time
}

type Manager struct {
	Servers []*Server
}

type Options struct {
	DataDir string
	LogDir  string
	DB      *sql.DB
	Vault   Vault
	Log     *slog.Logger
}

const (
	minBackoff         = time.Second
	maxBackoff         = 5 * time.Minute
	maxFailuresPerHour = 5
)

// Start connects to every configured server and returns the tools they offer. A tool is
// registered whatever its review status, and so is every tool known from an earlier run
// of a server that is down now, so names stay stable; a tool runs only while its server
// is connected and its definition is accepted. Call Supervise to keep servers running.
func Start(ctx context.Context, c Config, o Options) (*Manager, []tools.Tool) {
	names := make([]string, 0, len(c.Servers))
	for name := range c.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	m := &Manager{}
	st := newStore(o.DB, time.Now)
	var registered []tools.Tool
	for _, name := range names {
		s := newServer(name, c.Servers[name], o.DataDir, st, o.Log)
		s.vault = o.Vault
		if o.LogDir != "" && s.config.URL == "" {
			s.stderr = &cappedFile{path: filepath.Join(o.LogDir, "mcp-"+name+".log"), limit: 5 << 20}
		}
		m.Servers = append(m.Servers, s)
		if s.config.Disabled {
			s.setState(ctx, StateDisabled, "disabled in the config", time.Time{})
			registered = append(registered, s.restore(ctx)...)
			continue
		}
		found, err := s.start(ctx)
		if err != nil {
			s.failed(ctx, err)
			o.Log.Error("mcp server unavailable", "server", name, "err", err)
			registered = append(registered, s.restore(ctx)...)
			continue
		}
		registered = append(registered, found...)
		o.Log.Info("mcp server ready", "server", name, "tools", len(found))
	}
	return m, registered
}

// Supervise keeps every enabled server connected until ctx ends.
func (m *Manager) Supervise(ctx context.Context) {
	for _, s := range m.Servers {
		if !s.config.Disabled {
			go s.supervise(ctx)
		}
	}
}

func (m *Manager) Close() {
	for _, s := range m.Servers {
		s.close()
	}
}

func newServer(name string, c ServerConfig, dataDir string, st *store, log *slog.Logger) *Server {
	limit := c.MaxConcurrent
	if limit <= 0 {
		limit = 3
	}
	s := &Server{Name: name, config: c, log: log.With("mcp_server", name), store: st, pinTTL: 2 * time.Second,
		poll: 2 * time.Second, backoff: [2]time.Duration{minBackoff, maxBackoff}, sem: make(chan struct{}, limit), dropped: make(chan struct{}, 1), state: StateConnecting}
	s.connect = func(context.Context) (mcp.Transport, error) {
		if c.URL != "" {
			return s.remoteTransport()
		}
		env := map[string]string{}
		for k, v := range c.Env {
			value, err := s.vault.resolve(v)
			if err != nil {
				return nil, fmt.Errorf("env %s: %w", k, err)
			}
			env[k] = value
		}
		dir := c.Dir
		if dir == "" {
			dir = filepath.Join(dataDir, "mcp", name)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, err
			}
		}
		cmd := exec.Command(c.Command, c.Args...)
		cmd.Dir = dir
		cmd.Env = childEnv(env, dir)
		if s.stderr != nil {
			cmd.Stderr = s.stderr
		} else {
			cmd.Stderr = &lineLog{log: s.log}
		}
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
	s.authProblem = ""
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
			select {
			case s.dropped <- struct{}{}:
			default:
			}
		}
	}()
	s.ready(ctx)
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
	s.mu.Lock()
	s.stateError = ""
	if len(review) > 0 {
		s.stateError = fmt.Sprintf("%d tool(s) await review", len(review))
	}
	s.mu.Unlock()
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
		if s.authProblem != "" {
			if now := s.currentFingerprint(); now != "" && now != s.failedWith {
				s.authProblem = ""
			}
		}
	}
	review := "run: agentd mcp review " + s.Name
	current, offered := s.listed[remote]
	accepted := s.accepted[remote]
	switch {
	case s.authProblem != "":
		return accepted, s.authProblem
	case s.session == nil:
		return accepted, fmt.Sprintf("server %q is %s%s", s.Name, s.state, suffix(s.stateError))
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

func (s *Server) needSignIn(reason, fingerprint string) {
	how := "run: agentd mcp auth " + s.Name
	if s.config.Auth != nil && s.config.Auth.Type == "bearer" {
		how = "store a valid token where auth.token points"
	}
	s.mu.Lock()
	first := s.authProblem == ""
	s.failedWith = fingerprint
	s.authProblem = fmt.Sprintf("server %q needs sign-in (%s); %s", s.Name, reason, how)
	problem := s.authProblem
	s.mu.Unlock()
	if !first {
		return
	}
	s.log.Error("mcp server needs sign-in", "reason", reason)
	s.setState(context.Background(), StateNeedsAuth, problem, time.Time{})
	if err := s.store.notifyAssignments(context.Background(), s.Name, nil, "mcp-auth:"+s.Name,
		fmt.Sprintf("MCP server %q needs sign-in", s.Name), problem+". Tasks that need its tools wait until then."); err != nil {
		s.log.Error("mcp sign-in notification", "err", err)
	}
}

func (s *Server) signedIn() {
	s.mu.Lock()
	was, connected := s.authProblem, s.session != nil
	s.authProblem = ""
	s.mu.Unlock()
	if was != "" {
		s.log.Info("mcp server signed in again")
		if connected {
			s.ready(context.Background())
		}
	}
}

func suffix(detail string) string {
	if detail == "" {
		return ""
	}
	return ": " + detail
}

func (s *Server) transport() string {
	if s.config.URL != "" {
		return "http"
	}
	return "stdio"
}

func (s *Server) setState(ctx context.Context, state, detail string, retry time.Time) {
	s.mu.Lock()
	s.state, s.stateError = state, detail
	probe := s.probe
	s.mu.Unlock()
	if probe {
		return
	}
	if err := s.store.setState(ctx, s.Name, state, s.transport(), detail, retry); err != nil {
		s.log.Error("mcp server state", "err", err)
	}
}

func (s *Server) ready(ctx context.Context) {
	s.mu.Lock()
	state, detail := StateReady, s.stateError
	if detail != "" {
		state = StateNeedsReview
	}
	s.mu.Unlock()
	s.setState(ctx, state, detail, time.Time{})
}

func (s *Server) failed(ctx context.Context, err error) {
	s.mu.Lock()
	auth := s.authProblem
	s.mu.Unlock()
	if auth != "" {
		s.setState(ctx, StateNeedsAuth, auth, time.Time{})
		return
	}
	s.setState(ctx, StateFailed, err.Error(), time.Time{})
}

// restore registers the tools a server offered on an earlier run, so that its name stays
// stable while it is down; they run once it connects again.
func (s *Server) restore(ctx context.Context) []tools.Tool {
	pins, err := ListPins(ctx, s.store.db, s.Name)
	if err != nil {
		s.log.Error("mcp restore tools", "err", err)
		return nil
	}
	var out []tools.Tool
	for _, p := range pins {
		var d Definition
		if p.Status == StatusRemoved || json.Unmarshal(p.Definition, &d) != nil || !s.config.Tools.allows(d.Name) {
			continue
		}
		remote := &mcp.Tool{Name: d.Name, Description: d.Description, InputSchema: d.InputSchema}
		if len(d.Annotations) > 0 {
			json.Unmarshal(d.Annotations, &remote.Annotations)
		}
		t, _, err := s.wrap(remote)
		if err != nil {
			continue
		}
		out = append(out, t)
	}
	return out
}

// supervise reconnects the server whenever it drops, backing off from one second to five
// minutes. After five failures within an hour it stays down until `agentd mcp restart`. A
// server that rejects agentd's credentials waits for a new sign-in instead.
func (s *Server) supervise(ctx context.Context) {
	backoff := s.backoff[0]
	for ctx.Err() == nil {
		if s.live() != nil {
			select {
			case <-ctx.Done():
				return
			case <-s.dropped:
				s.recordFailure(ctx, "the server disconnected")
			case <-time.After(s.poll):
				if s.store.takeRestart(ctx, s.Name) {
					s.log.Info("mcp server restart requested")
					s.close()
					s.resetFailures()
					backoff = s.backoff[0]
				}
			}
			continue
		}
		if s.gaveUp() || s.waitingForSignIn() {
			if !s.sleep(ctx, s.poll) {
				return
			}
			if s.store.takeRestart(ctx, s.Name) {
				s.resetFailures()
				s.signedIn()
				backoff = s.backoff[0]
			}
			continue
		}
		s.setState(ctx, StateConnecting, "", time.Time{})
		if _, err := s.start(ctx); err == nil {
			backoff = s.backoff[0]
			continue
		} else if s.waitingForSignIn() {
			s.failed(ctx, err)
			continue
		} else if s.recordFailure(ctx, err.Error()) {
			continue
		}
		s.setState(ctx, StateFailed, s.lastError(), time.Now().Add(backoff))
		if !s.sleep(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, s.backoff[1])
	}
}

// recordFailure counts a crash or failed start and reports whether the server gave up.
func (s *Server) recordFailure(ctx context.Context, detail string) bool {
	now := time.Now()
	s.mu.Lock()
	recent := s.failures[:0]
	for _, t := range s.failures {
		if now.Sub(t) < time.Hour {
			recent = append(recent, t)
		}
	}
	s.failures = append(recent, now)
	s.stateError = detail
	gaveUp := len(s.failures) >= maxFailuresPerHour
	s.mu.Unlock()
	if !gaveUp {
		return false
	}
	s.setState(ctx, StateFailed, fmt.Sprintf("%s; stopped after %d failures in an hour, run: agentd mcp restart %s", detail, maxFailuresPerHour, s.Name), time.Time{})
	if err := s.store.notifyAssignments(ctx, s.Name, nil, "mcp-failed:"+s.Name, fmt.Sprintf("MCP server %q stopped", s.Name),
		fmt.Sprintf("It failed %d times within an hour (%s). Tasks that need its tools wait. Fix it, then run: agentd mcp restart %s",
			maxFailuresPerHour, detail, s.Name)); err != nil {
		s.log.Error("mcp failure notification", "err", err)
	}
	return true
}

func (s *Server) gaveUp() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.failures) >= maxFailuresPerHour
}

func (s *Server) resetFailures() {
	s.mu.Lock()
	s.failures = nil
	s.mu.Unlock()
}

func (s *Server) lastError() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stateError
}

func (s *Server) waitingForSignIn() bool {
	s.mu.Lock()
	problem := s.authProblem
	s.mu.Unlock()
	if problem == "" {
		return false
	}
	if now := s.currentFingerprint(); now != "" && now != s.failedWith {
		s.signedIn()
		return false
	}
	return true
}

func (s *Server) sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// cappedFile appends a server's stderr to its own log file, keeping one older file.
type cappedFile struct {
	path  string
	limit int64
	mu    sync.Mutex
}

func (c *cappedFile) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if info, err := os.Stat(c.path); err == nil && info.Size()+int64(len(p)) > c.limit {
		os.Rename(c.path, c.path+".1")
	}
	f, err := os.OpenFile(c.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return len(p), nil
	}
	defer f.Close()
	return f.Write(p)
}

// closeSession ends one session, such as a hung local server's; supervise starts a new one.
func (s *Server) closeSession(cs *mcp.ClientSession) {
	if err := cs.Close(); err != nil {
		s.log.Warn("mcp server close", "err", err)
	}
}

// Probe connects to one server, lists its tools and records them for review, without
// calling any tool and without touching the state a running agentd reports.
func Probe(ctx context.Context, name string, c ServerConfig, o Options) ([]Pin, error) {
	s := newServer(name, c, o.DataDir, newStore(o.DB, time.Now), o.Log)
	s.vault, s.probe = o.Vault, true
	if _, err := s.start(ctx); err != nil {
		return nil, err
	}
	s.close()
	return ListPins(ctx, o.DB, name)
}

// Check resolves every secret a server needs, without connecting to it.
func Check(name string, c ServerConfig, v Vault) []error {
	var errs []error
	for k, secret := range c.Env {
		if _, err := v.resolve(secret); err != nil {
			errs = append(errs, fmt.Errorf("server %q env %s: %w", name, k, err))
		}
	}
	if a := c.Auth; a != nil {
		if a.Token != nil {
			if _, err := v.resolve(*a.Token); err != nil {
				errs = append(errs, fmt.Errorf("server %q auth.token: %w", name, err))
			}
		}
		if a.ClientSecret != nil {
			if _, err := v.resolve(*a.ClientSecret); err != nil {
				errs = append(errs, fmt.Errorf("server %q auth.client_secret: %w", name, err))
			}
		}
		if a.Type == "oauth" {
			if _, ok := v.loadCredential(name, c.URL); !ok {
				errs = append(errs, fmt.Errorf("server %q is not signed in: run agentd mcp auth %s", name, name))
			}
		}
	}
	return errs
}
