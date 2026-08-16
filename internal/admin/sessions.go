package admin

// In-memory session cookies. A random token is stored server-side with an
// expiry; the browser keeps only the token. On server restart, sessions are
// cleared (the user must log in again) — acceptable for a local console.

import (
	"net/http"
	"sync"
	"time"
)

const sessionCookie = "dsh_admin"

type session struct {
	token   string
	expires time.Time
}

type sessions struct {
	mu       sync.Mutex
	ttl      time.Duration
	store    map[string]time.Time // token -> expiry
	lookupMu sync.Mutex
}

func newSessions(ttl time.Duration) *sessions {
	return &sessions{ttl: ttl, store: map[string]time.Time{}}
}

// Create issues a new session token.
func (s *sessions) Create() string {
	tok := randToken(24)
	s.mu.Lock()
	s.store[tok] = time.Now().Add(s.ttl)
	s.mu.Unlock()
	return tok
}

// Valid reports whether a token is a live session.
func (s *sessions) Valid(token string) bool {
	if token == "" {
		return false
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.store[token]
	if !ok {
		return false
	}
	if now.After(exp) {
		delete(s.store, token)
		return false
	}
	return true
}

// Destroy removes a session token (logout).
func (s *sessions) Destroy(token string) {
	s.mu.Lock()
	delete(s.store, token)
	s.mu.Unlock()
}

func setSessionCookie(w http.ResponseWriter, token string, ttl time.Duration, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ttl.Seconds()),
	})
}

func clearSessionCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, Secure: secure,
		SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

func (s *Server) sessionToken(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

// isAuthed reports whether the request carries a live session.
func (s *Server) isAuthed(r *http.Request) bool {
	return s.sess.Valid(s.sessionToken(r))
}

// requireAuth wraps a handler with a login check.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.isAuthed(r) {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		next(w, r)
	}
}
