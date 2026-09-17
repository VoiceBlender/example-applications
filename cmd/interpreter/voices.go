package main

// Which voice each person is interpreted in.
//
// The voice stands in for the SPEAKER, not the listener: when Alice talks, Bob
// hears Alice's words in Alice's voice. Matching it to her gender is what keeps
// an interpreted call feeling like a conversation with a person rather than with
// a system — a man's words arriving in a woman's voice is a jarring, and
// occasionally misleading, thing to sit through.
//
// So the selector is per-participant and applies to what the OTHER side hears.
//
// Only the speaker's own choice is used; nobody can pick the voice they hear.
// There is deliberately a "prefer not to say" option, and it is the default: the
// app must work for someone who does not want to answer, so that case gets a
// configured fallback voice rather than a guess.
//
// The TTS model is multilingual, so one voice per option covers every language
// the app offers — the voice does not change when a participant switches
// language.

type voiceOption struct {
	Code  string // what the browser sends, and the config key suffix
	Label string // what the selector shows
}

// genders are the selector's options, in display order.
var genders = []voiceOption{
	{Code: "unspecified", Label: "Prefer not to say"},
	{Code: "female", Label: "Female"},
	{Code: "male", Label: "Male"},
}

// defaultGender is what a participant is until they choose.
const defaultGender = "unspecified"

// defaultGenderVoices are stock ElevenLabs voice ids.
//
// There is no stock voice that is convincingly androgynous, so "unspecified"
// does not get a neutral voice — it gets a documented fallback that happens to
// be the female one. Set TTS_VOICE_DEFAULT to whatever suits your deployment
// rather than treating this choice as meaningful.
var defaultGenderVoices = map[string]string{
	"female":      "21m00Tcm4TlvDq8ikWAM", // Rachel
	"male":        "pNInz6obpgDQGcFmaJgB", // Adam
	"unspecified": "21m00Tcm4TlvDq8ikWAM", // Rachel, as the fallback
}

var genderByCode = func() map[string]voiceOption {
	m := make(map[string]voiceOption, len(genders))
	for _, g := range genders {
		m[g.Code] = g
	}
	return m
}()

// knownGender reports whether code is one we offer, so a bad gender.set is
// rejected rather than silently reassigning someone.
func knownGender(code string) bool {
	_, ok := genderByCode[code]
	return ok
}

// voiceFor returns the TTS voice a participant is heard in. An unrecognised
// value falls back rather than failing: a stale browser tab should not take the
// conversation down.
func (a *app) voiceFor(gender string) string {
	if v, ok := a.cfg.ttsVoices[gender]; ok && v != "" {
		return v
	}
	return a.cfg.ttsVoices[defaultGender]
}
