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

// The person you share a link with has no account, so the invite token has to
// admit them — to that one conversation and nothing else.
func TestInviteTokenAdmitsWithoutLogin(t *testing.T) {
	a := testAuthApp(t, "alice", "s3cret")
	h := a.serveHTTP()
	s := a.sessions.create()

	// No login, no token: refused.
	req := httptest.NewRequest(http.MethodGet, "/s/"+s.id, nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("no credentials returned %d, want a redirect to login", rec.Code)
	}

	// With the token: admitted.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/s/"+s.id+"?t="+s.invite, nil)
	req.Header.Set("Accept", "text/html")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("valid invite returned %d, want 200", rec.Code)
	}

	// The socket must accept it too, or the page loads and then cannot connect.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/interpreter/stream?session="+s.id+"&t="+s.invite, nil)
	req.Header.Set("Upgrade", "websocket")
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized {
		t.Error("socket rejected a valid invite token")
	}
}

// The token is scoped: it must not open a different conversation, and a wrong
// one must not open anything.
func TestInviteTokenIsScopedAndChecked(t *testing.T) {
	a := testAuthApp(t, "alice", "s3cret")
	h := a.serveHTTP()
	mine := a.sessions.create()
	theirs := a.sessions.create()

	cases := []struct{ name, path string }{
		{"another session's token", "/s/" + theirs.id + "?t=" + mine.invite},
		{"a wrong token", "/s/" + mine.id + "?t=" + newID(24)},
		{"an empty token", "/s/" + mine.id + "?t="},
		{"an unknown session", "/s/doesnotexist?t=" + mine.invite},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.Header.Set("Accept", "text/html")
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Errorf("%s was admitted", tc.name)
		}
	}
}

// An invitee must not be able to mint sessions of their own — the token buys
// access to one conversation, not an account.
func TestInviteTokenCannotCreateSessions(t *testing.T) {
	a := testAuthApp(t, "alice", "s3cret")
	s := a.sessions.create()

	req := httptest.NewRequest(http.MethodPost, "/api/sessions?t="+s.invite, nil)
	rec := httptest.NewRecorder()
	a.serveHTTP().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("an invitee created a session (%d)", rec.Code)
	}
}

// Two sessions must not share a token, and it must be long enough to be worth
// calling a secret — it is the only thing guarding the conversation.
func TestInviteTokensAreUniqueAndUnguessable(t *testing.T) {
	a := testAuthApp(t, "alice", "s3cret")
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		s := a.sessions.create()
		if len(s.invite) < 32 {
			t.Fatalf("invite token is only %d chars", len(s.invite))
		}
		if seen[s.invite] {
			t.Fatal("duplicate invite token")
		}
		seen[s.invite] = true
	}
}

// With no password configured there is no gate at all, so a link with no token
// must still work.
func TestInviteIrrelevantWhenAuthIsOff(t *testing.T) {
	a := testAuthApp(t, "interpreter", "")
	s := a.sessions.create()
	req := httptest.NewRequest(http.MethodGet, "/s/"+s.id, nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	a.serveHTTP().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("returned %d with auth disabled, want 200", rec.Code)
	}
}
