package main

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
	"time"
)

// agent is one logged-in operator. Sessions live only in memory and vanish
// when the process restarts — fine for an example app.
type agent struct {
	ID          string    `json:"agent_id"`
	Name        string    `json:"name"`
	LoggedInAt  time.Time `json:"logged_in_at"`
	WebRTCLegID string    `json:"webrtc_leg_id,omitempty"`
}

type agentRegistry struct {
	mu     sync.Mutex
	by     map[string]*agent
	notify func() // called after every mutation; lets subscribers refresh
}

func newAgentRegistry() *agentRegistry {
	return &agentRegistry{by: make(map[string]*agent)}
}

// onChange installs a callback fired after every login/logout/leg change.
// The supervisor WS uses this to push a fresh snapshot when agents come and go.
func (r *agentRegistry) onChange(fn func()) { r.notify = fn }

func (r *agentRegistry) fireNotify() {
	if r.notify != nil {
		r.notify()
	}
}

// list returns a snapshot of every logged-in agent, sorted oldest-login first.
func (r *agentRegistry) list() []agent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]agent, 0, len(r.by))
	for _, a := range r.by {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LoggedInAt.Before(out[j].LoggedInAt) })
	return out
}

// login creates a new session for the given name. Whitespace is trimmed; an
// empty name yields (nil, false).
func (r *agentRegistry) login(name string) (*agent, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, false
	}
	id, err := newAgentID()
	if err != nil {
		return nil, false
	}
	a := &agent{
		ID:         id,
		Name:       name,
		LoggedInAt: time.Now(),
	}
	r.mu.Lock()
	r.by[id] = a
	r.mu.Unlock()
	r.fireNotify()
	return a, true
}

func (r *agentRegistry) logout(id string) {
	r.mu.Lock()
	_, existed := r.by[id]
	delete(r.by, id)
	r.mu.Unlock()
	if existed {
		r.fireNotify()
	}
}

func (r *agentRegistry) get(id string) (*agent, bool) {
	if id == "" {
		return nil, false
	}
	r.mu.Lock()
	a, ok := r.by[id]
	r.mu.Unlock()
	return a, ok
}

// setWebRTCLeg associates a WebRTC leg with an agent. Returns the previous
// leg id (if any) so the caller can hang it up — callers should never hold
// the registry mutex while issuing SDK calls.
func (r *agentRegistry) setWebRTCLeg(agentID, legID string) (prev string) {
	r.mu.Lock()
	a, ok := r.by[agentID]
	if !ok {
		r.mu.Unlock()
		return ""
	}
	prev = a.WebRTCLegID
	a.WebRTCLegID = legID
	r.mu.Unlock()
	r.fireNotify()
	return prev
}

func (r *agentRegistry) webRTCLeg(agentID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a, ok := r.by[agentID]; ok {
		return a.WebRTCLegID
	}
	return ""
}

func newAgentID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
