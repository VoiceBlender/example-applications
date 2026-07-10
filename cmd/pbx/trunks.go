package main

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// Trunk types.
const (
	trunkRegister = "register" // VoiceBlender REGISTERs to an upstream provider
	trunkIP       = "ip"       // static IP peering; inbound authenticated by source IP
)

// Trunk is an upstream SIP connection. A "register" trunk makes VoiceBlender
// register to a provider (credential auth). An "ip" trunk is a static peer;
// inbound calls from it are authenticated purely by source IP (PeerIPs).
type Trunk struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	Name     string `json:"name"`
	Type     string `json:"type"` // register | ip

	// register-type fields.
	RegistrarURI string `json:"registrar_uri,omitempty"`
	AOR          string `json:"aor,omitempty"`
	Username     string `json:"username,omitempty"`
	Password     string `json:"password,omitempty"`
	Expires      int    `json:"expires,omitempty"`

	// ip-type fields.
	PeerURI string   `json:"peer_uri,omitempty"`
	PeerIPs []string `json:"peer_ips,omitempty"` // source IPs trusted for inbound calls
}

// dialHost is the SIP host outbound external calls via this trunk are sent to.
func (t Trunk) dialHost() string {
	if t.Type == trunkIP {
		return sipHost(t.PeerURI)
	}
	return sipHost(t.RegistrarURI)
}

// trunkStatus is the live server-side state of a trunk.
type trunkStatus struct {
	ServerID  string
	State     string // pending | registered | failed | expired | active
	LastError string
	UpdatedAt time.Time
}

// trunkView is the password-free projection sent to the web UI.
type trunkView struct {
	ID           string   `json:"id"`
	TenantID     string   `json:"tenant_id,omitempty"`
	Name         string   `json:"name"`
	Type         string   `json:"type"`
	RegistrarURI string   `json:"registrar_uri,omitempty"`
	AOR          string   `json:"aor,omitempty"`
	Username     string   `json:"username,omitempty"`
	PeerURI      string   `json:"peer_uri,omitempty"`
	PeerIPs      []string `json:"peer_ips,omitempty"`
	State        string   `json:"state"`
	LastError    string   `json:"last_error,omitempty"`
}

// trunkRegistry holds configured trunks plus their live status.
type trunkRegistry struct {
	mu         sync.RWMutex
	byID       map[string]*Trunk
	status     map[string]*trunkStatus // keyed by our Trunk.ID
	byServerID map[string]string       // server trunk id → our Trunk.ID
	store      *redisStore
	notify     func()
}

func newTrunkRegistry(store *redisStore) *trunkRegistry {
	return &trunkRegistry{
		byID:       make(map[string]*Trunk),
		status:     make(map[string]*trunkStatus),
		byServerID: make(map[string]string),
		store:      store,
	}
}

func (r *trunkRegistry) load(ctx context.Context) error {
	trunks, err := r.store.LoadTrunks(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range trunks {
		t := trunks[i]
		r.byID[t.ID] = &t
		r.status[t.ID] = &trunkStatus{State: "pending"}
	}
	return nil
}

// list returns copies of all configured trunks.
func (r *trunkRegistry) list() []Trunk {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Trunk, 0, len(r.byID))
	for _, t := range r.byID {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// listForTenant returns copies of every trunk owned by tenantID.
func (r *trunkRegistry) listForTenant(tenantID string) []Trunk {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Trunk
	for _, t := range r.byID {
		if t.TenantID == tenantID {
			out = append(out, *t)
		}
	}
	return out
}

// ownedBy reports whether the trunk id exists and belongs to tenantID.
func (r *trunkRegistry) ownedBy(id, tenantID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.byID[id]
	return ok && t.TenantID == tenantID
}

func (r *trunkRegistry) get(id string) (Trunk, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.byID[id]
	if !ok {
		return Trunk{}, false
	}
	return *t, true
}

func (r *trunkRegistry) upsert(ctx context.Context, t Trunk) (Trunk, error) {
	if t.ID == "" {
		t.ID = newID()
	}
	if err := r.store.SaveTrunk(ctx, t); err != nil {
		return Trunk{}, err
	}
	r.mu.Lock()
	r.byID[t.ID] = &t
	if r.status[t.ID] == nil {
		r.status[t.ID] = &trunkStatus{State: "pending"}
	}
	r.mu.Unlock()
	r.changed()
	return t, nil
}

func (r *trunkRegistry) delete(ctx context.Context, id string) error {
	r.mu.Lock()
	_, ok := r.byID[id]
	r.mu.Unlock()
	if !ok {
		return errNotFound
	}
	if err := r.store.DeleteTrunk(ctx, id); err != nil {
		return err
	}
	r.mu.Lock()
	if st := r.status[id]; st != nil && st.ServerID != "" {
		delete(r.byServerID, st.ServerID)
	}
	delete(r.byID, id)
	delete(r.status, id)
	r.mu.Unlock()
	r.changed()
	return nil
}

// setServerID records the server-assigned trunk id and its initial state.
func (r *trunkRegistry) setServerID(localID, serverID, state string) {
	r.mu.Lock()
	st := r.status[localID]
	if st == nil {
		st = &trunkStatus{}
		r.status[localID] = st
	}
	if st.ServerID != "" {
		delete(r.byServerID, st.ServerID)
	}
	st.ServerID = serverID
	st.State = state
	st.UpdatedAt = time.Now()
	if serverID != "" {
		r.byServerID[serverID] = localID
	}
	r.mu.Unlock()
	r.changed()
}

// setStatusByServerID updates a trunk's state from an outbound-registration
// event, which references the server-assigned trunk id.
func (r *trunkRegistry) setStatusByServerID(serverID, state, lastErr string) {
	r.mu.Lock()
	localID, ok := r.byServerID[serverID]
	if ok {
		if st := r.status[localID]; st != nil {
			st.State = state
			st.LastError = lastErr
			st.UpdatedAt = time.Now()
		}
	}
	r.mu.Unlock()
	if ok {
		r.changed()
	}
}

// trunkForSourceIP returns the trunk an inbound call from the given source IP
// belongs to, if any. IP-type trunks match on their configured PeerIPs; both
// trunk kinds also match when the source IP equals the peer/registrar host
// written as an IP literal.
func (r *trunkRegistry) trunkForSourceIP(ip string) (Trunk, bool) {
	if ip == "" {
		return Trunk{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, t := range r.byID {
		for _, p := range t.PeerIPs {
			if strings.TrimSpace(p) == ip {
				return *t, true
			}
		}
		if h := t.dialHost(); h == ip {
			return *t, true
		}
	}
	return Trunk{}, false
}

// trunkForInbound resolves the trunk (and hence tenant) an inbound call belongs
// to. It tries, in order: (1) the source IP — ip trunks' PeerIPs or a registrar
// written as a literal IP; (2) the dialed number matching a register trunk's
// auth username or AOR user-part, since providers address inbound calls to the
// trunk's DID/account (a register trunk's registrar is usually a hostname, so
// its source IP won't match). dialedUser is sipUser(ring.To).
func (r *trunkRegistry) trunkForInbound(sourceIP, dialedUser string) (Trunk, bool) {
	if t, ok := r.trunkForSourceIP(sourceIP); ok {
		return t, true
	}
	if dialedUser == "" {
		return Trunk{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, t := range r.byID {
		if t.Type != trunkRegister {
			continue
		}
		if strings.EqualFold(t.Username, dialedUser) || strings.EqualFold(sipUser(t.AOR), dialedUser) {
			return *t, true
		}
	}
	return Trunk{}, false
}

// outboundTrunk picks the trunk to route a tenant's external (non-extension)
// calls out of. Prefers a register trunk, then the tenant's first trunk.
func (r *trunkRegistry) outboundTrunk(tenantID string) (Trunk, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var first *Trunk
	for _, t := range r.byID {
		if t.TenantID != tenantID {
			continue
		}
		if first == nil {
			first = t
		}
		if t.Type == trunkRegister {
			return *t, true
		}
	}
	if first != nil {
		return *first, true
	}
	return Trunk{}, false
}

// viewOf projects one trunk into its UI view (caller holds the read lock).
func (r *trunkRegistry) viewOf(t *Trunk) trunkView {
	v := trunkView{
		ID: t.ID, TenantID: t.TenantID, Name: t.Name, Type: t.Type,
		RegistrarURI: t.RegistrarURI, AOR: t.AOR, Username: t.Username,
		PeerURI: t.PeerURI, PeerIPs: t.PeerIPs,
		State: "pending",
	}
	if st := r.status[t.ID]; st != nil {
		v.State = st.State
		v.LastError = st.LastError
	}
	return v
}

func (r *trunkRegistry) views(tenantID string) []trunkView {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]trunkView, 0, len(r.byID))
	for _, t := range r.byID {
		if t.TenantID == tenantID {
			out = append(out, r.viewOf(t))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// viewsAll returns every trunk across all tenants (for the superadmin).
func (r *trunkRegistry) viewsAll() []trunkView {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]trunkView, 0, len(r.byID))
	for _, t := range r.byID {
		out = append(out, r.viewOf(t))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TenantID != out[j].TenantID {
			return out[i].TenantID < out[j].TenantID
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// serverID returns the server-assigned trunk id for a local trunk, if created.
func (r *trunkRegistry) serverID(localID string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if st := r.status[localID]; st != nil {
		return st.ServerID
	}
	return ""
}

func (r *trunkRegistry) upCount(tenantID string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for id, t := range r.byID {
		if t.TenantID != tenantID {
			continue
		}
		if st := r.status[id]; st != nil && (st.State == "registered" || st.State == "active") {
			n++
		}
	}
	return n
}

func (r *trunkRegistry) count(tenantID string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, t := range r.byID {
		if t.TenantID == tenantID {
			n++
		}
	}
	return n
}

func (r *trunkRegistry) changed() {
	if r.notify != nil {
		r.notify()
	}
}
