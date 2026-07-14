package main

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"html/template"
	"net/http"
	"strings"
	"sync"
	"time"
)

//go:embed web/login.html
var loginHTML string

var loginTemplate = template.Must(template.New("login").Parse(loginHTML))

type loginPageData struct {
	Next  string
	Error string
}

const (
	cookieUser = "ptt_user"
	sessionTTL = 12 * time.Hour
)

// ctxKey is the type for request-context keys owned by this package.
type ctxKey string

// ctxUser carries the resolved username through gated handlers.
const ctxUser ctxKey = "user"

// userFromCtx returns the username the gate resolved for this request.
func userFromCtx(r *http.Request) string {
	v, _ := r.Context().Value(ctxUser).(string)
	return v
}

// identity is what a session carries: just the username (usernames are the only
// credential — this is a demo login).
type identity struct {
	username string
	expiry   time.Time
}

// sessionStore maps opaque tokens to usernames. Backed by Redis so sessions
// survive a restart; the in-memory map is the fast path. Entries are evicted
// lazily on read when expired.
type sessionStore struct {
	mu    sync.Mutex
	m     map[string]identity // token → identity
	store *redisStore
}

func newSessionStore(store *redisStore) *sessionStore {
	return &sessionStore{m: make(map[string]identity), store: store}
}

// load restores persisted sessions on startup (dropping expired ones).
func (s *sessionStore) load(ctx context.Context) {
	recs, err := s.store.LoadSessions(ctx)
	if err != nil {
		return
	}
	now := time.Now()
	s.mu.Lock()
	for _, r := range recs {
		if now.After(r.Expiry) {
			_ = s.store.DeleteSession(ctx, r.Token)
			continue
		}
		s.m[r.Token] = identity{username: r.Username, expiry: r.Expiry}
	}
	s.mu.Unlock()
}

func (s *sessionStore) create(username string) string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	token := hex.EncodeToString(buf)
	exp := time.Now().Add(sessionTTL)
	s.mu.Lock()
	s.m[token] = identity{username: username, expiry: exp}
	s.mu.Unlock()
	_ = s.store.SaveSession(context.Background(), sessionRecord{Token: token, Username: username, Expiry: exp})
	return token
}

// get returns the session identity, rolling its in-memory TTL. ok=false if
// missing/expired.
func (s *sessionStore) get(token string) (identity, bool) {
	if token == "" {
		return identity{}, false
	}
	s.mu.Lock()
	id, ok := s.m[token]
	if !ok {
		s.mu.Unlock()
		return identity{}, false
	}
	if time.Now().After(id.expiry) {
		delete(s.m, token)
		s.mu.Unlock()
		_ = s.store.DeleteSession(context.Background(), token)
		return identity{}, false
	}
	id.expiry = time.Now().Add(sessionTTL) // rolling (in memory)
	s.m[token] = id
	s.mu.Unlock()
	return id, true
}

func (s *sessionStore) delete(token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	delete(s.m, token)
	s.mu.Unlock()
	_ = s.store.DeleteSession(context.Background(), token)
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieUser,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieUser,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
		MaxAge:   -1,
	})
}

// requireUser gates a handler behind a valid session and threads the username
// into the request context. Browser navigations redirect to /login; APIs and
// WebSockets get a 401.
func (a *app) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(cookieUser); err == nil {
			if id, ok := a.sessions.get(c.Value); ok {
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxUser, id.username)))
				return
			}
		}
		if wantsHTML(r) {
			http.Redirect(w, r, "/login?next="+r.URL.RequestURI(), http.StatusSeeOther)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

func wantsHTML(r *http.Request) bool {
	if r.Header.Get("Upgrade") != "" {
		return false
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// validUsername accepts 1–24 chars of letters, digits, dash, underscore, dot.
func validUsername(s string) bool {
	if s == "" || len(s) > 24 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.':
		default:
			return false
		}
	}
	return true
}

// handleLogin renders the username form (GET) and claims a username + issues a
// session (POST). There is no password — this is a demo identity.
func (a *app) handleLogin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = loginTemplate.Execute(w, loginPageData{
			Next:  sanitiseNext(r.URL.Query().Get("next")),
			Error: loginError(r.URL.Query().Get("err")),
		})
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		username := strings.TrimSpace(r.Form.Get("username"))
		next := sanitiseNext(r.Form.Get("next"))
		if !validUsername(username) {
			http.Redirect(w, r, "/login?err=bad&next="+next, http.StatusSeeOther)
			return
		}
		if _, err := a.store.ClaimUser(r.Context(), username); err != nil {
			a.log.Warn("claim username", "user", username, "error", err)
		}
		setSessionCookie(w, r, a.sessions.create(username))
		a.log.Info("login", "user", username, "remote", r.RemoteAddr)
		http.Redirect(w, r, next, http.StatusSeeOther)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *app) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieUser); err == nil {
		a.sessions.delete(c.Value)
	}
	clearSessionCookie(w, r)
	if wantsHTML(r) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func loginError(code string) string {
	if code == "bad" {
		return "Pick a username: 1–24 letters, digits, dash, underscore or dot."
	}
	return ""
}

// sanitiseNext clamps redirects to same-origin relative paths.
func sanitiseNext(raw string) string {
	if raw == "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return "/"
	}
	return raw
}
