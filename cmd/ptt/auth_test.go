package main

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Passwords must be 8–72 characters. The upper bound matches bcrypt's input
// limit, so a longer password is rejected rather than silently truncated.
func TestValidPassword(t *testing.T) {
	tests := []struct {
		name string
		pw   string
		want bool
	}{
		{"empty", "", false},
		{"too short", strings.Repeat("a", 7), false},
		{"minimum", strings.Repeat("a", 8), true},
		{"typical", "correct horse", true},
		{"maximum", strings.Repeat("a", 72), true},
		{"too long", strings.Repeat("a", 73), false},
	}
	for _, tt := range tests {
		if got := validPassword(tt.pw); got != tt.want {
			t.Errorf("%s: validPassword(len %d) = %v, want %v", tt.name, len(tt.pw), got, tt.want)
		}
	}
}

// TestLoginFlow exercises the real /login handler end to end: a new username is
// registered on first login, a wrong password is rejected, the right one is
// accepted, and the stored password is a bcrypt hash — never the plaintext.
func TestLoginFlow(t *testing.T) {
	a := newTestApp(t) // skips if Redis is unavailable; starts with a clean users key
	srv := httptest.NewServer(a.serveHTTP())
	defer srv.Close()

	const user, pass = "eve", "hunter2hunter2"

	// post logs in with a fresh cookie jar and returns the redirect Location.
	post := func(username, password string) (*http.Client, string) {
		jar, _ := cookiejar.New(nil)
		c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := c.PostForm(srv.URL+"/login", url.Values{"username": {username}, "password": {password}, "next": {"/"}})
		if err != nil {
			t.Fatalf("POST /login: %v", err)
		}
		resp.Body.Close()
		return c, resp.Header.Get("Location")
	}

	// A short password is rejected before any account is created.
	if _, loc := post(user, "short"); !strings.Contains(loc, "err=weakpw") {
		t.Fatalf("weak password: redirect = %q, want err=weakpw", loc)
	}
	if _, ok, _ := a.store.GetUser(context.Background(), user); ok {
		t.Fatal("a rejected weak-password login must not create an account")
	}

	// First real login registers the account and lands on next (/).
	c1, loc := post(user, pass)
	if loc != "/" {
		t.Fatalf("first login: redirect = %q, want /", loc)
	}
	if !authed(t, srv, c1) {
		t.Fatal("first login did not establish a session")
	}

	// Stored credential is a bcrypt hash, not the plaintext.
	rec, ok, err := a.store.GetUser(context.Background(), user)
	if err != nil || !ok {
		t.Fatalf("GetUser after register: ok=%v err=%v", ok, err)
	}
	if rec.PasswordHash == pass || !strings.HasPrefix(rec.PasswordHash, "$2") {
		t.Fatalf("stored hash %q is not a bcrypt hash of the password", rec.PasswordHash)
	}

	// Wrong password on the existing account is denied and grants no session.
	c2, loc := post(user, "wrongwrongwrong")
	if !strings.Contains(loc, "err=denied") {
		t.Fatalf("wrong password: redirect = %q, want err=denied", loc)
	}
	if authed(t, srv, c2) {
		t.Fatal("wrong password must not establish a session")
	}

	// Right password on the existing account signs in.
	c3, loc := post(user, pass)
	if loc != "/" || !authed(t, srv, c3) {
		t.Fatalf("re-login with correct password failed: loc=%q", loc)
	}
}

// authed reports whether the client's cookies resolve to a signed-in session,
// by hitting a gated JSON endpoint (200 = in, 401 = out).
func authed(t *testing.T, srv *httptest.Server, c *http.Client) bool {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/rooms", nil)
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET /api/rooms: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// hashPassword must never return the plaintext, must produce a bcrypt string,
// and checkPassword must accept the right password and reject a wrong one.
func TestHashAndCheckPassword(t *testing.T) {
	const pw = "correct horse battery staple"

	hash, err := hashPassword(pw)
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	if hash == pw {
		t.Fatal("hash equals the plaintext password — nothing was hashed")
	}
	if !strings.HasPrefix(hash, "$2") { // bcrypt hashes are $2a$/$2b$…
		t.Errorf("hash %q is not a bcrypt string", hash)
	}
	if !checkPassword(hash, pw) {
		t.Error("checkPassword rejected the correct password")
	}
	if checkPassword(hash, "wrong password") {
		t.Error("checkPassword accepted a wrong password")
	}

	// A fresh hash of the same password differs (random per-hash salt), yet both verify.
	hash2, err := hashPassword(pw)
	if err != nil {
		t.Fatalf("hashPassword (2): %v", err)
	}
	if hash2 == hash {
		t.Error("two hashes of the same password are identical — salt is not random")
	}
	if !checkPassword(hash2, pw) {
		t.Error("checkPassword rejected the correct password against the second hash")
	}
}
