package gmail

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"golang.org/x/oauth2"
)

var Scopes = []string{
	"https://www.googleapis.com/auth/gmail.readonly",
	"https://www.googleapis.com/auth/gmail.compose",
}

var googleEndpoint = oauth2.Endpoint{
	AuthURL:  "https://accounts.google.com/o/oauth2/auth",
	TokenURL: "https://oauth2.googleapis.com/token",
}

type Credentials struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

func ReadClientSecret(path string) (Credentials, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return Credentials{}, err
	}
	var f struct {
		Installed *Credentials `json:"installed"`
	}
	if err := json.Unmarshal(body, &f); err != nil {
		return Credentials{}, fmt.Errorf("%s: %w", path, err)
	}
	if f.Installed == nil || f.Installed.ClientID == "" || f.Installed.ClientSecret == "" {
		return Credentials{}, fmt.Errorf("%s is not a desktop-app OAuth client (expected an \"installed\" section)", path)
	}
	return Credentials{ClientID: f.Installed.ClientID, ClientSecret: f.Installed.ClientSecret}, nil
}

func (c Credentials) config(redirect string, endpoint oauth2.Endpoint) *oauth2.Config {
	return &oauth2.Config{ClientID: c.ClientID, ClientSecret: c.ClientSecret, Endpoint: endpoint, Scopes: Scopes, RedirectURL: redirect}
}

func (c Credentials) HTTPClient(ctx context.Context) *http.Client {
	return c.config("", googleEndpoint).Client(ctx, &oauth2.Token{RefreshToken: c.RefreshToken})
}

func Authorize(ctx context.Context, c Credentials, open func(url string) error) (Credentials, error) {
	return authorize(ctx, c, googleEndpoint, open)
}

func authorize(ctx context.Context, c Credentials, endpoint oauth2.Endpoint, open func(string) error) (Credentials, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return c, err
	}
	defer ln.Close()
	cfg := c.config(fmt.Sprintf("http://%s/callback", ln.Addr()), endpoint)
	state := randomHex(16)
	verifier := oauth2.GenerateVerifier()

	codes := make(chan string, 1)
	failures := make(chan error, 1)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		switch {
		case q.Get("state") != state:
			http.Error(w, "state mismatch", http.StatusBadRequest)
			select {
			case failures <- errors.New("oauth state mismatch; start again"):
			default:
			}
		case q.Get("error") != "":
			http.Error(w, "authorization denied", http.StatusForbidden)
			select {
			case failures <- fmt.Errorf("authorization denied: %s", q.Get("error")):
			default:
			}
		default:
			fmt.Fprintln(w, "Baseet is connected to Gmail. You can close this tab.")
			select {
			case codes <- q.Get("code"):
			default:
			}
		}
	})}
	go srv.Serve(ln)
	defer srv.Close()

	authURL := cfg.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("prompt", "consent"))
	if err := open(authURL); err != nil {
		return c, err
	}
	select {
	case <-ctx.Done():
		return c, ctx.Err()
	case err := <-failures:
		return c, err
	case code := <-codes:
		tok, err := cfg.Exchange(ctx, code, oauth2.VerifierOption(verifier))
		if err != nil {
			return c, err
		}
		if tok.RefreshToken == "" {
			return c, errors.New("google returned no refresh token; remove Baseet's access in your Google account and try again")
		}
		c.RefreshToken = tok.RefreshToken
		return c, nil
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
