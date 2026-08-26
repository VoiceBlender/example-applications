package main

import (
	"embed"
	"encoding/json"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
)

//go:embed web/index.html
var indexHTML string

//go:embed web/session.html
var sessionHTML string

//go:embed web/login.html
var loginHTML string

//go:embed web/static
var staticFS embed.FS

var indexTemplate = template.Must(template.New("index").Parse(indexHTML))
var sessionTemplate = template.Must(template.New("session").Parse(sessionHTML))
var loginTemplate = template.Must(template.New("login").Parse(loginHTML))

func (a *app) serveHTTP() http.Handler {
	mux := http.NewServeMux()

	if sub, err := fs.Sub(staticFS, "web/static"); err == nil {
		mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(sub)))
	}

	// The login form is the only ungated page. With AUTH_PASSWORD unset the gate
	// is a no-op and every requireLogin below passes straight through.
	mux.HandleFunc("/login", a.handleLogin)
	mux.HandleFunc("/logout", a.handleLogout)

	mux.Handle("GET /{$}", a.requireLogin(a.handleIndex))
	mux.Handle("POST /api/sessions", a.requireLogin(a.handleCreateSession))
	mux.Handle("GET /s/{id}", a.requireLogin(a.handleSessionPage))

	// The session's signalling WebSocket: WebRTC negotiation, language changes,
	// captions and presence all ride this one socket. Gated too — it is the
	// expensive one, since a connected leg streams audio to the STT vendor.
	mux.Handle("GET /api/interpreter/stream", a.requireLogin(a.handleStream))

	return mux
}

// handleIndex renders the landing page: start a session, or join one by code.
func (a *app) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = indexTemplate.Execute(w, map[string]any{
		"Languages": a.cfg.offeredLanguages(),
		"Genders":   genders,
		"Auth":      a.auth.enabled(),
	})
}

// handleCreateSession mints a session and returns its id. The id is the only
// credential — there are no accounts here, so whoever holds the link is in.
func (a *app) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	s := a.sessions.create()
	a.log.Info("session created", "session", s.id)
	writeJSON(w, http.StatusCreated, map[string]any{"id": s.id})
}

// handleSessionPage renders the call view for a session that still exists.
func (a *app) handleSessionPage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := a.sessions.get(id); !ok {
		http.Redirect(w, r, "/?gone=1", http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = sessionTemplate.Execute(w, map[string]any{
		"SessionID": id,
		"Languages": a.cfg.offeredLanguages(),
		"Genders":   genders,
		"Auth":      a.auth.enabled(),
	})
}

// urlEscape percent-encodes a value for use in a query string.
func urlEscape(s string) string { return url.QueryEscape(s) }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// cleanName trims a display name to something safe and short. It is only ever
// shown back to the two people in the session, so this is tidying, not security
// — the templates escape on output.
func cleanName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "guest"
	}
	if len(s) > 24 {
		s = s[:24]
	}
	return s
}
