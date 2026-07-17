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

// Room visibility.
const (
	visibilityPublic  = "public"
	visibilityPrivate = "private"
)

// Roger (courtesy) tone styles played at the end of a transmission — the beep a
// radio sends when the operator releases PTT. "off" disables it. The tone is
// synthesized in the browser (see web/static/roger.js, which holds the exact
// frequency/duration of each); the room only stores which style to use, so this
// list must stay in step with the ROGER table there.
const defaultRoger = "classic"

var rogerStyles = map[string]bool{
	"off": true,

	"classic": true, // repeater "Beep" — 880 Hz, 100 ms
	"boop":    true, // repeater "Boop" — 440 Hz, 100 ms
	"honk":    true, // repeater "Honk" — 500+700 Hz, 100 ms

	"quindar": true, // Apollo outro tone — 2475 Hz, 250 ms
	"nasa":    true, // NASA "over" beep — 2450 Hz, 200 ms

	"cuckoo": true, // Baofeng — 1123 Hz/136 ms then 865 Hz/202 ms
	"nextel": true, // 1760 Hz triple chirp, 30 ms on / 30 ms off

	"bumblebee":    true, // 330 / 495 / 660 Hz, 100 ms each
	"piano":        true, // "Piano Chord" — 660+880 Hz, 100 ms
	"shootingstar": true, // 800 / 800 / 540 Hz, 100 ms each
	"stardust":     true, // 750 Hz/125 ms, 880 Hz/80 ms, 880+1200 Hz/80 ms
	"wasp":         true, // 660 / 500 / 385 Hz, 100 ms each
	"tumbleweed":   true, // 1000 / 800 / 600 Hz, 20 ms each
	"doorbell":     true, // 800 Hz/75 ms then 400 Hz/50 ms
	"chirp":        true, // "Ascending" — 500 / 750 / 1000 Hz, 50 ms each
	"descending":   true, // 1000 / 750 / 500 Hz, 50 ms each
}

// normRoger clamps a roger style to a known value, defaulting unknown/empty
// input to defaultRoger.
func normRoger(s string) string {
	if rogerStyles[s] {
		return s
	}
	return defaultRoger
}

// Comms-radio band-limit band (Hz) applied when a channel's "Radio bandwidth"
// option is enabled at creation.
const (
	radioLow  = 500
	radioHigh = 2000
)

var (
	errRoomNotFound = errors.New("room not found")
	errBadName      = errors.New("channel name is required")
)

// Room is a push-to-talk channel. Public rooms are joinable by anyone; private
// rooms are joinable only by their owner or by users who have redeemed the
// invite code (tracked in the member set).
type Room struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Owner      string `json:"owner"`
	Visibility string `json:"visibility"` // public | private
	InviteCode string `json:"invite_code"`
	Roger      string `json:"roger"`      // end-of-transmission courtesy tone style
	CreatedAt  string `json:"created_at"` // RFC3339

	// Comms-radio band-limit filter: when Radio is on, each listener's browser
	// band-passes incoming audio to [RadioLow, RadioHigh] Hz so voices sound like
	// they're coming over the air (see web/static/channel.js).
	Radio     bool `json:"radio,omitempty"`
	RadioLow  int  `json:"radio_low,omitempty"`
	RadioHigh int  `json:"radio_high,omitempty"`
}

// roomView is the lobby's per-room JSON shape (adds live listener count and
// whether the viewer owns it).
type roomView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Owner      string `json:"owner"`
	Visibility string `json:"visibility"`
	Present    int    `json:"present"`
	Owned      bool   `json:"owned"`
}

// roomRegistry is the durable room store with an in-memory index and a
// per-room member allowlist for private rooms.
type roomRegistry struct {
	mu      sync.Mutex
	rooms   map[string]Room            // id → room
	members map[string]map[string]bool // id → set of admitted usernames (lower)
	store   *redisStore
	notify  func() // = a.notifyChanged (lobby changed)
}

func newRoomRegistry(store *redisStore) *roomRegistry {
	return &roomRegistry{
		rooms:   make(map[string]Room),
		members: make(map[string]map[string]bool),
		store:   store,
	}
}

// load restores rooms and their member sets from Redis.
func (r *roomRegistry) load(ctx context.Context) error {
	rooms, err := r.store.LoadRooms(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, room := range rooms {
		r.rooms[room.ID] = room
		set := make(map[string]bool)
		if names, err := r.store.LoadMembers(ctx, room.ID); err == nil {
			for _, n := range names {
				set[n] = true
			}
		}
		r.members[room.ID] = set
	}
	return nil
}

func (r *roomRegistry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.rooms)
}

// create makes and persists a new room owned by owner. radio enables the
// comms-radio band-limit filter (fixed 500–2000 Hz) for the channel.
func (r *roomRegistry) create(ctx context.Context, name, owner, visibility, roger string, radio bool) (Room, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Room{}, errBadName
	}
	if visibility != visibilityPrivate {
		visibility = visibilityPublic
	}
	room := Room{
		ID:         randID(12),
		Name:       name,
		Owner:      owner,
		Visibility: visibility,
		InviteCode: randID(8),
		Roger:      normRoger(roger),
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
		Radio:      radio,
	}
	if radio {
		room.RadioLow, room.RadioHigh = radioLow, radioHigh
	}
	if err := r.store.SaveRoom(ctx, room); err != nil {
		return Room{}, err
	}
	r.mu.Lock()
	r.rooms[room.ID] = room
	r.members[room.ID] = map[string]bool{strings.ToLower(owner): true}
	r.mu.Unlock()
	r.changed()
	return room, nil
}

func (r *roomRegistry) get(id string) (Room, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	room, ok := r.rooms[id]
	return room, ok
}

// setRoger updates a room's courtesy-tone style and persists it.
func (r *roomRegistry) setRoger(ctx context.Context, id, roger string) (Room, error) {
	r.mu.Lock()
	room, ok := r.rooms[id]
	if !ok {
		r.mu.Unlock()
		return Room{}, errRoomNotFound
	}
	room.Roger = normRoger(roger)
	r.rooms[id] = room
	r.mu.Unlock()
	if err := r.store.SaveRoom(ctx, room); err != nil {
		return Room{}, err
	}
	return room, nil
}

// delete removes a room (owner-only enforced by the caller).
func (r *roomRegistry) delete(ctx context.Context, id string) error {
	r.mu.Lock()
	room, ok := r.rooms[id]
	if !ok {
		r.mu.Unlock()
		return errRoomNotFound
	}
	delete(r.rooms, id)
	delete(r.members, id)
	r.mu.Unlock()
	if err := r.store.DeleteRoom(ctx, room); err != nil {
		return err
	}
	r.changed()
	return nil
}

// joinByCode resolves an invite code to its room and admits the user to it.
func (r *roomRegistry) joinByCode(ctx context.Context, code, username string) (Room, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return Room{}, errRoomNotFound
	}
	id, err := r.store.RoomIDByInvite(ctx, code)
	if err != nil {
		return Room{}, err
	}
	if id == "" {
		return Room{}, errRoomNotFound
	}
	room, ok := r.get(id)
	if !ok {
		return Room{}, errRoomNotFound
	}
	if err := r.admit(ctx, room.ID, username); err != nil {
		return Room{}, err
	}
	return room, nil
}

// admit adds a user to a room's member allowlist (idempotent).
func (r *roomRegistry) admit(ctx context.Context, roomID, username string) error {
	if err := r.store.AddMember(ctx, roomID, username); err != nil {
		return err
	}
	r.mu.Lock()
	set := r.members[roomID]
	if set == nil {
		set = make(map[string]bool)
		r.members[roomID] = set
	}
	set[strings.ToLower(username)] = true
	r.mu.Unlock()
	return nil
}

// canAccess reports whether username may enter the room: public rooms are open;
// private rooms require ownership or membership.
func (r *roomRegistry) canAccess(room Room, username string) bool {
	if room.Visibility == visibilityPublic {
		return true
	}
	if strings.EqualFold(room.Owner, username) {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.members[room.ID][strings.ToLower(username)]
}

// visibleTo returns the rooms username should see in the lobby: all public
// rooms, plus any private rooms they own or have been admitted to. present is a
// func returning the live headcount for a room id.
func (r *roomRegistry) visibleTo(username string, present func(string) int) []roomView {
	r.mu.Lock()
	rooms := make([]Room, 0, len(r.rooms))
	for _, room := range r.rooms {
		rooms = append(rooms, room)
	}
	membersByRoom := make(map[string]bool, len(r.rooms))
	for id, set := range r.members {
		membersByRoom[id] = set[strings.ToLower(username)]
	}
	r.mu.Unlock()

	out := make([]roomView, 0, len(rooms))
	for _, room := range rooms {
		owned := strings.EqualFold(room.Owner, username)
		if room.Visibility == visibilityPrivate && !owned && !membersByRoom[room.ID] {
			continue
		}
		out = append(out, roomView{
			ID:         room.ID,
			Name:       room.Name,
			Owner:      room.Owner,
			Visibility: room.Visibility,
			Present:    present(room.ID),
			Owned:      owned,
		})
	}
	// Stable order: most-populated first, then name.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Present != out[j].Present {
			return out[i].Present > out[j].Present
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

func (r *roomRegistry) changed() {
	if r.notify != nil {
		r.notify()
	}
}

// randID returns n hex characters of cryptographic randomness.
func randID(n int) string {
	buf := make([]byte, (n+1)/2)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return hex.EncodeToString(buf)[:n]
}
