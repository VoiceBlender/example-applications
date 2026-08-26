package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// The machine-translation hop.
//
// VoiceBlender's STT providers transcribe but do not translate — there is no
// translation option anywhere in its STT surface — so the MT hop lives here, in
// the app, between the stt.turn event and the leg_tts that speaks the result.
//
// It is deliberately an interface with a trivial default. Adding another backend
// (a dedicated MT service, an LLM, a self-hosted model) is one file and one case
// in newTranslator; nothing else in the app knows which one is in use.
type translator interface {
	// Translate renders text from one language into another. from and to are
	// our own language codes (see langs.go); the implementation maps them to
	// whatever its provider wants.
	Translate(ctx context.Context, text, from, to string) (string, error)
	Name() string
}

// newTranslator builds the configured backend.
func newTranslator(provider string, log *slog.Logger) (translator, error) {
	base := func(t translator) translator { return &cachingTranslator{inner: t} }
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "", "deepl":
		key := os.Getenv("DEEPL_API_KEY")
		if key == "" {
			// Refusing here rather than falling back keeps a missing key from
			// looking like a broken translator: the session would run and every
			// utterance would arrive untranslated.
			return nil, fmt.Errorf("DEEPL_API_KEY is required for TRANSLATE_PROVIDER=deepl (use TRANSLATE_PROVIDER=none to run without translation)")
		}
		return base(&deeplTranslator{
			key: key,
			url: envOr("DEEPL_URL", "https://api-free.deepl.com/v2/translate"),
			hc:  &http.Client{Timeout: 5 * time.Second},
			log: log,
		}), nil
	case "none", "passthrough":
		return &passthroughTranslator{}, nil
	default:
		return nil, fmt.Errorf("unknown TRANSLATE_PROVIDER %q (want deepl or none)", provider)
	}
}

// ── passthrough ───────────────────────────────────────────────────────────────

// passthroughTranslator echoes the text unchanged. It exists so the example runs
// end to end — WebRTC, routing, STT, TTS, captions — with no MT key at all: you
// hear your own words spoken back in a synthetic voice, which is enough to prove
// every other hop works.
type passthroughTranslator struct{}

func (p *passthroughTranslator) Translate(_ context.Context, text, _, _ string) (string, error) {
	return text, nil
}
func (p *passthroughTranslator) Name() string { return "passthrough" }

// ── DeepL ─────────────────────────────────────────────────────────────────────

// deeplTranslator calls the DeepL REST API. It is a plain net/http call on
// purpose: no SDK, so the module keeps the dependency set it already has.
type deeplTranslator struct {
	key string
	url string
	hc  *http.Client
	log *slog.Logger
}

func (d *deeplTranslator) Name() string { return "deepl" }

func (d *deeplTranslator) Translate(ctx context.Context, text, from, to string) (string, error) {
	src, dst := lookupLang(from), lookupLang(to)
	body, err := json.Marshal(map[string]any{
		"text":        []string{text},
		"source_lang": src.DeepLFrom,
		"target_lang": dst.DeepLTo,
		// Utterances are conversational fragments, not documents: keeping the
		// text as one "sentence" stops DeepL splitting a half-finished eager
		// turn into pieces it then translates out of context.
		"split_sentences": "0",
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "DeepL-Auth-Key "+d.key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := d.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("deepl: %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	var out struct {
		Translations []struct {
			Text string `json:"text"`
		} `json:"translations"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if len(out.Translations) == 0 {
		return "", fmt.Errorf("deepl: empty response")
	}
	return out.Translations[0].Text, nil
}

// ── cache ─────────────────────────────────────────────────────────────────────

// cacheMax bounds the translation cache. A session generates a handful of
// entries per minute, so this is generous; it exists to stop an unbounded map,
// not to be tuned.
const cacheMax = 512

// cachingTranslator memoizes on (text, from, to).
//
// This is not a general-purpose optimization, it is load-bearing for latency.
// The hot path translates twice for most turns: once speculatively at
// eager_end_of_turn, and again at end_of_turn to check the text did not change.
// In the common case the two texts are identical, so the second call — the one
// that sits on the critical path, between the speaker stopping and the listener
// hearing — becomes a map lookup and does no network I/O at all.
type cachingTranslator struct {
	inner translator

	mu    sync.Mutex
	m     map[string]string
	order []string // insertion order, for eviction
}

func (c *cachingTranslator) Name() string { return c.inner.Name() + "+cache" }

func (c *cachingTranslator) Translate(ctx context.Context, text, from, to string) (string, error) {
	key := from + "\x00" + to + "\x00" + text
	if v, ok := c.get(key); ok {
		return v, nil
	}
	v, err := c.inner.Translate(ctx, text, from, to)
	if err != nil {
		return "", err
	}
	c.put(key, v)
	return v, nil
}

func (c *cachingTranslator) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[key]
	return v, ok
}

func (c *cachingTranslator) put(key, val string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[string]string, cacheMax)
	}
	if _, exists := c.m[key]; exists {
		return
	}
	if len(c.order) >= cacheMax {
		delete(c.m, c.order[0])
		c.order = c.order[1:]
	}
	c.m[key] = val
	c.order = append(c.order, key)
}
