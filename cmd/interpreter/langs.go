package main

// The languages the interpreter offers, and the four different spellings each
// one needs.
//
// Every hop in the cascade names languages its own way, and getting one wrong
// fails QUIETLY rather than loudly — which is exactly how this bit an early
// version of this app: Polish was offered, Deepgram Flux does not support it,
// the transcriber's dial was rejected, and that side of the conversation simply
// produced nothing while the other direction kept working. So the mapping lives
// here, in one table, with the unsupported combinations spelled out.
//
//   - Flux         Deepgram Flux `language_hint`. EMPTY means flux-general-multi
//                  cannot do this language at all. Flux supports exactly ten:
//                  en, es, fr, de, hi, ru, pt, ja, it, nl — and rejects the dial
//                  outright (400 INVALID_PARAMETER) for anything else, so an
//                  unsupported hint must never be sent. Note also that a hint
//                  BIASES detection, it does not pin it.
//   - Speechmatics Speechmatics language code. Much broader coverage, and it
//                  pins rather than biases — which is why it is the provider to
//                  use for anything outside Flux's ten. Mostly ISO 639-1, but
//                  Mandarin is the three-letter "cmn", not "zh".
//   - DeepLTo      DeepL *target* code. Not always the same as the source: some
//                  languages require a regional variant as a target ("EN-GB",
//                  "PT-PT") and are rejected without one.
//   - DeepLFrom    DeepL *source* code, which never takes the regional variant.
//
// The TTS side needs no per-language entry: eleven_flash_v2_5 is multilingual
// and picks up the language from the text itself. Which voice each participant
// is heard in is chosen separately, by the gender they select — see voices.go.

type language struct {
	Code         string // our key, and what the browser sends
	Label        string // what the language selector shows
	Flux         string // Deepgram Flux language_hint; "" = unsupported
	Speechmatics string // Speechmatics language code
	DeepLFrom    string // DeepL source_lang
	DeepLTo      string // DeepL target_lang
}

var languages = []language{
	// ── the ten Deepgram Flux supports ───────────────────────────────────────
	{Code: "en", Label: "English", Flux: "en", Speechmatics: "en", DeepLFrom: "EN", DeepLTo: "EN-GB"},
	{Code: "es", Label: "Spanish", Flux: "es", Speechmatics: "es", DeepLFrom: "ES", DeepLTo: "ES"},
	{Code: "fr", Label: "French", Flux: "fr", Speechmatics: "fr", DeepLFrom: "FR", DeepLTo: "FR"},
	{Code: "de", Label: "German", Flux: "de", Speechmatics: "de", DeepLFrom: "DE", DeepLTo: "DE"},
	{Code: "it", Label: "Italian", Flux: "it", Speechmatics: "it", DeepLFrom: "IT", DeepLTo: "IT"},
	{Code: "pt", Label: "Portuguese", Flux: "pt", Speechmatics: "pt", DeepLFrom: "PT", DeepLTo: "PT-PT"},
	{Code: "nl", Label: "Dutch", Flux: "nl", Speechmatics: "nl", DeepLFrom: "NL", DeepLTo: "NL"},
	{Code: "hi", Label: "Hindi", Flux: "hi", Speechmatics: "hi", DeepLFrom: "HI", DeepLTo: "HI"},
	{Code: "ru", Label: "Russian", Flux: "ru", Speechmatics: "ru", DeepLFrom: "RU", DeepLTo: "RU"},
	{Code: "ja", Label: "Japanese", Flux: "ja", Speechmatics: "ja", DeepLFrom: "JA", DeepLTo: "JA"},

	// ── Speechmatics only (STT_PROVIDER=speechmatics) ────────────────────────
	// Offered only when the configured transcriber can actually handle them, so
	// nobody can pick a language that will silently transcribe nothing.
	{Code: "pl", Label: "Polish", Speechmatics: "pl", DeepLFrom: "PL", DeepLTo: "PL"},
	{Code: "uk", Label: "Ukrainian", Speechmatics: "uk", DeepLFrom: "UK", DeepLTo: "UK"},
	{Code: "tr", Label: "Turkish", Speechmatics: "tr", DeepLFrom: "TR", DeepLTo: "TR"},
	{Code: "ko", Label: "Korean", Speechmatics: "ko", DeepLFrom: "KO", DeepLTo: "KO"},
	{Code: "zh", Label: "Chinese (Mandarin)", Speechmatics: "cmn", DeepLFrom: "ZH", DeepLTo: "ZH"},
}

// defaultLang is what a participant gets before they choose. It must be one
// every provider supports.
const defaultLang = "en"

var langByCode = func() map[string]language {
	m := make(map[string]language, len(languages))
	for _, l := range languages {
		m[l.Code] = l
	}
	return m
}()

// lookupLang resolves a language code, falling back to the default rather than
// failing: an unknown code from a stale browser tab should degrade to English,
// not break the session.
func lookupLang(code string) language {
	if l, ok := langByCode[code]; ok {
		return l
	}
	return langByCode[defaultLang]
}

// sttCode returns the code to give the transcriber for a language, and whether
// that provider can transcribe it at all.
//
// The boolean is the important half. Sending Flux a language it does not know
// gets the dial rejected, and the symptom is silence from that participant
// rather than an error anyone sees — so callers must check it rather than
// send whatever comes back.
func sttCode(provider string, l language) (string, bool) {
	if provider == "deepgram_flux" {
		return l.Flux, l.Flux != ""
	}
	return l.Speechmatics, l.Speechmatics != ""
}

// providerForLang picks the transcriber for one participant's language.
//
// The choice is per LEG, not per session, because leg_stt_start takes its own
// provider and options — so an English speaker can be on Flux, with its eager
// end-of-turn and the low latency that buys, while the Polish speaker on the
// other side of the same call is on Speechmatics. Forcing the whole session onto
// one engine would make everyone pay for the least-supported language.
//
// The preferred provider wins whenever it can do the language; otherwise the
// fallback is used. A provider with no API key configured is not considered
// available at all — routing to it would just fail at the dial.
func (c config) providerForLang(code string) (string, bool) {
	l, known := langByCode[code]
	if !known {
		return "", false
	}
	for _, provider := range []string{c.sttProvider, c.sttFallback} {
		if provider == "" || c.sttKeys[provider] == "" {
			continue
		}
		if _, ok := sttCode(provider, l); ok {
			return provider, true
		}
	}
	return "", false
}

// offeredLanguages is the list to show in the selector: everything some
// configured transcriber can actually handle.
func (c config) offeredLanguages() []language {
	out := make([]language, 0, len(languages))
	for _, l := range languages {
		if _, ok := c.providerForLang(l.Code); ok {
			out = append(out, l)
		}
	}
	return out
}

// langSupported reports whether a language can be transcribed at all with the
// current configuration, so a value from a stale tab is rejected rather than
// acted on.
func (c config) langSupported(code string) bool {
	_, ok := c.providerForLang(code)
	return ok
}

// knownLang reports whether code is in the table at all, regardless of provider.
func knownLang(code string) bool {
	_, ok := langByCode[code]
	return ok
}
