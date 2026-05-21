package main

import (
	"sync"
	"time"
)

// TranscriptLine is one final stt.text result with the speaker resolved at
// capture time. Speaker is a friendly label ("customer", an agent's name,
// or "Supervisor") so that persisted log entries stay readable even after
// the agent has logged out.
type TranscriptLine struct {
	LegID   string    `json:"leg_id"`
	Speaker string    `json:"speaker"`
	Text    string    `json:"text"`
	At      time.Time `json:"at"`
}

// transcriptStore is an in-memory per-call buffer of finalised transcript
// lines, keyed by the caller's leg id. Lifetime is bounded by the call:
// handle() drops the entry after the call log entry has been written.
type transcriptStore struct {
	mu sync.Mutex
	m  map[string][]TranscriptLine
}

func newTranscriptStore() *transcriptStore {
	return &transcriptStore{m: make(map[string][]TranscriptLine)}
}

func (s *transcriptStore) Append(callerLegID string, line TranscriptLine) {
	s.mu.Lock()
	s.m[callerLegID] = append(s.m[callerLegID], line)
	s.mu.Unlock()
}

// Get returns a copy of the buffered lines for the given call. The copy
// keeps callers from racing against further Appends.
func (s *transcriptStore) Get(callerLegID string) []TranscriptLine {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.m[callerLegID]
	if len(src) == 0 {
		return nil
	}
	out := make([]TranscriptLine, len(src))
	copy(out, src)
	return out
}

func (s *transcriptStore) Drop(callerLegID string) {
	s.mu.Lock()
	delete(s.m, callerLegID)
	s.mu.Unlock()
}
