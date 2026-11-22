package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"agentd/internal/policy"
	"agentd/internal/testutil"
	"agentd/internal/tools"
)

type memVault struct {
	mu    sync.Mutex
	items map[string][]byte
}

func (m *memVault) vault() Vault {
	return Vault{
		Keychain: func(service, account string) string {
			m.mu.Lock()
			defer m.mu.Unlock()
			return string(m.items[service+"/"+account])
		},
		Load: func(account string) ([]byte, bool) {
			m.mu.Lock()
			defer m.mu.Unlock()
			b, ok := m.items["agentd/"+account]
			return b, ok
		},
		Store: func(account string, value []byte) error {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.items["agentd/"+account] = value
			return nil
		},
	}
}

func (m *memVault) dump() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var b strings.Builder
	for k, v := range m.items {
		fmt.Fprintf(&b, "%s=%s\n", k, v)
	}
	return b.String()
}

// remote is a fake hosted MCP server with its own OAuth authorization server.
type remote struct {
	t      *testing.T
	fake   *fake
	mcp    *httptest.Server
	as     *httptest.Server
	inner  atomic.Pointer[http.Handler]
	oauth  bool
	bearer string

	mu        sync.Mutex
	valid     map[string]bool
	refresh   map[string]bool
	codes     map[string]string
	issued    []string
	refreshes atomic.Int32
	calls     atomic.Int32
	firstTTL  int
}

func newRemote(t *testing.T, jsonResponse bool) *remote {
	r := &remote{t: t, fake: newFake(), valid: map[string]bool{}, refresh: map[string]bool{}, codes: map[string]string{}, firstTTL: 3600}
	r.fake.add("slow", "Takes a while.", func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		time.Sleep(300 * time.Millisecond)
		return text("late"), nil
	})
	r.swap(jsonResponse)
	r.mcp = httptest.NewServer(http.HandlerFunc(r.serveMCP))
	t.Cleanup(r.mcp.Close)
	return r
}

func (r *remote) swap(jsonResponse bool) {
	h := http.Handler(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return r.fake.srv },
		&mcp.StreamableHTTPOptions{JSONResponse: jsonResponse}))
	r.inner.Store(&h)
}

func (r *remote) url() string { return r.mcp.URL + "/mcp" }

func (r *remote) serveMCP(w http.ResponseWriter, req *http.Request) {
	if strings.HasPrefix(req.URL.Path, "/.well-known/oauth-protected-resource") {
		resource := r.mcp.URL
		if strings.HasSuffix(req.URL.Path, "/mcp") {
			resource = r.url()
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"resource": resource, "authorization_servers": []string{r.as.URL}})
		return
	}
	if r.oauth || r.bearer != "" {
		token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		r.mu.Lock()
		ok := r.valid[token] || (r.bearer != "" && token == r.bearer)
		r.mu.Unlock()
		if !ok {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource/mcp"`, r.mcp.URL))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	if req.Method == http.MethodPost {
		body, _ := io.ReadAll(req.Body)
		if bytes.Contains(body, []byte(`"tools/call"`)) {
			r.calls.Add(1)
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	(*r.inner.Load()).ServeHTTP(w, req)
}

func (r *remote) withOAuth() *remote {
	r.oauth = true
	r.as = httptest.NewServer(http.HandlerFunc(r.serveAS))
	r.t.Cleanup(r.as.Close)
	return r
}

func (r *remote) serveAS(w http.ResponseWriter, req *http.Request) {
	switch req.URL.Path {
	case "/.well-known/oauth-authorization-server", "/.well-known/openid-configuration":
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": r.as.URL, "authorization_endpoint": r.as.URL + "/authorize", "token_endpoint": r.as.URL + "/token",
			"registration_endpoint": r.as.URL + "/register", "response_types_supported": []string{"code"},
			"grant_types_supported": []string{"authorization_code", "refresh_token"}, "code_challenge_methods_supported": []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"none"}, "scopes_supported": []string{"read", "write", "offline_access"},
		})
	case "/register":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"client_id": "registered-client", "redirect_uris": []string{"http://127.0.0.1/callback"}})
	case "/authorize":
		q := req.URL.Query()
		if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
			http.Error(w, "pkce required", http.StatusBadRequest)
			return
		}
		code := fmt.Sprintf("code-%d", time.Now().UnixNano())
		r.mu.Lock()
		r.codes[code] = q.Get("code_challenge")
		r.mu.Unlock()
		http.Redirect(w, req, q.Get("redirect_uri")+"?code="+code+"&state="+q.Get("state"), http.StatusFound)
	case "/token":
		req.ParseForm()
		r.mu.Lock()
		defer r.mu.Unlock()
		ttl := 3600
		switch req.Form.Get("grant_type") {
		case "authorization_code":
			sum := sha256.Sum256([]byte(req.Form.Get("code_verifier")))
			if r.codes[req.Form.Get("code")] != base64.RawURLEncoding.EncodeToString(sum[:]) {
				http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
				return
			}
			delete(r.codes, req.Form.Get("code"))
			ttl = r.firstTTL
		case "refresh_token":
			if !r.refresh[req.Form.Get("refresh_token")] {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			r.refreshes.Add(1)
		default:
			http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
			return
		}
		access, refresh := fmt.Sprintf("access-%d", len(r.issued)), fmt.Sprintf("refresh-%d", len(r.issued))
		r.issued = append(r.issued, access, refresh)
		r.valid[access], r.refresh[refresh] = true, true
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": ttl,
			"refresh_token": refresh, "scope": "read"})
	default:
		http.NotFound(w, req)
	}
}

func (r *remote) revokeAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.valid, r.refresh = map[string]bool{}, map[string]bool{}
}

func browser(target string) error {
	go func() {
		c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := c.Get(target)
		if err != nil {
			return
		}
		resp.Body.Close()
		if loc := resp.Header.Get("Location"); loc != "" {
			if r, err := http.Get(loc); err == nil {
				r.Body.Close()
			}
		}
	}()
	return nil
}

func (r *remote) server(t *testing.T, c ServerConfig, v Vault, log *slog.Logger) (*Server, map[string]tools.Tool, error) {
	c.URL = r.url()
	db := testutil.Open(t, testutil.DBPath(t))
	s := newServer("remote", c, t.TempDir(), newStore(db, time.Now), log)
	s.vault, s.pinTTL = v, 0
	found, err := s.start(context.Background())
	if err != nil {
		return s, nil, err
	}
	t.Cleanup(s.close)
	acceptAll(t, db, "remote")
	return s, index(found), nil
}

func TestRemoteServerOverJSONAndSSE(t *testing.T) {
	for _, jsonResponse := range []bool{true, false} {
		r := newRemote(t, jsonResponse)
		_, all, err := r.server(t, ServerConfig{}, Vault{}, quiet)
		if err != nil {
			t.Fatal(err)
		}
		res, err := call(t, all["remote__echo"], `{"x":1}`)
		if err != nil || !strings.Contains(string(res.Output), `\"x\":1`) {
			t.Fatalf("json=%v: echo = %s, %v", jsonResponse, res.Output, err)
		}
	}
}

func TestBearerTokenIsNeverSentToAnotherHost(t *testing.T) {
	var leaked atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "" {
			leaked.Add(1)
		}
	}))
	defer other.Close()
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, other.URL+"/mcp", http.StatusTemporaryRedirect)
	}))
	defer redirecting.Close()

	v := &memVault{items: map[string][]byte{"agentd/remote-token": []byte("secret-bearer")}}
	db := testutil.Open(t, testutil.DBPath(t))
	s := newServer("remote", ServerConfig{URL: redirecting.URL + "/mcp", Auth: &Auth{Type: "bearer", Token: &Secret{Keychain: "agentd/remote-token"}}},
		t.TempDir(), newStore(db, time.Now), quiet)
	s.vault = v.vault()
	if _, err := s.start(context.Background()); err == nil {
		t.Fatal("connected through a cross-host redirect")
	}
	if leaked.Load() != 0 {
		t.Fatal("the bearer token reached another host")
	}
}

func TestBearerTokenAuthenticates(t *testing.T) {
	r := newRemote(t, true)
	r.bearer = "secret-bearer"
	v := &memVault{items: map[string][]byte{"agentd/remote-token": []byte("secret-bearer")}}
	_, all, err := r.server(t, ServerConfig{Auth: &Auth{Type: "bearer", Token: &Secret{Keychain: "agentd/remote-token"}}}, v.vault(), quiet)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, all["remote__echo"], `{}`); err != nil {
		t.Fatal(err)
	}
}

func TestOAuthSignInRefreshesOnceAndRecoversFromRevocation(t *testing.T) {
	r := newRemote(t, true).withOAuth()
	v := &memVault{items: map[string][]byte{}}
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	cfg := ServerConfig{URL: r.url(), Auth: &Auth{Type: "oauth", Scopes: []string{"read"}}}

	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := SignIn(ctx, "remote", cfg, v.vault(), browser, &out); err != nil {
		t.Fatalf("sign-in: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "scopes: read") {
		t.Fatalf("sign-in output does not show the granted scopes:\n%s", out.String())
	}

	expireStoredToken(t, v, "remote")
	s, all, err := r.server(t, ServerConfig{Auth: cfg.Auth}, v.vault(), log)
	if err != nil {
		t.Fatal(err)
	}
	echo := all["remote__echo"]
	var wg sync.WaitGroup
	var failed atomic.Int32
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := call(t, echo, `{}`); err != nil {
				failed.Add(1)
			}
		}()
	}
	wg.Wait()
	if failed.Load() != 0 || r.refreshes.Load() != 1 {
		t.Logf("stored: %s", v.dump())
		t.Fatalf("%d calls failed, %d refreshes; want none failed and exactly one refresh", failed.Load(), r.refreshes.Load())
	}

	r.revokeAll()
	callsBefore := r.calls.Load()
	if _, err := call(t, echo, `{}`); !errors.Is(err, tools.ErrNotExecuted) {
		t.Fatalf("call with a revoked token err = %v, want ErrNotExecuted", err)
	}
	if _, reason := echo.(tools.Pinned).Pin(); !strings.Contains(reason, "needs sign-in") {
		t.Fatalf("pin after revocation = %q, want needs sign-in", reason)
	}
	if tools.Ready(echo) {
		t.Fatal("a tool whose server needs sign-in is still offered")
	}
	if r.calls.Load() != callsBefore {
		t.Fatal("a tool call reached the server without valid credentials")
	}

	if err := SignIn(ctx, "remote", cfg, v.vault(), browser, &out); err != nil {
		t.Fatal(err)
	}
	if !tools.Ready(echo) {
		t.Fatal("signing in again did not make the tool available without a restart")
	}
	if _, err := call(t, echo, `{}`); err != nil {
		t.Fatalf("call after signing in again: %v", err)
	}
	_ = s

	for _, secret := range r.issued {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("token %s appears in the log", secret)
		}
	}
	if !strings.Contains(v.dump(), "refresh-") {
		t.Fatal("credentials were not kept in the vault")
	}
}

func TestSignInNotificationIsSentOnce(t *testing.T) {
	r := newRemote(t, true).withOAuth()
	v := &memVault{items: map[string][]byte{}}
	cfg := ServerConfig{URL: r.url(), Auth: &Auth{Type: "oauth"}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := SignIn(ctx, "remote", cfg, v.vault(), browser, io.Discard); err != nil {
		t.Fatal(err)
	}
	w := newWorldWith(t, newFake(), ToolFilter{}, policy.Policy{Tools: map[string]policy.Rule{"remote__echo": {Mode: policy.Allow}}}, true)
	s := newServer("remote", cfg, t.TempDir(), newStore(w.app.DB, time.Now), quiet)
	s.vault, s.pinTTL = v.vault(), 0
	found, err := s.start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	acceptAll(t, w.app.DB, "remote")
	echo := index(found)["remote__echo"]
	r.revokeAll()
	call(t, echo, `{}`)
	call(t, echo, `{}`)
	if n := w.count(`SELECT COUNT(*) FROM notifications WHERE dedup_key = 'mcp-auth:remote'`); n != 1 {
		t.Fatalf("sign-in notifications = %d, want 1", n)
	}
	var stored int
	for _, secret := range r.issued {
		stored += w.count(`SELECT COUNT(*) FROM notifications WHERE body LIKE ? OR title LIKE ?`, "%"+secret+"%", "%"+secret+"%")
		stored += w.count(`SELECT COUNT(*) FROM audit_log WHERE payload_json LIKE ?`, "%"+secret+"%")
	}
	if stored != 0 {
		t.Fatal("a token was written to the database")
	}
}

func TestRemoteTimeoutAfterSendIsAmbiguous(t *testing.T) {
	r := newRemote(t, true)
	_, all, err := r.server(t, ServerConfig{CallTimeout: policy.Duration{Duration: 100 * time.Millisecond}}, Vault{}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	_, err = call(t, all["remote__slow"], `{}`)
	if err == nil || errors.Is(err, tools.ErrNotExecuted) {
		t.Fatalf("timeout err = %v, want an ambiguous error", err)
	}
	if r.calls.Load() != 1 {
		t.Fatalf("server received %d calls, want exactly 1 (no retry)", r.calls.Load())
	}
}

func TestRemoteServerDownIsNotSentAndSessionLossReconnects(t *testing.T) {
	r := newRemote(t, true)
	_, all, err := r.server(t, ServerConfig{}, Vault{}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	echo := all["remote__echo"]

	r.swap(true)
	call(t, echo, `{}`)
	eventually(t, "a call to succeed after the server lost the session", func() bool {
		_, err := call(t, echo, `{}`)
		return err == nil
	})

	r.mcp.Listener.Close()
	r.mcp.CloseClientConnections()
	http.DefaultTransport.(*http.Transport).CloseIdleConnections()
	before := r.calls.Load()
	if _, err := call(t, echo, `{}`); !errors.Is(err, tools.ErrNotExecuted) {
		t.Fatalf("call to a stopped server err = %v, want ErrNotExecuted", err)
	}
	if r.calls.Load() != before {
		t.Fatal("the stopped server received a call")
	}
}

func expireStoredToken(t *testing.T, v *memVault, server string) {
	t.Helper()
	c, ok := v.vault().loadCredential(server, "")
	_ = ok
	b, _ := v.vault().Load(credentialAccount(server))
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	c.Token.Expiry = time.Now().Add(-time.Hour)
	if err := v.vault().saveCredential(server, c); err != nil {
		t.Fatal(err)
	}
}
