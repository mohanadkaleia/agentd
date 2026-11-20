package gmail

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestAuthorizeUsesPKCEAndChecksState(t *testing.T) {
	var gotVerifier string
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		gotVerifier = r.Form.Get("code_verifier")
		if r.Form.Get("code") != "the-code" {
			http.Error(w, "bad code", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "refresh_token": "rt", "token_type": "Bearer", "expires_in": 3600})
	}))
	defer tokenSrv.Close()
	endpoint := oauth2.Endpoint{AuthURL: "https://accounts.example/auth", TokenURL: tokenSrv.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	browser := func(state string) func(string) error {
		return func(authURL string) error {
			u, _ := url.Parse(authURL)
			q := u.Query()
			if q.Get("code_challenge_method") != "S256" || q.Get("access_type") != "offline" || q.Get("scope") == "" {
				t.Errorf("auth url = %s", authURL)
			}
			if state == "" {
				state = q.Get("state")
			}
			go http.Get(q.Get("redirect_uri") + "?code=the-code&state=" + url.QueryEscape(state))
			return nil
		}
	}

	creds, err := authorize(ctx, Credentials{ClientID: "id", ClientSecret: "secret"}, endpoint, browser(""))
	if err != nil {
		t.Fatal(err)
	}
	if creds.RefreshToken != "rt" || gotVerifier == "" {
		t.Fatalf("refresh=%q verifier=%q", creds.RefreshToken, gotVerifier)
	}

	if _, err := authorize(ctx, Credentials{ClientID: "id", ClientSecret: "secret"}, endpoint, browser("forged")); err == nil {
		t.Fatal("a callback with the wrong state was accepted")
	}
}
