package main

import (
	"sync"
	"time"
)

// callView is what the web panel sees. State transitions through "ringing"
// (between leg.ringing and add-to-room), "queued" (in the waiting room), and
// "in_call" (an agent has picked up).
type callView struct {
	LegID             string    `json:"leg_id"`
	From              string    `json:"from"`
	To                string    `json:"to"`
	State             string    `json:"state"`
	Position          int       `json:"position"`
	StartedAt         time.Time `json:"started_at"`
	RoomID            string    `json:"room_id,omitempty"`
	AnsweredByAgentID string    `json:"answered_by_agent_id,omitempty"`
	AnsweredByName    string    `json:"answered_by_name,omitempty"`
	AnsweredAgentLeg  string    `json:"answered_agent_leg,omitempty"`
	AnsweredAt        time.Time `json:"answered_at,omitempty"`
	OnHold            bool      `json:"on_hold,omitempty"`

	// answer is a buffered (size 1) wake-up signal that the per-call
	// goroutine selects on. Not serialized.
	answer chan struct{} `json:"-"`
	// holdPlaybackID is the room playback id of the hold music started
	// when the call was placed on hold; needed to stop it on resume.
	holdPlaybackID string `json:"-"`
}

type stats struct {
	Active  int `json:"active"`
	Ringing int `json:"ringing"`
	Queued  int `json:"queued"`
	InCall  int `json:"in_call"`
}

type snapshot struct {
	Calls []callView `json:"calls"`
	Stats stats      `json:"stats"`
	At    time.Time  `json:"at"`
}

// subscriber receives a one-bit "something changed" signal on every registry
// mutation. The WebSocket handler reads it, snapshots the registry, and
// sends. Buffer of 1 + non-blocking send coalesces bursts into a single
// re-render for slow consumers.
type subscriber struct {
	trigger chan struct{}
}

type registry struct {
	mu    sync.Mutex
	calls []*callView
	subs  map[*subscriber]struct{}
}

func newRegistry() *registry {
	return &registry{subs: make(map[*subscriber]struct{})}
}

// addRinging records a new inbound call and returns its answer wake-up
// channel. The per-call goroutine should select on the returned channel to
// be notified when an agent claims the call.
func (r *registry) addRinging(legID, from, to string) <-chan struct{} {
	answer := make(chan struct{}, 1)
	r.mu.Lock()
	r.calls = append(r.calls, &callView{
		LegID:     legID,
		From:      from,
		To:        to,
		State:     "ringing",
		StartedAt: time.Now(),
		answer:    answer,
	})
	r.broadcastLocked()
	r.mu.Unlock()
	return answer
}

func (r *registry) markQueued(legID, roomID string) {
	r.mu.Lock()
	for _, c := range r.calls {
		if c.LegID == legID {
			c.State = "queued"
			c.RoomID = roomID
			break
		}
	}
	r.assignPositionsLocked()
	r.broadcastLocked()
	r.mu.Unlock()
}

// setHold atomically toggles the call's on-hold flag and stashes the
// playback id of the currently-playing hold music (so resume can stop it).
// Returns false if the call doesn't exist or the new state matches the old
// (so the caller can avoid duplicate SDK calls).
func (r *registry) setHold(legID string, onHold bool, holdPlaybackID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		if c.LegID != legID {
			continue
		}
		if c.OnHold == onHold {
			return false
		}
		c.OnHold = onHold
		c.holdPlaybackID = holdPlaybackID
		r.broadcastLocked()
		return true
	}
	return false
}

// claim atomically transitions a queued call to "in_call" and wakes the
// per-call goroutine. Returns false if the call is not currently claimable
// (gone, still ringing, or already answered).
func (r *registry) claim(legID, agentID, agentName, agentLegID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		if c.LegID != legID {
			continue
		}
		if c.State != "queued" {
			return false
		}
		c.State = "in_call"
		c.AnsweredByAgentID = agentID
		c.AnsweredByName = agentName
		c.AnsweredAgentLeg = agentLegID
		c.AnsweredAt = time.Now()
		select {
		case c.answer <- struct{}{}:
		default:
		}
		r.assignPositionsLocked()
		r.broadcastLocked()
		return true
	}
	return false
}

// lookup returns a copy of the callView for the given leg, or nil if absent.
func (r *registry) lookup(legID string) *callView {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		if c.LegID == legID {
			cp := *c
			cp.answer = nil
			return &cp
		}
	}
	return nil
}

// callByRoom returns the active call currently associated with the given
// room id, or nil. Used by the STT pump to attribute incoming transcripts.
func (r *registry) callByRoom(roomID string) *callView {
	r.mu.Lock()
	defer r.mu.Unlock()
	if roomID == "" {
		return nil
	}
	for _, c := range r.calls {
		if c.RoomID == roomID {
			cp := *c
			cp.answer = nil
			return &cp
		}
	}
	return nil
}

// callAnsweredBy returns the (in_call) call currently owned by the given
// agent, or nil if the agent isn't on a call.
func (r *registry) callAnsweredBy(agentID string) *callView {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		if c.State == "in_call" && c.AnsweredByAgentID == agentID {
			cp := *c
			cp.answer = nil
			return &cp
		}
	}
	return nil
}

func (r *registry) remove(legID string) {
	r.mu.Lock()
	for i, c := range r.calls {
		if c.LegID == legID {
			r.calls = append(r.calls[:i], r.calls[i+1:]...)
			break
		}
	}
	r.assignPositionsLocked()
	r.broadcastLocked()
	r.mu.Unlock()
}

// queuePosition returns the caller's 1-based rank among queued calls, or 0
// if they're not currently queued.
func (r *registry) queuePosition(legID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		if c.LegID == legID {
			return c.Position
		}
	}
	return 0
}

// snapshot is the operator view: every active call.
func (r *registry) snapshot() snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := snapshot{
		Calls: make([]callView, len(r.calls)),
		At:    time.Now(),
	}
	for i, c := range r.calls {
		out.Calls[i] = *c
		out.Calls[i].answer = nil
		r.bumpStat(&out.Stats, c.State)
	}
	out.Stats.Active = len(out.Calls)
	return out
}

// queueSnapshot is the agent view: only queued calls, with full stats.
func (r *registry) queueSnapshot() snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := snapshot{
		Calls: make([]callView, 0, len(r.calls)),
		At:    time.Now(),
	}
	for _, c := range r.calls {
		r.bumpStat(&out.Stats, c.State)
		if c.State == "queued" {
			cp := *c
			cp.answer = nil
			out.Calls = append(out.Calls, cp)
		}
	}
	out.Stats.Active = len(r.calls)
	return out
}

func (r *registry) bumpStat(s *stats, state string) {
	switch state {
	case "ringing":
		s.Ringing++
	case "queued":
		s.Queued++
	case "in_call":
		s.InCall++
	}
}

// NotifyChanged wakes every current subscriber without mutating the call
// list. Other subsystems (e.g. agentRegistry) call this so the supervisor
// WS picks up changes that aren't strictly call-related.
func (r *registry) NotifyChanged() {
	r.mu.Lock()
	r.broadcastLocked()
	r.mu.Unlock()
}

func (r *registry) subscribe() *subscriber {
	s := &subscriber{trigger: make(chan struct{}, 1)}
	r.mu.Lock()
	r.subs[s] = struct{}{}
	r.mu.Unlock()
	return s
}

func (r *registry) unsubscribe(s *subscriber) {
	r.mu.Lock()
	delete(r.subs, s)
	r.mu.Unlock()
}

func (r *registry) assignPositionsLocked() {
	pos := 0
	for _, c := range r.calls {
		if c.State == "queued" {
			pos++
			c.Position = pos
		} else {
			c.Position = 0
		}
	}
}

func (r *registry) broadcastLocked() {
	for s := range r.subs {
		select {
		case s.trigger <- struct{}{}:
		default:
		}
	}
}
