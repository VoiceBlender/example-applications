package main

import (
	"strings"
	"sync"
)

// lower is strings.ToLower, aliased for terse map-key normalisation.
func lower(s string) string { return strings.ToLower(s) }

// A WebRTC softphone is an extra "device" for an extension: a browser signs in
// with a WebRTCAccount credential, establishes a persistent WebRTC media leg,
// and from then on rings alongside the extension's SIP registration(s) and can
// place calls as that extension. The leg is created once at sign-in and reused
// for every call — it is *detached* from a call's room when the call ends, never
// deleted, so it survives for the next call (deleted only on sign-out / WS drop).
//
// This registry is the live, in-memory presence layer (never persisted). The
// account credentials live on the Extension in Redis; only the ephemeral
// leg/session state lives here.

// Phone session states.
const (
	phoneIdle    = "idle"
	phoneRinging = "ringing"
	phoneInCall  = "in-call"
)

// extKey composes the tenant-scoped key for byExtNum (numbers repeat across
// tenants, so a bare number is ambiguous).
func extKey(tenantID, number string) string { return tenantID + "\x00" + number }

// phoneSession is one signed-in softphone browser.
type phoneSession struct {
	account   string   // WebRTC account username (the softphone login, globally unique)
	extNumber string   // parent extension number it acts as (tenant-scoped)
	tenantID  string   // owning tenant
	outbox    chan any // → this browser's phone WS

	mu       sync.Mutex
	legID    string // persistent WebRTC media leg ("" until the browser offers)
	state    string // phoneIdle | phoneRinging | phoneInCall
	forkRoom string // room the leg is currently mixed into (for detach); "" when idle
	ringFrom string // caller number of the current incoming ring (for call history)
	dnd      bool   // do-not-disturb: excluded from incoming-call ring fan-out
}

func (s *phoneSession) setDnd(on bool) {
	s.mu.Lock()
	s.dnd = on
	s.mu.Unlock()
}

func (s *phoneSession) isDnd() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dnd
}

// setRing records the caller number of an incoming ring (for history).
func (s *phoneSession) setRing(from string) {
	s.mu.Lock()
	s.ringFrom = from
	s.mu.Unlock()
}

// takeRing returns and clears the pending incoming-ring caller number.
func (s *phoneSession) takeRing() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	from := s.ringFrom
	s.ringFrom = ""
	return from
}

func (s *phoneSession) setLeg(legID string) {
	s.mu.Lock()
	s.legID = legID
	s.mu.Unlock()
}

func (s *phoneSession) leg() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.legID
}

func (s *phoneSession) setState(state, room string) {
	s.mu.Lock()
	s.state = state
	s.forkRoom = room
	s.mu.Unlock()
}

func (s *phoneSession) setIdle() {
	s.setState(phoneIdle, "")
}

func (s *phoneSession) snapshot() (state, room string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, s.forkRoom
}

// send pushes a message to the browser, dropping it if the WS writer has stalled
// (never blocks a caller holding a lock).
func (s *phoneSession) send(msg any) {
	select {
	case s.outbox <- msg:
	default:
	}
}

// phoneRegistry indexes live softphone sessions three ways: by account (one
// session per login), by leg (the O(1) "is this leg a live softphone?" lookup
// every call-teardown site needs), and by extension number (fork fan-out — every
// device that should ring when the extension is dialed).
type phoneRegistry struct {
	mu       sync.Mutex
	byAcct   map[string]*phoneSession   // account username (lower) → session
	byLeg    map[string]*phoneSession   // legID → session
	byExtNum map[string][]*phoneSession // extKey(tenant,number) → sessions
	notify   func()                     // = a.notifyChanged (presence changed)
}

func newPhoneRegistry() *phoneRegistry {
	return &phoneRegistry{
		byAcct:   make(map[string]*phoneSession),
		byLeg:    make(map[string]*phoneSession),
		byExtNum: make(map[string][]*phoneSession),
	}
}

// add registers a freshly signed-in session (no leg yet). If the same account is
// already signed in elsewhere, the previous session is returned so the caller
// can tear down its leg (a second sign-in replaces the first).
func (r *phoneRegistry) add(s *phoneSession) *phoneSession {
	key := lower(s.account)
	r.mu.Lock()
	prev := r.byAcct[key]
	if prev != nil {
		r.removeLocked(prev)
	}
	r.byAcct[key] = s
	ek := extKey(s.tenantID, s.extNumber)
	r.byExtNum[ek] = append(r.byExtNum[ek], s)
	r.mu.Unlock()
	r.changed()
	return prev
}

// bindLeg records the session's WebRTC leg id once the browser has offered.
func (r *phoneRegistry) bindLeg(s *phoneSession, legID string) {
	r.mu.Lock()
	if old := s.leg(); old != "" {
		delete(r.byLeg, old)
	}
	s.setLeg(legID)
	if legID != "" {
		r.byLeg[legID] = s
	}
	r.mu.Unlock()
	r.changed()
}

// remove drops a session from every index (sign-out / WS close).
func (r *phoneRegistry) remove(s *phoneSession) {
	r.mu.Lock()
	r.removeLocked(s)
	r.mu.Unlock()
	r.changed()
}

func (r *phoneRegistry) removeLocked(s *phoneSession) {
	if r.byAcct[lower(s.account)] == s {
		delete(r.byAcct, lower(s.account))
	}
	if leg := s.leg(); leg != "" {
		delete(r.byLeg, leg)
	}
	ek := extKey(s.tenantID, s.extNumber)
	list := r.byExtNum[ek]
	kept := list[:0]
	for _, e := range list {
		if e != s {
			kept = append(kept, e)
		}
	}
	if len(kept) == 0 {
		delete(r.byExtNum, ek)
	} else {
		r.byExtNum[ek] = kept
	}
}

// sessionForLeg returns the softphone session owning legID, if any. This is the
// hot path every call-teardown site uses to decide detach-vs-hangup.
func (r *phoneRegistry) sessionForLeg(legID string) (*phoneSession, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.byLeg[legID]
	return s, ok
}

func (r *phoneRegistry) isWebRTCLeg(legID string) bool {
	_, ok := r.sessionForLeg(legID)
	return ok
}

// liveForExt returns the signed-in, idle sessions that currently have a live leg
// for the given extension number (candidates for an incoming call). Busy devices
// (ringing / in a call) are skipped so a second call doesn't disturb an active
// one. Do-not-disturb devices ARE returned — the fork rings them as candidates
// but declines them immediately (like a SIP phone), so a DND-only extension
// returns busy while any non-DND device still rings.
func (r *phoneRegistry) liveForExt(tenantID, extNumber string) []*phoneSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*phoneSession
	for _, s := range r.byExtNum[extKey(tenantID, extNumber)] {
		if st, _ := s.snapshot(); s.leg() != "" && st == phoneIdle {
			out = append(out, s)
		}
	}
	return out
}

// status reports a WebRTC account's live presence for the console (extView).
func (r *phoneRegistry) status(account string) (online bool, state string) {
	r.mu.Lock()
	s := r.byAcct[lower(account)]
	r.mu.Unlock()
	if s == nil || s.leg() == "" {
		return false, ""
	}
	st, _ := s.snapshot()
	if s.isDnd() && st == phoneIdle {
		st = "dnd"
	}
	return true, st
}

func (r *phoneRegistry) changed() {
	if r.notify != nil {
		r.notify()
	}
}
