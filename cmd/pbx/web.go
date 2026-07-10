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

	voiceblender "github.com/VoiceBlender/voiceblender-go"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

//go:embed web/layout.html web/page_calls.html web/page_extensions.html web/page_trunks.html web/page_dialplan.html web/page_config.html
var pagesFS embed.FS

//go:embed web/static
var staticFS embed.FS

// pageData is the render context passed to every console page template.
type pageData struct{ Title string }

// pageTemplates is one parsed template set per console page (the shared layout
// plus that page's body/modals/script blocks).
var pageTemplates = map[string]*template.Template{
	"calls":      mustPage("page_calls"),
	"extensions": mustPage("page_extensions"),
	"trunks":     mustPage("page_trunks"),
	"dialplan":   mustPage("page_dialplan"),
	"config":     mustPage("page_config"),
}

func mustPage(name string) *template.Template {
	return template.Must(template.ParseFS(pagesFS, "web/layout.html", "web/"+name+".html"))
}

// renderPage executes a console page through the shared layout.
func renderPage(w http.ResponseWriter, name, title string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := pageTemplates[name].ExecuteTemplate(w, "layout", pageData{Title: title}); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// webHub fans a "state changed" signal out to every connected management-page
// WebSocket so it can re-fetch and re-render the snapshot.
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

// notifyChanged pushes a fresh snapshot to all connected consoles. Wired into
// every registry as its change callback.
func (a *app) notifyChanged() {
	if a.hub != nil {
		a.hub.broadcast()
	}
}

// ── snapshot ───────────────────────────────────────────────────────────────

type snapStats struct {
	ExtRegistered int `json:"ext_registered"`
	ExtTotal      int `json:"ext_total"`
	TrunksUp      int `json:"trunks_up"`
	TrunksTotal   int `json:"trunks_total"`
	ActiveCalls   int `json:"active_calls"`
}

// callView is one active call in the live-calls panel.
type callView struct {
	ID    string `json:"id"`
	From  string `json:"from"`
	To    string `json:"to"`
	Kind  string `json:"kind"`          // internal | external | inbound | forward
	Via   string `json:"via,omitempty"` // trunk name, when the call is from/to a trunk
	State string `json:"state"`         // ringing | connected | dial plan | menu | ivr
	Since string `json:"since"`         // RFC3339 anchor for the client's duration counter
}

type snapshot struct {
	Type       string      `json:"type"`
	Extensions []extView   `json:"extensions"`
	Trunks     []trunkView `json:"trunks"`
	Calls      []callView  `json:"calls"`
	Config     Config      `json:"config"`
	Dialplan   DPGraph     `json:"dialplan"`
	Stats      snapStats   `json:"stats"`
	At         string      `json:"at"`
}

func (a *app) snapshot(tenantID string) snapshot {
	// Active calls (this tenant only) = bridged two-party calls, plus inbound
	// trunk calls still in the dial plan or IVR (not yet bridged).
	calls := a.bridges.views(tenantID)
	calls = append(calls, a.forkViews(tenantID)...)
	calls = append(calls, a.dpExecViews(tenantID)...)
	calls = append(calls, a.ivrViews(tenantID)...)
	return snapshot{
		Type:       "snapshot",
		Extensions: a.exts.views(tenantID),
		Trunks:     a.trunks.views(tenantID),
		Calls:      calls,
		Config:     a.config.get(tenantID),
		Dialplan:   a.dialplan.get(tenantID),
		Stats: snapStats{
			ExtRegistered: a.exts.registeredCount(tenantID),
			ExtTotal:      a.exts.count(tenantID),
			TrunksUp:      a.trunks.upCount(tenantID),
			TrunksTotal:   a.trunks.count(tenantID),
			ActiveCalls:   len(calls),
		},
		At: time.Now().UTC().Format(time.RFC3339),
	}
}

// trunkName resolves a trunk id to its display name (empty if unknown).
func (a *app) trunkName(id string) string {
	if id == "" {
		return ""
	}
	if t, ok := a.trunks.get(id); ok {
		return t.Name
	}
	return ""
}

// ── HTTP wiring ────────────────────────────────────────────────────────────

func (a *app) serveHTTP() http.Handler {
	mux := http.NewServeMux()

	// Public.
	mux.HandleFunc("/login", a.handleLogin)
	mux.HandleFunc("/logout", a.handleLogout)
	mux.HandleFunc("/signup", a.handleSignup)
	// Shared static assets (theme + JS) — no data, so ungated.
	if sub, err := fs.Sub(staticFS, "web/static"); err == nil {
		mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(sub)))
	}

	// Softphone (separate WebRTC-account identity).
	mux.HandleFunc("/phone/login", a.handlePhoneLogin)
	mux.HandleFunc("/phone/logout", a.handlePhoneLogout)
	phoned := func(h http.HandlerFunc) http.Handler { return a.requirePhone(h) }
	mux.Handle("GET /phone", phoned(a.handlePhoneIndex))
	mux.Handle("GET /api/phone/stream", phoned(a.handlePhoneStream))

	// Superadmin (cross-tenant) console + API.
	super := func(h http.HandlerFunc) http.Handler { return a.requireSuper(h) }
	mux.Handle("GET /admin", super(a.handleAdminPage))
	mux.Handle("GET /api/admin/data", super(a.handleAdminData))
	mux.Handle("POST /api/admin/tenants", super(a.handleAdminCreateTenant))
	mux.Handle("DELETE /api/admin/tenants/{id}", super(a.handleAdminDeleteTenant))
	mux.Handle("POST /api/admin/users", super(a.handleAdminCreateUser))
	mux.Handle("DELETE /api/admin/users/{username}", super(a.handleAdminDeleteUser))
	mux.Handle("GET /api/admin/dialplan/{tenant}", super(a.handleAdminGetDialplan))
	mux.Handle("PUT /api/admin/dialplan/{tenant}", super(a.handleAdminUpdateDialplan))
	mux.Handle("POST /api/admin/extensions", super(a.handleAdminCreateExtension))
	mux.Handle("PUT /api/admin/extensions/{id}", super(a.handleAdminUpdateExtension))
	mux.Handle("DELETE /api/admin/extensions/{id}", super(a.handleAdminDeleteExtension))
	mux.Handle("POST /api/admin/trunks", super(a.handleAdminCreateTrunk))
	mux.Handle("PUT /api/admin/trunks/{id}", super(a.handleAdminUpdateTrunk))
	mux.Handle("DELETE /api/admin/trunks/{id}", super(a.handleAdminDeleteTrunk))

	// Gated console + API.
	gated := func(h http.HandlerFunc) http.Handler { return a.requireAuth(h) }

	// Console pages (one per section, shared layout/nav).
	mux.Handle("GET /{$}", gated(func(w http.ResponseWriter, r *http.Request) { renderPage(w, "calls", "PBX · Live calls") }))
	mux.Handle("GET /extensions", gated(func(w http.ResponseWriter, r *http.Request) { renderPage(w, "extensions", "PBX · Extensions") }))
	mux.Handle("GET /trunks", gated(func(w http.ResponseWriter, r *http.Request) { renderPage(w, "trunks", "PBX · Trunks") }))
	mux.Handle("GET /dialplan", gated(func(w http.ResponseWriter, r *http.Request) { renderPage(w, "dialplan", "PBX · Dial plan") }))
	mux.Handle("GET /config", gated(func(w http.ResponseWriter, r *http.Request) { renderPage(w, "config", "PBX · Configuration") }))

	mux.Handle("GET /api/extensions", gated(a.handleListExtensions))
	mux.Handle("POST /api/extensions", gated(a.handleCreateExtension))
	mux.Handle("PUT /api/extensions/{id}", gated(a.handleUpdateExtension))
	mux.Handle("DELETE /api/extensions/{id}", gated(a.handleDeleteExtension))
	mux.Handle("GET /api/trunks", gated(a.handleListTrunks))
	mux.Handle("POST /api/trunks", gated(a.handleCreateTrunk))
	mux.Handle("PUT /api/trunks/{id}", gated(a.handleUpdateTrunk))
	mux.Handle("DELETE /api/trunks/{id}", gated(a.handleDeleteTrunk))
	mux.Handle("GET /api/config", gated(a.handleGetConfig))
	mux.Handle("PUT /api/config", gated(a.handleUpdateConfig))
	mux.Handle("GET /api/dialplan", gated(a.handleGetDialplan))
	mux.Handle("PUT /api/dialplan", gated(a.handleUpdateDialplan))
	mux.Handle("GET /api/stream", gated(a.handleStream))

	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ── extensions CRUD ────────────────────────────────────────────────────────

func (a *app) handleListExtensions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"extensions": a.exts.views(tenantFromCtx(r))})
}

func (a *app) handleCreateExtension(w http.ResponseWriter, r *http.Request) {
	var e Extension
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	e.ID = "" // force create
	e.TenantID = tenantFromCtx(r)
	if e.Number == "" {
		http.Error(w, "number is required", http.StatusBadRequest)
		return
	}
	saved, err := a.exts.upsert(r.Context(), e)
	if err == errDuplicateIdentity {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		a.log.Error("save extension", "error", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	// Pick up an already-active registration for this extension right away
	// instead of waiting for the periodic reconcile tick.
	go a.reconcileRegistrations(context.Background())
	writeJSON(w, http.StatusCreated, saved)
}

func (a *app) handleUpdateExtension(w http.ResponseWriter, r *http.Request) {
	var e Extension
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	e.ID = r.PathValue("id")
	tenant := tenantFromCtx(r)
	if !a.exts.ownedBy(e.ID, tenant) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	e.TenantID = tenant
	saved, err := a.exts.upsert(r.Context(), e)
	if err == errDuplicateIdentity {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		a.log.Error("update extension", "error", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	go a.reconcileRegistrations(context.Background())
	writeJSON(w, http.StatusOK, saved)
}

func (a *app) handleDeleteExtension(w http.ResponseWriter, r *http.Request) {
	if !a.exts.ownedBy(r.PathValue("id"), tenantFromCtx(r)) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err := a.exts.delete(r.Context(), r.PathValue("id")); err != nil {
		if err == errNotFound {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		a.log.Error("delete extension", "error", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── trunks CRUD ────────────────────────────────────────────────────────────

func (a *app) handleListTrunks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"trunks": a.trunks.views(tenantFromCtx(r))})
}

func (a *app) handleCreateTrunk(w http.ResponseWriter, r *http.Request) {
	var t Trunk
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	t.ID = ""
	t.TenantID = tenantFromCtx(r)
	if t.Type != trunkRegister && t.Type != trunkIP {
		http.Error(w, "type must be 'register' or 'ip'", http.StatusBadRequest)
		return
	}
	saved, err := a.trunks.upsert(r.Context(), t)
	if err != nil {
		a.log.Error("save trunk", "error", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if err := a.applyTrunkToServer(r.Context(), saved); err != nil {
		a.log.Warn("create trunk on server", "trunk", saved.Name, "error", err)
	}
	writeJSON(w, http.StatusCreated, saved)
}

func (a *app) handleUpdateTrunk(w http.ResponseWriter, r *http.Request) {
	var t Trunk
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	t.ID = r.PathValue("id")
	tenant := tenantFromCtx(r)
	if !a.trunks.ownedBy(t.ID, tenant) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	t.TenantID = tenant
	// Recreate on the server: remove the old registration/peer, add the new one.
	if old, ok := a.trunks.get(t.ID); ok {
		a.removeTrunkFromServer(r.Context(), old)
	}
	saved, err := a.trunks.upsert(r.Context(), t)
	if err != nil {
		a.log.Error("update trunk", "error", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if err := a.applyTrunkToServer(r.Context(), saved); err != nil {
		a.log.Warn("recreate trunk on server", "trunk", saved.Name, "error", err)
	}
	writeJSON(w, http.StatusOK, saved)
}

func (a *app) handleDeleteTrunk(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !a.trunks.ownedBy(id, tenantFromCtx(r)) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if t, ok := a.trunks.get(id); ok {
		a.removeTrunkFromServer(r.Context(), t)
	}
	if err := a.trunks.delete(r.Context(), id); err != nil {
		if err == errNotFound {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		a.log.Error("delete trunk", "error", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── global config ──────────────────────────────────────────────────────────

func (a *app) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.config.get(tenantFromCtx(r)))
}

func (a *app) handleUpdateConfig(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFromCtx(r)
	var cfg Config
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := a.config.update(r.Context(), tenant, cfg); err != nil {
		a.log.Error("save config", "error", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, a.config.get(tenant))
}

// ── inbound dial plan ──────────────────────────────────────────────────────

func (a *app) handleGetDialplan(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.dialplan.get(tenantFromCtx(r)))
}

func (a *app) handleUpdateDialplan(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFromCtx(r)
	var g DPGraph
	if err := json.NewDecoder(r.Body).Decode(&g); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := a.dialplan.update(r.Context(), tenant, g); err != nil {
		a.log.Error("save dial plan", "error", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, a.dialplan.get(tenant))
}

// ── trunk ↔ server synchronisation ─────────────────────────────────────────

// applyTrunkToServer creates a register-type trunk on the VoiceBlender server
// and records the server-assigned id for event correlation.
//
// IP trunks are NOT created server-side: the server has no ip_ip trunk object
// (create_sip_trunk returns 501 for it), and inbound calls from an IP peer are
// authenticated entirely in-app by matching the ringing event's source IP. So
// for an ip trunk we just mark it active locally.
func (a *app) applyTrunkToServer(ctx context.Context, t Trunk) error {
	if t.Type == trunkIP {
		a.trunks.setServerID(t.ID, "", "active")
		return nil
	}
	if t.Type != trunkRegister {
		return nil
	}
	resp, err := a.vsi().CreateSIPTrunk(ctx, voiceblender.CreateTrunkRequest{
		Type: "sip_register",
		SIPRegister: voiceblender.SIPRegisterTrunkSpec{
			RegistrarURI:   t.RegistrarURI,
			Aor:            t.AOR,
			Username:       t.Username,
			Password:       t.Password,
			ExpiresSeconds: t.Expires,
		},
	})
	if err != nil {
		a.trunks.setServerID(t.ID, "", "failed")
		return err
	}
	state := resp.Status
	if state == "" {
		state = "pending"
	}
	a.trunks.setServerID(t.ID, resp.ID, state)
	a.log.Info("trunk created on server", "trunk", t.Name, "server_id", resp.ID, "status", resp.Status)
	return nil
}

// removeTrunkFromServer deletes the trunk's server-side registration/peer.
func (a *app) removeTrunkFromServer(ctx context.Context, t Trunk) {
	serverID := a.trunks.serverID(t.ID)
	if serverID == "" {
		return
	}
	if _, err := a.vsi().DeleteSIPTrunk(ctx, voiceblender.IDPayload{ID: serverID}); err != nil && !isVSINotFound(err) {
		a.log.Warn("delete trunk on server", "trunk", t.Name, "error", err)
	}
}

// ── snapshot WebSocket ─────────────────────────────────────────────────────

func (a *app) handleStream(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromCtx(r)
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if err != nil {
		return
	}
	defer c.CloseNow()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Cancel when the client goes away (or sends anything).
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

	if err := wsjson.Write(ctx, c, a.snapshot(tenantID)); err != nil {
		return
	}

	// Periodic refresh keeps relative timestamps / expiry countdowns current.
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
		case <-ticker.C:
		}
		if err := wsjson.Write(ctx, c, a.snapshot(tenantID)); err != nil {
			return
		}
	}
}
