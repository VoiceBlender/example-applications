package main

import (
	"context"
	"sync"
)

// dialplanStore is the per-tenant view of the inbound dial-plan graph, persisted
// to Redis and fanned out to the console on change. Mirrors configStore; each
// tenant's graph is loaded lazily and seeded with a default (start → ivr) on
// first access so a fresh tenant routes inbound trunk calls to the IVR.
type dialplanStore struct {
	mu       sync.RWMutex
	byTenant map[string]DPGraph
	store    *redisStore
	notify   func()
}

func newDialplanStore(store *redisStore) *dialplanStore {
	return &dialplanStore{byTenant: make(map[string]DPGraph), store: store}
}

func (d *dialplanStore) get(tenantID string) DPGraph {
	d.mu.RLock()
	g, ok := d.byTenant[tenantID]
	d.mu.RUnlock()
	if ok {
		return g
	}
	g, stored, err := d.store.LoadDialplan(context.Background(), tenantID)
	if err != nil || !stored {
		g = defaultDialplan()
		_ = d.store.SaveDialplan(context.Background(), tenantID, g)
	}
	d.mu.Lock()
	d.byTenant[tenantID] = g
	d.mu.Unlock()
	return g
}

// forget drops a tenant's dial plan from Redis and the in-memory cache.
func (d *dialplanStore) forget(ctx context.Context, tenantID string) {
	_ = d.store.DeleteDialplan(ctx, tenantID)
	d.mu.Lock()
	delete(d.byTenant, tenantID)
	d.mu.Unlock()
}

func (d *dialplanStore) update(ctx context.Context, tenantID string, g DPGraph) error {
	if err := d.store.SaveDialplan(ctx, tenantID, g); err != nil {
		return err
	}
	d.mu.Lock()
	d.byTenant[tenantID] = g
	d.mu.Unlock()
	if d.notify != nil {
		d.notify()
	}
	return nil
}
