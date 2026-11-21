package api

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookie = "agentd_session"
	csrfHeader    = "X-CSRF-Token"
	sessionTTL    = 12 * time.Hour
	maxSessions   = 20
)

//go:embed ui
var uiFiles embed.FS

type session struct {
	csrf    string
	expires time.Time
}

type sessions struct {
	mu   sync.Mutex
	byID map[string]session
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *sessions) create(now time.Time) (string, session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byID == nil {
		s.byID = map[string]session{}
	}
	for id, v := range s.byID {
		if now.After(v.expires) {
			delete(s.byID, id)
		}
	}
	for len(s.byID) >= maxSessions {
		var oldest string
		for id, v := range s.byID {
			if oldest == "" || v.expires.Before(s.byID[oldest].expires) {
				oldest = id
			}
		}
		delete(s.byID, oldest)
	}
	id, v := randomHex(32), session{csrf: randomHex(32), expires: now.Add(sessionTTL)}
	s.byID[id] = v
	return id, v
}

func (s *sessions) get(r *http.Request, now time.Time) (string, session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", session{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.byID[c.Value]
	if !ok || now.After(v.expires) {
		return "", session{}, false
	}
	return c.Value, v, true
}

func (s *sessions) drop(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byID, id)
}

func (s *Server) bearer(r *http.Request) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return s.Token != "" && ok && subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) == 1
}

func (s *Server) cookieAuth(r *http.Request) bool {
	_, v, ok := s.sessions.get(r, time.Now())
	if !ok {
		return false
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(r.Header.Get(csrfHeader)), []byte(v.csrf)) == 1
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Token string `json:"token"`
	}
	if err := decode(r, &body); err != nil {
		return err
	}
	if s.Token == "" || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(body.Token)), []byte(s.Token)) != 1 {
		time.Sleep(500 * time.Millisecond)
		return statusError{http.StatusUnauthorized, "wrong token"}
	}
	id, v := s.sessions.create(time.Now())
	setSessionCookie(w, r, id, int(sessionTTL.Seconds()))
	return ok(w, map[string]string{"csrf": v.csrf})
}

func (s *Server) currentSession(w http.ResponseWriter, r *http.Request) error {
	_, v, found := s.sessions.get(r, time.Now())
	if !found {
		return statusError{http.StatusUnauthorized, "not signed in"}
	}
	return ok(w, map[string]string{"csrf": v.csrf})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) error {
	if id, _, found := s.sessions.get(r, time.Now()); found {
		s.sessions.drop(id)
	}
	setSessionCookie(w, r, "", -1)
	return ok(w, map[string]string{"status": "signed out"})
}

func (s *Server) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	return origin == "" || strings.TrimPrefix(strings.TrimPrefix(origin, "http://"), "https://") == r.Host
}

func uiHandler() http.Handler {
	static, _ := fs.Sub(uiFiles, "ui")
	files := http.FileServer(http.FS(static))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; "+
			"base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != "/" && strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		files.ServeHTTP(w, r)
	})
}
