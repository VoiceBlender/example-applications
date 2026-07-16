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

	"golang.org/x/crypto/bcrypt"
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

	minPasswordLen = 8
	maxPasswordLen = 72 // bcrypt only hashes the first 72 bytes — reject longer rather than silently truncate
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

// validPassword accepts 8–72 characters (any bytes). The upper bound matches
// bcrypt's input limit so nothing is silently truncated at hashing time.
func validPassword(s string) bool {
	return len(s) >= minPasswordLen && len(s) <= maxPasswordLen
}

// hashPassword returns a bcrypt hash (salt + cost encoded in the string). The
// plaintext is never stored.
func hashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(h), err
}

// checkPassword reports whether pw matches the stored bcrypt hash. The
// comparison is constant-time inside bcrypt.
func checkPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// handleLogin renders the sign-in form (GET) and authenticates (POST). There is
// one form: a username the user doesn't have yet is registered with the password
// they typed; an existing username has its password verified. Passwords are only
// ever stored as bcrypt hashes.
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
		password := r.Form.Get("password")
		next := sanitiseNext(r.Form.Get("next"))
		if !validUsername(username) {
			http.Redirect(w, r, "/login?err=bad&next="+next, http.StatusSeeOther)
			return
		}
		if !validPassword(password) {
			http.Redirect(w, r, "/login?err=weakpw&next="+next, http.StatusSeeOther)
			return
		}
		if !a.authenticate(r.Context(), username, password) {
			http.Redirect(w, r, "/login?err=denied&next="+next, http.StatusSeeOther)
			return
		}
		setSessionCookie(w, r, a.sessions.create(username))
		a.log.Info("login", "user", username, "remote", r.RemoteAddr)
		http.Redirect(w, r, next, http.StatusSeeOther)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// authenticate verifies an existing account's password, or registers a new
// account on first use of a username. Registration is atomic (CreateUser uses
// HSETNX); if two first-logins race, the loser falls back to verifying against
// the record the winner just wrote. Returns false only when an existing
// account's password does not match.
func (a *app) authenticate(ctx context.Context, username, password string) bool {
	rec, ok, err := a.store.GetUser(ctx, username)
	if err != nil {
		a.log.Error("get user", "user", username, "error", err)
		return false
	}
	if ok {
		return checkPassword(rec.PasswordHash, password)
	}

	hash, err := hashPassword(password)
	if err != nil {
		a.log.Error("hash password", "user", username, "error", err)
		return false
	}
	created, err := a.store.CreateUser(ctx, userRecord{Username: username, PasswordHash: hash, CreatedAt: time.Now().UTC()})
	if err != nil {
		a.log.Error("create user", "user", username, "error", err)
		return false
	}
	if created {
		a.log.Info("account created", "user", username)
		return true
	}
	// Lost the create race: an account now exists — verify against it.
	rec, ok, err = a.store.GetUser(ctx, username)
	if err != nil || !ok {
		a.log.Error("verify after create race", "user", username, "error", err)
		return false
	}
	return checkPassword(rec.PasswordHash, password)
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
	switch code {
	case "bad":
		return "Pick a username: 1–24 letters, digits, dash, underscore or dot."
	case "weakpw":
		return "Password must be 8–72 characters."
	case "denied":
		return "Incorrect password."
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
