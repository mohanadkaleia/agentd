package mcp

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

// Vault reads and writes agentd's secrets: the login keychain, or owner-only files under
// the dedicated-account install. cmd/ provides it, so this package never shells out.
type Vault struct {
	Keychain func(service, account string) string
	Load     func(account string) ([]byte, bool)
	Store    func(account string, value []byte) error
}

var (
	errNotSent   = errors.New("request was not sent")
	errNeedsAuth = errors.New("server needs sign-in")
)

func (v Vault) resolve(s Secret) (string, error) {
	switch {
	case s.Keychain != "":
		service, account, _ := strings.Cut(s.Keychain, "/")
		value := ""
		if v.Keychain != nil {
			value = v.Keychain(service, account)
		}
		if value == "" {
			return "", fmt.Errorf("keychain item %s is missing", s.Keychain)
		}
		return value, nil
	case s.File != "":
		info, err := os.Stat(s.File)
		if err != nil {
			return "", err
		}
		if info.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("secret file %s must be readable by its owner only (chmod 600)", s.File)
		}
		b, err := os.ReadFile(s.File)
		return strings.TrimSpace(string(b)), err
	}
	return s.Literal, nil
}

// credential is what `agentd mcp auth` stores for an OAuth server. It is bound to the URL it
// was issued for, so changing a server's url makes it sign in again.
type credential struct {
	URL          string        `json:"url"`
	ClientID     string        `json:"client_id"`
	ClientSecret string        `json:"client_secret,omitempty"`
	TokenURL     string        `json:"token_url"`
	AuthStyle    int           `json:"auth_style"`
	Scopes       []string      `json:"scopes,omitempty"`
	Token        *oauth2.Token `json:"token"`
}

func credentialAccount(server string) string {
	return "mcp-oauth-" + hex.EncodeToString([]byte(server))
}

func (v Vault) loadCredential(server, endpoint string) (credential, bool) {
	if v.Load == nil {
		return credential{}, false
	}
	b, ok := v.Load(credentialAccount(server))
	if !ok {
		return credential{}, false
	}
	var c credential
	if json.Unmarshal(b, &c) != nil || c.URL != endpoint || c.Token == nil {
		return credential{}, false
	}
	return c, true
}

func (v Vault) saveCredential(server string, c credential) error {
	if v.Store == nil {
		return errors.New("no secret store configured")
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return v.Store(credentialAccount(server), b)
}

func (c credential) config() *oauth2.Config {
	return &oauth2.Config{ClientID: c.ClientID, ClientSecret: c.ClientSecret, Scopes: c.Scopes,
		Endpoint: oauth2.Endpoint{TokenURL: c.TokenURL, AuthStyle: oauth2.AuthStyle(c.AuthStyle)}}
}

// httpClient refuses redirects to another host, so a token is never sent anywhere but the
// configured server, and marks a request whose body was never read as not sent.
func httpClient(endpoint string) *http.Client {
	home, _ := url.Parse(endpoint)
	return &http.Client{
		Transport: sentTracker{base: http.DefaultTransport},
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			if req.URL.Host != home.Host || req.URL.Scheme != home.Scheme {
				return fmt.Errorf("refusing redirect from %s to %s", home.Host, req.URL.Host)
			}
			return nil
		},
	}
}

type sentTracker struct{ base http.RoundTripper }

func (t sentTracker) RoundTrip(req *http.Request) (*http.Response, error) {
	var read atomic.Int64
	if req.Body != nil {
		req = req.Clone(req.Context())
		req.Body = &countingBody{ReadCloser: req.Body, n: &read}
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil && read.Load() == 0 {
		return nil, fmt.Errorf("%w: %w", errNotSent, err)
	}
	if denied, ok := req.Context().Value(deniedKey{}).(*atomic.Bool); ok && resp != nil &&
		(resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
		denied.Store(true)
	}
	return resp, err
}

// deniedKey marks a call's context so a 401 or 403 answer to it, which the server sends
// before doing anything, is known to mean "not executed".
type deniedKey struct{}

type countingBody struct {
	io.ReadCloser
	n *atomic.Int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n.Add(int64(n))
	return n, err
}

// tokens serves the stored token and, on a 401 or 403, checks whether the owner has signed
// in again since; if not, the server needs sign-in and its tools wait.
type tokens struct {
	server *Server
	mu     sync.Mutex
	source oauth2.TokenSource
	seen   atomic.Pointer[string]
}

func (h *tokens) TokenSource(context.Context) (oauth2.TokenSource, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.source == nil {
		if err := h.reload(); err != nil {
			return nil, err
		}
	}
	return h.source, nil
}

func (h *tokens) Authorize(_ context.Context, _ *http.Request, resp *http.Response) error {
	resp.Body.Close()
	h.mu.Lock()
	defer h.mu.Unlock()
	before := *h.seen.Load()
	if err := h.reload(); err == nil && *h.seen.Load() != before {
		h.server.signedIn()
		return nil
	}
	h.server.needSignIn(fmt.Sprintf("the server refused agentd's credentials (HTTP %d)", resp.StatusCode), *h.seen.Load())
	return errNeedsAuth
}

func (h *tokens) reload() error {
	s := h.server
	a := s.config.Auth
	switch a.Type {
	case "bearer":
		token, err := s.vault.resolve(*a.Token)
		if err != nil {
			return err
		}
		h.source = oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
		h.seen.Store(&token)
	case "oauth":
		c, ok := s.vault.loadCredential(s.Name, s.config.URL)
		if !ok {
			return fmt.Errorf("%w: run agentd mcp auth %s", errNeedsAuth, s.Name)
		}
		h.source = &saving{base: c.config().TokenSource(context.Background(), c.Token), last: c.Token.AccessToken, save: func(t *oauth2.Token) {
			c.Token = t
			if err := s.vault.saveCredential(s.Name, c); err != nil {
				s.log.Error("mcp token store", "err", err)
			}
			h.seen.Store(ptr(fingerprint(c)))
		}}
		h.seen.Store(ptr(fingerprint(c)))
	}
	return nil
}

func fingerprint(c credential) string { return c.Token.AccessToken + "|" + c.Token.RefreshToken }

func ptr[T any](v T) *T { return &v }

// currentFingerprint reads what is stored now, to notice that the owner signed in again.
func (s *Server) currentFingerprint() string {
	a := s.config.Auth
	if a == nil {
		return ""
	}
	if a.Type == "bearer" {
		token, _ := s.vault.resolve(*a.Token)
		return token
	}
	c, ok := s.vault.loadCredential(s.Name, s.config.URL)
	if !ok {
		return ""
	}
	return fingerprint(c)
}

// saving refreshes one token at a time and stores every new token.
type saving struct {
	mu   sync.Mutex
	base oauth2.TokenSource
	last string
	save func(*oauth2.Token)
}

func (s *saving) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.base.Token()
	if err != nil {
		return nil, err
	}
	if t.AccessToken != s.last {
		s.last = t.AccessToken
		s.save(t)
	}
	return t, nil
}

func (s *Server) remoteTransport() (mcp.Transport, error) {
	t := &mcp.StreamableClientTransport{Endpoint: s.config.URL, HTTPClient: httpClient(s.config.URL), MaxEventSize: 4 << 20}
	if s.config.Auth != nil {
		h := &tokens{server: s}
		if _, err := h.TokenSource(context.Background()); err != nil {
			s.needSignIn(err.Error(), "")
			return nil, err
		}
		t.OAuthHandler = h
	}
	return t, nil
}

// SignIn runs the OAuth authorization-code flow with PKCE for one server, in the
// foreground, and stores the result where `agentd serve` reads it.
func SignIn(ctx context.Context, name string, c ServerConfig, v Vault, open func(string) error, out io.Writer) error {
	if c.URL == "" || c.Auth == nil || c.Auth.Type != "oauth" {
		return fmt.Errorf("server %q is not a url server with oauth auth", name)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()
	redirect := fmt.Sprintf("http://%s/callback", ln.Addr())
	results := make(chan *auth.AuthorizationResult, 1)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		if q.Get("error") != "" {
			http.Error(w, "authorization denied", http.StatusForbidden)
		} else {
			fmt.Fprintln(w, "agentd is connected. You can close this tab.")
		}
		select {
		case results <- &auth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}:
		default:
		}
	})}
	go srv.Serve(ln)
	defer srv.Close()

	var saved credential
	cfg := &auth.AuthorizationCodeHandlerConfig{
		RedirectURL:         redirect,
		RequestRefreshToken: true,
		Client:              &http.Client{Timeout: 30 * time.Second},
		AuthorizationCodeFetcher: func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
			fmt.Fprintf(out, "Opening the sign-in page. If it does not open, visit:\n%s\n", args.URL)
			if err := open(args.URL); err != nil {
				fmt.Fprintf(out, "(could not open a browser: %v)\n", err)
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case r := <-results:
				if r.Code == "" {
					return nil, errors.New("sign-in was denied or cancelled")
				}
				return r, nil
			}
		},
		NewTokenSource: func(ctx context.Context, oc *oauth2.Config, t *oauth2.Token) (oauth2.TokenSource, error) {
			saved = credential{URL: c.URL, ClientID: oc.ClientID, ClientSecret: oc.ClientSecret, TokenURL: oc.Endpoint.TokenURL,
				AuthStyle: int(oc.Endpoint.AuthStyle), Scopes: oc.Scopes, Token: t}
			return oc.TokenSource(ctx, t), nil
		},
	}
	if len(c.Auth.Scopes) > 0 {
		cfg.ScopeFilter = func([]string) []string { return c.Auth.Scopes }
	}
	if c.Auth.ClientID != "" {
		creds := &oauthex.ClientCredentials{ClientID: c.Auth.ClientID}
		if c.Auth.ClientSecret != nil {
			secret, err := v.resolve(*c.Auth.ClientSecret)
			if err != nil {
				return err
			}
			creds.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: secret}
		}
		cfg.PreregisteredClient = creds
	} else {
		cfg.DynamicClientRegistrationConfig = &auth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{
			ClientName: "agentd", RedirectURIs: []string{redirect},
			GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"},
			TokenEndpointAuthMethod: "none",
		}}
	}
	handler, err := auth.NewAuthorizationCodeHandler(cfg)
	if err != nil {
		return err
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "agentd", Version: "1"}, &mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}})
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: c.URL, HTTPClient: httpClient(c.URL), OAuthHandler: handler,
		DisableStandaloneSSE: true}, nil)
	if err != nil {
		return fmt.Errorf("sign-in: %w", err)
	}
	defer cs.Close()
	if saved.Token == nil {
		return errors.New("the server accepted agentd without signing in; auth.type oauth is not needed")
	}
	if ts, err := handler.TokenSource(ctx); err == nil && ts != nil {
		if latest, err := ts.Token(); err == nil {
			saved.Token = latest
		}
	}
	if granted, ok := saved.Token.Extra("scope").(string); ok && granted != "" {
		saved.Scopes = strings.Fields(granted)
	}
	if saved.Token.RefreshToken == "" {
		fmt.Fprintln(out, "warning: no refresh token was issued, so you will need to sign in again when this one expires")
	}
	if err := v.saveCredential(name, saved); err != nil {
		return err
	}
	scopes := "(none listed)"
	if len(saved.Scopes) > 0 {
		scopes = strings.Join(saved.Scopes, " ")
	}
	fmt.Fprintf(out, "signed in to %s; scopes: %s\nA running agentd uses the new sign-in on its next call.\n", name, scopes)
	return nil
}
