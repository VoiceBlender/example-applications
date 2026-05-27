package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// Transfer states.
const (
	transferRinging    = "ringing"
	transferConsulting = "consulting" // attended only: original + target talking, customer on hold
	transferCompleted  = "completed"
	transferFailed     = "failed"
	transferCancelled  = "cancelled"
)

// transfer is one in-flight blind-transfer attempt of a customer call
// from one agent to another agent or supervisor. The customer is on
// hold for the entire ringing phase; on answer the target takes over
// the call and the original agent goes idle.
type transfer struct {
	ID            string    `json:"transfer_id"`
	CallLegID     string    `json:"call_leg_id"`
	RoomID        string    `json:"room_id"`     // customer's room
	ConsultRoomID string    `json:"consult_room_id,omitempty"` // attended only — temp room with original+target
	FromAgentID   string    `json:"from_agent_id"`
	FromName      string    `json:"from_name"`
	FromLegID     string    `json:"-"` // original agent's WebRTC leg (re-added on cancel/fail)
	TargetKind    string    `json:"target_kind"` // reuses intercom constants
	Target        string    `json:"target"`
	TargetName    string    `json:"target_name,omitempty"`
	TargetLegID   string    `json:"-"` // target's WebRTC leg, set on answer
	Attended      bool      `json:"attended,omitempty"`
	State         string    `json:"state"`
	StartedAt     time.Time `json:"started_at"`
}

// transferRegistry holds in-flight transfers and enforces one-per-call.
// All transitions are atomic so the handlers can race safely (caller
// cancel vs target answer vs timeout vs customer hangup).
type transferRegistry struct {
	mu     sync.Mutex
	byID   map[string]*transfer
	byCall map[string]string // call leg id → transfer id
}

func newTransferRegistry() *transferRegistry {
	return &transferRegistry{
		byID:   make(map[string]*transfer),
		byCall: make(map[string]string),
	}
}

// Create registers a new ringing transfer. `fromID` is the originator's
// identity — for an agent this is the agent.ID; for a supervisor it's
// "supervisor:<username>". `fromName` is the display name shown to the
// target. Errors if the call already has a transfer in flight or
// required fields are missing.
func (r *transferRegistry) Create(callLegID, roomID, fromID, fromName, kind, target, targetName, fromLegID string, attended bool) (*transfer, error) {
	if callLegID == "" || roomID == "" || fromID == "" || target == "" {
		return nil, errors.New("missing fields")
	}
	if kind != intercomTargetSupervisor && kind != intercomTargetAgent {
		return nil, errors.New("unknown target kind")
	}
	id, err := newTransferID()
	if err != nil {
		return nil, err
	}
	t := &transfer{
		ID:          id,
		CallLegID:   callLegID,
		RoomID:      roomID,
		FromAgentID: fromID,
		FromName:    fromName,
		FromLegID:   fromLegID,
		TargetKind:  kind,
		Target:      target,
		TargetName:  targetName,
		Attended:    attended,
		State:       transferRinging,
		StartedAt:   time.Now(),
	}
	if attended {
		t.ConsultRoomID = "consult-" + id
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, busy := r.byCall[callLegID]; busy {
		return nil, errors.New("transfer already in flight for this call")
	}
	r.byID[id] = t
	r.byCall[callLegID] = id
	return t, nil
}

// ClaimAnswer atomically flips a ringing transfer to its next state.
// For a blind transfer that's `completed` (the target picks up the
// customer immediately). For attended it's `consulting` (original +
// target talk privately; the bridge to the customer happens later in
// Complete). targetLegID is the answering side's WebRTC leg.
// Returns ok=false if the transfer was already settled or unknown.
// The transfer record stays in the registry until Settle (same
// rationale as Complete — keeps the bridge-loop safety net visible
// while finalize runs the actual room hand-off).
func (r *transferRegistry) ClaimAnswer(id, targetLegID string) (*transfer, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.byID[id]
	if !ok || t.State != transferRinging {
		return nil, false
	}
	t.TargetLegID = targetLegID
	if t.Attended {
		t.State = transferConsulting
		return t, true
	}
	t.State = transferCompleted
	return t, true
}

// Complete atomically transitions a consulting attended transfer to
// completed. The transfer record STAYS in the registry until Settle is
// called — keeping it there means the per-call bridge loop's safety
// checks can still see the transfer during the finalize hand-off window
// (room teardown, customer-room AddLeg, etc.), so stray leg-disconnect
// events from the consult room don't get misread as the agent dropping.
// The caller is responsible for doing the actual media work (target →
// customer room, stop hold music) and then calling Settle.
func (r *transferRegistry) Complete(id string) (*transfer, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.byID[id]
	if !ok || t.State != transferConsulting {
		return nil, false
	}
	t.State = transferCompleted
	return t, true
}

// Settle removes a settled transfer (any state) from the registry. Idempotent.
func (r *transferRegistry) Settle(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.byID[id]
	if !ok {
		return
	}
	delete(r.byCall, t.CallLegID)
	delete(r.byID, id)
}

// Cancel atomically transitions a ringing or consulting transfer to
// cancelled. The transfer record STAYS in the registry until Settle is
// called (same rationale as Complete — the bridge loop's safety checks
// need to see the in-flight transfer while the consult-room teardown
// happens). The customer is restored to the original agent.
func (r *transferRegistry) Cancel(id string) (*transfer, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.byID[id]
	if !ok || (t.State != transferRinging && t.State != transferConsulting) {
		return nil, false
	}
	t.State = transferCancelled
	return t, true
}

// Fail atomically transitions a ringing or consulting transfer to
// failed. Same end-state as Cancel from the caller's perspective —
// the difference is just which reason is propagated to clients. Like
// Cancel, the record stays in the registry until Settle.
func (r *transferRegistry) Fail(id string) (*transfer, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.byID[id]
	if !ok || (t.State != transferRinging && t.State != transferConsulting) {
		return nil, false
	}
	t.State = transferFailed
	return t, true
}

// Get returns a copy of the transfer for id, or nil.
func (r *transferRegistry) Get(id string) *transfer {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.byID[id]
	if !ok {
		return nil
	}
	cp := *t
	return &cp
}

// ByCall returns the in-flight transfer for the given customer call,
// or nil. Useful for the customer-disconnect path that needs to fail
// any pending transfer when the call drops.
func (r *transferRegistry) ByCall(callLegID string) *transfer {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.byCall[callLegID]
	if !ok {
		return nil
	}
	t := r.byID[id]
	if t == nil {
		return nil
	}
	cp := *t
	return &cp
}

// ByTarget returns every ringing transfer aimed at (kind, target).
// Used by disconnect cleanup to fail transfers when their target goes
// away.
func (r *transferRegistry) ByTarget(kind, target string) []transfer {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []transfer
	for _, t := range r.byID {
		if t.TargetKind == kind && t.Target == target {
			out = append(out, *t)
		}
	}
	return out
}

// ByFromAgent returns the agent's in-flight transfer, or nil.
func (r *transferRegistry) ByFromAgent(agentID string) *transfer {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.byID {
		if t.FromAgentID == agentID {
			cp := *t
			return &cp
		}
	}
	return nil
}

func newTransferID() (string, error) {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
