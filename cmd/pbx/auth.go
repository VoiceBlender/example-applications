package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
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

//go:embed web/signup.html
var signupHTML string

var loginTemplate = template.Must(template.New("login").Parse(loginHTML))
var signupTemplate = template.Must(template.New("signup").Parse(signupHTML))

type loginPageData struct {
	Next   string
	Failed bool
	Error  string
}

const (
	cookieAdmin = "pbx_admin"
	cookiePhone = "pbx_phone"
	sessionTTL  = 12 * time.Hour
)

// ctxKey is the type for request-context keys owned by this package.
type ctxKey string

// ctxTenant carries the resolved tenant id through gated handlers.
const ctxTenant ctxKey = "tenant"

// tenantFromCtx returns the tenant id the gate resolved for this request.
func tenantFromCtx(r *http.Request) string {
	v, _ := r.Context().Value(ctxTenant).(string)
	return v
}

// adminIdentity is what an admin console session carries: which user, their
// tenant, and whether they are the cross-tenant superadmin. (Mirrors phoneIdentity.)
type adminIdentity struct {
	username string
	tenantID string
	super    bool
	expiry   time.Time
}

// sessionStore is the admin-session token store carrying the signed-in user's
// tenant. Backed by Redis so sessions survive a server restart; the in-memory
// map is the fast path. Entries are evicted lazily on read when expired.
type sessionStore struct {
	mu    sync.Mutex
	m     map[string]adminIdentity // token → identity
	store *redisStore
}

func newSessionStore(store *redisStore) *sessionStore {
	return &sessionStore{m: make(map[string]adminIdentity), store: store}
}

// load restores persisted sessions on startup (dropping expired ones).
func (s *sessionStore) load(ctx context.Context) {
	recs, err := s.store.LoadSessions(ctx, adminSessionsKey)
	if err != nil {
		return
	}
	now := time.Now()
	s.mu.Lock()
	for _, r := range recs {
		if now.After(r.Expiry) {
			_ = s.store.DeleteSession(ctx, adminSessionsKey, r.Token)
			continue
		}
		s.m[r.Token] = adminIdentity{username: r.Username, tenantID: r.TenantID, super: r.Super, expiry: r.Expiry}
	}
	s.mu.Unlock()
}

func (s *sessionStore) Create(username, tenantID string, super bool) string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	token := hex.EncodeToString(buf)
	exp := time.Now().Add(sessionTTL)
	s.mu.Lock()
	s.m[token] = adminIdentity{username: username, tenantID: tenantID, super: super, expiry: exp}
	s.mu.Unlock()
	_ = s.store.SaveSession(context.Background(), adminSessionsKey, sessionRecord{Token: token, Username: username, TenantID: tenantID, Super: super, Expiry: exp})
	return token
}

// get returns the session identity, rolling its in-memory TTL. ok=false if
// missing/expired.
func (s *sessionStore) get(token string) (adminIdentity, bool) {
	if token == "" {
		return adminIdentity{}, false
	}
	s.mu.Lock()
	id, ok := s.m[token]
	if !ok {
		s.mu.Unlock()
		return adminIdentity{}, false
	}
	if time.Now().After(id.expiry) {
		delete(s.m, token)
		s.mu.Unlock()
		_ = s.store.DeleteSession(context.Background(), adminSessionsKey, token)
		return adminIdentity{}, false
	}
	id.expiry = time.Now().Add(sessionTTL)
	s.m[token] = id
	s.mu.Unlock()
	return id, true
}

func (s *sessionStore) Delete(token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	delete(s.m, token)
	s.mu.Unlock()
	_ = s.store.DeleteSession(context.Background(), adminSessionsKey, token)
}

// phoneSession identity carried by a softphone cookie session.
type phoneIdentity struct {
	account   string
	extNumber string
	tenantID  string
	expiry    time.Time
}

// phoneSessionStore is the softphone equivalent of sessionStore (Redis-backed so
// sessions survive a restart); each token carries the signed-in account + its
// parent extension number + tenant.
type phoneSessionStore struct {
	mu    sync.Mutex
	m     map[string]phoneIdentity
	store *redisStore
}

func newPhoneSessionStore(store *redisStore) *phoneSessionStore {
	return &phoneSessionStore{m: make(map[string]phoneIdentity), store: store}
}

func (s *phoneSessionStore) load(ctx context.Context) {
	recs, err := s.store.LoadSessions(ctx, phoneSessionsKey)
	if err != nil {
		return
	}
	now := time.Now()
	s.mu.Lock()
	for _, r := range recs {
		if now.After(r.Expiry) {
			_ = s.store.DeleteSession(ctx, phoneSessionsKey, r.Token)
			continue
		}
		s.m[r.Token] = phoneIdentity{account: r.Account, extNumber: r.ExtNumber, tenantID: r.TenantID, expiry: r.Expiry}
	}
	s.mu.Unlock()
}

func (s *phoneSessionStore) create(account, extNumber, tenantID string) string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	token := hex.EncodeToString(buf)
	exp := time.Now().Add(sessionTTL)
	s.mu.Lock()
	s.m[token] = phoneIdentity{account: account, extNumber: extNumber, tenantID: tenantID, expiry: exp}
	s.mu.Unlock()
	_ = s.store.SaveSession(context.Background(), phoneSessionsKey, sessionRecord{Token: token, Account: account, ExtNumber: extNumber, TenantID: tenantID, Expiry: exp})
	return token
}

func (s *phoneSessionStore) valid(token string) bool {
	_, ok := s.get(token)
	return ok
}

func (s *phoneSessionStore) get(token string) (phoneIdentity, bool) {
	if token == "" {
		return phoneIdentity{}, false
	}
	s.mu.Lock()
	id, ok := s.m[token]
	if !ok {
		s.mu.Unlock()
		return phoneIdentity{}, false
	}
	if time.Now().After(id.expiry) {
		delete(s.m, token)
		s.mu.Unlock()
		_ = s.store.DeleteSession(context.Background(), phoneSessionsKey, token)
		return phoneIdentity{}, false
	}
	id.expiry = time.Now().Add(sessionTTL) // rolling (in memory)
	s.m[token] = id
	s.mu.Unlock()
	return id, true
}

func (s *phoneSessionStore) delete(token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	delete(s.m, token)
	s.mu.Unlock()
	_ = s.store.DeleteSession(context.Background(), phoneSessionsKey, token)
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, name, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
		MaxAge:   -1,
	})
}

// gate is the shared session guard: a valid cookie session is required, and the
// session's tenant is injected into the request context. Browser navigations are
// redirected to loginPath; APIs/WebSockets get a 401. resolve maps a token to its
// tenant id (ok=false when the session is missing/expired).
func gate(cookie, loginPath string, resolve func(token string) (string, bool), next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(cookie); err == nil {
			if tenantID, ok := resolve(c.Value); ok {
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxTenant, tenantID)))
				return
			}
		}
		if wantsHTML(r) {
			http.Redirect(w, r, loginPath+"?next="+r.URL.RequestURI(), http.StatusSeeOther)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// requireAuth gates a handler behind a valid admin session and threads the
// session's tenant into the request. Browser navigations redirect to /login.
func (a *app) requireAuth(next http.Handler) http.Handler {
	return gate(cookieAdmin, "/login", func(token string) (string, bool) {
		id, ok := a.sessions.get(token)
		return id.tenantID, ok
	}, next)
}

// requirePhone gates a softphone handler behind a valid WebRTC-account session
// and threads its tenant into the request.
func (a *app) requirePhone(next http.Handler) http.Handler {
	return gate(cookiePhone, "/phone/login", func(token string) (string, bool) {
		id, ok := a.phoneSessions.get(token)
		return id.tenantID, ok
	}, next)
}

// requireSuper gates a handler behind a valid superadmin session (cross-tenant).
// Browser navigations redirect to /login; APIs get a 403.
func (a *app) requireSuper(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(cookieAdmin); err == nil {
			if id, ok := a.sessions.get(c.Value); ok && id.super {
				next.ServeHTTP(w, r)
				return
			}
		}
		if wantsHTML(r) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		http.Error(w, "forbidden", http.StatusForbidden)
	})
}

// isSuperadmin reports whether username/password matches the env-configured
// cross-tenant superadmin (disabled when SUPERADMIN is unset).
func (a *app) isSuperadmin(username, password string) bool {
	if a.superUser == "" || a.superPass == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(username), []byte(a.superUser)) == 1 &&
		subtle.ConstantTimeCompare([]byte(password), []byte(a.superPass)) == 1
}

func wantsHTML(r *http.Request) bool {
	if r.Header.Get("Upgrade") != "" {
		return false
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// handleLogin renders the form (GET) and verifies username+password (POST). A
// successful login issues a session carrying the user's tenant.
func (a *app) handleLogin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = loginTemplate.Execute(w, loginPageData{
			Next:   sanitiseNext(r.URL.Query().Get("next")),
			Failed: r.URL.Query().Get("err") == "1",
		})
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		username := strings.TrimSpace(r.Form.Get("username"))
		password := r.Form.Get("password")
		next := sanitiseNext(r.Form.Get("next"))
		// Cross-tenant superadmin (env-configured), checked before tenant users.
		if a.isSuperadmin(username, password) {
			setSessionCookie(w, r, cookieAdmin, a.sessions.Create(username, "", true))
			a.log.Info("superadmin login", "remote", r.RemoteAddr)
			http.Redirect(w, r, "/admin", http.StatusSeeOther)
			return
		}
		user, found := a.tenants.user(username)
		ok := found && user.Password != "" && subtle.ConstantTimeCompare([]byte(password), []byte(user.Password)) == 1
		if !ok {
			time.Sleep(250 * time.Millisecond)
			http.Redirect(w, r, "/login?err=1&next="+next, http.StatusSeeOther)
			return
		}
		setSessionCookie(w, r, cookieAdmin, a.sessions.Create(user.Username, user.TenantID, false))
		a.log.Info("login", "user", user.Username, "tenant", user.TenantID, "remote", r.RemoteAddr)
		http.Redirect(w, r, next, http.StatusSeeOther)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleSignup renders the self-service signup form (GET) and creates a new
// tenant + admin user (POST), logging the new admin in.
func (a *app) handleSignup(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = signupTemplate.Execute(w, loginPageData{Error: signupError(r.URL.Query().Get("err"))})
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		name := r.Form.Get("company")
		username := r.Form.Get("username")
		password := r.Form.Get("password")
		if strings.TrimSpace(password) == "" {
			http.Redirect(w, r, "/signup?err=nopass", http.StatusSeeOther)
			return
		}
		tenant, user, err := a.tenants.signup(r.Context(), name, username, password)
		if err != nil {
			code := "bad"
			switch err {
			case errTenantExists:
				code = "tenant"
			case errUserExists:
				code = "user"
			}
			http.Redirect(w, r, "/signup?err="+code, http.StatusSeeOther)
			return
		}
		setSessionCookie(w, r, cookieAdmin, a.sessions.Create(user.Username, user.TenantID, false))
		a.log.Info("signup", "tenant", tenant.ID, "user", user.Username, "remote", r.RemoteAddr)
		http.Redirect(w, r, "/", http.StatusSeeOther)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// signupError maps a signup error code to a human message for the form.
func signupError(code string) string {
	switch code {
	case "tenant":
		return "That company name is already taken."
	case "user":
		return "That username is already taken."
	case "nopass":
		return "A password is required."
	case "bad":
		return "Company name and username are required."
	}
	return ""
}

func (a *app) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieAdmin); err == nil {
		a.sessions.Delete(c.Value)
	}
	clearSessionCookie(w, r, cookieAdmin)
	if wantsHTML(r) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sanitiseNext clamps redirects to same-origin relative paths.
func sanitiseNext(raw string) string {
	if raw == "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return "/"
	}
	return raw
}
