package main

import (
	"context"
	"embed"
	"encoding/json"
	"html/template"
	"io/fs"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

//go:embed web/lobby.html
var lobbyHTML string

//go:embed web/room.html
var roomHTML string

//go:embed web/static
var staticFS embed.FS

var lobbyTemplate = template.Must(template.New("lobby").Parse(lobbyHTML))
var roomTemplate = template.Must(template.New("room").Parse(roomHTML))

// roomPageData is the render context for the room page.
type roomPageData struct {
	RoomID string
	Name   string
	Owner  bool
	Invite string
	Roger  string
}

// ── lobby snapshot fan-out ────────────────────────────────────────────────────

// webHub fans a "state changed" signal out to every connected lobby WebSocket so
// it can re-fetch and re-render its room list.
type webHub struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

func newWebHub() *webHub {
	return &webHub{subs: make(map[chan struct{}]struct{})}
}

func (h *webHub) subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *webHub) unsubscribe(ch chan struct{}) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

func (h *webHub) broadcast() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default: // a signal is already pending; the reader will pick up latest
		}
	}
}

// notifyChanged pushes a fresh lobby snapshot to all connected lobbies.
func (a *app) notifyChanged() {
	if a.hub != nil {
		a.hub.broadcast()
	}
}

// ── HTTP wiring ───────────────────────────────────────────────────────────────

func (a *app) serveHTTP() http.Handler {
	mux := http.NewServeMux()

	// Public.
	mux.HandleFunc("/login", a.handleLogin)
	mux.HandleFunc("/logout", a.handleLogout)
	if sub, err := fs.Sub(staticFS, "web/static"); err == nil {
		mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(sub)))
	}

	gated := func(h http.HandlerFunc) http.Handler { return a.requireUser(h) }

	// Pages.
	mux.Handle("GET /{$}", gated(a.handleLobbyPage))
	mux.Handle("GET /room/{id}", gated(a.handleRoomPage))
	mux.Handle("GET /join", gated(a.handleJoinLink))

	// APIs.
	mux.Handle("POST /api/rooms", gated(a.handleCreateRoom))
	mux.Handle("POST /api/rooms/join", gated(a.handleJoinRoom))
	mux.Handle("PUT /api/rooms/{id}/roger", gated(a.handleSetRoger))
	mux.Handle("DELETE /api/rooms/{id}", gated(a.handleDeleteRoom))
	mux.Handle("GET /api/lobby/stream", gated(a.handleLobbyStream))

	// Room signalling WebSocket (floor control + WebRTC).
	mux.Handle("GET /api/ptt/stream", gated(a.handlePTTStream))

	return mux
}

func (a *app) handleLobbyPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = lobbyTemplate.Execute(w, map[string]any{"User": userFromCtx(r)})
}

func (a *app) handleRoomPage(w http.ResponseWriter, r *http.Request) {
	username := userFromCtx(r)
	room, ok := a.rooms.get(r.PathValue("id"))
	if !ok || !a.rooms.canAccess(room, username) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = roomTemplate.Execute(w, roomPageData{
		RoomID: room.ID,
		Name:   room.Name,
		Owner:  room.Owner == username,
		Invite: inviteFor(room, username),
		Roger:  room.Roger,
	})
}

// handleJoinLink redeems an invite code from a shared link (/join?code=…) and
// sends the user straight into the room.
func (a *app) handleJoinLink(w http.ResponseWriter, r *http.Request) {
	room, err := a.rooms.joinByCode(r.Context(), r.URL.Query().Get("code"), userFromCtx(r))
	if err != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.notifyChanged()
	http.Redirect(w, r, "/room/"+room.ID, http.StatusSeeOther)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *app) handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name       string `json:"name"`
		Visibility string `json:"visibility"`
		Roger      string `json:"roger"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	room, err := a.rooms.create(r.Context(), body.Name, userFromCtx(r), body.Visibility, body.Roger)
	if err == errBadName {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err != nil {
		a.log.Error("create room", "error", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": room.ID})
}

func (a *app) handleJoinRoom(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	room, err := a.rooms.joinByCode(r.Context(), body.Code, userFromCtx(r))
	if err == errRoomNotFound {
		http.Error(w, "invalid invite code", http.StatusNotFound)
		return
	}
	if err != nil {
		a.log.Error("join room", "error", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	a.notifyChanged()
	writeJSON(w, http.StatusOK, map[string]any{"id": room.ID})
}

// handleSetRoger updates a room's end-of-transmission courtesy tone (owner
// only) and pushes the new setting to everyone currently in the room.
func (a *app) handleSetRoger(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	room, ok := a.rooms.get(id)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if room.Owner != userFromCtx(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var body struct {
		Roger string `json:"roger"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	updated, err := a.rooms.setRoger(r.Context(), id, body.Roger)
	if err != nil {
		a.log.Error("set roger", "error", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	a.pushRoomConfig(id, updated.Roger)
	writeJSON(w, http.StatusOK, map[string]any{"roger": updated.Roger})
}

func (a *app) handleDeleteRoom(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	room, ok := a.rooms.get(id)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if room.Owner != userFromCtx(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := a.rooms.delete(r.Context(), id); err != nil {
		a.log.Error("delete room", "error", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleLobbyStream streams the viewer's room list, re-sending on any change and
// on a slow tick (to keep live headcounts fresh).
func (a *app) handleLobbyStream(w http.ResponseWriter, r *http.Request) {
	username := userFromCtx(r)
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if err != nil {
		return
	}
	defer c.CloseNow()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	go func() {
		for {
			if _, _, err := c.Read(ctx); err != nil {
				cancel()
				return
			}
		}
	}()

	ch := a.hub.subscribe()
	defer a.hub.unsubscribe(ch)

	send := func() bool {
		snap := map[string]any{
			"type":  "lobby",
			"rooms": a.rooms.visibleTo(username, a.presence.countInRoom),
			"at":    time.Now().UTC().Format(time.RFC3339),
		}
		return wsjson.Write(ctx, c, snap) == nil
	}
	if !send() {
		return
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
		case <-ticker.C:
		}
		if !send() {
			return
		}
	}
}
