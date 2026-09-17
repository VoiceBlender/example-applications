package main

import (
	"context"
	"testing"
	"time"
)

// markStaged simulates the tts.staged event VoiceBlender emits when a
// speculatively-synthesized utterance is buffered and ready to commit.
func markStaged(a *app, listenerLeg string) {
	_, listener, ok := a.sessions.forLeg(listenerLeg)
	if !ok {
		return
	}
	sess, _, _ := a.sessions.forLeg(listenerLeg)
	speaker := sess.peerOf(listener)
	if speaker == nil {
		return
	}
	speaker.mu.Lock()
	ids := make([]string, 0, len(speaker.stagedByID))
	for id := range speaker.stagedByID {
		ids = append(ids, id)
	}
	speaker.mu.Unlock()
	for _, id := range ids {
		speaker.markStagedReady(id)
	}
}

// waitFor polls until cond holds or the deadline passes. The hot path hands work
// to goroutines, so assertions need a moment rather than an immediate read.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The happy path: the speculative guess was right, so the turn ends with a
// commit — and crucially NOT with a second synthesis.
func TestEagerThenCommit(t *testing.T) {
	a, fake, tr, _, alice, bob := newTestApp(t)

	a.interp.onTurn(turn("leg-alice", "eager_end_of_turn", 0, "hello there"))
	waitFor(t, "preflight", func() bool { return fake.count("preflight") == 1 })

	c, _ := fake.find("preflight")
	if c.id != "leg-bob" {
		t.Fatalf("preflight went to %q, want the listener's leg leg-bob", c.id)
	}
	if c.text != "<hello there>" {
		t.Fatalf("preflight text %q, want the translation", c.text)
	}
	markStaged(a, "leg-bob")

	a.interp.onTurn(turn("leg-alice", "end_of_turn", 0, "hello there"))
	waitFor(t, "commit", func() bool { return fake.count("commit") == 1 })

	if n := fake.count("tts"); n != 0 {
		t.Errorf("re-synthesized %d time(s); the staged audio should have been committed", n)
	}
	if n := fake.count("discard"); n != 0 {
		t.Errorf("discarded %d staged utterance(s) on the happy path", n)
	}
	// Translated once for the eager pass; the end-of-turn check is a cache hit
	// in the real translator and an identical-text short-circuit here.
	if got := tr.count(); got != 1 {
		t.Errorf("translated %d times, want 1", got)
	}
	bob.mu.Lock()
	inflight := bob.inflight
	bob.mu.Unlock()
	if inflight != c.arg {
		t.Errorf("listener in-flight = %q, want the committed id %q", inflight, c.arg)
	}
	alice.mu.Lock()
	left := len(alice.staged)
	alice.mu.Unlock()
	if left != 0 {
		t.Errorf("%d staged entries left after commit, want 0", left)
	}
}

// The speaker paused mid-sentence and then carried on: the staged audio is the
// wrong words and must be thrown away, not spoken.
func TestTurnResumedDiscards(t *testing.T) {
	a, fake, _, _, alice, _ := newTestApp(t)

	a.interp.onTurn(turn("leg-alice", "eager_end_of_turn", 0, "hello"))
	waitFor(t, "preflight", func() bool { return fake.count("preflight") == 1 })

	a.interp.onTurn(turn("leg-alice", "turn_resumed", 0, "hello"))
	waitFor(t, "discard", func() bool { return fake.count("discard") == 1 })

	if n := fake.count("commit"); n != 0 {
		t.Errorf("committed %d time(s) after the speaker resumed", n)
	}
	alice.mu.Lock()
	left := len(alice.staged)
	alice.mu.Unlock()
	if left != 0 {
		t.Errorf("%d staged entries left after a resume, want 0", left)
	}
}

// The transcript was revised after the eager guess, so the staged audio says the
// wrong thing: discard it and synthesize the corrected sentence.
func TestRevisedTranscriptDiscardsAndRespeaks(t *testing.T) {
	a, fake, _, _, _, _ := newTestApp(t)

	a.interp.onTurn(turn("leg-alice", "eager_end_of_turn", 0, "let's meet at nine"))
	waitFor(t, "preflight", func() bool { return fake.count("preflight") == 1 })
	markStaged(a, "leg-bob")

	a.interp.onTurn(turn("leg-alice", "end_of_turn", 0, "let's meet at nine thirty"))
	waitFor(t, "fresh tts", func() bool { return fake.count("tts") == 1 })

	if n := fake.count("discard"); n != 1 {
		t.Errorf("discarded %d, want 1 (the stale staged audio)", n)
	}
	if n := fake.count("commit"); n != 0 {
		t.Errorf("committed %d stale utterance(s), want 0", n)
	}
	c, _ := fake.find("tts")
	if c.text != "<let's meet at nine thirty>" {
		t.Errorf("spoke %q, want the revised sentence", c.text)
	}
}

// With no eager event at all — the speculative path disabled, or a provider that
// cannot do it — a turn still gets spoken, just the slow way.
func TestEndOfTurnWithoutEagerSpeaksDirectly(t *testing.T) {
	a, fake, _, _, _, _ := newTestApp(t)

	a.interp.onTurn(turn("leg-alice", "end_of_turn", 0, "good morning"))
	waitFor(t, "tts", func() bool { return fake.count("tts") == 1 })

	if n := fake.count("preflight"); n != 0 {
		t.Errorf("preflighted %d time(s) with no eager event", n)
	}
	c, _ := fake.find("tts")
	if c.id != "leg-bob" || c.text != "<good morning>" {
		t.Errorf("spoke %q to %q, want the translation on the listener's leg", c.text, c.id)
	}
}

// VoiceBlender caps staged utterances per leg and REFUSES the one that would
// exceed it, so the app must evict its own oldest first.
func TestStagedCapEvictsOldest(t *testing.T) {
	a, fake, _, _, alice, _ := newTestApp(t)

	for i := 0; i < stagedMax; i++ {
		a.interp.onTurn(turn("leg-alice", "eager_end_of_turn", i, "utterance"))
		want := i + 1
		waitFor(t, "preflight", func() bool { return fake.count("preflight") == want })
	}
	alice.mu.Lock()
	n := len(alice.staged)
	alice.mu.Unlock()
	if n != stagedMax {
		t.Fatalf("staged %d, want %d before the cap bites", n, stagedMax)
	}
	if fake.count("discard") != 0 {
		t.Fatalf("evicted early")
	}

	// One more must evict, not stack up.
	a.interp.onTurn(turn("leg-alice", "eager_end_of_turn", stagedMax, "one more"))
	waitFor(t, "eviction", func() bool { return fake.count("discard") == 1 })
	waitFor(t, "preflight", func() bool { return fake.count("preflight") == stagedMax+1 })

	alice.mu.Lock()
	_, oldestStillThere := alice.staged[0]
	n = len(alice.staged)
	alice.mu.Unlock()
	if oldestStillThere {
		t.Errorf("evicted something other than the oldest turn")
	}
	if n != stagedMax {
		t.Errorf("holding %d staged, want to stay at the cap of %d", n, stagedMax)
	}
}

// Two translations must never overlap on one leg: VoiceBlender mixes concurrent
// leg_tts rather than queueing them, so the previous one has to be stopped.
func TestSecondUtteranceStopsTheFirst(t *testing.T) {
	a, fake, _, _, _, bob := newTestApp(t)

	a.interp.onTurn(turn("leg-alice", "end_of_turn", 0, "first"))
	waitFor(t, "first tts", func() bool { return fake.count("tts") == 1 })
	first, _ := fake.find("tts")

	bob.mu.Lock()
	got := bob.inflight
	bob.mu.Unlock()
	if got != first.arg {
		t.Fatalf("in-flight %q, want %q", got, first.arg)
	}

	a.interp.onTurn(turn("leg-alice", "end_of_turn", 1, "second"))
	waitFor(t, "second tts", func() bool { return fake.count("tts") == 2 })
	waitFor(t, "play_stop", func() bool { return fake.count("play_stop") == 1 })

	stop, _ := fake.find("play_stop")
	if stop.arg != first.arg {
		t.Errorf("stopped %q, want the first utterance %q", stop.arg, first.arg)
	}
	if stop.id != "leg-bob" {
		t.Errorf("stopped playback on %q, want leg-bob", stop.id)
	}
}

// A commit can fail — the staged audio expired, say. The sentence must still be
// spoken rather than silently dropped.
func TestCommitFailureFallsBackToFreshSynthesis(t *testing.T) {
	a, fake, _, _, _, _ := newTestApp(t)
	fake.failCommit = true

	a.interp.onTurn(turn("leg-alice", "eager_end_of_turn", 0, "hello"))
	waitFor(t, "preflight", func() bool { return fake.count("preflight") == 1 })
	markStaged(a, "leg-bob")

	a.interp.onTurn(turn("leg-alice", "end_of_turn", 0, "hello"))
	waitFor(t, "fallback tts", func() bool { return fake.count("tts") == 1 })

	if n := fake.count("commit_failed"); n != 1 {
		t.Errorf("expected the commit to be attempted and fail, got %d attempts", n)
	}
}

// A failed preflight must not strand the turn: end_of_turn takes the slow path.
func TestPreflightFailureStillSpeaks(t *testing.T) {
	a, fake, _, _, _, _ := newTestApp(t)
	fake.failPreflight = true

	a.interp.onTurn(turn("leg-alice", "eager_end_of_turn", 0, "hello"))
	waitFor(t, "failed preflight", func() bool { return fake.count("preflight_failed") == 1 })

	a.interp.onTurn(turn("leg-alice", "end_of_turn", 0, "hello"))
	waitFor(t, "tts", func() bool { return fake.count("tts") == 1 })
}

// Nothing should be transcribed or spoken while only one person is present.
func TestNoPeerNoSpeech(t *testing.T) {
	a, fake, _, s, alice, bob := newTestApp(t)
	a.sessions.leave(s, bob)

	a.interp.onTurn(turn("leg-alice", "end_of_turn", 0, "anyone there"))
	time.Sleep(50 * time.Millisecond)

	if n := fake.count("tts"); n != 0 {
		t.Errorf("spoke %d utterance(s) with nobody to hear them", n)
	}
	_ = alice
}

// Changing language mid-session must drop audio staged from the old language
// and restart transcription, not leave a stale utterance ready to commit.
func TestLanguageChangeDiscardsStaged(t *testing.T) {
	a, fake, _, s, alice, _ := newTestApp(t)

	a.interp.onTurn(turn("leg-alice", "eager_end_of_turn", 0, "hello"))
	waitFor(t, "preflight", func() bool { return fake.count("preflight") == 1 })

	alice.mu.Lock()
	alice.sttOn = true
	alice.mu.Unlock()

	a.media.setLanguage(context.Background(), s, alice, "fr")

	if n := fake.count("discard"); n != 1 {
		t.Errorf("discarded %d staged utterance(s) on a language change, want 1", n)
	}
	if n := fake.count("stt_stop"); n != 1 {
		t.Errorf("stopped stt %d time(s), want 1", n)
	}
	c, ok := fake.find("stt_start")
	if !ok {
		t.Fatal("transcription was not restarted in the new language")
	}
	if c.arg != "fr" {
		t.Errorf("restarted stt with hint %q, want fr", c.arg)
	}
	if got := alice.getLang(); got != "fr" {
		t.Errorf("language is %q, want fr", got)
	}
}

// tts.staged can land AFTER end_of_turn has already pulled the entry out of the
// turn map — the turn ended before the vendor finished synthesizing. The commit
// waits on synthesis, so that late event must still be able to wake it. If the
// wake-up is missed the commit times out and re-synthesizes from scratch, which
// throws away the entire point of preflighting.
func TestLateStagedEventStillWakesTheCommit(t *testing.T) {
	a, fake, _, _, _, _ := newTestApp(t)

	a.interp.onTurn(turn("leg-alice", "eager_end_of_turn", 0, "hello"))
	waitFor(t, "preflight", func() bool { return fake.count("preflight") == 1 })
	// Deliberately do NOT mark it ready yet: synthesis is still running.

	done := make(chan struct{})
	go func() {
		a.interp.onTurn(turn("leg-alice", "end_of_turn", 0, "hello"))
		close(done)
	}()

	// Let end_of_turn get as far as waiting on synthesis, then deliver the event
	// the way VoiceBlender would.
	time.Sleep(40 * time.Millisecond)
	c, _ := fake.find("preflight")
	a.interp.onStaged(stagedEvent("leg-bob", c.arg))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("end_of_turn never returned")
	}

	if n := fake.count("commit"); n != 1 {
		t.Errorf("committed %d time(s), want 1 — the late tts.staged should have woken the commit", n)
	}
	if n := fake.count("tts"); n != 0 {
		t.Errorf("re-synthesized %d time(s); the staged audio was ready and should have been used", n)
	}
}

// A tts id we do not know about must be ignored rather than panic — it can be
// another app's, or one we already resolved.
func TestUnknownStagedEventIsIgnored(t *testing.T) {
	a, _, _, _, alice, _ := newTestApp(t)

	a.interp.onStaged(stagedEvent("leg-bob", "tts-does-not-exist"))

	if alice.markStagedReady("tts-does-not-exist") {
		t.Error("markStagedReady claimed to wake an unknown id")
	}
}

// Resolving a turn must not leak the id index, or a long session would grow it
// without bound.
func TestStagedIndexIsReleased(t *testing.T) {
	a, fake, _, _, alice, _ := newTestApp(t)

	a.interp.onTurn(turn("leg-alice", "eager_end_of_turn", 0, "hello"))
	waitFor(t, "preflight", func() bool { return fake.count("preflight") == 1 })
	markStaged(a, "leg-bob")
	a.interp.onTurn(turn("leg-alice", "end_of_turn", 0, "hello"))
	waitFor(t, "commit", func() bool { return fake.count("commit") == 1 })

	waitFor(t, "index release", func() bool {
		alice.mu.Lock()
		defer alice.mu.Unlock()
		return len(alice.stagedByID) == 0 && len(alice.staged) == 0
	})
}
