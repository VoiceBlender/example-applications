package main

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// Per-session role within a talk burst.
const (
	roleNone     = ""
	roleSpeaker  = "speaker"
	roleListener = "listener"
)

// pttSession is one signed-in browser connected to a room's signalling
// WebSocket. It is the live presence + media handle for that browser; nothing
// here is persisted.
type pttSession struct {
	username string
	roomID   string
	outbox   chan any // → this browser's WS writer

	mu       sync.Mutex
	legID    string    // WebRTC media leg for the current burst ("" when idle)
	role     string    // roleNone | roleSpeaker | roleListener for the current burst
	lastRing time.Time // last channel alert sent (rate limit)
}

// ringCooldown is the minimum gap between one session's channel alerts. The
// alert is deliberately loud and interrupts everyone, so it must not be
// spammable.
const ringCooldown = 5 * time.Second

// takeRing reports whether this session may ring the channel now, recording the
// attempt when it may. Returns the wait remaining when it may not.
func (s *pttSession) takeRing(now time.Time) (bool, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.lastRing.IsZero() {
		if wait := ringCooldown - now.Sub(s.lastRing); wait > 0 {
			return false, wait
		}
	}
	s.lastRing = now
	return true, 0
}

func (s *pttSession) setLeg(legID string) {
	s.mu.Lock()
	s.legID = legID
	s.mu.Unlock()
}

func (s *pttSession) leg() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.legID
}

func (s *pttSession) setRole(role string) {
	s.mu.Lock()
	s.role = role
	s.mu.Unlock()
}

func (s *pttSession) getRole() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.role
}

// clearBurst resets the per-burst media state and returns the leg id that was
// set (so the caller can tear it down).
func (s *pttSession) clearBurst() string {
	s.mu.Lock()
	leg := s.legID
	s.legID = ""
	s.role = roleNone
	s.mu.Unlock()
	return leg
}

// send pushes a message to the browser, dropping it if the WS writer has stalled
// (never blocks a caller holding a lock).
func (s *pttSession) send(msg any) {
	select {
	case s.outbox <- msg:
	default:
	}
}

// presenceRegistry is the live, in-memory index of connected sessions. It maps
// a room to the sessions currently in it (for burst fan-out and the presence
// list) and a media leg back to its session (for LegDisconnected cleanup).
type presenceRegistry struct {
	mu     sync.Mutex
	byRoom map[string]map[*pttSession]bool
	byLeg  map[string]*pttSession
}

func newPresenceRegistry() *presenceRegistry {
	return &presenceRegistry{
		byRoom: make(map[string]map[*pttSession]bool),
		byLeg:  make(map[string]*pttSession),
	}
}

func (p *presenceRegistry) add(s *pttSession) {
	p.mu.Lock()
	set := p.byRoom[s.roomID]
	if set == nil {
		set = make(map[*pttSession]bool)
		p.byRoom[s.roomID] = set
	}
	set[s] = true
	p.mu.Unlock()
}

func (p *presenceRegistry) remove(s *pttSession) {
	p.mu.Lock()
	if set := p.byRoom[s.roomID]; set != nil {
		delete(set, s)
		if len(set) == 0 {
			delete(p.byRoom, s.roomID)
		}
	}
	if leg := s.leg(); leg != "" {
		delete(p.byLeg, leg)
	}
	p.mu.Unlock()
}

// bindLeg records the session's current media leg for reverse lookup.
func (p *presenceRegistry) bindLeg(s *pttSession, legID string) {
	p.mu.Lock()
	if old := s.leg(); old != "" {
		delete(p.byLeg, old)
	}
	s.setLeg(legID)
	if legID != "" {
		p.byLeg[legID] = s
	}
	p.mu.Unlock()
}

// unbindLeg drops the leg → session mapping and clears it on the session.
func (p *presenceRegistry) unbindLeg(s *pttSession) {
	p.mu.Lock()
	if leg := s.leg(); leg != "" {
		delete(p.byLeg, leg)
	}
	s.setLeg("")
	p.mu.Unlock()
}

func (p *presenceRegistry) sessionForLeg(legID string) (*pttSession, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.byLeg[legID]
	return s, ok
}

// membersInRoom returns the sessions currently in a room.
func (p *presenceRegistry) membersInRoom(roomID string) []*pttSession {
	p.mu.Lock()
	defer p.mu.Unlock()
	set := p.byRoom[roomID]
	out := make([]*pttSession, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	return out
}

// usernamesInRoom returns the distinct usernames present in a room, sorted.
func (p *presenceRegistry) usernamesInRoom(roomID string) []string {
	p.mu.Lock()
	set := p.byRoom[roomID]
	seen := make(map[string]bool, len(set))
	for s := range set {
		seen[s.username] = true
	}
	p.mu.Unlock()
	out := make([]string, 0, len(seen))
	for u := range seen {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out
}

// userPresent reports whether username has any live session in the room. Used to
// fire a join/leave activity event only on the user's first/last session, so
// multiple tabs don't spam the log.
func (p *presenceRegistry) userPresent(roomID, username string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for s := range p.byRoom[roomID] {
		if s.username == username {
			return true
		}
	}
	return false
}

// countInRoom returns the number of sessions present in a room.
func (p *presenceRegistry) countInRoom(roomID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.byRoom[roomID])
}
