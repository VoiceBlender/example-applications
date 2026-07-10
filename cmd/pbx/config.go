package main

import (
	"context"
	"strings"
	"sync"
)

// Config holds global, operator-editable PBX settings.
type Config struct {
	HoldMusicURL string `json:"hold_music_url"`
}

// configStore is the per-tenant view of config, persisted to Redis and fanned
// out to the console on change. Tenant configs are loaded lazily on first use.
type configStore struct {
	mu         sync.RWMutex
	byTenant   map[string]Config
	envDefault string // global fallback hold-music URL (HOLD_MUSIC_URL)
	store      *redisStore
	notify     func()
}

func newConfigStore(store *redisStore) *configStore {
	return &configStore{byTenant: make(map[string]Config), store: store}
}

// setDefault records the global fallback hold-music URL applied to any tenant
// that hasn't set its own.
func (c *configStore) setDefault(envDefault string) {
	c.envDefault = strings.TrimSpace(envDefault)
}

// get returns a tenant's config, lazily loading it from Redis (with the env
// fallback) on first access.
func (c *configStore) get(tenantID string) Config {
	c.mu.RLock()
	cfg, ok := c.byTenant[tenantID]
	c.mu.RUnlock()
	if ok {
		return cfg
	}
	cfg, _ = c.store.LoadConfig(context.Background(), tenantID)
	if strings.TrimSpace(cfg.HoldMusicURL) == "" {
		cfg.HoldMusicURL = c.envDefault
	}
	c.mu.Lock()
	c.byTenant[tenantID] = cfg
	c.mu.Unlock()
	return cfg
}

// holdMusicURL returns a tenant's on-hold music URL, or "" if unset.
func (c *configStore) holdMusicURL(tenantID string) string {
	return strings.TrimSpace(c.get(tenantID).HoldMusicURL)
}

// forget drops a tenant's config from Redis and the in-memory cache.
func (c *configStore) forget(ctx context.Context, tenantID string) {
	_ = c.store.DeleteConfig(ctx, tenantID)
	c.mu.Lock()
	delete(c.byTenant, tenantID)
	c.mu.Unlock()
}

// update persists and applies a tenant's config.
func (c *configStore) update(ctx context.Context, tenantID string, cfg Config) error {
	cfg.HoldMusicURL = strings.TrimSpace(cfg.HoldMusicURL)
	if err := c.store.SaveConfig(ctx, tenantID, cfg); err != nil {
		return err
	}
	c.mu.Lock()
	c.byTenant[tenantID] = cfg
	c.mu.Unlock()
	if c.notify != nil {
		c.notify()
	}
	return nil
}
