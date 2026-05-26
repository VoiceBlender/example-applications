package main

import (
	"context"
	"sync"
	"time"
)

// LogEntry is one historical call as captured at the moment it ended (caller
// disconnected or was hung up by the agent). Stored in whatever backend the
// supervisor app is configured with.
type LogEntry struct {
	LegID      string    `json:"leg_id"`
	From       string    `json:"from"`
	To         string    `json:"to"`
	StartedAt  time.Time `json:"started_at"`
	AnsweredAt time.Time `json:"answered_at,omitempty"`
	EndedAt    time.Time `json:"ended_at"`
	// Outcome is one of:
	//   "abandoned"  – caller hung up before an agent answered
	//   "answered"   – an agent took the call (it then ended for any reason)
	//   "failed"     – call never made it past the waiting room (no answer / error)
	Outcome         string           `json:"outcome"`
	AgentID         string           `json:"agent_id,omitempty"`
	AgentName       string           `json:"agent_name,omitempty"`
	TransferredFrom string           `json:"transferred_from,omitempty"`
	Transcript      []TranscriptLine `json:"transcript,omitempty"`
}

// newLogEntry builds a LogEntry from the registry's last view of a call,
// captured by handle()'s defer just before the call is removed. The
// transcript is passed in rather than read off the call, because the
// transcript store lives outside the registry.
func newLogEntry(c *callView, transcript []TranscriptLine) LogEntry {
	e := LogEntry{
		LegID:      c.LegID,
		From:       c.From,
		To:         c.To,
		StartedAt:  c.StartedAt,
		EndedAt:    time.Now(),
		Transcript: transcript,
	}
	if c.AnsweredByAgentID != "" {
		e.AnsweredAt = c.AnsweredAt
		e.AgentID = c.AnsweredByAgentID
		e.AgentName = c.AnsweredByName
		e.Outcome = "answered"
	} else {
		e.Outcome = "abandoned"
	}
	if c.transferredFrom != "" {
		e.TransferredFrom = c.transferredFrom
	}
	return e
}

// LogStore is the pluggable backend for the call log. The implementation
// chosen at startup must implement both methods.
type LogStore interface {
	// Append records a single completed call. Idempotency is not required —
	// each handle() goroutine calls Append exactly once per call.
	Append(ctx context.Context, e LogEntry) error
	// List returns the most-recent entries first, capped at limit. limit≤0
	// returns the full backlog (still bounded by whatever cap the store
	// internally enforces).
	List(ctx context.Context, limit int) ([]LogEntry, error)
	// Close releases any external resources (e.g. Redis connection).
	Close() error
}

// memStore is a process-local ring buffer. Appends overwrite the oldest
// entry once the buffer is full.
type memStore struct {
	mu    sync.Mutex
	buf   []LogEntry
	cap   int
	head  int // index of the next write
	count int // number of valid entries (≤ cap)
}

func newMemStore(capacity int) *memStore {
	if capacity <= 0 {
		capacity = 200
	}
	return &memStore{buf: make([]LogEntry, capacity), cap: capacity}
}

func (m *memStore) Append(_ context.Context, e LogEntry) error {
	m.mu.Lock()
	m.buf[m.head] = e
	m.head = (m.head + 1) % m.cap
	if m.count < m.cap {
		m.count++
	}
	m.mu.Unlock()
	return nil
}

func (m *memStore) List(_ context.Context, limit int) ([]LogEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := m.count
	if limit > 0 && limit < n {
		n = limit
	}
	out := make([]LogEntry, 0, n)
	// Walk backwards from the most-recent insertion.
	idx := m.head - 1
	if idx < 0 {
		idx += m.cap
	}
	for i := 0; i < n; i++ {
		out = append(out, m.buf[idx])
		idx--
		if idx < 0 {
			idx += m.cap
		}
	}
	return out, nil
}

func (m *memStore) Close() error { return nil }
