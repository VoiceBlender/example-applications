package main

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

// A tenant is an isolated PBX customer: its own extensions, trunks, dial plan,
// config, and softphone accounts. Extension NUMBERS may repeat across tenants
// (acme's 1001 ≠ globex's 1001); the underlying SIP username is namespaced to
// stay globally unique (see extRegistry.upsert), so registrations never collide.

// Tenant is one customer of the PBX.
type Tenant struct {
	ID        string `json:"id"` // slug, e.g. "acme"
	Name      string `json:"name"`
	CreatedAt string `json:"created_at,omitempty"`
}

// User is a console login. Username is globally unique; it maps to exactly one
// tenant. Password is stored plaintext for demo simplicity (see README), same as
// extension/trunk secrets.
type User struct {
	Username string `json:"username"`
	Password string `json:"password"`
	TenantID string `json:"tenant_id"`
}

var (
	errTenantExists   = errors.New("tenant already exists")
	errUserExists     = errors.New("username already taken")
	errBadSignup      = errors.New("tenant name and admin username are required")
	errTenantNotFound = errors.New("tenant not found")
)

// userView is the password-free projection of a console user.
type userView struct {
	Username string `json:"username"`
	TenantID string `json:"tenant_id"`
}

// tenantStore holds the tenants and console users, persisted to Redis.
type tenantStore struct {
	mu     sync.RWMutex
	byID   map[string]*Tenant
	byUser map[string]*User // keyed by lower-cased username
	store  *redisStore
	notify func()
}

func newTenantStore(store *redisStore) *tenantStore {
	return &tenantStore{
		byID:   make(map[string]*Tenant),
		byUser: make(map[string]*User),
		store:  store,
	}
}

// load reads all tenants and users into memory. Called once at startup.
func (s *tenantStore) load(ctx context.Context) error {
	tenants, err := s.store.LoadTenants(ctx)
	if err != nil {
		return err
	}
	users, err := s.store.LoadUsers(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range tenants {
		t := tenants[i]
		s.byID[t.ID] = &t
	}
	for i := range users {
		u := users[i]
		s.byUser[strings.ToLower(u.Username)] = &u
	}
	return nil
}

func (s *tenantStore) tenant(id string) (Tenant, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.byID[id]
	if !ok {
		return Tenant{}, false
	}
	return *t, true
}

func (s *tenantStore) user(username string) (User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.byUser[strings.ToLower(username)]
	if !ok {
		return User{}, false
	}
	return *u, true
}

func (s *tenantStore) count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byID)
}

// list returns all tenants, sorted by id (for the superadmin console).
func (s *tenantStore) list() []Tenant {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Tenant, 0, len(s.byID))
	for _, t := range s.byID {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// users returns all console users (password-free), sorted by tenant then name.
func (s *tenantStore) users() []userView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]userView, 0, len(s.byUser))
	for _, u := range s.byUser {
		out = append(out, userView{Username: u.Username, TenantID: u.TenantID})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TenantID != out[j].TenantID {
			return out[i].TenantID < out[j].TenantID
		}
		return out[i].Username < out[j].Username
	})
	return out
}

// addUser creates a console user in an existing tenant.
func (s *tenantStore) addUser(ctx context.Context, tenantID, username, password string) error {
	username = strings.TrimSpace(username)
	if username == "" || strings.TrimSpace(password) == "" {
		return errBadSignup
	}
	s.mu.RLock()
	_, tOK := s.byID[tenantID]
	_, uExists := s.byUser[strings.ToLower(username)]
	s.mu.RUnlock()
	if !tOK {
		return errTenantNotFound
	}
	if uExists {
		return errUserExists
	}
	u := User{Username: username, Password: password, TenantID: tenantID}
	if err := s.store.SaveUser(ctx, u); err != nil {
		return err
	}
	s.mu.Lock()
	s.byUser[strings.ToLower(u.Username)] = &u
	s.mu.Unlock()
	if s.notify != nil {
		s.notify()
	}
	return nil
}

// deleteUser removes a console user.
func (s *tenantStore) deleteUser(ctx context.Context, username string) error {
	if err := s.store.DeleteUser(ctx, username); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.byUser, strings.ToLower(username))
	s.mu.Unlock()
	if s.notify != nil {
		s.notify()
	}
	return nil
}

// deleteTenant removes a tenant and all its console users (from the store and
// memory). Cascading deletion of the tenant's extensions/trunks/config/dial plan
// is handled by the caller.
func (s *tenantStore) deleteTenant(ctx context.Context, id string) error {
	s.mu.RLock()
	_, ok := s.byID[id]
	var users []string
	for _, u := range s.byUser {
		if u.TenantID == id {
			users = append(users, u.Username)
		}
	}
	s.mu.RUnlock()
	if !ok {
		return errTenantNotFound
	}
	for _, u := range users {
		if err := s.store.DeleteUser(ctx, u); err != nil {
			return err
		}
	}
	if err := s.store.DeleteTenant(ctx, id); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.byID, id)
	for _, u := range users {
		delete(s.byUser, strings.ToLower(u))
	}
	s.mu.Unlock()
	if s.notify != nil {
		s.notify()
	}
	return nil
}

// signup creates a new tenant and its admin user (self-service). Returns the
// created tenant/user, or an error if the slug or username is taken.
func (s *tenantStore) signup(ctx context.Context, name, adminUser, adminPass string) (Tenant, User, error) {
	name = strings.TrimSpace(name)
	adminUser = strings.TrimSpace(adminUser)
	id := slug(name)
	if id == "" || adminUser == "" {
		return Tenant{}, User{}, errBadSignup
	}
	s.mu.RLock()
	_, tExists := s.byID[id]
	_, uExists := s.byUser[strings.ToLower(adminUser)]
	s.mu.RUnlock()
	if tExists {
		return Tenant{}, User{}, errTenantExists
	}
	if uExists {
		return Tenant{}, User{}, errUserExists
	}
	t := Tenant{ID: id, Name: name, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	u := User{Username: adminUser, Password: adminPass, TenantID: id}
	if err := s.store.SaveTenant(ctx, t); err != nil {
		return Tenant{}, User{}, err
	}
	if err := s.store.SaveUser(ctx, u); err != nil {
		return Tenant{}, User{}, err
	}
	s.mu.Lock()
	s.byID[t.ID] = &t
	s.byUser[strings.ToLower(u.Username)] = &u
	s.mu.Unlock()
	if s.notify != nil {
		s.notify()
	}
	return t, u, nil
}

// ensure creates a tenant + user if the tenant slug doesn't exist yet (used to
// seed a default tenant for local dev). No-op if the tenant already exists.
func (s *tenantStore) ensure(ctx context.Context, id, name, adminUser, adminPass string) error {
	if _, ok := s.tenant(id); ok {
		return nil
	}
	_, _, err := s.signup(ctx, name, adminUser, adminPass)
	return err
}

// slug lowercases a name and collapses non-alphanumeric runs to single dashes,
// yielding a stable tenant id (e.g. "Acme Corp!" → "acme-corp").
func slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r)
			dash = false
		} else {
			dash = true
		}
	}
	return b.String()
}
