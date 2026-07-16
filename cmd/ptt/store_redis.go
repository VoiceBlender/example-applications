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
	usersKey    = "ptt:users"    // hash: lower-case username → JSON userRecord
	sessionsKey = "ptt:sessions" // hash: token → sessionRecord
	roomsKey    = "ptt:rooms"    // hash: room id → JSON Room
	// invitePrefix + <code> → room id (reverse lookup for join-by-code).
	invitePrefix = "ptt:invite:"
	// membersPrefix + <room id> is a set of usernames admitted to a private room.
	membersPrefix = "ptt:members:"
	// watchPrefix + <lower username> → JSON array of the channel ids that user
	// watches, so their monitored set follows them across logins and devices.
	watchPrefix = "ptt:watch:"
	// eventsPrefix + <room id> is a capped list (newest first) of channelEvents.
	eventsPrefix = "ptt:events:"
	// maxEvents caps a room's stored activity log.
	maxEvents = 50
)

// sessionRecord is a persisted browser session.
type sessionRecord struct {
	Token    string    `json:"token"`
	Username string    `json:"username"`
	Expiry   time.Time `json:"expiry"`
}

// userRecord is a registered account. The password is stored only as a bcrypt
// hash (salt + cost baked into the string) — never in plaintext. Username keeps
// its original case for display; the hash is keyed by the lower-cased name.
type userRecord struct {
	Username     string    `json:"username"`
	PasswordHash string    `json:"password_hash"`
	CreatedAt    time.Time `json:"created_at"`
}

// channelEvent is one line in a room's activity log: who did what, and when.
// Kind is one of "join", "leave", "talk", "ring". Name is the room's display
// name at the time, so the combined feed can label events without a lookup.
type channelEvent struct {
	Room  string `json:"room"`
	Name  string `json:"name"`
	Actor string `json:"actor"`
	Kind  string `json:"kind"`
	At    string `json:"at"` // RFC3339Nano
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

// GetUser returns the account for username (case-insensitive). ok=false if no
// such account exists yet.
func (r *redisStore) GetUser(ctx context.Context, username string) (userRecord, bool, error) {
	raw, err := r.client.HGet(ctx, usersKey, strings.ToLower(username)).Result()
	if err == redis.Nil {
		return userRecord{}, false, nil
	}
	if err != nil {
		return userRecord{}, false, err
	}
	var rec userRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return userRecord{}, false, err
	}
	return rec, true, nil
}

// CreateUser registers a new account, storing only its bcrypt hash. It uses
// HSETNX so it is atomic: created=false means the username was already taken
// (e.g. two first-logins racing), and the caller should fall back to verifying.
func (r *redisStore) CreateUser(ctx context.Context, rec userRecord) (bool, error) {
	data, err := json.Marshal(rec)
	if err != nil {
		return false, err
	}
	return r.client.HSetNX(ctx, usersKey, strings.ToLower(rec.Username), data).Result()
}

// ── watched channels (per-user monitor set) ──────────────────────────────────

// SaveWatched replaces the ordered list of channel ids a user watches.
func (r *redisStore) SaveWatched(ctx context.Context, username string, ids []string) error {
	if ids == nil {
		ids = []string{}
	}
	data, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	return r.client.Set(ctx, watchPrefix+strings.ToLower(username), data, 0).Err()
}

// LoadWatched returns the channel ids a user watches (empty if none saved).
func (r *redisStore) LoadWatched(ctx context.Context, username string) ([]string, error) {
	raw, err := r.client.Get(ctx, watchPrefix+strings.ToLower(username)).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil, err
	}
	return ids, nil
}

// ── channel activity log ─────────────────────────────────────────────────────

// AppendEvent records an activity event, capping the room's log at maxEvents
// (newest first).
func (r *redisStore) AppendEvent(ctx context.Context, ev channelEvent) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	key := eventsPrefix + ev.Room
	pipe := r.client.TxPipeline()
	pipe.LPush(ctx, key, data)
	pipe.LTrim(ctx, key, 0, maxEvents-1)
	_, err = pipe.Exec(ctx)
	return err
}

// LoadEvents returns up to limit of a room's most recent events, newest first.
func (r *redisStore) LoadEvents(ctx context.Context, roomID string, limit int) ([]channelEvent, error) {
	if limit <= 0 || limit > maxEvents {
		limit = maxEvents
	}
	raw, err := r.client.LRange(ctx, eventsPrefix+roomID, 0, int64(limit-1)).Result()
	if err != nil {
		return nil, err
	}
	out := make([]channelEvent, 0, len(raw))
	for _, v := range raw {
		var ev channelEvent
		if json.Unmarshal([]byte(v), &ev) == nil {
			out = append(out, ev)
		}
	}
	return out, nil
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
	pipe.Del(ctx, eventsPrefix+room.ID)
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
