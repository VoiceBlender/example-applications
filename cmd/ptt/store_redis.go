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

// Redis keys.
const (
	usersKey    = "ptt:users"    // set: claimed usernames (lower-case)
	sessionsKey = "ptt:sessions" // hash: token → sessionRecord
	roomsKey    = "ptt:rooms"    // hash: room id → JSON Room
	// invitePrefix + <code> → room id (reverse lookup for join-by-code).
	invitePrefix = "ptt:invite:"
	// membersPrefix + <room id> is a set of usernames admitted to a private room.
	membersPrefix = "ptt:members:"
)

// sessionRecord is a persisted browser session.
type sessionRecord struct {
	Token    string    `json:"token"`
	Username string    `json:"username"`
	Expiry   time.Time `json:"expiry"`
}

// redisStore is the durable backing store. It's a thin wrapper over a go-redis
// client; all business logic lives in the registries that call it.
type redisStore struct {
	client *redis.Client
}

// newRedisStore parses a redis:// or rediss:// URL, dials, and PINGs to fail
// fast on misconfiguration.
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

// ── users ────────────────────────────────────────────────────────────────────

// ClaimUser records a username (idempotent). Returns whether it was newly added.
func (r *redisStore) ClaimUser(ctx context.Context, username string) (bool, error) {
	n, err := r.client.SAdd(ctx, usersKey, strings.ToLower(username)).Result()
	return n > 0, err
}

// ── sessions ─────────────────────────────────────────────────────────────────

func (r *redisStore) LoadSessions(ctx context.Context) ([]sessionRecord, error) {
	raw, err := r.client.HGetAll(ctx, sessionsKey).Result()
	if err != nil {
		return nil, err
	}
	out := make([]sessionRecord, 0, len(raw))
	for _, v := range raw {
		var rec sessionRecord
		if json.Unmarshal([]byte(v), &rec) == nil {
			out = append(out, rec)
		}
	}
	return out, nil
}

func (r *redisStore) SaveSession(ctx context.Context, rec sessionRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return r.client.HSet(ctx, sessionsKey, rec.Token, data).Err()
}

func (r *redisStore) DeleteSession(ctx context.Context, token string) error {
	return r.client.HDel(ctx, sessionsKey, token).Err()
}

// ── rooms ────────────────────────────────────────────────────────────────────

func (r *redisStore) LoadRooms(ctx context.Context) ([]Room, error) {
	raw, err := r.client.HGetAll(ctx, roomsKey).Result()
	if err != nil {
		return nil, err
	}
	out := make([]Room, 0, len(raw))
	for _, v := range raw {
		var room Room
		if json.Unmarshal([]byte(v), &room) == nil {
			out = append(out, room)
		}
	}
	return out, nil
}

// SaveRoom persists the room and its invite-code reverse index atomically.
func (r *redisStore) SaveRoom(ctx context.Context, room Room) error {
	data, err := json.Marshal(room)
	if err != nil {
		return err
	}
	pipe := r.client.TxPipeline()
	pipe.HSet(ctx, roomsKey, room.ID, data)
	if room.InviteCode != "" {
		pipe.Set(ctx, invitePrefix+room.InviteCode, room.ID, 0)
	}
	_, err = pipe.Exec(ctx)
	return err
}

// DeleteRoom removes the room, its invite index, and its member set.
func (r *redisStore) DeleteRoom(ctx context.Context, room Room) error {
	pipe := r.client.TxPipeline()
	pipe.HDel(ctx, roomsKey, room.ID)
	if room.InviteCode != "" {
		pipe.Del(ctx, invitePrefix+room.InviteCode)
	}
	pipe.Del(ctx, membersPrefix+room.ID)
	_, err := pipe.Exec(ctx)
	return err
}

// RoomIDByInvite resolves an invite code to a room id ("" if unknown).
func (r *redisStore) RoomIDByInvite(ctx context.Context, code string) (string, error) {
	id, err := r.client.Get(ctx, invitePrefix+code).Result()
	if err == redis.Nil {
		return "", nil
	}
	return id, err
}

// ── room membership (private-room allowlist) ─────────────────────────────────

func (r *redisStore) AddMember(ctx context.Context, roomID, username string) error {
	return r.client.SAdd(ctx, membersPrefix+roomID, strings.ToLower(username)).Err()
}

func (r *redisStore) LoadMembers(ctx context.Context, roomID string) ([]string, error) {
	return r.client.SMembers(ctx, membersPrefix+roomID).Result()
}
