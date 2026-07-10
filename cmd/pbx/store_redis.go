package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// silentRedisLogger discards go-redis' internal chatter; we surface errors
// through our own slog at the call sites where we can format them properly.
type silentRedisLogger struct{}

func (silentRedisLogger) Printf(_ context.Context, _ string, _ ...any) {}

func init() {
	redis.SetLogger(silentRedisLogger{})
}

// Redis keys. Extensions and trunks are each stored in a single Redis hash,
// field = object ID, value = JSON. Hashes give us cheap by-ID CRUD.
const (
	extensionsKey = "pbx:extensions"
	trunksKey     = "pbx:trunks"
	tenantsKey    = "pbx:tenants" // hash: tenant id → JSON
	usersKey      = "pbx:users"   // hash: username → JSON
	// config and dial plan are per-tenant: prefix + tenant id.
	configPrefix   = "pbx:config:"
	dialplanPrefix = "pbx:dialplan:"
	// callHistoryPrefix + <account> is a capped list of the account's recent
	// softphone calls (newest first), for the dialer's re-dial list.
	callHistoryPrefix = "pbx:phonehist:"
	callHistoryMax    = 30
	// phonebookPrefix + <account> stores the account's saved contacts (a JSON
	// list), for the softphone's phonebook.
	phonebookPrefix = "pbx:phonebook:"
	phonebookMax    = 200
	// Console/softphone sessions, persisted so they survive a server restart.
	adminSessionsKey = "pbx:sessions"      // hash: token → sessionRecord
	phoneSessionsKey = "pbx:phonesessions" // hash: token → sessionRecord
)

// sessionRecord is a persisted admin or softphone session (a superset of both
// identity shapes so one hash type serves either store).
type sessionRecord struct {
	Token     string    `json:"token"`
	Username  string    `json:"username,omitempty"`   // admin
	TenantID  string    `json:"tenant_id,omitempty"`  // both
	Super     bool      `json:"super,omitempty"`      // admin
	Account   string    `json:"account,omitempty"`    // phone
	ExtNumber string    `json:"ext_number,omitempty"` // phone
	Expiry    time.Time `json:"expiry"`
}

// callRecord is one entry in a softphone's recent-calls list.
type callRecord struct {
	Number string `json:"number"`
	Dir    string `json:"dir"` // out | in | missed
	At     string `json:"at"`  // RFC3339
}

// contact is one saved phonebook entry.
type contact struct {
	Name   string `json:"name"`
	Number string `json:"number"`
}

// redisStore is the durable backing store for extensions and trunks. It is a
// thin wrapper over a go-redis client; all business logic lives in the
// registries that call it.
type redisStore struct {
	client *redis.Client
}

// newRedisStore parses a redis:// or rediss:// URL, dials, and PINGs to fail
// fast on misconfiguration. Mirrors cmd/contact-centre/calllog_redis.go.
func newRedisStore(ctx context.Context, url string) (*redisStore, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	// Don't let the pool retry 5× before reporting an unreachable server.
	opt.MaxRetries = -1
	c := redis.NewClient(opt)
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := c.Ping(pingCtx).Err(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return &redisStore{client: c}, nil
}

func (r *redisStore) Close() error { return r.client.Close() }

// loadHash reads every field of a hash and unmarshals each value into T.
func loadHash[T any](ctx context.Context, r *redisStore, key string) ([]T, error) {
	raw, err := r.client.HGetAll(ctx, key).Result()
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, len(raw))
	for _, v := range raw {
		var item T
		if err := json.Unmarshal([]byte(v), &item); err != nil {
			continue // skip a corrupt row rather than failing the whole load
		}
		out = append(out, item)
	}
	return out, nil
}

func (r *redisStore) LoadExtensions(ctx context.Context) ([]Extension, error) {
	return loadHash[Extension](ctx, r, extensionsKey)
}

func (r *redisStore) SaveExtension(ctx context.Context, e Extension) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return r.client.HSet(ctx, extensionsKey, e.ID, data).Err()
}

func (r *redisStore) DeleteExtension(ctx context.Context, id string) error {
	return r.client.HDel(ctx, extensionsKey, id).Err()
}

func (r *redisStore) LoadConfig(ctx context.Context, tenantID string) (Config, error) {
	var c Config
	v, err := r.client.Get(ctx, configPrefix+tenantID).Result()
	if err == redis.Nil {
		return c, nil // no config stored yet
	}
	if err != nil {
		return c, err
	}
	_ = json.Unmarshal([]byte(v), &c)
	return c, nil
}

func (r *redisStore) SaveConfig(ctx context.Context, tenantID string, c Config) error {
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return r.client.Set(ctx, configPrefix+tenantID, data, 0).Err()
}

// LoadDialplan returns a tenant's stored graph and whether one existed (ok=false
// when nothing has been saved yet, so the caller can seed a default).
func (r *redisStore) LoadDialplan(ctx context.Context, tenantID string) (DPGraph, bool, error) {
	var g DPGraph
	v, err := r.client.Get(ctx, dialplanPrefix+tenantID).Result()
	if err == redis.Nil {
		return g, false, nil
	}
	if err != nil {
		return g, false, err
	}
	if err := json.Unmarshal([]byte(v), &g); err != nil {
		return g, false, err
	}
	return g, true, nil
}

func (r *redisStore) SaveDialplan(ctx context.Context, tenantID string, g DPGraph) error {
	data, err := json.Marshal(g)
	if err != nil {
		return err
	}
	return r.client.Set(ctx, dialplanPrefix+tenantID, data, 0).Err()
}

// DeleteConfig / DeleteDialplan remove a tenant's config / dial-plan keys.
func (r *redisStore) DeleteConfig(ctx context.Context, tenantID string) error {
	return r.client.Del(ctx, configPrefix+tenantID).Err()
}

func (r *redisStore) DeleteDialplan(ctx context.Context, tenantID string) error {
	return r.client.Del(ctx, dialplanPrefix+tenantID).Err()
}

// ── tenants & users ──────────────────────────────────────────────────────────

func (r *redisStore) LoadTenants(ctx context.Context) ([]Tenant, error) {
	return loadHash[Tenant](ctx, r, tenantsKey)
}

func (r *redisStore) SaveTenant(ctx context.Context, t Tenant) error {
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return r.client.HSet(ctx, tenantsKey, t.ID, data).Err()
}

func (r *redisStore) DeleteTenant(ctx context.Context, id string) error {
	return r.client.HDel(ctx, tenantsKey, id).Err()
}

func (r *redisStore) LoadUsers(ctx context.Context) ([]User, error) {
	return loadHash[User](ctx, r, usersKey)
}

func (r *redisStore) SaveUser(ctx context.Context, u User) error {
	data, err := json.Marshal(u)
	if err != nil {
		return err
	}
	return r.client.HSet(ctx, usersKey, strings.ToLower(u.Username), data).Err()
}

func (r *redisStore) DeleteUser(ctx context.Context, username string) error {
	return r.client.HDel(ctx, usersKey, strings.ToLower(username)).Err()
}

// ── persisted sessions ───────────────────────────────────────────────────────

func (r *redisStore) LoadSessions(ctx context.Context, hashKey string) ([]sessionRecord, error) {
	return loadHash[sessionRecord](ctx, r, hashKey)
}

func (r *redisStore) SaveSession(ctx context.Context, hashKey string, rec sessionRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return r.client.HSet(ctx, hashKey, rec.Token, data).Err()
}

func (r *redisStore) DeleteSession(ctx context.Context, hashKey, token string) error {
	return r.client.HDel(ctx, hashKey, token).Err()
}

// AppendCallHistory prepends a record to the account's recent-calls list and
// trims it to the cap (newest-first).
func (r *redisStore) AppendCallHistory(ctx context.Context, account string, rec callRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	key := callHistoryPrefix + strings.ToLower(account)
	pipe := r.client.TxPipeline()
	pipe.LPush(ctx, key, data)
	pipe.LTrim(ctx, key, 0, callHistoryMax-1)
	_, err = pipe.Exec(ctx)
	return err
}

// RemoveCallHistoryAt deletes the record at index (0 = newest) from the
// account's recent-calls list. Out-of-range indexes are a no-op.
func (r *redisStore) RemoveCallHistoryAt(ctx context.Context, account string, index int) error {
	if index < 0 {
		return nil
	}
	key := callHistoryPrefix + strings.ToLower(account)
	const tomb = "\x00__pbx_removed__"
	if err := r.client.LSet(ctx, key, int64(index), tomb).Err(); err != nil {
		return nil // index out of range / empty list — nothing to remove
	}
	return r.client.LRem(ctx, key, 1, tomb).Err()
}

// LoadCallHistory returns the account's recent calls, newest first.
func (r *redisStore) LoadCallHistory(ctx context.Context, account string) ([]callRecord, error) {
	key := callHistoryPrefix + strings.ToLower(account)
	vals, err := r.client.LRange(ctx, key, 0, callHistoryMax-1).Result()
	if err != nil {
		return nil, err
	}
	out := make([]callRecord, 0, len(vals))
	for _, v := range vals {
		var rec callRecord
		if json.Unmarshal([]byte(v), &rec) == nil {
			out = append(out, rec)
		}
	}
	return out, nil
}

// LoadPhonebook returns the account's saved contacts.
func (r *redisStore) LoadPhonebook(ctx context.Context, account string) ([]contact, error) {
	v, err := r.client.Get(ctx, phonebookPrefix+strings.ToLower(account)).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []contact
	_ = json.Unmarshal([]byte(v), &out)
	return out, nil
}

// SavePhonebook replaces the account's contacts (capped).
func (r *redisStore) SavePhonebook(ctx context.Context, account string, items []contact) error {
	if len(items) > phonebookMax {
		items = items[:phonebookMax]
	}
	data, err := json.Marshal(items)
	if err != nil {
		return err
	}
	return r.client.Set(ctx, phonebookPrefix+strings.ToLower(account), data, 0).Err()
}

func (r *redisStore) LoadTrunks(ctx context.Context) ([]Trunk, error) {
	return loadHash[Trunk](ctx, r, trunksKey)
}

func (r *redisStore) SaveTrunk(ctx context.Context, t Trunk) error {
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return r.client.HSet(ctx, trunksKey, t.ID, data).Err()
}

func (r *redisStore) DeleteTrunk(ctx context.Context, id string) error {
	return r.client.HDel(ctx, trunksKey, id).Err()
}
