package main

import (
	"context"
	"testing"
)

// The point of the setting: Bob hears Alice's words in a voice matching the
// gender ALICE selected — not Bob's, and not the seat's.
func TestSpeakerGenderPicksTheVoice(t *testing.T) {
	a, fake, _, _, alice, bob := newTestApp(t)

	alice.mu.Lock()
	alice.gender = "male"
	alice.mu.Unlock()
	bob.mu.Lock()
	bob.gender = "female"
	bob.mu.Unlock()

	// Alice speaks → Bob hears it, in Alice's (male) voice.
	a.interp.onTurn(turn("leg-alice", "end_of_turn", 0, "hello"))
	waitFor(t, "alice's utterance", func() bool { return fake.count("tts") == 1 })
	if got, want := fake.lastVoice(), a.voiceFor("male"); got != want {
		t.Errorf("alice was spoken in %q, want the male voice %q", got, want)
	}

	// Bob speaks → Alice hears it, in Bob's (female) voice.
	a.interp.onTurn(turn("leg-bob", "end_of_turn", 0, "hola"))
	waitFor(t, "bob's utterance", func() bool { return fake.count("tts") == 2 })
	if got, want := fake.lastVoice(), a.voiceFor("female"); got != want {
		t.Errorf("bob was spoken in %q, want the female voice %q", got, want)
	}
}

// Somebody who declines to say still gets a working voice.
func TestUnspecifiedGetsTheFallbackVoice(t *testing.T) {
	a, fake, _, _, alice, _ := newTestApp(t)
	if alice.getGender() != defaultGender {
		t.Fatalf("a new participant starts as %q, want %q", alice.getGender(), defaultGender)
	}

	a.interp.onTurn(turn("leg-alice", "end_of_turn", 0, "hello"))
	waitFor(t, "utterance", func() bool { return fake.count("tts") == 1 })

	if got, want := fake.lastVoice(), a.voiceFor(defaultGender); got != want {
		t.Errorf("spoke in %q, want the fallback voice %q", got, want)
	}
	if fake.lastVoice() == "" {
		t.Error("no voice at all was requested")
	}
}

// The speculative path must use the same voice as the plain one, or a committed
// utterance would arrive in a different voice from a re-synthesized one.
func TestPreflightUsesTheSpeakerVoiceToo(t *testing.T) {
	a, fake, _, _, alice, _ := newTestApp(t)
	alice.mu.Lock()
	alice.gender = "male"
	alice.mu.Unlock()

	a.interp.onTurn(turn("leg-alice", "eager_end_of_turn", 0, "hello"))
	waitFor(t, "preflight", func() bool { return fake.count("preflight") == 1 })

	if got, want := fake.lastVoice(), a.voiceFor("male"); got != want {
		t.Errorf("preflight used %q, want %q", got, want)
	}
}

// Changing the setting must drop audio already staged in the old voice —
// otherwise one sentence arrives in a different voice from the rest, which is
// exactly the jarring effect the setting exists to prevent.
func TestGenderChangeDiscardsStaged(t *testing.T) {
	a, fake, _, s, alice, _ := newTestApp(t)

	a.interp.onTurn(turn("leg-alice", "eager_end_of_turn", 0, "hello"))
	waitFor(t, "preflight", func() bool { return fake.count("preflight") == 1 })

	a.media.setGender(context.Background(), s, alice, "male")

	if n := fake.count("discard"); n != 1 {
		t.Errorf("discarded %d staged utterance(s) on a voice change, want 1", n)
	}
	if got := alice.getGender(); got != "male" {
		t.Errorf("gender is %q, want male", got)
	}
	// Unlike a language change, the speaker's own audio is unaffected, so
	// transcription must NOT be restarted.
	if n := fake.count("stt_stop"); n != 0 {
		t.Errorf("restarted transcription on a voice change (%d stt_stop)", n)
	}
}

// A value we do not offer must be ignored rather than applied.
func TestUnknownGenderIsIgnored(t *testing.T) {
	a, _, _, s, alice, _ := newTestApp(t)
	a.media.setGender(context.Background(), s, alice, "wizard")
	if got := alice.getGender(); got != defaultGender {
		t.Errorf("gender became %q, want it left at %q", got, defaultGender)
	}
}

// Setting the same value twice must not churn — it would needlessly throw away
// staged audio mid-conversation.
func TestGenderSetIsIdempotent(t *testing.T) {
	a, fake, _, s, alice, _ := newTestApp(t)
	a.media.setGender(context.Background(), s, alice, "female")
	fake.reset()
	a.media.setGender(context.Background(), s, alice, "female")
	if n := len(fake.ops()); n != 0 {
		t.Errorf("re-selecting the same voice issued %d command(s)", n)
	}
}

// Every offered option needs a configured voice, or picking it would silently
// fall back and the selector would appear broken.
func TestEveryOfferedGenderHasAVoice(t *testing.T) {
	a, _, _, _, _, _ := newTestApp(t)
	for _, g := range genders {
		if _, ok := a.cfg.ttsVoices[g.Code]; !ok {
			t.Errorf("offered option %q has no configured voice", g.Code)
		}
		if g.Label == "" {
			t.Errorf("option %q has no label", g.Code)
		}
	}
	if len(genderByCode) != len(genders) {
		t.Errorf("duplicate gender code: %d entries, %d unique", len(genders), len(genderByCode))
	}
}

// A participant must be seated with the language and voice they chose, not at
// the defaults with a correction to follow.
//
// This is the bug that shipped: the invite link carried no choices, so the
// second person joined as an English-speaking "guest" and either stayed that
// way or was corrected a moment later — and anything said in that window was
// transcribed in the wrong language.
func TestJoinSeatsTheChosenLanguageAndVoice(t *testing.T) {
	a, _, _, _, _, _ := newTestApp(t)
	s := a.sessions.create()

	_, p, err := a.sessions.join(s.id, "bob", "es", "female", a.cfg)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if got := p.getLang(); got != "es" {
		t.Errorf("seated speaking %q, want es — the opening words would be transcribed wrong", got)
	}
	if got := p.getGender(); got != "female" {
		t.Errorf("seated with voice %q, want female", got)
	}
}

// A missing or unrecognised choice must fall back, not fail the join.
func TestJoinFallsBackOnUnknownChoices(t *testing.T) {
	a, _, _, _, _, _ := newTestApp(t)
	s := a.sessions.create()

	_, p, err := a.sessions.join(s.id, "carol", "klingon", "wizard", a.cfg)
	if err != nil {
		t.Fatalf("join with unknown values should still succeed: %v", err)
	}
	if got := p.getLang(); got != defaultLang {
		t.Errorf("lang = %q, want the default %q", got, defaultLang)
	}
	if got := p.getGender(); got != defaultGender {
		t.Errorf("gender = %q, want the default %q", got, defaultGender)
	}

	_, p2, err := a.sessions.join(s.id, "dave", "", "", a.cfg)
	if err != nil {
		t.Fatalf("join with empty values: %v", err)
	}
	if p2.getLang() != defaultLang || p2.getGender() != defaultGender {
		t.Errorf("empty choices gave %q/%q, want the defaults", p2.getLang(), p2.getGender())
	}
}

// The two participants keep their own settings — seating the second must not
// disturb the first.
func TestSecondJoinDoesNotDisturbTheFirst(t *testing.T) {
	a, _, _, _, _, _ := newTestApp(t)
	s := a.sessions.create()

	_, first, _ := a.sessions.join(s.id, "alice", "en", "female", a.cfg)
	_, second, _ := a.sessions.join(s.id, "bob", "ja", "male", a.cfg)

	if first.getLang() != "en" || first.getGender() != "female" {
		t.Errorf("first participant changed to %q/%q", first.getLang(), first.getGender())
	}
	if second.getLang() != "ja" || second.getGender() != "male" {
		t.Errorf("second participant seated as %q/%q, want ja/male", second.getLang(), second.getGender())
	}
	if first.role() == second.role() {
		t.Error("both participants share a role")
	}
}
