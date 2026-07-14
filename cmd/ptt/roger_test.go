package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// newTestApp builds an app wired for the HTTP/WS handlers (no VSI stream: the
// roger paths never touch a.vsi()). It skips the test if Redis is unreachable.
func newTestApp(t *testing.T) *app {
	t.Helper()
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://127.0.0.1:6379/9"
	}
	store, err := newRedisStore(context.Background(), redisURL)
	if err != nil {
		t.Skipf("redis not available (%v) — set REDIS_URL to run", err)
	}
	t.Cleanup(func() { store.Close() })

	a := &app{
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		store: store,
		hub:   newWebHub(),
	}
	a.sessions = newSessionStore(store)
	a.rooms = newRoomRegistry(store)
	a.rooms.notify = a.notifyChanged
	a.presence = newPresenceRegistry()
	a.floor = newFloorManager(a)
	return a
}

// loginClient returns an http.Client with a cookie jar signed in as user.
func loginClient(t *testing.T, srv *httptest.Server, user string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.PostForm(srv.URL+"/login", url.Values{"username": {user}, "next": {"/"}})
	if err != nil {
		t.Fatalf("login %s: %v", user, err)
	}
	resp.Body.Close()
	return c
}

func TestRogerPerRoom(t *testing.T) {
	a := newTestApp(t)
	srv := httptest.NewServer(a.serveHTTP())
	defer srv.Close()

	alice := loginClient(t, srv, "alice")
	bob := loginClient(t, srv, "bob")

	// Create a room with roger=quindar.
	resp, err := alice.Post(srv.URL+"/api/rooms", "application/json",
		strings.NewReader(`{"name":"Roger Room","visibility":"public","roger":"quindar"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create room: got %d", resp.StatusCode)
	}
	var created struct{ ID string }
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	room := created.ID
	t.Cleanup(func() { a.rooms.delete(context.Background(), room) })

	if got, _ := a.rooms.get(room); got.Roger != "quindar" {
		t.Fatalf("stored roger = %q, want quindar", got.Roger)
	}

	// An unknown roger value must be clamped to the default on create.
	resp, _ = alice.Post(srv.URL+"/api/rooms", "application/json",
		strings.NewReader(`{"name":"Bad","visibility":"public","roger":"bogus"}`))
	var bad struct{ ID string }
	json.NewDecoder(resp.Body).Decode(&bad)
	resp.Body.Close()
	t.Cleanup(func() { a.rooms.delete(context.Background(), bad.ID) })
	if got, _ := a.rooms.get(bad.ID); got.Roger != defaultRoger {
		t.Fatalf("unknown roger clamped to %q, want %q", got.Roger, defaultRoger)
	}

	// hello over the room WebSocket must carry the roger style.
	hello := readHello(t, srv, alice, room)
	if hello["roger"] != "quindar" {
		t.Fatalf("hello.roger = %v, want quindar", hello["roger"])
	}

	// Owner can change the roger; it persists.
	if code := putRoger(t, alice, srv, room, "chirp"); code != http.StatusOK {
		t.Fatalf("owner set roger: got %d, want 200", code)
	}
	if got, _ := a.rooms.get(room); got.Roger != "chirp" {
		t.Fatalf("after owner update, roger = %q, want chirp", got.Roger)
	}

	// Non-owner is forbidden from changing it.
	if code := putRoger(t, bob, srv, room, "off"); code != http.StatusForbidden {
		t.Fatalf("non-owner set roger: got %d, want 403", code)
	}
	if got, _ := a.rooms.get(room); got.Roger != "chirp" {
		t.Fatalf("roger changed by non-owner to %q", got.Roger)
	}
}

func putRoger(t *testing.T, c *http.Client, srv *httptest.Server, room, roger string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/rooms/"+room+"/roger",
		strings.NewReader(`{"roger":"`+roger+`"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// readHello connects to the room WS and returns the first hello frame.
func readHello(t *testing.T, srv *httptest.Server, c *http.Client, room string) map[string]any {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/ptt/stream?room=" + room
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPClient: c})
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	for {
		var msg map[string]any
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			t.Fatalf("ws read: %v", err)
		}
		if msg["type"] == "hello" {
			return msg
		}
	}
}
