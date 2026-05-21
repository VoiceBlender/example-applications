package main

import (
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
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
	Role   string
	Next   string
	Failed bool
	Title  string
}

// Role constants for session cookies and the /login form. Two cookie
// names so the same browser can hold both a supervisor and an agent
// session at once (e.g. for local testing).
const (
	roleSupervisor = "supervisor"
	roleAgent      = "agent"

	cookieSupervisor = "cc_supervisor"
	cookieAgent      = "cc_agent"

	sessionTTL = 12 * time.Hour
)

// authConfig is the static-password configuration. An empty password
// means "no auth on this side" — useful for local dev.
//
// The login form takes a username + password; only the password is
// validated against the configured value. The username is captured for
// the audit log and (for the agent role) makes a natural identity
// label, but any value is accepted — there are no per-user accounts.
type authConfig struct {
	SupervisorPassword string
	AgentPassword      string
}

// authEnabled reports whether at least one side requires auth.
func (c authConfig) authEnabled() bool {
	return c.SupervisorPassword != "" || c.AgentPassword != ""
}

func (c authConfig) requiresAuth(role string) bool {
	switch role {
	case roleSupervisor:
		return c.SupervisorPassword != ""
	case roleAgent:
		return c.AgentPassword != ""
	}
	return false
}

// expected returns the static password configured for the role.
// Empty means auth is disabled for that role.
func (c authConfig) expected(role string) string {
	switch role {
	case roleSupervisor:
		return c.SupervisorPassword
	case roleAgent:
		return c.AgentPassword
	}
	return ""
}

func cookieNameFor(role string) string {
	if role == roleSupervisor {
		return cookieSupervisor
	}
	return cookieAgent
}

// sessionStore is an in-memory token store. Sessions are issued on
// successful login and expire after sessionTTL of inactivity. We do
// not bother with periodic cleanup — entries are evicted on Get when
// expired, and the absolute volume is small (one entry per login).
type sessionStore struct {
	mu sync.Mutex
	m  map[string]storedSession
}

type storedSession struct {
	Role      string
	Username  string
	ExpiresAt time.Time
}

func newSessionStore() *sessionStore {
	return &sessionStore{m: make(map[string]storedSession)}
}

// Create issues a new opaque token bound to (role, username) with a TTL.
// The username is whatever the user typed on the login form — used as
// an audit label and by the agent panel to skip its own name prompt.
func (s *sessionStore) Create(role, username string) string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// rand.Read on Linux returns an error only when the kernel
		// pool isn't initialised yet; in practice unreachable. Fall
		// back to a degenerate token so we never panic in handlers.
		return ""
	}
	token := hex.EncodeToString(buf)
	s.mu.Lock()
	s.m[token] = storedSession{Role: role, Username: username, ExpiresAt: time.Now().Add(sessionTTL)}
	s.mu.Unlock()
	return token
}

// Get returns the session for token, refreshing its expiry on access
// (rolling session). Returns ok=false if the token is unknown or has
// expired.
func (s *sessionStore) Get(token string) (storedSession, bool) {
	if token == "" {
		return storedSession{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.m[token]
	if !ok {
		return storedSession{}, false
	}
	if time.Now().After(sess.ExpiresAt) {
		delete(s.m, token)
		return storedSession{}, false
	}
	sess.ExpiresAt = time.Now().Add(sessionTTL)
	s.m[token] = sess
	return sess, true
}

func (s *sessionStore) Delete(token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	delete(s.m, token)
	s.mu.Unlock()
}

// setSessionCookie writes the cookie for the role on a successful
// login. Secure is set only when the request itself was over TLS so
// local-http development still works.
func setSessionCookie(w http.ResponseWriter, r *http.Request, role, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieNameFor(role),
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request, role string) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieNameFor(role),
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
		MaxAge:   -1,
	})
}

// requireRole wraps next so it only runs if the request carries a
// valid session cookie for the role. If the role's password is empty
// (auth disabled), next runs unchanged.
//
// On rejection: HTML-shaped requests redirect to /login?role=...;
// everything else (API calls, WebSocket upgrades) gets a 401. This
// keeps fetch/XHR/WebSocket clients from silently following the
// redirect into HTML.
func (a *app) requireRole(role string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.auth.requiresAuth(role) {
			next.ServeHTTP(w, r)
			return
		}
		cookie, err := r.Cookie(cookieNameFor(role))
		if err == nil {
			if sess, ok := a.sessions.Get(cookie.Value); ok && sess.Role == role {
				next.ServeHTTP(w, r)
				return
			}
		}
		if wantsHTML(r) {
			http.Redirect(w, r, "/login?role="+role+"&next="+r.URL.RequestURI(), http.StatusSeeOther)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// wantsHTML is a small heuristic for "this looks like a browser
// navigation we can redirect, not an API/WS call we should 401."
// The supervisor + agent WebSocket clients fetch JSON or upgrade —
// neither advertises text/html.
func wantsHTML(r *http.Request) bool {
	if r.Header.Get("Upgrade") != "" {
		return false
	}
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "text/html")
}

// handleLogin handles GET (render form) and POST (verify password,
// issue session, redirect to ?next=). The form posts to the same path.
func (a *app) handleLogin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		role := r.URL.Query().Get("role")
		if role != roleSupervisor && role != roleAgent {
			role = roleSupervisor
		}
		// If auth is disabled for this role, send the user straight in.
		if !a.auth.requiresAuth(role) {
			http.Redirect(w, r, defaultNextFor(role), http.StatusSeeOther)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = loginTemplate.Execute(w, loginPageData{
			Role:    role,
			Next:    sanitiseNext(r.URL.Query().Get("next"), role),
			Failed:  r.URL.Query().Get("err") == "1",
			Title:   loginTitleFor(role),
		})
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		role := r.Form.Get("role")
		username := r.Form.Get("username")
		password := r.Form.Get("password")
		next := sanitiseNext(r.Form.Get("next"), role)
		wantPass := a.auth.expected(role)
		// Only the password is validated against the configured value;
		// the username is just a label, accepted as-is for the audit log.
		// ConstantTimeCompare keeps response time independent of how
		// close the guess was.
		passOK := wantPass != "" && subtle.ConstantTimeCompare([]byte(password), []byte(wantPass)) == 1
		if !passOK {
			// Rate-limit-ish: a tiny sleep keeps a naive attacker from
			// spinning the login endpoint flat-out. Not a real defence;
			// real deployments would use proper rate limiting.
			time.Sleep(250 * time.Millisecond)
			http.Redirect(w, r, "/login?role="+role+"&err=1&next="+next, http.StatusSeeOther)
			return
		}
		token := a.sessions.Create(role, username)
		setSessionCookie(w, r, role, token)
		a.log.Info("login", "role", role, "username", username, "remote", r.RemoteAddr)
		http.Redirect(w, r, next, http.StatusSeeOther)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleLogout deletes the session for the role indicated by the
// form's `role` (or by whichever cookie is present) and clears the
// cookie. Accepts POST and GET; POST is preferred from forms.
func (a *app) handleLogout(w http.ResponseWriter, r *http.Request) {
	role := r.URL.Query().Get("role")
	if role == "" && r.Method == http.MethodPost {
		_ = r.ParseForm()
		role = r.Form.Get("role")
	}
	roles := []string{role}
	if role == "" {
		// No role specified: clear any cookies we find.
		roles = []string{roleSupervisor, roleAgent}
	}
	for _, role := range roles {
		if c, err := r.Cookie(cookieNameFor(role)); err == nil {
			a.sessions.Delete(c.Value)
		}
		clearSessionCookie(w, r, role)
	}
	if wantsHTML(r) {
		dest := "/login"
		if role != "" {
			dest = "/login?role=" + role
		}
		http.Redirect(w, r, dest, http.StatusSeeOther)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAuthWhoami reports the role + username of the current auth
// session, if any. Returns 204 No Content when neither cookie is set
// (or auth is disabled). Used by the agent panel to skip its own
// name-prompt screen — the username from the login form is reused as
// the agent's display name.
func (a *app) handleAuthWhoami(w http.ResponseWriter, r *http.Request) {
	for _, role := range []string{roleAgent, roleSupervisor} {
		c, err := r.Cookie(cookieNameFor(role))
		if err != nil {
			continue
		}
		sess, ok := a.sessions.Get(c.Value)
		if !ok || sess.Role != role {
			continue
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"role":     sess.Role,
			"username": sess.Username,
		})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func defaultNextFor(role string) string {
	if role == roleAgent {
		return "/agent"
	}
	return "/"
}

func loginTitleFor(role string) string {
	if role == roleAgent {
		return "Agent sign-in"
	}
	return "Supervisor sign-in"
}

// sanitiseNext clamps redirect targets to same-origin relative paths
// so a crafted ?next= can't bounce the freshly-authenticated user to
// an arbitrary external URL.
func sanitiseNext(raw, role string) string {
	if raw == "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return defaultNextFor(role)
	}
	return raw
}
