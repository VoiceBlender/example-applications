package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// silentRedisLogger discards everything; we route errors through our slog
// at the surface where we can format them properly.
type silentRedisLogger struct{}

func (silentRedisLogger) Printf(_ context.Context, _ string, _ ...any) {}

func init() {
	redis.SetLogger(silentRedisLogger{})
}

// redisStore implements LogStore against any Redis-compatible server
// (Redis, Valkey, KeyDB, Dragonfly…). Entries live in a single Redis list:
// RPUSH on append, LTRIM to cap, LRANGE on read.
type redisStore struct {
	client *redis.Client
	key    string
	cap    int
}

// newRedisStore parses a redis:// or rediss:// URL, dials, runs a PING to
// fail fast on misconfiguration, and returns a ready store.
func newRedisStore(ctx context.Context, url, key string, capacity int) (*redisStore, error) {
	if key == "" {
		key = "contactcentre:call_log"
	}
	if capacity <= 0 {
		capacity = 200
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	// Fail fast on startup: don't let the pool retry 5× before reporting
	// that the configured server is unreachable.
	opt.MaxRetries = -1
	c := redis.NewClient(opt)
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := c.Ping(pingCtx).Err(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return &redisStore{client: c, key: key, cap: capacity}, nil
}

func (r *redisStore) Append(ctx context.Context, e LogEntry) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	// Pipeline RPUSH + LTRIM so the cap is enforced atomically per call.
	pipe := r.client.TxPipeline()
	pipe.RPush(ctx, r.key, data)
	pipe.LTrim(ctx, r.key, int64(-r.cap), -1)
	_, err = pipe.Exec(ctx)
	return err
}

func (r *redisStore) List(ctx context.Context, limit int) ([]LogEntry, error) {
	// LRANGE returns oldest→newest; we want newest first and at most limit.
	// Read the tail of the list (the cap-most-recent), then reverse + trim.
	start := int64(0)
	if limit > 0 {
		start = int64(-limit)
	} else {
		start = int64(-r.cap)
	}
	raws, err := r.client.LRange(ctx, r.key, start, -1).Result()
	if err != nil {
		return nil, err
	}
	out := make([]LogEntry, 0, len(raws))
	for i := len(raws) - 1; i >= 0; i-- {
		var e LogEntry
		if err := json.Unmarshal([]byte(raws[i]), &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func (r *redisStore) Close() error { return r.client.Close() }
