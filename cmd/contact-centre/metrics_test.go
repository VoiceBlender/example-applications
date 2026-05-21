package main

import (
	"math"
	"testing"
	"time"
)

func TestComputeMetrics_AnsweredAndAbandoned(t *testing.T) {
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	log := []LogEntry{
		// Answered in 5s, talked for 30s, ended 1 minute ago.
		{
			Outcome:    "answered",
			StartedAt:  now.Add(-2 * time.Minute),
			AnsweredAt: now.Add(-2*time.Minute + 5*time.Second),
			EndedAt:    now.Add(-1 * time.Minute),
		},
		// Abandoned after 8s, 30s ago.
		{
			Outcome:   "abandoned",
			StartedAt: now.Add(-38 * time.Second),
			EndedAt:   now.Add(-30 * time.Second),
		},
	}

	got := computeMetrics(nil, log, now, 30*time.Minute, 20*time.Second)

	if got.OfferedInWindow != 2 {
		t.Errorf("OfferedInWindow = %d, want 2", got.OfferedInWindow)
	}
	if got.AnsweredInWindow != 1 {
		t.Errorf("AnsweredInWindow = %d, want 1", got.AnsweredInWindow)
	}
	if got.AbandonedInWindow != 1 {
		t.Errorf("AbandonedInWindow = %d, want 1", got.AbandonedInWindow)
	}
	// SL: 1 met (the answered@5s, beat 20s) / 2 eligible = 50%.
	if got.ServiceLevelPct == nil || !approxEqual(*got.ServiceLevelPct, 50.0) {
		t.Errorf("ServiceLevelPct = %v, want 50.0", deref(got.ServiceLevelPct))
	}
	if got.ASASecs == nil || !approxEqual(*got.ASASecs, 5.0) {
		t.Errorf("ASASecs = %v, want 5", deref(got.ASASecs))
	}
	if got.AHTSecs == nil || !approxEqual(*got.AHTSecs, 55.0) {
		t.Errorf("AHTSecs = %v, want 55", deref(got.AHTSecs))
	}
	if got.AbandonRatePct == nil || !approxEqual(*got.AbandonRatePct, 50.0) {
		t.Errorf("AbandonRatePct = %v, want 50.0", deref(got.AbandonRatePct))
	}
	// Peak wait: abandoned waited 8s, answered 5s — peak is 8s.
	if got.LongestWaitSecs != 8 {
		t.Errorf("LongestWaitSecs = %d, want 8", got.LongestWaitSecs)
	}
}

func TestComputeMetrics_EmptyWindow(t *testing.T) {
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	// One answered call but it's outside the window.
	log := []LogEntry{
		{
			Outcome:    "answered",
			StartedAt:  now.Add(-2 * time.Hour),
			AnsweredAt: now.Add(-2*time.Hour + 3*time.Second),
			EndedAt:    now.Add(-2*time.Hour + 1*time.Minute),
		},
	}

	got := computeMetrics(nil, log, now, 30*time.Minute, 20*time.Second)

	if got.OfferedInWindow != 0 || got.AnsweredInWindow != 0 || got.AbandonedInWindow != 0 {
		t.Errorf("counters = %+v, want all zero", got)
	}
	if got.ServiceLevelPct != nil || got.ASASecs != nil || got.AHTSecs != nil || got.AbandonRatePct != nil {
		t.Errorf("expected nil pointers for empty window, got %+v", got)
	}
}

func TestComputeMetrics_LongestWait(t *testing.T) {
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	active := []callView{
		{State: "ringing", StartedAt: now.Add(-2 * time.Second)},
		{State: "queued", StartedAt: now.Add(-45 * time.Second)},
		{State: "queued", StartedAt: now.Add(-2 * time.Minute)},
		{State: "in_call", StartedAt: now.Add(-10 * time.Minute)},
	}

	got := computeMetrics(active, nil, now, 30*time.Minute, 20*time.Second)

	if got.LongestWaitSecs != 120 {
		t.Errorf("LongestWaitSecs = %d, want 120", got.LongestWaitSecs)
	}
}

func TestComputeMetrics_LongestWaitPersistsAfterCallEnds(t *testing.T) {
	// Regression: a single call that waited 90s then was answered must
	// keep that 90s peak visible after the queue empties.
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	log := []LogEntry{
		{
			Outcome:    "answered",
			StartedAt:  now.Add(-5 * time.Minute),
			AnsweredAt: now.Add(-5*time.Minute + 90*time.Second),
			EndedAt:    now.Add(-2 * time.Minute),
		},
	}

	got := computeMetrics(nil, log, now, 30*time.Minute, 20*time.Second)

	if got.LongestWaitSecs != 90 {
		t.Errorf("LongestWaitSecs = %d, want 90 (peak from completed call)", got.LongestWaitSecs)
	}
}

func TestComputeMetrics_AbandonOnly(t *testing.T) {
	// All callers hung up before being answered: SL is 0% (none met),
	// Abandon Rate is 100%, ASA/AHT remain nil (no answered calls).
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	log := []LogEntry{
		{Outcome: "abandoned", StartedAt: now.Add(-1 * time.Minute), EndedAt: now.Add(-50 * time.Second)},
		{Outcome: "abandoned", StartedAt: now.Add(-30 * time.Second), EndedAt: now.Add(-20 * time.Second)},
	}

	got := computeMetrics(nil, log, now, 30*time.Minute, 20*time.Second)

	if got.ServiceLevelPct == nil || !approxEqual(*got.ServiceLevelPct, 0.0) {
		t.Errorf("ServiceLevelPct = %v, want 0.0", deref(got.ServiceLevelPct))
	}
	if got.AbandonRatePct == nil || !approxEqual(*got.AbandonRatePct, 100.0) {
		t.Errorf("AbandonRatePct = %v, want 100.0", deref(got.AbandonRatePct))
	}
	if got.ASASecs != nil || got.AHTSecs != nil {
		t.Errorf("expected ASA/AHT nil when no answered calls, got asa=%v aht=%v", deref(got.ASASecs), deref(got.AHTSecs))
	}
	// Both abandoned calls waited 10s; peak is 10.
	if got.LongestWaitSecs != 10 {
		t.Errorf("LongestWaitSecs = %d, want 10", got.LongestWaitSecs)
	}
}

func approxEqual(a, b float64) bool { return math.Abs(a-b) < 0.0001 }
func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}
