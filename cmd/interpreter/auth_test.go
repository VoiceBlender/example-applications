package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func testAuthApp(t *testing.T, user, pass string) *app {
	t.Helper()
	a, _, _, _, _, _ := newTestApp(t)
	a.auth = authConfig{Username: user, Password: pass}
	a.logins = newLoginStore()
	return a
}

// With no password configured the gate must be entirely transparent, so local
// trials stay friction-free.
func TestAuthDisabledLetsEverythingThrough(t *testing.T) {
	a := testAuthApp(t, "interpreter", "")
	if a.auth.enabled() {
		t.Fatal("auth should be disabled with an empty password")
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept", "text/html")
	a.serveHTTP().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("index returned %d with auth off, want 200", rec.Code)
	}
}

// Both halves of the credential must matter.
func TestCredentialCheck(t *testing.T) {
	c := authConfig{Username: "alice", Password: "s3cret"}
	cases := []struct {
		user, pass string
		want       bool
	}{
		{"alice", "s3cret", true},
		{"alice", "wrong", false},
		{"bob", "s3cret", false},
		{"", "", false},
		{"alice", "", false},
		{"", "s3cret", false},
		// A prefix must not pass — a naive comparison could accept it.
		{"alice", "s3cre", false},
		{"alic", "s3cret", false},
	}
	for _, tc := range cases {
		if got := c.check(tc.user, tc.pass); got != tc.want {
			t.Errorf("check(%q,%q) = %v, want %v", tc.user, tc.pass, got, tc.want)
		}
	}
}

// A page load without a cookie is redirected to the form; a fetch or a
// WebSocket upgrade gets a 401 instead, so a JS client never tries to parse the
// login page as its response.
func TestUnauthenticatedRoutingByRequestKind(t *testing.T) {
	a := testAuthApp(t, "alice", "s3cret")
	h := a.serveHTTP()

	page := httptest.NewRequest(http.MethodGet, "/", nil)
	page.Header.Set("Accept", "text/html,application/xhtml+xml")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, page)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("page load returned %d, want a 303 to the login form", rec.Code)
	}

	ws := httptest.NewRequest(http.MethodGet, "/api/interpreter/stream?session=x", nil)
	ws.Header.Set("Accept", "text/html")
	ws.Header.Set("Upgrade", "websocket")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, ws)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("websocket upgrade returned %d, want 401", rec.Code)
	}

	api := httptest.NewRequest(http.MethodPost, "/api/sessions", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, api)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("api call returned %d, want 401", rec.Code)
	}
}

// The full round trip: log in, get a cookie, reach a gated page with it.
func TestLoginIssuesAWorkingCookie(t *testing.T) {
	a := testAuthApp(t, "alice", "s3cret")
	h := a.serveHTTP()

	form := url.Values{"username": {"alice"}, "password": {"s3cret"}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login returned %d, want 303", rec.Code)
	}
	cookies := rec.Result().Cookies()
	var token string
	for _, c := range cookies {
		if c.Name == authCookie {
			token = c.Value
			if !c.HttpOnly {
				t.Error("session cookie is not HttpOnly")
			}
		}
	}
	if token == "" {
		t.Fatal("login set no session cookie")
	}

	page := httptest.NewRequest(http.MethodGet, "/", nil)
	page.Header.Set("Accept", "text/html")
	page.AddCookie(&http.Cookie{Name: authCookie, Value: token})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, page)
	if rec.Code != http.StatusOK {
		t.Errorf("authenticated index returned %d, want 200", rec.Code)
	}

	// Logging out must invalidate the token server-side, not just clear the
	// cookie — otherwise a copied token stays valid.
	out := httptest.NewRequest(http.MethodGet, "/logout", nil)
	out.AddCookie(&http.Cookie{Name: authCookie, Value: token})
	h.ServeHTTP(httptest.NewRecorder(), out)

	rec = httptest.NewRecorder()
	page2 := httptest.NewRequest(http.MethodGet, "/", nil)
	page2.Header.Set("Accept", "text/html")
	page2.AddCookie(&http.Cookie{Name: authCookie, Value: token})
	h.ServeHTTP(rec, page2)
	if rec.Code == http.StatusOK {
		t.Error("token still worked after logout")
	}
}

func TestBadLoginIsRejected(t *testing.T) {
	a := testAuthApp(t, "alice", "s3cret")
	form := url.Values{"username": {"alice"}, "password": {"nope"}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	a.serveHTTP().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("bad login returned %d, want 401", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == authCookie && c.Value != "" {
			t.Error("a failed login handed out a session cookie")
		}
	}
}

// The post-login redirect must not become an open redirect.
func TestNextIsConfinedToThisSite(t *testing.T) {
	cases := map[string]string{
		"/s/abc123":            "/s/abc123",
		"":                     "/",
		"//evil.example/":      "/",
		"https://evil.example": "/",
		"javascript:alert(1)":  "/",
	}
	for in, want := range cases {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

// An unknown or expired token must not authenticate.
func TestUnknownTokenIsRejected(t *testing.T) {
	a := testAuthApp(t, "alice", "s3cret")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: authCookie, Value: "not-a-real-token"})
	if a.authed(req) {
		t.Error("an unknown token authenticated")
	}
}
