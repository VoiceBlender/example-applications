package main

import (
	"context"
	"testing"
	"time"
)

// The limits are a spend control, so the table below is really a statement about
// when money stops being spent.
func TestExpiryRules(t *testing.T) {
	now := time.Now()
	cfg := config{
		maxDuration:  time.Hour,
		idleTimeout:  5 * time.Minute,
		emptyTimeout: 5 * time.Minute,
	}

	cases := []struct {
		name  string
		setup func(s *session)
		want  string
	}{
		{
			name: "live conversation, recent speech",
			setup: func(s *session) {
				s.startedAt = now.Add(-10 * time.Minute)
				s.lastActivity = now.Add(-30 * time.Second)
			},
			want: "",
		},
		{
			name: "nobody has spoken for longer than the idle timeout",
			setup: func(s *session) {
				s.startedAt = now.Add(-10 * time.Minute)
				s.lastActivity = now.Add(-6 * time.Minute)
			},
			want: "no speech for a while",
		},
		{
			name: "someone is talking, but the hard cap is reached",
			setup: func(s *session) {
				s.startedAt = now.Add(-61 * time.Minute)
				s.lastActivity = now.Add(-1 * time.Second)
			},
			want: "time limit reached",
		},
		{
			name: "link minted and never opened",
			setup: func(s *session) {
				s.emptySince = now.Add(-6 * time.Minute)
			},
			want: "abandoned",
		},
		{
			name: "empty, but not for long enough yet",
			setup: func(s *session) {
				s.emptySince = now.Add(-1 * time.Minute)
			},
			want: "",
		},
		{
			name: "joined, legs not up yet, recently joined",
			setup: func(s *session) {
				// startedAt is only stamped once interpreting begins, but the
				// idle clock starts at join.
				s.lastActivity = now.Add(-30 * time.Second)
			},
			want: "",
		},
		{
			name: "joined but never spoke and the media never came up",
			setup: func(s *session) {
				s.lastActivity = now.Add(-6 * time.Minute)
			},
			want: "no speech for a while",
		},
		{
			name: "already closed",
			setup: func(s *session) {
				s.startedAt = now.Add(-99 * time.Hour)
				s.closed = true
			},
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &session{id: "x"}
			tc.setup(s)
			if got := s.expiry(now, cfg); got != tc.want {
				t.Errorf("expiry = %q, want %q", got, tc.want)
			}
		})
	}
}

// A zero limit disables that limit — the documented escape hatch.
func TestZeroDisablesEachLimit(t *testing.T) {
	now := time.Now()
	s := &session{
		startedAt:    now.Add(-99 * time.Hour),
		lastActivity: now.Add(-99 * time.Hour),
		emptySince:   now.Add(-99 * time.Hour),
	}
	if got := s.expiry(now, config{}); got != "" {
		t.Errorf("expiry = %q with every limit zero, want none", got)
	}
}

// Speech must hold the idle timeout off — otherwise an active conversation
// would be cut off mid-sentence.
func TestSpeechResetsTheIdleClock(t *testing.T) {
	a, _, _, s, _, _ := newTestApp(t)
	s.markStarted()

	s.mu.Lock()
	s.lastActivity = time.Now().Add(-10 * time.Minute)
	s.mu.Unlock()

	cfg := config{idleTimeout: time.Minute}
	if s.expiry(time.Now(), cfg) == "" {
		t.Fatal("a long-silent session should have been due for reaping")
	}

	// A turn event is activity.
	a.interp.onTurn(turn("leg-alice", "start_of_turn", 0, ""))

	if got := s.expiry(time.Now(), cfg); got != "" {
		t.Errorf("expiry = %q right after someone spoke, want none", got)
	}
}

// Ending a session must stop the meters and release the media, in that order,
// and must retire the link.
func TestEndSessionStopsTheMetersAndReleasesMedia(t *testing.T) {
	a, fake, _, s, alice, bob := newTestApp(t)
	alice.sttOn, bob.sttOn = true, true
	s.markStarted()

	a.endSession(context.Background(), s, "no speech for a while")

	if n := fake.count("stt_stop"); n != 2 {
		t.Errorf("stopped stt on %d leg(s), want 2 — that is the meter that bills", n)
	}
	if n := fake.count("delete_leg"); n != 2 {
		t.Errorf("deleted %d leg(s), want 2", n)
	}
	if n := fake.count("delete_room"); n != 1 {
		t.Errorf("deleted %d room(s), want 1", n)
	}
	// STT must stop before the legs go, or we pay for the tail.
	ops := fake.ops()
	firstStop, firstDelete := -1, -1
	for i, op := range ops {
		if op == "stt_stop" && firstStop < 0 {
			firstStop = i
		}
		if op == "delete_leg" && firstDelete < 0 {
			firstDelete = i
		}
	}
	if firstStop > firstDelete {
		t.Errorf("legs were deleted before stt was stopped: %v", ops)
	}
	if _, ok := a.sessions.get(s.id); ok {
		t.Error("the session link still resolves after being ended")
	}
	if !s.isClosed() {
		t.Error("session not marked closed")
	}
}

// Teardown must be idempotent: a reaper and a departing participant can race.
func TestEndSessionRunsOnce(t *testing.T) {
	a, fake, _, s, _, _ := newTestApp(t)
	s.markStarted()

	a.endSession(context.Background(), s, "first")
	a.endSession(context.Background(), s, "second")

	if n := fake.count("delete_room"); n != 1 {
		t.Errorf("deleted the room %d times, want exactly 1", n)
	}
}

// Both browsers must be told why the audio stopped, then hung up on.
func TestEndSessionNotifiesAndHangsUp(t *testing.T) {
	a, _, _, s, alice, bob := newTestApp(t)
	s.markStarted()

	a.endSession(context.Background(), s, "time limit reached")

	for _, p := range []*participant{alice, bob} {
		var reason string
		for len(p.c.outbox) > 0 {
			msg := (<-p.c.outbox).(map[string]any)
			if msg["type"] == "ended" {
				reason, _ = msg["reason"].(string)
			}
		}
		if reason != "time limit reached" {
			t.Errorf("%s was not told why the session ended (got %q)", p.name, reason)
		}
		select {
		case <-p.c.quit:
		default:
			t.Errorf("%s's socket was not hung up", p.name)
		}
	}
}

// A reaped session's link must stop admitting people.
func TestClosedSessionRefusesJoins(t *testing.T) {
	a, _, _, s, _, _ := newTestApp(t)
	id := s.id
	a.endSession(context.Background(), s, "abandoned")

	if _, _, err := a.sessions.join(id, "carol", "en", defaultGender, a.cfg); err == nil {
		t.Error("joined a session that had already been ended")
	}
}

// The reaper itself: a session past its idle limit is ended without anyone
// touching it.
func TestReaperEndsAnIdleSession(t *testing.T) {
	a, fake, _, s, _, _ := newTestApp(t)
	a.cfg.idleTimeout = 50 * time.Millisecond
	a.cfg.maxDuration = 0
	a.cfg.emptyTimeout = 0
	s.markStarted()
	s.mu.Lock()
	s.lastActivity = time.Now().Add(-time.Second)
	s.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Drive one pass directly rather than waiting on the 15 s ticker.
	for _, sess := range a.sessions.all() {
		if reason := sess.expiry(time.Now(), a.cfg); reason != "" {
			a.endSession(ctx, sess, reason)
		}
	}
	if n := fake.count("delete_room"); n != 1 {
		t.Errorf("the idle session was not reaped (delete_room x%d)", n)
	}
}

// With every limit off, the reaper must not run at all.
func TestReaperExitsWhenAllLimitsDisabled(t *testing.T) {
	a, _, _, _, _, _ := newTestApp(t)
	a.cfg.maxDuration, a.cfg.idleTimeout, a.cfg.emptyTimeout = 0, 0, 0

	done := make(chan struct{})
	go func() { a.reapSessions(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("reapSessions kept running with every limit disabled")
	}
}

// Shutting down must end live sessions, not abandon them. A leg left behind on
// the media server keeps streaming audio to the STT vendor, and once this
// process is gone there is nobody left to stop it.
func TestShutdownEndsLiveSessions(t *testing.T) {
	a, fake, _, s, alice, bob := newTestApp(t)
	alice.sttOn, bob.sttOn = true, true
	s.markStarted()

	a.shutdownSessions()

	if n := fake.count("stt_stop"); n != 2 {
		t.Errorf("stopped stt on %d leg(s) at shutdown, want 2", n)
	}
	if n := fake.count("delete_room"); n != 1 {
		t.Errorf("deleted %d room(s) at shutdown, want 1", n)
	}
	if !s.isClosed() {
		t.Error("session was left live across shutdown")
	}
}

func TestShutdownWithNoSessionsIsANoop(t *testing.T) {
	a, fake, _, s, _, _ := newTestApp(t)
	a.sessions.drop(s.id)
	a.shutdownSessions()
	if n := len(fake.ops()); n != 0 {
		t.Errorf("issued %d command(s) with no live sessions", n)
	}
}
