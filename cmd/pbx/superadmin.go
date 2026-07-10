package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
)

//go:embed web/page_admin.html
var adminHTML []byte

// The superadmin is a cross-tenant operator (env SUPERADMIN=user:pass) who can
// view and manage every tenant's extensions and trunks from a dedicated console
// at /admin. These handlers are NOT tenant-scoped — they operate on any object
// by id, with the owning tenant taken from the object itself.

func (a *app) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(adminHTML)
}

// handleAdminData returns every tenant, user, extension, and trunk system-wide.
func (a *app) handleAdminData(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"tenants":    a.tenants.list(),
		"users":      a.tenants.users(),
		"extensions": a.exts.viewsAll(),
		"trunks":     a.trunks.viewsAll(),
	})
}

// validTenant reports whether tenantID names an existing tenant.
func (a *app) validTenant(tenantID string) bool {
	_, ok := a.tenants.tenant(tenantID)
	return ok
}

// ── superadmin tenants & users ───────────────────────────────────────────────

func (a *app) handleAdminCreateTenant(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string `json:"name"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	tenant, _, err := a.tenants.signup(r.Context(), body.Name, body.Username, body.Password)
	switch err {
	case nil:
	case errTenantExists, errUserExists:
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case errBadSignup:
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	default:
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if strings.TrimSpace(body.Password) == "" {
		// signup allows a blank password; a login needs one, so require it here.
		_ = a.tenants.deleteTenant(r.Context(), tenant.ID)
		http.Error(w, "password is required", http.StatusBadRequest)
		return
	}
	a.log.Info("superadmin created tenant", "tenant", tenant.ID, "admin", body.Username)
	writeJSON(w, http.StatusCreated, tenant)
}

// handleAdminDeleteTenant removes a tenant and cascades: its trunks (server-side
// too), extensions, config, dial plan, and console users.
func (a *app) handleAdminDeleteTenant(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !a.validTenant(id) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	ctx := r.Context()
	for _, t := range a.trunks.listForTenant(id) {
		a.removeTrunkFromServer(ctx, t)
		_ = a.trunks.delete(ctx, t.ID)
	}
	for _, extID := range a.exts.idsForTenant(id) {
		_ = a.exts.delete(ctx, extID)
	}
	a.config.forget(ctx, id)
	a.dialplan.forget(ctx, id)
	if err := a.tenants.deleteTenant(ctx, id); err != nil {
		a.log.Error("delete tenant", "tenant", id, "error", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	a.log.Info("superadmin deleted tenant", "tenant", id)
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) handleAdminCreateUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TenantID string `json:"tenant_id"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	switch err := a.tenants.addUser(r.Context(), body.TenantID, body.Username, body.Password); err {
	case nil:
		w.WriteHeader(http.StatusCreated)
	case errUserExists:
		http.Error(w, err.Error(), http.StatusConflict)
	case errTenantNotFound, errBadSignup:
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, "storage error", http.StatusInternalServerError)
	}
}

func (a *app) handleAdminDeleteUser(w http.ResponseWriter, r *http.Request) {
	if err := a.tenants.deleteUser(r.Context(), r.PathValue("username")); err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── superadmin dial plans ────────────────────────────────────────────────────

func (a *app) handleAdminGetDialplan(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	if !a.validTenant(tenant) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, a.dialplan.get(tenant))
}

func (a *app) handleAdminUpdateDialplan(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	if !a.validTenant(tenant) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var g DPGraph
	if err := json.NewDecoder(r.Body).Decode(&g); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := a.dialplan.update(r.Context(), tenant, g); err != nil {
		a.log.Error("admin save dial plan", "tenant", tenant, "error", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, a.dialplan.get(tenant))
}

// ── superadmin extensions ────────────────────────────────────────────────────

func (a *app) handleAdminCreateExtension(w http.ResponseWriter, r *http.Request) {
	var e Extension
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	e.ID = ""
	if e.Number == "" || !a.validTenant(e.TenantID) {
		http.Error(w, "number and a valid tenant are required", http.StatusBadRequest)
		return
	}
	a.adminUpsertExtension(w, r, e)
}

func (a *app) handleAdminUpdateExtension(w http.ResponseWriter, r *http.Request) {
	var e Extension
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	e.ID = r.PathValue("id")
	if !a.exts.exists(e.ID) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if !a.validTenant(e.TenantID) {
		http.Error(w, "a valid tenant is required", http.StatusBadRequest)
		return
	}
	a.adminUpsertExtension(w, r, e)
}

func (a *app) adminUpsertExtension(w http.ResponseWriter, r *http.Request, e Extension) {
	saved, err := a.exts.upsert(r.Context(), e)
	if err == errDuplicateIdentity {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		a.log.Error("admin save extension", "error", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	go a.reconcileRegistrations(context.Background())
	writeJSON(w, http.StatusOK, saved)
}

func (a *app) handleAdminDeleteExtension(w http.ResponseWriter, r *http.Request) {
	if err := a.exts.delete(r.Context(), r.PathValue("id")); err != nil {
		if err == errNotFound {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		a.log.Error("admin delete extension", "error", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── superadmin trunks ────────────────────────────────────────────────────────

func (a *app) handleAdminCreateTrunk(w http.ResponseWriter, r *http.Request) {
	var t Trunk
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	t.ID = ""
	if (t.Type != trunkRegister && t.Type != trunkIP) || !a.validTenant(t.TenantID) {
		http.Error(w, "type must be 'register' or 'ip' and a valid tenant is required", http.StatusBadRequest)
		return
	}
	saved, err := a.trunks.upsert(r.Context(), t)
	if err != nil {
		a.log.Error("admin save trunk", "error", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if err := a.applyTrunkToServer(r.Context(), saved); err != nil {
		a.log.Warn("admin create trunk on server", "trunk", saved.Name, "error", err)
	}
	writeJSON(w, http.StatusCreated, saved)
}

func (a *app) handleAdminUpdateTrunk(w http.ResponseWriter, r *http.Request) {
	var t Trunk
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	t.ID = r.PathValue("id")
	if _, ok := a.trunks.get(t.ID); !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if !a.validTenant(t.TenantID) {
		http.Error(w, "a valid tenant is required", http.StatusBadRequest)
		return
	}
	if old, ok := a.trunks.get(t.ID); ok {
		a.removeTrunkFromServer(r.Context(), old)
	}
	saved, err := a.trunks.upsert(r.Context(), t)
	if err != nil {
		a.log.Error("admin update trunk", "error", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if err := a.applyTrunkToServer(r.Context(), saved); err != nil {
		a.log.Warn("admin recreate trunk on server", "trunk", saved.Name, "error", err)
	}
	writeJSON(w, http.StatusOK, saved)
}

func (a *app) handleAdminDeleteTrunk(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if t, ok := a.trunks.get(id); ok {
		a.removeTrunkFromServer(r.Context(), t)
	}
	if err := a.trunks.delete(r.Context(), id); err != nil {
		if err == errNotFound {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		a.log.Error("admin delete trunk", "error", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
