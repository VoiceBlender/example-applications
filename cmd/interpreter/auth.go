package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"
)

// A single static credential, shared by everyone who is allowed in.
//
// This is a demo gate, not an identity system: it keeps a public deployment from
// being a free STT/TTS meter for passers-by. It deliberately does NOT identify
// who is who — both participants log in with the same credential and still pick
// their own display name for the call, exactly as they did before.
//
// Auth is opt-in: with AUTH_PASSWORD unset the gate is a no-op, so local trials
// stay friction-free. That mirrors the contact-centre example, whose panels are
// likewise unguarded until a password is configured.

const (
	authCookie = "interp_auth"
	authTTL    = 12 * time.Hour
)

// authConfig is the configured credential. An empty Password disables the gate.
type authConfig struct {
	Username string
	Password string
}

func (c authConfig) enabled() bool { return c.Password != "" }

// check validates a submitted credential.
//
// Both halves are compared in constant time and BOTH are always compared, even
// when the username is already wrong, so response time leaks neither which half
// failed nor how much of the password matched.
func (c authConfig) check(username, password string) bool {
	if !c.enabled() {
		return true
	}
	userOK := subtle.ConstantTimeCompare([]byte(username), []byte(c.Username)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(password), []byte(c.Password)) == 1
	return userOK && passOK
}

// ── login sessions ────────────────────────────────────────────────────────────

// loginSession is a browser's proof it got past the gate. Deliberately named
// apart from `session`, which in this app means an interpreted conversation.
type loginSession struct {
	Username  string
	ExpiresAt time.Time
}

// loginStore holds live logins in memory. Restarting the app signs everyone out,
// which is fine for a demo and one less thing to persist.
type loginStore struct {
	mu sync.Mutex
	m  map[string]loginSession
}

func newLoginStore() *loginStore { return &loginStore{m: make(map[string]loginSession)} }

func (s *loginStore) issue(username string) string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	token := hex.EncodeToString(b)
	s.mu.Lock()
	s.m[token] = loginSession{Username: username, ExpiresAt: time.Now().Add(authTTL)}
	s.mu.Unlock()
	return token
}

// get returns the login for a token, rolling its expiry forward on use.
func (s *loginStore) get(token string) (loginSession, bool) {
	if token == "" {
		return loginSession{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.m[token]
	if !ok {
		return loginSession{}, false
	}
	if time.Now().After(sess.ExpiresAt) {
		delete(s.m, token)
		return loginSession{}, false
	}
	sess.ExpiresAt = time.Now().Add(authTTL)
	s.m[token] = sess
	return sess, true
}

func (s *loginStore) drop(token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	delete(s.m, token)
	s.mu.Unlock()
}

// ── HTTP ──────────────────────────────────────────────────────────────────────

// authed reports whether a request carries a valid login (or the gate is off).
func (a *app) authed(r *http.Request) bool {
	if !a.auth.enabled() {
		return true
	}
	c, err := r.Cookie(authCookie)
	if err != nil {
		return false
	}
	_, ok := a.logins.get(c.Value)
	return ok
}

// requireLogin wraps a handler so it only runs for an authenticated request.
//
// Page loads are redirected to the login form; everything else — fetch calls and
// WebSocket upgrades — gets a 401 instead, so a JS client never silently follows
// the redirect and tries to parse the login page as its response.
func (a *app) requireLogin(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.authed(r) {
			next(w, r)
			return
		}
		if wantsHTML(r) {
			http.Redirect(w, r, "/login?next="+urlEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// wantsHTML distinguishes a browser navigating to a page from a fetch or a
// WebSocket upgrade, which want a status code rather than a redirect.
func wantsHTML(r *http.Request) bool {
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	if r.Header.Get("X-Requested-With") != "" {
		return false
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

func (a *app) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !a.auth.enabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	next := safeNext(r.FormValue("next"))

	if r.Method == http.MethodPost {
		if a.auth.check(r.FormValue("username"), r.FormValue("password")) {
			token := a.logins.issue(r.FormValue("username"))
			if token == "" {
				http.Error(w, "could not start a session", http.StatusInternalServerError)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name: authCookie, Value: token, Path: "/",
				HttpOnly: true, SameSite: http.SameSiteLaxMode,
				Secure: r.TLS != nil, MaxAge: int(authTTL.Seconds()),
			})
			http.Redirect(w, r, next, http.StatusSeeOther)
			return
		}
		a.log.Warn("failed login", "remote", r.RemoteAddr)
		w.WriteHeader(http.StatusUnauthorized)
		renderLogin(w, next, true)
		return
	}
	renderLogin(w, next, false)
}

func (a *app) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(authCookie); err == nil {
		a.logins.drop(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: authCookie, Value: "", Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: r.TLS != nil, MaxAge: -1,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func renderLogin(w http.ResponseWriter, next string, failed bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = loginTemplate.Execute(w, map[string]any{"Next": next, "Failed": failed})
}

// safeNext keeps the post-login redirect on this site: an attacker-supplied
// absolute URL (or a scheme-relative //evil.example) would otherwise turn the
// login form into an open redirect.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	return next
}
