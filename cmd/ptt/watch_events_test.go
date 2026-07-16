package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// The watched set is remembered per user through /api/watch, and inaccessible
// ids (deleted room, or a private room the user can't reach) are filtered out.
func TestWatchedChannelsRoundTrip(t *testing.T) {
	a := newTestApp(t)
	srv := httptest.NewServer(a.serveHTTP())
	defer srv.Close()

	alice := loginClient(t, srv, "alice")

	r1 := createRoom(t, alice, srv, `{"name":"One","visibility":"public"}`)
	r2 := createRoom(t, alice, srv, `{"name":"Two","visibility":"public"}`)
	t.Cleanup(func() { a.rooms.delete(context.Background(), r1); a.rooms.delete(context.Background(), r2) })

	// Save both, plus a bogus id that must be filtered out on read.
	putWatch(t, alice, srv, []string{r1, "does-not-exist", r2})

	got := getWatch(t, alice, srv)
	if len(got) != 2 || got[0] != r1 || got[1] != r2 {
		t.Fatalf("watched = %v, want [%s %s] in order", got, r1, r2)
	}

	// A fresh login for the same account sees the same set — it's stored per user,
	// not per browser.
	alice2 := loginClient(t, srv, "alice")
	if got := getWatch(t, alice2, srv); len(got) != 2 {
		t.Fatalf("second login watched = %v, want 2 entries", got)
	}
}

// The activity log records events, replays recent history to a newly-connected
// browser, and streams live events to those already present. (Join and ring are
// exercised here because they don't touch the VoiceBlender stream; the "talk"
// event is wired the same way at the floor grant.)
func TestActivityLog(t *testing.T) {
	a := newTestApp(t)
	srv := httptest.NewServer(a.serveHTTP())
	defer srv.Close()

	alice := loginClient(t, srv, "alice")
	bob := loginClient(t, srv, "bob")
	room := createRoom(t, alice, srv, `{"name":"Ops","visibility":"public"}`)
	t.Cleanup(func() { a.rooms.delete(context.Background(), room) })

	// Alice joins (→ join event) and rings (→ ring event).
	aliceWS := dialRoom(t, srv, alice, room)
	writeWS(t, aliceWS, map[string]any{"type": "ring"})

	// Poll the stored log until both events land (recording is fire-and-forget
	// from the WS read loop).
	waitEvents(t, a, room, eventJoin, eventRing)

	// Bob opens the channel now: the history frame must replay alice's events.
	bobWS := dialRoom(t, srv, bob, room)
	hist := readMatch(t, bobWS, func(m map[string]any) bool { return m["type"] == "history" })
	raw, _ := json.Marshal(hist["events"])
	if !strings.Contains(string(raw), `"kind":"ring"`) || !strings.Contains(string(raw), `"kind":"join"`) {
		t.Fatalf("history replay missing prior events: %s", raw)
	}

	// A live event must reach bob (already present): carol joins the channel, and
	// bob should receive her join as a live "event" frame. (A second ring would be
	// swallowed by the per-session ring cooldown, so use a join instead.)
	carol := loginClient(t, srv, "carol")
	dialRoom(t, srv, carol, room)
	ev := readMatch(t, bobWS, func(m map[string]any) bool {
		if m["type"] != "event" {
			return false
		}
		e, _ := m["event"].(map[string]any)
		return e != nil && e["kind"] == eventJoin && e["actor"] == "carol"
	})
	e := ev["event"].(map[string]any)
	if e["room"] != room {
		t.Fatalf("live join event = %v, want room=%s", e, room)
	}
}

// waitEvents polls the stored log until every wanted kind is present (or fails).
func waitEvents(t *testing.T, a *app, room string, want ...string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		evs, err := a.store.LoadEvents(context.Background(), room, maxEvents)
		if err != nil {
			t.Fatalf("load events: %v", err)
		}
		have := map[string]bool{}
		for _, e := range evs {
			if e.Room != room || e.Actor == "" || e.At == "" {
				t.Fatalf("malformed event: %+v", e)
			}
			have[e.Kind] = true
		}
		all := true
		for _, k := range want {
			all = all && have[k]
		}
		if all {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("events %v not all recorded in time", want)
}

// ── HTTP + WS test helpers ───────────────────────────────────────────────────

func createRoom(t *testing.T, c *http.Client, srv *httptest.Server, body string) string {
	t.Helper()
	resp, err := c.Post(srv.URL+"/api/rooms", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("create room: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create room: got %d", resp.StatusCode)
	}
	var out struct{ ID string }
	json.NewDecoder(resp.Body).Decode(&out)
	return out.ID
}

func putWatch(t *testing.T, c *http.Client, srv *httptest.Server, ids []string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"rooms": ids})
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/watch", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("put watch: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("put watch: got %d", resp.StatusCode)
	}
}

func getWatch(t *testing.T, c *http.Client, srv *httptest.Server) []string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/watch", nil)
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("get watch: %v", err)
	}
	defer resp.Body.Close()
	var ids []string
	json.NewDecoder(resp.Body).Decode(&ids)
	return ids
}

func dialRoom(t *testing.T, srv *httptest.Server, c *http.Client, room string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/ptt/stream?room=" + room
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPClient: c})
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	t.Cleanup(func() { conn.Close(websocket.StatusNormalClosure, "") })
	return conn
}

func writeWS(t *testing.T, conn *websocket.Conn, v any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, conn, v); err != nil {
		t.Fatalf("ws write: %v", err)
	}
}

// readMatch reads frames until one satisfies pred (or times out).
func readMatch(t *testing.T, conn *websocket.Conn, pred func(map[string]any) bool) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		var msg map[string]any
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			t.Fatalf("ws read: %v", err)
		}
		if pred(msg) {
			return msg
		}
	}
}
