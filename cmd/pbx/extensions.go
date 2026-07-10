package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

// Extension is a SIP endpoint the PBX authenticates and routes to. Number is
// the dialable extension (e.g. "1001"); Username is the SIP auth username and
// the user-part of the extension's Address-of-Record (sip:<username>@<domain>).
// In the common case Number == Username. Password is the digest secret — stored
// in plaintext here for demo simplicity (see README).
type Extension struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	Number   string `json:"number"`
	Name     string `json:"name"`
	// Username is the SIP auth username / AOR user-part. It is globally unique
	// across tenants (auto-derived as "<tenant>-<number>" when blank), so that
	// two tenants' identical Numbers register without colliding.
	Username string `json:"username"`
	Password string `json:"password"`
	// Codecs is the codec preference order offered when ringing this extension
	// (highest priority first, e.g. ["opus","PCMA","PCMU"]). Empty = server/
	// global default.
	Codecs []string `json:"codecs,omitempty"`
	// RegExpiry caps the granted REGISTER expiry (seconds) for this extension —
	// the phone re-registers at least this often. 0 = default (defaultRegExpiry).
	RegExpiry int `json:"reg_expiry,omitempty"`
	// WebRTCAccounts are browser-softphone logins that act as extra devices for
	// this same extension number: signing into one establishes a live WebRTC leg
	// that rings alongside the SIP registration(s) and can place calls as this
	// extension. Stored plaintext (demo), same as Password.
	WebRTCAccounts []WebRTCAccount `json:"webrtc_accounts,omitempty"`
}

// WebRTCAccount is a browser-softphone credential attached to an extension.
// Username is unique across all extensions (it's the softphone login).
type WebRTCAccount struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Label    string `json:"label,omitempty"` // free-text device/person name
}

// defaultRegExpiry is the registration expiry (seconds) granted to an extension
// that hasn't set its own.
const defaultRegExpiry = 60

// regExpiry returns the extension's configured registration expiry, or the
// default when unset.
func (e Extension) regExpiry() int {
	if e.RegExpiry > 0 {
		return e.RegExpiry
	}
	return defaultRegExpiry
}

// regStatus is the live registration state of an extension, derived from the
// registrar events (sip.registration_active / sip.registration_expired).
type regStatus struct {
	Registered bool
	AOR        string // canonical registered AOR, e.g. sip:1002@192.168.1.50
	Contact    string
	Socket     string
	UserAgent  string
	ExpiresAt  time.Time
	UpdatedAt  time.Time
}

// extView is the password-free projection sent to the web UI.
type extView struct {
	ID         string   `json:"id"`
	TenantID   string   `json:"tenant_id,omitempty"`
	Number     string   `json:"number"`
	Name       string   `json:"name"`
	Username   string   `json:"username"`
	Registered bool     `json:"registered"`
	Contact    string   `json:"contact,omitempty"`
	Source     string   `json:"source,omitempty"` // ip:port the REGISTER arrived from
	UserAgent  string   `json:"user_agent,omitempty"`
	ExpiresAt  string   `json:"expires_at,omitempty"`
	Codecs     []string `json:"codecs,omitempty"`
	RegExpiry  int      `json:"reg_expiry,omitempty"`
	// WebRTCDevices lists this extension's WebRTC softphone accounts (no
	// password), each with live presence joined from the phone registry.
	WebRTCDevices []webRTCDeviceView `json:"webrtc_devices,omitempty"`
}

// webRTCDeviceView is the password-free, presence-annotated projection of a
// WebRTCAccount for the console.
type webRTCDeviceView struct {
	Username string `json:"username"`
	Label    string `json:"label,omitempty"`
	Online   bool   `json:"online"`
	State    string `json:"state,omitempty"` // idle | ringing | in-call (when online)
}

var errNotFound = errors.New("not found")

// extRegistry holds the configured extensions plus their live registration
// status. It persists mutations through the store and fans out a change
// notification (for the web snapshot) after every write.
type extRegistry struct {
	mu     sync.RWMutex
	byID   map[string]*Extension
	reg    map[string]*regStatus // keyed by Username (AOR user-part)
	store  *redisStore
	notify func()
	// deviceStatus reports the live presence of a WebRTC softphone account
	// (keyed by account username). Wired to the phone registry; nil = no live
	// WebRTC presence tracking (all devices shown offline).
	deviceStatus func(account string) (online bool, state string)
}

func newExtRegistry(store *redisStore) *extRegistry {
	return &extRegistry{
		byID:  make(map[string]*Extension),
		reg:   make(map[string]*regStatus),
		store: store,
	}
}

// load reads all extensions from the store into memory. Called once at startup.
func (r *extRegistry) load(ctx context.Context) error {
	exts, err := r.store.LoadExtensions(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range exts {
		e := exts[i]
		r.byID[e.ID] = &e
	}
	return nil
}

// byNumber returns a copy of the tenant's extension with the given dialable
// number. Numbers are only unique within a tenant, so a tenant id is required.
func (r *extRegistry) byNumber(tenantID, number string) (Extension, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.byID {
		if e.TenantID == tenantID && e.Number == number {
			return *e, true
		}
	}
	return Extension{}, false
}

// byUsername returns a copy of the extension with the given SIP username.
func (r *extRegistry) byUsername(username string) (Extension, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.byID {
		if strings.EqualFold(e.Username, username) {
			return *e, true
		}
	}
	return Extension{}, false
}

// byWebRTCAccount returns the extension owning the given WebRTC softphone
// account username, plus the account itself.
func (r *extRegistry) byWebRTCAccount(username string) (Extension, WebRTCAccount, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.byID {
		for _, acct := range e.WebRTCAccounts {
			if strings.EqualFold(acct.Username, username) {
				return *e, acct, true
			}
		}
	}
	return Extension{}, WebRTCAccount{}, false
}

// idsForTenant returns the ids of every extension owned by tenantID.
func (r *extRegistry) idsForTenant(tenantID string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var ids []string
	for id, e := range r.byID {
		if e.TenantID == tenantID {
			ids = append(ids, id)
		}
	}
	return ids
}

// exists reports whether an extension with the given id exists.
func (r *extRegistry) exists(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.byID[id]
	return ok
}

// ownedBy reports whether the extension id exists and belongs to tenantID.
func (r *extRegistry) ownedBy(id, tenantID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.byID[id]
	return ok && e.TenantID == tenantID
}

// isRegistered reports whether the extension with the given username currently
// has a live registration binding.
func (r *extRegistry) isRegistered(username string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s := r.reg[strings.ToLower(username)]
	return s != nil && s.Registered
}

// registeredAOR returns the canonical AOR the extension is currently registered
// under, if any. Callers dial this AOR so the server's registrar resolves it to
// the bound contact/socket — reconstructing sip:<user>@<domain> would miss when
// the phone registered against a different host than PBX_DOMAIN.
func (r *extRegistry) registeredAOR(username string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s := r.reg[strings.ToLower(username)]
	if s != nil && s.Registered && s.AOR != "" {
		return s.AOR, true
	}
	return "", false
}

// upsert creates or updates an extension, assigning an ID on create. It writes
// through to the store before updating memory so a failed persist doesn't leave
// a phantom in-memory row.
func (r *extRegistry) upsert(ctx context.Context, e Extension) (Extension, error) {
	if e.ID == "" {
		e.ID = newID()
	}
	// SIP username is globally unique; default it to "<tenant>-<number>" so two
	// tenants' identical Numbers don't collide on the registrar.
	if e.Username == "" {
		e.Username = e.TenantID + "-" + e.Number
	}
	// Preserve secrets the console can't resend: it never receives passwords in
	// the snapshot, so a blank password on edit means "unchanged", not "clear".
	r.mu.RLock()
	old := r.byID[e.ID]
	r.mu.RUnlock()
	if old != nil {
		if e.Password == "" {
			e.Password = old.Password
		}
		e.WebRTCAccounts = mergeWebRTCAccounts(old.WebRTCAccounts, e.WebRTCAccounts)
	}
	if err := r.ensureUniqueIdentities(e); err != nil {
		return Extension{}, err
	}
	if err := r.store.SaveExtension(ctx, e); err != nil {
		return Extension{}, err
	}
	r.mu.Lock()
	r.byID[e.ID] = &e
	r.mu.Unlock()
	r.changed()
	return e, nil
}

// errDuplicateIdentity is returned when an extension's SIP username or a WebRTC
// account username is not globally unique.
var errDuplicateIdentity = errors.New("SIP username or WebRTC account already in use")

// ensureUniqueIdentities checks that e's SIP username and each WebRTC account
// username are unique across ALL tenants (they are the real, non-namespaced SIP
// identities), ignoring e's own current row.
func (r *extRegistry) ensureUniqueIdentities(e Extension) error {
	taken := make(map[string]struct{})
	r.mu.RLock()
	for _, other := range r.byID {
		if other.ID == e.ID {
			continue
		}
		taken[strings.ToLower(other.Username)] = struct{}{}
		for _, acct := range other.WebRTCAccounts {
			taken[strings.ToLower(acct.Username)] = struct{}{}
		}
	}
	r.mu.RUnlock()
	if _, dup := taken[strings.ToLower(e.Username)]; dup {
		return errDuplicateIdentity
	}
	seen := map[string]struct{}{strings.ToLower(e.Username): {}}
	for _, acct := range e.WebRTCAccounts {
		k := strings.ToLower(acct.Username)
		if _, dup := taken[k]; dup {
			return errDuplicateIdentity
		}
		if _, dup := seen[k]; dup {
			return errDuplicateIdentity // duplicate within this extension
		}
		seen[k] = struct{}{}
	}
	return nil
}

// mergeWebRTCAccounts fills in blank submitted passwords from the stored
// accounts (matched by username), so the console can manage accounts without
// re-typing every secret. Accounts absent from next are dropped (deleted);
// accounts with a non-blank password are updated.
func mergeWebRTCAccounts(old, next []WebRTCAccount) []WebRTCAccount {
	if next == nil {
		return old // field omitted → keep existing (an explicit [] clears them)
	}
	if len(next) == 0 {
		return next
	}
	byUser := make(map[string]WebRTCAccount, len(old))
	for _, a := range old {
		byUser[strings.ToLower(a.Username)] = a
	}
	for i := range next {
		if next[i].Password == "" {
			if prev, ok := byUser[strings.ToLower(next[i].Username)]; ok {
				next[i].Password = prev.Password
			}
		}
	}
	return next
}

// delete removes an extension by ID.
func (r *extRegistry) delete(ctx context.Context, id string) error {
	r.mu.Lock()
	_, ok := r.byID[id]
	r.mu.Unlock()
	if !ok {
		return errNotFound
	}
	if err := r.store.DeleteExtension(ctx, id); err != nil {
		return err
	}
	r.mu.Lock()
	delete(r.byID, id)
	r.mu.Unlock()
	r.changed()
	return nil
}

// setRegistered / setUnregistered update live registration status from the
// registrar events and trigger a snapshot push.
func (r *extRegistry) setRegistered(username string, s regStatus) {
	s.Registered = true
	s.UpdatedAt = time.Now()
	r.mu.Lock()
	r.reg[strings.ToLower(username)] = &s
	r.mu.Unlock()
	r.changed()
}

func (r *extRegistry) setUnregistered(username string) {
	r.mu.Lock()
	if s := r.reg[strings.ToLower(username)]; s != nil {
		s.Registered = false
		s.UpdatedAt = time.Now()
	}
	r.mu.Unlock()
	r.changed()
}

// reconcile replaces the live registration status with an authoritative
// snapshot (from ListSIPRegistrations): every known extension whose username
// appears in present is marked registered, everything else offline. This seeds
// state at startup and self-heals if a live active/expired event was missed —
// registrations created before the app started emit no event until they
// refresh, so without this the panel would show them offline for up to a full
// registration interval. present is keyed by lower-cased username.
func (r *extRegistry) reconcile(present map[string]regStatus) {
	now := time.Now()
	r.mu.Lock()
	for _, e := range r.byID {
		key := strings.ToLower(e.Username)
		if st, ok := present[key]; ok {
			st.Registered = true
			st.UpdatedAt = now
			r.reg[key] = &st
		} else if cur := r.reg[key]; cur != nil && cur.Registered {
			cur.Registered = false
			cur.UpdatedAt = now
		}
	}
	r.mu.Unlock()
	r.changed()
}

// viewOf projects one extension into its UI view (caller holds the read lock).
func (r *extRegistry) viewOf(e *Extension) extView {
	v := extView{ID: e.ID, TenantID: e.TenantID, Number: e.Number, Name: e.Name, Username: e.Username, Codecs: e.Codecs, RegExpiry: e.RegExpiry}
	if s := r.reg[strings.ToLower(e.Username)]; s != nil {
		v.Registered = s.Registered
		v.Contact = s.Contact
		v.Source = s.Socket
		v.UserAgent = s.UserAgent
		if !s.ExpiresAt.IsZero() {
			v.ExpiresAt = s.ExpiresAt.UTC().Format(time.RFC3339)
		}
	}
	for _, acct := range e.WebRTCAccounts {
		d := webRTCDeviceView{Username: acct.Username, Label: acct.Label}
		if r.deviceStatus != nil {
			d.Online, d.State = r.deviceStatus(acct.Username)
		}
		v.WebRTCDevices = append(v.WebRTCDevices, d)
	}
	return v
}

// views returns the UI projection of a tenant's extensions, sorted by number.
func (r *extRegistry) views(tenantID string) []extView {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]extView, 0, len(r.byID))
	for _, e := range r.byID {
		if e.TenantID == tenantID {
			out = append(out, r.viewOf(e))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

// viewsAll returns every extension across all tenants (for the superadmin),
// sorted by tenant then number.
func (r *extRegistry) viewsAll() []extView {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]extView, 0, len(r.byID))
	for _, e := range r.byID {
		out = append(out, r.viewOf(e))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TenantID != out[j].TenantID {
			return out[i].TenantID < out[j].TenantID
		}
		return out[i].Number < out[j].Number
	})
	return out
}

func (r *extRegistry) registeredCount(tenantID string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, e := range r.byID {
		if e.TenantID != tenantID {
			continue
		}
		if s := r.reg[strings.ToLower(e.Username)]; s != nil && s.Registered {
			n++
		}
	}
	return n
}

func (r *extRegistry) count(tenantID string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, e := range r.byID {
		if e.TenantID == tenantID {
			n++
		}
	}
	return n
}

func (r *extRegistry) changed() {
	if r.notify != nil {
		r.notify()
	}
}

// newID returns a short random hex identifier for a stored object.
func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// Unreachable in practice; fall back to a time-based id.
		return "id-" + time.Now().UTC().Format("20060102150405.000000000")
	}
	return hex.EncodeToString(b)
}
