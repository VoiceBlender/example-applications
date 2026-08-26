package main

import (
	"context"
	"sync"
	"testing"
)

// The bug this file exists for.
//
// Polish was offered in the selector, Deepgram Flux does not support it, and
// the transcriber's dial was rejected — so that participant's speech produced
// nothing at all while the other direction kept working. It looked like "it
// still interprets from English".
func TestFluxDoesNotOfferLanguagesItCannotTranscribe(t *testing.T) {
	// No Speechmatics key, so there is nothing to fall back to.
	cfg := fluxOnlyConfig()
	offered := cfg.offeredLanguages()

	// Deepgram documents exactly these ten for flux-general-multi. Anything
	// else gets a 400 INVALID_PARAMETER on the dial.
	want := map[string]bool{
		"en": true, "es": true, "fr": true, "de": true, "hi": true,
		"ru": true, "pt": true, "ja": true, "it": true, "nl": true,
	}
	if len(offered) != len(want) {
		t.Errorf("offering %d languages on Flux, want %d", len(offered), len(want))
	}
	for _, l := range offered {
		if !want[l.Code] {
			t.Errorf("offering %q on Flux, which Flux cannot transcribe", l.Code)
		}
	}
	for _, code := range []string{"pl", "uk", "tr", "ko", "zh"} {
		if cfg.langSupported(code) {
			t.Errorf("%q reported as available with Flux alone; the dial would be rejected", code)
		}
	}
}

// Those languages are the whole reason Speechmatics is an option, so it must
// offer them.
func TestSpeechmaticsFallbackCoversTheRest(t *testing.T) {
	cfg := testConfig() // Flux preferred, Speechmatics as fallback
	offered := cfg.offeredLanguages()
	if len(offered) != len(languages) {
		t.Errorf("offering %d of %d languages with both engines configured", len(offered), len(languages))
	}
	for _, code := range []string{"pl", "uk", "tr", "ko", "zh"} {
		if !cfg.langSupported(code) {
			t.Errorf("%q not offered even with the Speechmatics fallback", code)
		}
	}
}

// The point of routing per language: the preferred engine keeps the languages
// it can do (and with them the eager end-of-turn path), and only the rest fall
// through. A Polish speaker must not drag the English speaker off Flux.
func TestEachLanguageRoutesToTheBestEngine(t *testing.T) {
	cfg := testConfig()
	for _, code := range []string{"en", "es", "fr", "de", "hi", "ru", "pt", "ja", "it", "nl"} {
		if got, _ := cfg.providerForLang(code); got != "deepgram_flux" {
			t.Errorf("%q routed to %q, want deepgram_flux — it would lose the eager path", code, got)
		}
	}
	for _, code := range []string{"pl", "uk", "tr", "ko", "zh"} {
		if got, _ := cfg.providerForLang(code); got != "speechmatics" {
			t.Errorf("%q routed to %q, want the speechmatics fallback", code, got)
		}
	}
	if _, ok := cfg.providerForLang("klingon"); ok {
		t.Error("an unknown language was routed somewhere")
	}
}

// A provider with no key must not be routed to — it would only fail at the dial.
func TestProviderWithoutAKeyIsNotUsed(t *testing.T) {
	cfg := fluxOnlyConfig()
	if _, ok := cfg.providerForLang("pl"); ok {
		t.Error("routed Polish to Speechmatics with no SPEECHMATICS_API_KEY set")
	}
	if got, _ := cfg.providerForLang("en"); got != "deepgram_flux" {
		t.Errorf("English routed to %q", got)
	}
}

// The transcriber must be given ITS OWN spelling of the language.
func TestSTTCodeIsProviderSpecific(t *testing.T) {
	zh := lookupLang("zh")
	if code, ok := sttCode("speechmatics", zh); !ok || code != "cmn" {
		t.Errorf("Speechmatics Mandarin = %q/%v, want cmn/true", code, ok)
	}
	if _, ok := sttCode("deepgram_flux", zh); ok {
		t.Error("Flux reported as able to transcribe Mandarin")
	}
	es := lookupLang("es")
	if code, ok := sttCode("deepgram_flux", es); !ok || code != "es" {
		t.Errorf("Flux Spanish = %q/%v, want es/true", code, ok)
	}
}

// Building an STT request for a language the provider cannot do must fail
// rather than produce a payload that will be rejected downstream.
func TestSTTPayloadRefusesUnsupportedLanguage(t *testing.T) {
	a, _, _, _, _, _ := newTestApp(t) // configured for deepgram_flux

	a.cfg = fluxOnlyConfig()
	if _, ok := a.media.sttPayload("leg-x", "pl"); ok {
		t.Error("built a Flux request for Polish; Deepgram would reject the dial")
	}
	a.cfg = testConfig()
	p, ok := a.media.sttPayload("leg-x", "pl")
	if !ok {
		t.Fatal("Speechmatics should transcribe Polish")
	}
	if p.Provider != "speechmatics" {
		t.Errorf("Polish routed to %q, want speechmatics", p.Provider)
	}
	if p.Language != "pl" {
		t.Errorf("Speechmatics language = %q, want pl", p.Language)
	}
	if p.APIKey != "sm-key" {
		t.Errorf("used key %q, want the Speechmatics one", p.APIKey)
	}
	if len(p.LanguageHints) != 0 {
		t.Errorf("sent language_hints %v to Speechmatics; only Flux takes them", p.LanguageHints)
	}
}

// Nothing must ever dial the transcriber with a language it cannot handle —
// the failure is invisible, so it has to be prevented rather than reported.
func TestUnsupportedLanguageNeverStartsSTT(t *testing.T) {
	a, fake, _, s, alice, _ := newTestApp(t)
	a.cfg = fluxOnlyConfig() // nothing configured can do Polish

	alice.mu.Lock()
	alice.lang = "pl" // as if forced past the selector
	alice.mu.Unlock()

	a.media.startSTT(context.Background(), s, alice)

	if n := fake.count("stt_start"); n != 0 {
		t.Errorf("dialled the transcriber %d time(s) for a language it cannot do", n)
	}
	alice.mu.Lock()
	on := alice.sttOn
	alice.mu.Unlock()
	if on {
		t.Error("marked transcription as running when it never started")
	}
}

// Selecting an unsupported language mid-call must be refused, not applied —
// applying it would stop transcription and never restart it.
func TestSetLanguageRejectsUnsupported(t *testing.T) {
	a, fake, _, s, alice, _ := newTestApp(t)
	a.cfg = fluxOnlyConfig()

	a.media.setLanguage(context.Background(), s, alice, "pl")

	if got := alice.getLang(); got == "pl" {
		t.Error("accepted a language the transcriber cannot handle")
	}
	if n := fake.count("stt_stop"); n != 0 {
		t.Errorf("stopped transcription (%d) for a change that was rejected", n)
	}

	// The same change is fine once a transcriber that covers it is configured.
	a.cfg = testConfig()
	a.media.setLanguage(context.Background(), s, alice, "pl")
	if got := alice.getLang(); got != "pl" {
		t.Errorf("lang = %q on Speechmatics, want pl", got)
	}
}

// Joining with an unsupported language must fall back to one that works rather
// than seating someone who will be silently untranscribed.
func TestJoinFallsBackOnUnsupportedLanguage(t *testing.T) {
	a, _, _, _, _, _ := newTestApp(t)
	s := a.sessions.create()

	_, p, err := a.sessions.join(s.id, "bob", "pl", "male", fluxOnlyConfig())
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if p.getLang() != defaultLang {
		t.Errorf("seated speaking %q with Flux alone, want the default %q", p.getLang(), defaultLang)
	}

	_, p2, err := a.sessions.join(s.id, "carol", "pl", "female", testConfig())
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if p2.getLang() != "pl" {
		t.Errorf("seated speaking %q with the Speechmatics fallback, want pl", p2.getLang())
	}
}

// A language change must actually replace the running transcriber.
//
// The bug: startSTT released the participant lock across the VSI round trip, so
// a leg connecting and a language change could interleave — the connect dialled
// the OLD language, the change's start came back 409, and the 409 branch simply
// returned. The wrong language kept transcribing and nothing recorded the drift.
func TestLanguageChangeReplacesTheRunningTranscriber(t *testing.T) {
	a, fake, _, s, alice, _ := newTestApp(t)

	a.media.startSTT(context.Background(), s, alice)
	if got := fake.count("stt_start"); got != 1 {
		t.Fatalf("initial start count %d, want 1", got)
	}
	if alice.runningLang() != "en" {
		t.Fatalf("running language %q, want en", alice.runningLang())
	}

	a.media.setLanguage(context.Background(), s, alice, "de")

	if got := alice.runningLang(); got != "de" {
		t.Errorf("still transcribing %q after switching to German", got)
	}
	if n := fake.count("stt_stop"); n != 1 {
		t.Errorf("stopped the old transcriber %d time(s), want 1", n)
	}
	if n := fake.count("stt_start"); n != 2 {
		t.Errorf("started %d time(s), want 2", n)
	}
	// The new dial must carry the new language.
	c, _ := fake.last("stt_start")
	if c.arg != "de" {
		t.Errorf("restarted with hint %q, want de", c.arg)
	}
}

// Reconciling must be idempotent: asking again when the running language is
// already correct must not churn the transcriber mid-conversation.
func TestStartSTTIsIdempotent(t *testing.T) {
	a, fake, _, s, alice, _ := newTestApp(t)

	a.media.startSTT(context.Background(), s, alice)
	a.media.startSTT(context.Background(), s, alice)
	a.media.startSTT(context.Background(), s, alice)

	if n := fake.count("stt_start"); n != 1 {
		t.Errorf("dialled %d times for one unchanged language", n)
	}
	if n := fake.count("stt_stop"); n != 0 {
		t.Errorf("stopped a correctly-running transcriber %d time(s)", n)
	}
}

// If the server reports one already attached, we must replace it rather than
// assume it is the one we wanted.
func TestConflictReplacesRatherThanAssumes(t *testing.T) {
	a, fake, _, s, alice, _ := newTestApp(t)
	fake.conflictNextSTTStart = true

	a.media.startSTT(context.Background(), s, alice)

	if n := fake.count("stt_stop"); n != 1 {
		t.Errorf("did not clear the conflicting transcriber (stt_stop x%d)", n)
	}
	if n := fake.count("stt_start"); n != 2 {
		t.Errorf("did not retry the start after clearing (stt_start x%d)", n)
	}
	if got := alice.runningLang(); got != "en" {
		t.Errorf("running language %q after the retry, want en", got)
	}
}

// Concurrent reconciles must converge on the language actually selected, not on
// whichever goroutine happened to finish last.
func TestConcurrentStartsConvergeOnTheSelectedLanguage(t *testing.T) {
	a, _, _, s, alice, _ := newTestApp(t)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.media.startSTT(context.Background(), s, alice) }()
	go func() { defer wg.Done(); a.media.setLanguage(context.Background(), s, alice, "fr") }()
	wg.Wait()

	// Whatever the interleaving, the running language must match the selection.
	if got, want := alice.runningLang(), alice.getLang(); got != want {
		t.Errorf("transcribing %q while %q is selected", got, want)
	}
}
