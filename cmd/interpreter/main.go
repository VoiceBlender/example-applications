// Command interpreter is a live simultaneous interpreter built on VoiceBlender.
//
// Two people open a shared session link, each picks their own language, and talk
// over WebRTC. Each hears ONLY the translated voice of the other: the original
// speech is never mixed through. Live captions (original + translation) stream
// alongside the audio.
//
// The whole pipeline is a cascade — speech-to-text, machine translation,
// text-to-speech — and every hop is chosen for latency:
//
//   - Both browser legs sit in ONE VoiceBlender room, with the room's routing
//     matrix zeroed out ({"pa":[], "pb":[]}) so neither participant hears the
//     other's raw audio. See media.go.
//   - Per-leg STT taps that leg's own ingress audio, before mixing, so a
//     speaker's transcript never contains their peer.
//   - The translated speech goes back via leg_tts, which injects into ONE leg's
//     mixed-minus-self output — a private channel to that listener.
//   - Deepgram Flux's eager end-of-turn lets us translate and synthesize
//     speculatively while the speaker is still finishing, stage the audio with
//     leg_tts_preflight, and then leg_tts_commit it the instant the turn really
//     ends. The commit costs no upstream round trip, which takes roughly half a
//     second off the perceived latency. See interpret.go.
//
// Like the other examples, every VoiceBlender command is issued over a single
// VSI WebSocket (Client.Events): inbound frames are piped into the client hub so
// Subscribe works; outbound commands (create_room, webrtc_offer, add_leg_to_room,
// room_routing_set, leg_stt_start, leg_tts_preflight, leg_tts_commit, …) go
// through the same *EventStream. The REST client is never used for commands.
//
// Sessions are transient and held in memory — there is no login and no Redis.
//
// Environment variables:
//
//	VOICEBLENDER_URL         VoiceBlender base URL (default http://localhost:8080/v1)
//	LISTEN_ADDR              Web console listen address (default :8093)
//	APP_ID                   Tags this app's rooms and scopes the events it acts on,
//	                         so several examples can share one VoiceBlender
//	                         (default interpreter)
//	VSI_LOG                  Log every VSI wire frame (default on; 0/false/off/no to silence)
//
//	STT_PROVIDER             Preferred transcriber (default deepgram_flux)
//	STT_FALLBACK_PROVIDER    Covers languages the preferred one cannot do
//	                         (default speechmatics). The choice is made PER
//	                         PARTICIPANT: an English speaker can be on Flux, with
//	                         its eager end-of-turn, while the Polish speaker on
//	                         the same call is on Speechmatics.
//	DEEPGRAM_API_KEY         Key for Deepgram Flux
//	SPEECHMATICS_API_KEY     Key for Speechmatics — without it, the five
//	                         languages Flux cannot do are simply not offered
//	STT_API_KEY              Fallback key for either, if you use only one
//	STT_EAGER_EOT_THRESHOLD  Flux eager end-of-turn confidence, 0.3–0.9 (default 0.7).
//	                         0 disables the speculative path entirely. Deepgram
//	                         requires it to be <= STT_EOT_THRESHOLD.
//	STT_EOT_THRESHOLD        Confidence needed to end a turn, 0.5–0.9
//	                         (unset = Deepgram's default of 0.7)
//	STT_EOT_TIMEOUT_MS       Silence that forces a turn to end, 500–60000
//	                         (unset = Deepgram's default of 5000)
//
//	TTS_PROVIDER             elevenlabs (default)
//	TTS_MODEL_ID             default eleven_flash_v2_5
//	TTS_API_KEY              TTS key (falls back to ELEVENLABS_API_KEY)
//	TTS_VOICE_FEMALE         ElevenLabs voice IDs. Each participant is heard in
//	TTS_VOICE_MALE           the voice matching the gender they select; those who
//	TTS_VOICE_DEFAULT        prefer not to say get TTS_VOICE_DEFAULT.
//
//	AUTH_USERNAME            Static username for the console (default interpreter)
//	AUTH_PASSWORD            Static password. UNSET DISABLES THE LOGIN entirely,
//	                         which is fine locally and reckless anywhere public.
//
//	SESSION_MAX_DURATION     Hard cap on one conversation (default 1h; 0 = none)
//	SESSION_IDLE_TIMEOUT     End a session after this long with nobody speaking
//	                         (default 5m; 0 = none). This is the setting that
//	                         stops an abandoned tab billing STT by the minute.
//	SESSION_EMPTY_TIMEOUT    Drop a session nobody is in (default 5m; 0 = none)
//
//	TRANSLATE_PROVIDER       deepl (default) or none
//	DEEPL_API_KEY            DeepL auth key
//	DEEPL_URL                default https://api-free.deepl.com/v2/translate
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	voiceblender "github.com/VoiceBlender/voiceblender-go"
)

// config holds the provider settings read from the environment once at start.
type config struct {
	// sttProvider is the PREFERRED transcriber; sttFallback covers the
	// languages it cannot do. The choice is made per participant, per language
	// — see config.providerForLang.
	sttProvider string
	sttFallback string
	sttKeys     map[string]string // provider → API key
	eagerEOT    float64           // 0 disables the preflight/commit path
	// eotThreshold / eotTimeoutMs are Flux's turn-detection knobs. Zero leaves
	// Deepgram's defaults (0.7 confidence, and a 5 s silence timeout that forces
	// the turn to end). Raise the timeout for speakers who pause a lot; lower it
	// to make the interpreter cut in sooner.
	eotThreshold float64
	eotTimeoutMs int
	ttsProvider  string
	ttsModelID   string
	ttsAPIKey    string
	ttsVoices    map[string]string // gender code → ElevenLabs voice id
	translateVia string

	// Session limits. Every VoiceBlender leg that is up is streaming audio to
	// the STT vendor, billed by the minute, so an abandoned tab is a meter left
	// running. Zero disables a limit.
	maxDuration  time.Duration
	idleTimeout  time.Duration
	emptyTimeout time.Duration
}

// app holds the shared interpreter resources.
type app struct {
	client *voiceblender.Client
	// stream is the current VSI WebSocket (events + commands). It's swapped on
	// reconnect, so read it via vsi() — never touch the field directly.
	stream atomic.Pointer[voiceblender.EventStream]
	log    *slog.Logger

	// appID tags everything this demo creates on VoiceBlender and scopes the
	// events it acts on. Several examples share one VoiceBlender instance, so
	// the stream carries their traffic too — see runEventLoop.
	appID string

	// vsiFake replaces the live stream in tests. Nil in every real run.
	vsiFake vsiClient

	auth   authConfig
	logins *loginStore

	cfg      config
	tr       translator
	sessions *sessionRegistry
	media    *mediaManager
	interp   *interpreter
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	baseURL := envOr("VOICEBLENDER_URL", "http://localhost:8080/v1")

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	cfg := config{
		sttProvider:  envOr("STT_PROVIDER", "deepgram_flux"),
		sttFallback:  envOr("STT_FALLBACK_PROVIDER", "speechmatics"),
		sttKeys:      sttKeysFromEnv(envOr("STT_PROVIDER", "deepgram_flux")),
		eagerEOT:     envFloat("STT_EAGER_EOT_THRESHOLD", 0.7),
		eotThreshold: envFloat("STT_EOT_THRESHOLD", 0),
		eotTimeoutMs: envInt("STT_EOT_TIMEOUT_MS", 0),
		ttsProvider:  envOr("TTS_PROVIDER", "elevenlabs"),
		ttsModelID:   envOr("TTS_MODEL_ID", "eleven_flash_v2_5"),
		ttsAPIKey:    firstEnv("TTS_API_KEY", "ELEVENLABS_API_KEY"),
		translateVia: envOr("TRANSLATE_PROVIDER", "deepl"),
		ttsVoices: map[string]string{
			"female":      envOr("TTS_VOICE_FEMALE", defaultGenderVoices["female"]),
			"male":        envOr("TTS_VOICE_MALE", defaultGenderVoices["male"]),
			"unspecified": envOr("TTS_VOICE_DEFAULT", defaultGenderVoices["unspecified"]),
		},
		maxDuration:  envDuration("SESSION_MAX_DURATION", time.Hour),
		idleTimeout:  envDuration("SESSION_IDLE_TIMEOUT", 5*time.Minute),
		emptyTimeout: envDuration("SESSION_EMPTY_TIMEOUT", 5*time.Minute),
	}

	// Deepgram requires eager_eot_threshold <= eot_threshold and rejects the
	// connection otherwise — which shows up as a leg that is simply never
	// transcribed. Clamp rather than let that happen silently.
	effectiveEOT := cfg.eotThreshold
	if effectiveEOT == 0 {
		effectiveEOT = 0.7 // Deepgram's default
	}
	if cfg.eagerEOT > effectiveEOT {
		log.Warn("STT_EAGER_EOT_THRESHOLD must not exceed STT_EOT_THRESHOLD — clamping",
			"requested", cfg.eagerEOT, "clamped_to", effectiveEOT)
		cfg.eagerEOT = effectiveEOT
	}

	tr, err := newTranslator(cfg.translateVia, log)
	if err != nil {
		log.Error("translator", "provider", cfg.translateVia, "error", err)
		os.Exit(1)
	}

	a := &app{
		client: voiceblender.New(voiceblender.WithBaseURL(baseURL)),
		log:    log,
		appID:  envOr("APP_ID", "interpreter"),
		cfg:    cfg,
		tr:     tr,
		auth: authConfig{
			Username: envOr("AUTH_USERNAME", "interpreter"),
			Password: os.Getenv("AUTH_PASSWORD"),
		},
		logins: newLoginStore(),
	}
	a.sessions = newSessionRegistry()
	a.media = newMediaManager(a)
	a.interp = newInterpreter(a)

	// VSI WebSocket (events + commands). Several examples can share one
	// VoiceBlender, so ask the server to send us only our own app's events —
	// everything we create (rooms and WebRTC legs alike) is tagged with app_id,
	// so nothing of ours is lost to the filter and nothing of theirs reaches us.
	streamOpts := []voiceblender.EventStreamOption{
		voiceblender.WithAppFilter(a.appFilter()),
	}
	// Frame logging is on by default; set VSI_LOG=0 to silence it.
	if vsiFrameLogEnabled() {
		streamOpts = append(streamOpts, voiceblender.WithFrameLogger(a.logVSIFrame))
		log.Info("VSI frame logging enabled (set VSI_LOG=0 to disable)")
	}
	// manageStream connects (retrying) and reconnects if the stream drops. Block
	// until the first connect so a.vsi() is live before the console starts.
	//
	// The stream gets its OWN context, not the signal one. On shutdown we still
	// need it to tear live sessions down (see shutdownSessions below), and a
	// stream cancelled by the same Ctrl-C would be gone before we could.
	streamCtx, stopStream := context.WithCancel(context.Background())
	defer stopStream()
	ready := make(chan struct{})
	go a.manageStream(streamCtx, streamOpts, ready)
	select {
	case <-ready:
	case <-ctx.Done():
		return
	}

	// Expire staged TTS locally before VoiceBlender's own 30 s preflight TTL, so
	// a commit is never attempted against an id the server has already dropped.
	go a.interp.sweepStaged(ctx)

	// Reap idle, over-long and abandoned sessions. This is the one that keeps a
	// forgotten tab from billing STT all afternoon.
	go a.reapSessions(ctx)

	srv := &http.Server{Addr: envOr("LISTEN_ADDR", ":8093"), Handler: a.serveHTTP()}
	go func() {
		log.Info("interpreter console", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("http server", "error", err)
			cancel()
		}
	}()
	defer func() {
		shutdownCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Info("interpreter ready",
		"base_url", baseURL, "app_id", a.appID,
		"stt", cfg.sttProvider, "eager_eot", cfg.eagerEOT,
		"tts", cfg.ttsProvider+"/"+cfg.ttsModelID, "translate", tr.Name())
	// Flux covers ten languages; Speechmatics covers far more. Which set is on
	// offer depends entirely on STT_PROVIDER, so say so rather than leaving
	// someone to wonder where their language went.
	// Which engine each language will use, so the routing is visible rather than
	// something to deduce from a failure.
	offered := cfg.offeredLanguages()
	byProvider := map[string][]string{}
	for _, l := range offered {
		prov, _ := cfg.providerForLang(l.Code)
		byProvider[prov] = append(byProvider[prov], l.Code)
	}
	for prov, codes := range byProvider {
		log.Info("languages offered", "provider", prov, "count", len(codes), "codes", strings.Join(codes, ","))
	}
	if len(offered) < len(languages) {
		missing := make([]string, 0, len(languages)-len(offered))
		for _, l := range languages {
			if !cfg.langSupported(l.Code) {
				missing = append(missing, l.Code)
			}
		}
		log.Warn("some languages are unavailable — no configured transcriber covers them",
			"codes", strings.Join(missing, ","),
			"hint", "set SPEECHMATICS_API_KEY to cover "+strings.Join(missing, ","))
	}

	log.Info("session limits",
		"max_duration", cfg.maxDuration, "idle_timeout", cfg.idleTimeout, "empty_timeout", cfg.emptyTimeout)
	if cfg.eagerEOT == 0 {
		log.Warn("eager end-of-turn disabled — translations will play a full turn late")
	}
	if !tr.Translates() {
		log.Warn("TRANSLATION IS OFF (TRANSLATE_PROVIDER=none) — each participant will hear "+
			"the other's ORIGINAL words read aloud, not a translation. This looks exactly like a "+
			"broken language setting. Set TRANSLATE_PROVIDER=deepl and DEEPL_API_KEY to translate.",
			"provider", tr.Name())
	}
	if !a.auth.enabled() {
		log.Warn("no AUTH_PASSWORD set — the console is open to anyone who can reach it")
	}
	if cfg.idleTimeout == 0 && cfg.maxDuration == 0 {
		log.Warn("no session time limits — an abandoned tab will stream audio to the STT vendor indefinitely")
	}

	a.runEventLoop(ctx)

	// Signal received. Tear down every live session before the process exits.
	//
	// This is not tidiness, it is the same spend control as the timeouts: a leg
	// left behind on the media server keeps streaming audio to the STT vendor,
	// and with this process gone there is nobody left to stop it. Killing the
	// app must not leave a meter running.
	a.shutdownSessions()
	stopStream()
}

// shutdownSessions ends every live session on the way out, on a short deadline
// so a wedged media server cannot hold the process open.
func (a *app) shutdownSessions() {
	live := a.sessions.all()
	if len(live) == 0 {
		return
	}
	a.log.Info("shutting down: ending live sessions", "count", len(live))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range live {
		a.endSession(ctx, s, "server shutting down")
	}
}

// vsi returns the VSI command surface. It is non-nil once main() has passed the
// first-connect barrier, and the underlying stream is only ever swapped (never
// cleared) afterwards. Tests set vsiFake to drive the app without a server.
func (a *app) vsi() vsiClient {
	if a.vsiFake != nil {
		return a.vsiFake
	}
	return a.stream.Load()
}

// manageStream keeps a live VSI connection: connect (retrying with backoff),
// pump events until the stream errors, then reconnect — so the app survives a
// VoiceBlender restart or a dropped socket instead of exiting.
func (a *app) manageStream(ctx context.Context, opts []voiceblender.EventStreamOption, ready chan struct{}) {
	backoff := time.Second
	first := true
	for ctx.Err() == nil {
		stream, err := a.client.Events(ctx, opts...)
		if err != nil {
			a.log.Warn("VSI connect failed, retrying", "error", err, "retry_in", backoff.String())
			if !sleepCtx(ctx, backoff) {
				return
			}
			if backoff < 15*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		a.stream.Store(stream)
		a.log.Info("VSI connected")
		if first {
			first = false
			close(ready)
		}
		// Pump events + demux command replies until the stream drops.
		err = stream.PipeTo(ctx, a.client)
		_ = stream.Close()
		if ctx.Err() != nil {
			return
		}
		a.log.Warn("VSI stream dropped, reconnecting", "error", err)
		if !sleepCtx(ctx, time.Second) {
			return
		}
	}
}

// sleepCtx sleeps for d or until ctx is cancelled; returns false if cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// runEventLoop consumes the VSI event stream. This app is almost entirely
// event-driven, so this is the spine of it:
//
//   - leg.connected     the browser's media is up; only now may STT start
//     (leg_stt_start 409s on a leg that isn't connected yet)
//   - leg.disconnected  a leg dropped; tear its half of the session down
//   - stt.turn          the hot path — see interpret.go
//   - stt.text          partial transcripts, used only for live captions
//   - tts.staged        a preflighted utterance finished synthesizing
//   - tts.finished      playback ended; the listener's leg is free again
//   - tts.error         synthesis or playback failed
//
// Several examples share one VoiceBlender instance, but the stream is filtered
// server-side to our app_id (see manageStream), so everything arriving here is
// already ours — there is nothing to sort out in the loop.
func (a *app) runEventLoop(ctx context.Context) {
	sub := a.client.Subscribe()
	defer sub.Close()

	for {
		ev, err := sub.Next(ctx)
		if err != nil {
			return
		}
		switch e := ev.(type) {
		case *voiceblender.LegConnectedEvent:
			go a.media.legConnected(e.LegID)
		case *voiceblender.LegDisconnectedEvent:
			go a.media.legGone(e.LegID)
		case *voiceblender.STTTurnEvent:
			go a.interp.onTurn(e)
		case *voiceblender.STTTextEvent:
			go a.interp.onText(e)
		case *voiceblender.TTSStagedEvent:
			go a.interp.onStaged(e)
		case *voiceblender.TTSFinishedEvent:
			go a.interp.onFinished(e)
		case *voiceblender.TTSErrorEvent:
			go a.interp.onTTSError(e)
		}
	}
}

// appFilter is the VSI app_id filter for this app: an anchored, literal match on
// our app id, so a neighbouring example called e.g. "interpreter-staging" can't
// leak in.
func (a *app) appFilter() string { return "^" + regexp.QuoteMeta(a.appID) + "$" }

// vbRoom namespaces a session id into the VoiceBlender room id. app_id keeps the
// *events* separate, but room ids are still one flat global space on the server,
// so prefixing keeps this demo from colliding with a room another example
// happens to name the same.
func (a *app) vbRoom(sessionID string) string { return a.appID + "-" + sessionID }

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// firstEnv returns the first of keys that is set, so a provider-specific key
// (DEEPGRAM_API_KEY) can stand in for the generic one (STT_API_KEY).
func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// envFloat reads a float env var, falling back to def when unset or unparseable.
func envFloat(key string, def float64) float64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

// sttKeysFromEnv resolves one API key per transcriber.
//
// STT_API_KEY stands in only for the PREFERRED provider — the "I use a single
// engine" case. It deliberately does NOT apply to the others: a provider is
// treated as available if it has a key, so letting one generic key cover
// everything would advertise languages we cannot actually transcribe and fail
// at the dial with somebody else's credentials.
func sttKeysFromEnv(preferred string) map[string]string {
	keys := map[string]string{
		"deepgram_flux": os.Getenv("DEEPGRAM_API_KEY"),
		"deepgram":      os.Getenv("DEEPGRAM_API_KEY"),
		"speechmatics":  os.Getenv("SPEECHMATICS_API_KEY"),
	}
	if generic := os.Getenv("STT_API_KEY"); generic != "" && keys[preferred] == "" {
		keys[preferred] = generic
	}
	return keys
}

// envInt reads an integer env var, falling back to def when unset or unparseable.
func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// envDuration reads a Go duration string ("90s", "10m", "1h30m") from the
// environment. An explicit "0" disables the limit; anything unparseable falls
// back to the default rather than silently disabling a spend control.
func envDuration(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return def
	}
	return d
}

// logVSIFrame logs one raw VSI wire frame. dir is "send" (a command we issued)
// or "recv" (an event or command reply from the server).
func (a *app) logVSIFrame(dir string, data []byte) {
	var env struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(data, &env)
	a.log.Info("vsi", "dir", dir, "type", env.Type, "frame", string(data))
}

// vsiFrameLogEnabled reports whether raw VSI frame logging is on.
//
// Default OFF here, unlike the push-to-talk example. That app's VSI traffic is
// sparse — a burst of frames per button press — but this one runs two
// transcribers continuously, and Flux alone emits a `stt.turn` update roughly
// every 200 ms per leg. Logging every frame buries everything else.
func vsiFrameLogEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("VSI_LOG"))) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

// isVSINotFound reports whether err is a VSI error with code 404.
func isVSINotFound(err error) bool {
	e, ok := err.(*voiceblender.VSIError)
	return ok && e.Code == 404
}

// isVSIConflict reports whether err is a VSI error with code 409.
func isVSIConflict(err error) bool {
	e, ok := err.(*voiceblender.VSIError)
	return ok && e.Code == 409
}
