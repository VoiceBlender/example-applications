package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	voiceblender "github.com/VoiceBlender/voiceblender-go"
)

// The single most important assertion in this package.
//
// The routing matrix is what stops the two participants hearing each other's
// untranslated voice. An empty-but-present source list means "hears nothing";
// a MISSING or null list means "hears everyone". Those two differ by one
// character on the wire, and Go's zero value for a slice marshals to the wrong
// one — so this pins the serialization, not just the Go value.
func TestRoutingMatrixMarshalsAsEmptyArraysNotNull(t *testing.T) {
	payload := voiceblender.RoomRoutingSetPayload{
		RoomID: "interpreter-abc",
		Matrix: map[string][]string{roleA: {}, roleB: {}},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(raw)
	if strings.Contains(got, "null") {
		t.Fatalf("routing matrix marshalled a null source list — that means FULL MESH, "+
			"i.e. the participants hear each other untranslated: %s", got)
	}
	for _, role := range []string{roleA, roleB} {
		want := `"` + role + `":[]`
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
}

// applyRouting must always write BOTH rows: a role with no row in the matrix
// falls back to full mesh, so writing one row and not the other would silently
// leave one participant hearing the other's raw voice.
func TestApplyRoutingWritesBothRoles(t *testing.T) {
	a, fake, _, s, _, _ := newTestApp(t)

	a.media.applyRouting(context.Background(), s)

	c, ok := fake.find("routing_set")
	if !ok {
		t.Fatal("no routing matrix was applied")
	}
	// The fake records "<role>=<present>/<len>;" per role.
	if c.arg != "pa=true/0;pb=true/0;" {
		t.Errorf("routing matrix = %q, want both roles present with empty source lists", c.arg)
	}
	if c.id != a.vbRoom(s.id) {
		t.Errorf("routing applied to room %q, want %q", c.id, a.vbRoom(s.id))
	}
	if !s.routing {
		t.Error("session not marked as routed")
	}
}

// Every leg must join with a non-empty role, for the same reason.
func TestLegJoinsWithARole(t *testing.T) {
	a, fake, _, s, _, _ := newTestApp(t)

	// A third arrival is refused, so reuse a seat: drop alice and re-offer.
	_, p, err := a.sessions.join(s.id, "carol", "en", defaultGender, a.cfg)
	if err == nil {
		t.Fatalf("expected a full session to refuse a third participant, got %v", p)
	}

	for _, seat := range []int{0, 1} {
		got := (&participant{slot: seat}).role()
		if got == "" {
			t.Fatalf("seat %d has an empty role — that silently means full mesh", seat)
		}
	}
	if (&participant{slot: 0}).role() == (&participant{slot: 1}).role() {
		t.Fatal("both seats share a role; the matrix could not tell them apart")
	}
	_ = fake
}

// Transcription must not start until BOTH legs are connected: leg_stt_start
// rejects a leg that has not connected, and there is no point transcribing with
// nobody to translate for.
func TestSTTWaitsForBothLegs(t *testing.T) {
	a, fake, _, s, alice, bob := newTestApp(t)

	bob.mu.Lock()
	bob.live = false
	bob.mu.Unlock()

	a.media.maybeStartSTT(context.Background(), s)
	if n := fake.count("stt_start"); n != 0 {
		t.Fatalf("started stt %d time(s) with only one leg up", n)
	}

	bob.mu.Lock()
	bob.live = true
	bob.mu.Unlock()
	a.media.maybeStartSTT(context.Background(), s)

	if n := fake.count("stt_start"); n != 2 {
		t.Fatalf("started stt on %d leg(s), want both", n)
	}
	_ = alice
}

// The Flux path must send a language_hint and never a `language` — Flux has no
// language parameter, and hints are what select the multilingual model.
func TestFluxSTTPayloadUsesHintsNotLanguage(t *testing.T) {
	a, _, _, _, _, _ := newTestApp(t)

	p, ok := a.media.sttPayload("leg-x", "es")
	if !ok {
		t.Fatal("Spanish should be supported on Flux")
	}
	if p.Language != "" {
		t.Errorf("Flux payload set language=%q; Flux has no language parameter", p.Language)
	}
	if len(p.LanguageHints) != 1 || p.LanguageHints[0] != "es" {
		t.Errorf("language hints = %v, want [es]", p.LanguageHints)
	}
	if p.Model != "" {
		t.Errorf("model = %q; leaving it empty is what selects flux-general-multi", p.Model)
	}
	if p.EagerEotThreshold != 0.7 {
		t.Errorf("eager threshold = %v, want the configured 0.7 — zero disables the speculative path", p.EagerEotThreshold)
	}

	// The non-Flux provider is the mirror image.
	a.cfg.sttProvider = "speechmatics"
	p, ok = a.media.sttPayload("leg-x", "es")
	if !ok {
		t.Fatal("Spanish should be supported on Speechmatics")
	}
	if p.Language != "es" {
		t.Errorf("speechmatics payload language = %q, want es", p.Language)
	}
	if len(p.LanguageHints) != 0 {
		t.Errorf("speechmatics payload sent hints %v; only Flux takes them", p.LanguageHints)
	}
}

// The voice must follow the speaker's declared gender.
func TestVoiceFollowsGender(t *testing.T) {
	a, _, _, _, _, _ := newTestApp(t)

	if a.voiceFor("female") == a.voiceFor("male") {
		t.Error("female and male map to the same voice; the setting would do nothing")
	}
	for _, g := range []string{"female", "male", "unspecified"} {
		if a.voiceFor(g) == "" {
			t.Errorf("no voice configured for %q", g)
		}
	}
	// A stale browser tab must not take the conversation down.
	if got := a.voiceFor("nonsense"); got != a.voiceFor(defaultGender) {
		t.Errorf("unknown gender gave %q, want the fallback voice", got)
	}
}

// Only the options we offer are accepted, so a crafted message cannot reassign
// someone to an arbitrary value.
func TestKnownGender(t *testing.T) {
	for _, g := range genders {
		if !knownGender(g.Code) {
			t.Errorf("offered option %q is not accepted", g.Code)
		}
	}
	if knownGender("other-thing") {
		t.Error("an unoffered value was accepted")
	}
	if !knownGender(defaultGender) {
		t.Fatalf("default gender %q is not an offered option", defaultGender)
	}
}
