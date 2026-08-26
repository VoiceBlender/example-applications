package main

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// countingTranslator reports how many times the real backend was reached.
type countingTranslator struct {
	mu   sync.Mutex
	n    int
	fail error
}

func (c *countingTranslator) Translate(_ context.Context, text, _, _ string) (string, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	if c.fail != nil {
		return "", c.fail
	}
	return "T:" + text, nil
}
func (c *countingTranslator) Name() string { return "counting" }
func (c *countingTranslator) hits() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// The cache is load-bearing for latency, not just tidiness: a turn is usually
// translated twice — speculatively at eager_end_of_turn and again at
// end_of_turn — and the second call sits on the critical path between the
// speaker stopping and the listener hearing. It must not touch the network.
func TestCacheMakesTheSecondTranslationFree(t *testing.T) {
	inner := &countingTranslator{}
	c := &cachingTranslator{inner: inner}
	ctx := context.Background()

	first, err := c.Translate(ctx, "hello there", "en", "es")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := c.Translate(ctx, "hello there", "en", "es")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first != second {
		t.Errorf("cache returned %q then %q", first, second)
	}
	if inner.hits() != 1 {
		t.Errorf("reached the backend %d times, want 1 — the second call is on the critical path", inner.hits())
	}
}

// The same words in a different direction are a different translation.
func TestCacheKeyIncludesDirection(t *testing.T) {
	inner := &countingTranslator{}
	c := &cachingTranslator{inner: inner}
	ctx := context.Background()

	_, _ = c.Translate(ctx, "si", "es", "en")
	_, _ = c.Translate(ctx, "si", "en", "es")
	if inner.hits() != 2 {
		t.Errorf("reached the backend %d times, want 2 — direction is part of the key", inner.hits())
	}
}

// A failure must not be cached, or one blip would poison that sentence for the
// rest of the session.
func TestFailuresAreNotCached(t *testing.T) {
	inner := &countingTranslator{fail: errors.New("boom")}
	c := &cachingTranslator{inner: inner}
	ctx := context.Background()

	if _, err := c.Translate(ctx, "hello", "en", "es"); err == nil {
		t.Fatal("expected an error")
	}
	inner.fail = nil
	got, err := c.Translate(ctx, "hello", "en", "es")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got != "T:hello" {
		t.Errorf("got %q after recovery", got)
	}
	if inner.hits() != 2 {
		t.Errorf("backend hits %d, want 2 (the failure was not cached)", inner.hits())
	}
}

// The cache is bounded, so a long session cannot grow it without limit.
func TestCacheEvictsOldest(t *testing.T) {
	inner := &countingTranslator{}
	c := &cachingTranslator{inner: inner}
	ctx := context.Background()

	for i := 0; i < cacheMax+10; i++ {
		_, _ = c.Translate(ctx, string(rune('a'+i%26))+string(rune(i)), "en", "es")
	}
	c.mu.Lock()
	size, order := len(c.m), len(c.order)
	c.mu.Unlock()
	if size > cacheMax || order > cacheMax {
		t.Errorf("cache grew to %d entries (order %d), cap is %d", size, order, cacheMax)
	}
}

// Without a key, the DeepL backend must refuse at startup rather than run and
// leave every utterance silently untranslated.
func TestDeepLRequiresAKey(t *testing.T) {
	t.Setenv("DEEPL_API_KEY", "")
	if _, err := newTranslator("deepl", nil); err == nil {
		t.Error("expected newTranslator to refuse without DEEPL_API_KEY")
	}
}

func TestPassthroughNeedsNoKey(t *testing.T) {
	tr, err := newTranslator("none", nil)
	if err != nil {
		t.Fatalf("none: %v", err)
	}
	got, err := tr.Translate(context.Background(), "unchanged", "en", "es")
	if err != nil || got != "unchanged" {
		t.Errorf("passthrough returned %q, %v", got, err)
	}
}

func TestUnknownTranslatorIsRejected(t *testing.T) {
	if _, err := newTranslator("babelfish", nil); err == nil {
		t.Error("expected an unknown provider to be rejected")
	}
}

// Every language needs the spellings that are not optional: a label, a
// Speechmatics code and both DeepL codes. The Flux code is allowed to be empty
// — that is precisely how the table records "Flux cannot do this one".
func TestEveryLanguageIsFullySpelled(t *testing.T) {
	for _, l := range languages {
		if l.Code == "" || l.Label == "" || l.Speechmatics == "" || l.DeepLFrom == "" || l.DeepLTo == "" {
			t.Errorf("language %+v has an empty required field", l)
		}
	}
	if len(langByCode) != len(languages) {
		t.Errorf("duplicate language code: %d entries, %d unique", len(languages), len(langByCode))
	}
	if !knownLang(defaultLang) {
		t.Fatalf("default language %q is not in the table", defaultLang)
	}
	// The default must work on every engine, since it is the fallback everyone
	// lands on.
	for _, l := range []language{lookupLang(defaultLang)} {
		for _, provider := range []string{"deepgram_flux", "speechmatics"} {
			if _, ok := sttCode(provider, l); !ok {
				t.Errorf("default language %q is unsupported on %s", defaultLang, provider)
			}
		}
	}
	// English and Portuguese need a regional variant as a DeepL TARGET and are
	// rejected without one — the easiest of these to get wrong.
	for _, code := range []string{"en", "pt"} {
		if l := lookupLang(code); l.DeepLTo == l.DeepLFrom {
			t.Errorf("%s target %q needs a regional variant (e.g. EN-GB, PT-PT)", code, l.DeepLTo)
		}
	}
	// Mandarin is "cmn" to Speechmatics, not "zh" — a two-letter guess here
	// silently transcribes nothing.
	if got := lookupLang("zh").Speechmatics; got != "cmn" {
		t.Errorf("Speechmatics Mandarin code is %q, want cmn", got)
	}
	// An unknown code must degrade, not panic or return an empty language.
	if lookupLang("klingon").Code != defaultLang {
		t.Error("unknown language should fall back to the default")
	}
}
