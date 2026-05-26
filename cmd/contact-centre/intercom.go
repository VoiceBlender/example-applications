package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"
)

// Intercom states.
const (
	intercomRinging = "ringing"
	intercomActive  = "active"
	intercomEnded   = "ended"
)

// Intercom target kinds. Agents can intercom either a named supervisor
// (broadcast to every browser tab of that auth username) or another
// agent directly (single recipient by agent_id).
const (
	intercomTargetSupervisor = "supervisor"
	intercomTargetAgent      = "agent"
)

// intercom is one agent → {supervisor | agent} call attempt. Lives in
// memory for the duration of the attempt; not persisted to the call log.
type intercom struct {
	ID         string    `json:"intercom_id"`
	AgentID    string    `json:"agent_id"`
	AgentName  string    `json:"agent_name"`
	TargetKind string    `json:"target_kind"` // "supervisor" | "agent"
	Target     string    `json:"target"`      // supervisor username, or callee agent_id
	TargetName string    `json:"target_name,omitempty"`
	AgentLeg   string    `json:"-"` // calling agent's WebRTC leg
	CalleeLeg  string    `json:"-"` // answering side's WebRTC leg (set on answer)
	RoomID     string    `json:"room_id,omitempty"`
	State      string    `json:"state"`
	StartedAt  time.Time `json:"started_at"`
	AnsweredAt time.Time `json:"answered_at,omitempty"`
}

// intercomRegistry holds active and recently-ended intercoms keyed by
// id and, separately, by agent id so we can enforce one intercom per
// agent. Atomic state transitions (ringing→active, ringing→ended,
// active→ended) live on this type so each WS handler can claim
// ownership outside the SDK call path.
type intercomRegistry struct {
	mu      sync.Mutex
	byID    map[string]*intercom
	byAgent map[string]string // agentID → intercom ID
}

func newIntercomRegistry() *intercomRegistry {
	return &intercomRegistry{
		byID:    make(map[string]*intercom),
		byAgent: make(map[string]string),
	}
}

// Create starts a new ringing intercom for ag against (kind, target).
// Returns an error if the agent already has one. Caller is responsible
// for creating the room and adding the agent leg.
func (r *intercomRegistry) Create(ag *agent, kind, target, targetName, agentLeg string) (*intercom, error) {
	if ag == nil {
		return nil, errors.New("no agent")
	}
	if kind != intercomTargetSupervisor && kind != intercomTargetAgent {
		return nil, errors.New("unknown target kind")
	}
	if target == "" {
		return nil, errors.New("target required")
	}
	if agentLeg == "" {
		return nil, errors.New("agent leg required")
	}
	id, err := newIntercomID()
	if err != nil {
		return nil, err
	}
	ic := &intercom{
		ID:         id,
		AgentID:    ag.ID,
		AgentName:  ag.Name,
		TargetKind: kind,
		Target:     target,
		TargetName: targetName,
		AgentLeg:   agentLeg,
		RoomID:     "intercom-" + id,
		State:      intercomRinging,
		StartedAt:  time.Now(),
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, busy := r.byAgent[ag.ID]; busy {
		return nil, errors.New("already in intercom")
	}
	r.byID[id] = ic
	r.byAgent[ag.ID] = id
	return ic, nil
}

// ClaimAnswer atomically flips a ringing intercom to active and
// records the answering side's leg. Returns ok=false if it was already
// answered, rejected, or unknown.
func (r *intercomRegistry) ClaimAnswer(id, calleeLeg string) (*intercom, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ic, ok := r.byID[id]
	if !ok || ic.State != intercomRinging {
		return nil, false
	}
	ic.State = intercomActive
	ic.CalleeLeg = calleeLeg
	ic.AnsweredAt = time.Now()
	return ic, true
}

// Reject atomically ends a still-ringing intercom. Returns ok=false if
// it was already answered, rejected, or unknown.
func (r *intercomRegistry) Reject(id string) (*intercom, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ic, ok := r.byID[id]
	if !ok || ic.State != intercomRinging {
		return nil, false
	}
	ic.State = intercomEnded
	delete(r.byAgent, ic.AgentID)
	delete(r.byID, id)
	return ic, true
}

// End atomically transitions a ringing or active intercom to ended.
// Used by either side's hangup path and by disconnect cleanup.
func (r *intercomRegistry) End(id string) (*intercom, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ic, ok := r.byID[id]
	if !ok || ic.State == intercomEnded {
		return nil, false
	}
	ic.State = intercomEnded
	delete(r.byAgent, ic.AgentID)
	delete(r.byID, id)
	return ic, true
}

// Get returns the intercom for id, or nil. Result is a shallow copy
// (no internal pointers worth protecting).
func (r *intercomRegistry) Get(id string) *intercom {
	r.mu.Lock()
	defer r.mu.Unlock()
	ic, ok := r.byID[id]
	if !ok {
		return nil
	}
	cp := *ic
	return &cp
}

// ByAgent returns the agent's current intercom, or nil.
func (r *intercomRegistry) ByAgent(agentID string) *intercom {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.byAgent[agentID]
	if !ok {
		return nil
	}
	ic := r.byID[id]
	if ic == nil {
		return nil
	}
	cp := *ic
	return &cp
}

// activeForTarget returns every intercom whose (kind, target) matches
// — used for disconnect-cleanup fan-out (e.g. all intercoms ringing
// the supervisor `alice` or the agent `abc123`).
func (r *intercomRegistry) activeForTarget(kind, target string) []intercom {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []intercom
	for _, ic := range r.byID {
		if ic.TargetKind == kind && ic.Target == target {
			out = append(out, *ic)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

func newIntercomID() (string, error) {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
