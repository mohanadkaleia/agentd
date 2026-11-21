package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"

	"agentd/internal/demo"
)

func browser(t *testing.T, e *env) (*http.Client, string) {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	res, err := c.Post(e.srv.URL+"/ui/session", "application/json", strings.NewReader(`{"token":"`+token+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body struct{ CSRF string }
	json.NewDecoder(res.Body).Decode(&body)
	if res.StatusCode != http.StatusOK || body.CSRF == "" {
		t.Fatalf("login = %d", res.StatusCode)
	}
	cookie := res.Header.Get("Set-Cookie")
	for _, want := range []string{"HttpOnly", "SameSite=Strict", "agentd_session="} {
		if !strings.Contains(cookie, want) {
			t.Fatalf("session cookie %q lacks %s", cookie, want)
		}
	}
	return c, body.CSRF
}

func send(t *testing.T, c *http.Client, method, url string, headers map[string]string) int {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func TestBrowserSessionNeedsCSRFForChanges(t *testing.T) {
	e := setup(t, demo.Script)
	if res, _ := http.Post(e.srv.URL+"/ui/session", "application/json", strings.NewReader(`{"token":"wrong"}`)); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token = %d", res.StatusCode)
	}
	c, csrf := browser(t, e)
	if code := send(t, c, "GET", e.srv.URL+"/api/assignments", nil); code != http.StatusOK {
		t.Fatalf("read with session = %d", code)
	}
	stop := e.srv.URL + "/api/system/emergency-stop"
	if code := send(t, c, "POST", stop, nil); code != http.StatusUnauthorized {
		t.Fatalf("change without CSRF token = %d, want 401", code)
	}
	if code := send(t, c, "POST", stop, map[string]string{"X-CSRF-Token": csrf, "Origin": "http://evil.example"}); code != http.StatusForbidden {
		t.Fatalf("cross-origin change = %d, want 403", code)
	}
	if code := send(t, c, "POST", stop, map[string]string{"X-CSRF-Token": csrf}); code != http.StatusOK {
		t.Fatalf("change with CSRF token = %d", code)
	}
	if code := send(t, c, "DELETE", e.srv.URL+"/ui/session", nil); code != http.StatusOK {
		t.Fatalf("logout = %d", code)
	}
	if code := send(t, c, "GET", e.srv.URL+"/api/assignments", nil); code != http.StatusUnauthorized {
		t.Fatalf("read after logout = %d, want 401", code)
	}
}

func TestUIIsServedWithAStrictPolicy(t *testing.T) {
	e := setup(t, demo.Script)
	res, err := http.Get(e.srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	csp := res.Header.Get("Content-Security-Policy")
	if res.StatusCode != http.StatusOK || !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe") {
		t.Fatalf("ui = %d, csp %q", res.StatusCode, csp)
	}
	if code := send(t, http.DefaultClient, "GET", e.srv.URL+"/app.js", nil); code != http.StatusOK {
		t.Fatalf("app.js = %d", code)
	}
}
