// Command pbx is a small SIP PBX built on VoiceBlender.
//
// It demonstrates the full telephony surface of the voiceblender-go SDK:
//
//   - Outbound trunks where VoiceBlender REGISTERs to an upstream provider.
//   - Inbound IP-authenticated trunks — calls are trusted by matching the
//     source IP in the ringing event, with no digest challenge.
//   - Extensions authenticated on both REGISTER (registrar) and INVITE (digest
//     challenge on the ringing leg).
//   - Extension-to-extension calls, bridged through a room.
//   - Extension-to-external calls, routed out via a trunk.
//   - Inbound trunk calls land in a dial-by-extension IVR.
//
// Extensions and trunks are managed from a web console (same theme as the
// contact-centre example) and persisted in Redis.
//
// Events and commands share a single VoiceBlender VSI WebSocket
// (Client.Events): inbound frames are piped into the client hub so Subscribe
// works; outbound commands (answer_leg, create_leg, challenge_registration,
// create_sip_trunk, …) are sent through the same *EventStream.
//
// Environment variables:
//
//	VOICEBLENDER_URL   VoiceBlender base URL (default http://localhost:8080/v1)
//	REDIS_URL          Redis connection URL (required), e.g. redis://localhost:6379/0
//	LISTEN_ADDR        Management console listen address (default :8091)
//	PBX_DOMAIN         Extension AOR domain, e.g. pbx.local (default pbx.local)
//	PBX_REALM          SIP digest realm (default: PBX_DOMAIN)
//	ADMIN_PASSWORD     Console password. Empty disables auth (dev only).
//	COMPANY_NAME       Name spoken by the IVR greeting (default Acme Corp)
//	TTS_VOICE          IVR TTS voice (default Rachel)
//	TTS_PROVIDER       IVR TTS provider (default elevenlabs)
//	TTS_API_KEY        IVR TTS API key (optional if preconfigured server-side)
//	ANSWER_CODECS      Comma-separated codec preference order for answered legs
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	voiceblender "github.com/VoiceBlender/voiceblender-go"
)

// app holds shared PBX resources.
type app struct {
	client *voiceblender.Client
	// stream is the current VSI WebSocket (events + commands). It's swapped on
	// reconnect, so read it via vsi() — never touch the field directly.
	stream atomic.Pointer[voiceblender.EventStream]
	log    *slog.Logger

	domain string // extension AOR domain
	realm  string // digest realm

	companyName  string
	ttsVoice     string
	ttsProvider  string
	ttsAPIKey    string
	sttProvider  string
	sttAPIKey    string
	sttLanguage  string
	answerCodecs []string

	store    *redisStore
	tenants  *tenantStore
	exts     *extRegistry
	trunks   *trunkRegistry
	config   *configStore
	dialplan *dialplanStore
	bridges  *bridgeRegistry
	forks    *forkRegistry

	sessions      *sessionStore
	phoneSessions *phoneSessionStore
	phones        *phoneRegistry
	hub           *webHub

	superUser string // cross-tenant superadmin login (from SUPERADMIN env)
	superPass string

	calls   sync.Map // legID → *ivrCall (inbound trunk calls in the IVR)
	dpExecs sync.Map // legID → *dpExec (inbound calls walking the dial plan)
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	baseURL := envOr("VOICEBLENDER_URL", "http://localhost:8080/v1")
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		log.Error("REDIS_URL is required")
		os.Exit(1)
	}
	domain := envOr("PBX_DOMAIN", "pbx.local")

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	store, err := newRedisStore(ctx, redisURL)
	if err != nil {
		log.Error("connect redis", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	a := &app{
		client:        voiceblender.New(voiceblender.WithBaseURL(baseURL)),
		log:           log,
		domain:        domain,
		realm:         envOr("PBX_REALM", domain),
		companyName:   envOr("COMPANY_NAME", "Acme Corp"),
		ttsVoice:      envOr("TTS_VOICE", "Rachel"),
		ttsProvider:   envOr("TTS_PROVIDER", "elevenlabs"),
		ttsAPIKey:     os.Getenv("TTS_API_KEY"),
		sttProvider:   envOr("STT_PROVIDER", "deepgram"),
		sttAPIKey:     envOr("STT_API_KEY", os.Getenv("DEEPGRAM_API_KEY")),
		sttLanguage:   envOr("STT_LANGUAGE", "en"),
		answerCodecs:  splitCSV(os.Getenv("ANSWER_CODECS")),
		store:         store,
		bridges:       newBridgeRegistry(),
		forks:         newForkRegistry(),
		sessions:      newSessionStore(store),
		phoneSessions: newPhoneSessionStore(store),
		phones:        newPhoneRegistry(),
		hub:           newWebHub(),
	}
	a.tenants = newTenantStore(store)
	a.exts = newExtRegistry(store)
	a.trunks = newTrunkRegistry(store)
	a.config = newConfigStore(store)
	a.dialplan = newDialplanStore(store)
	a.tenants.notify = a.notifyChanged
	a.phones.notify = a.notifyChanged
	a.exts.deviceStatus = a.phones.status
	a.exts.notify = a.notifyChanged
	a.trunks.notify = a.notifyChanged
	a.config.notify = a.notifyChanged
	a.dialplan.notify = a.notifyChanged
	a.config.setDefault(os.Getenv("HOLD_MUSIC_URL"))
	// Cross-tenant superadmin (SUPERADMIN=user:pass); disabled when unset.
	if su := os.Getenv("SUPERADMIN"); su != "" {
		if p := strings.SplitN(su, ":", 2); len(p) == 2 {
			a.superUser, a.superPass = p[0], p[1]
			log.Info("superadmin enabled", "user", a.superUser)
		}
	}

	// Load tenants/users, extensions, and trunks from Redis. Config and dial
	// plans are per-tenant and loaded lazily on first use.
	if err := a.tenants.load(ctx); err != nil {
		log.Error("load tenants", "error", err)
		os.Exit(1)
	}
	// Restore console/softphone sessions so a restart doesn't force everyone to
	// sign in again (cookies stay valid).
	a.sessions.load(ctx)
	a.phoneSessions.load(ctx)
	if err := a.exts.load(ctx); err != nil {
		log.Error("load extensions", "error", err)
		os.Exit(1)
	}
	if err := a.trunks.load(ctx); err != nil {
		log.Error("load trunks", "error", err)
		os.Exit(1)
	}
	// Seed a default tenant + admin for local dev (SEED_TENANT=slug:name:user:pass),
	// so a fresh install has something to log into; skipped if it already exists.
	if seed := os.Getenv("SEED_TENANT"); seed != "" {
		p := strings.SplitN(seed, ":", 4)
		if len(p) == 4 {
			if err := a.tenants.ensure(ctx, p[0], p[1], p[2], p[3]); err != nil {
				log.Warn("seed tenant", "error", err)
			}
		}
	}

	// VSI WebSocket (events + commands). Frame logging (every command sent /
	// event received) is on by default; set VSI_LOG=0 to silence it.
	var streamOpts []voiceblender.EventStreamOption
	if vsiFrameLogEnabled() {
		streamOpts = append(streamOpts, voiceblender.WithFrameLogger(a.logVSIFrame))
		log.Info("VSI frame logging enabled (set VSI_LOG=0 to disable)")
	}
	// manageStream connects (retrying), pumps events, and reconnects if the
	// stream drops (e.g. VoiceBlender restart) — re-creating trunks and
	// reconciling registrations on each (re)connect. Block until the first
	// successful connect so a.vsi() is live before the console/event loop start.
	ready := make(chan struct{})
	go a.manageStream(ctx, streamOpts, ready)
	select {
	case <-ready:
	case <-ctx.Done():
		return
	}

	// Keep registration status reconciled (live events handle changes between).
	go a.registrationReconciler(ctx)

	// Start the management console.
	srv := &http.Server{Addr: envOr("LISTEN_ADDR", ":8091"), Handler: a.serveHTTP()}
	go func() {
		log.Info("management console", "addr", srv.Addr)
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

	log.Info("PBX ready", "base_url", baseURL, "domain", a.domain, "tenants", a.tenants.count())

	a.runEventLoop(ctx)
}

// vsi returns the current VSI stream. It is non-nil once main() has passed the
// first-connect barrier, and is only ever swapped (never cleared) afterwards.
func (a *app) vsi() *voiceblender.EventStream { return a.stream.Load() }

// manageStream keeps a live VSI connection: connect (retrying with backoff),
// pump events until the stream errors, then reconnect — so the PBX survives a
// VoiceBlender restart or a dropped socket instead of exiting. On every
// (re)connect it re-establishes server-side state (trunks + registrations), and
// closes ready once after the first successful connect.
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
		// Re-establish server-side state concurrently — its VSI commands need the
		// pump below to receive their replies.
		go a.onStreamUp(ctx)
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

// onStreamUp re-creates configured trunks on the server and reconciles
// registration status. Runs on every (re)connect so the PBX self-heals after a
// VoiceBlender restart; the delete-then-create avoids duplicate server-side
// trunks when only the socket dropped (the server kept the old trunk).
func (a *app) onStreamUp(ctx context.Context) {
	for _, t := range a.trunks.list() {
		a.removeTrunkFromServer(ctx, t)
		if err := a.applyTrunkToServer(ctx, t); err != nil {
			a.log.Warn("create trunk on server", "trunk", t.Name, "error", err)
		}
	}
	a.reconcileRegistrations(ctx)
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

// runEventLoop consumes the VSI event stream and dispatches the events the PBX
// cares about, each on its own goroutine so slow handlers don't stall the loop.
func (a *app) runEventLoop(ctx context.Context) {
	sub := a.client.Subscribe()
	defer sub.Close()

	for {
		ev, err := sub.Next(ctx)
		if err != nil {
			return
		}
		switch e := ev.(type) {
		case *voiceblender.LegRingingEvent:
			go a.onRinging(e)
		case *voiceblender.LegConnectedEvent:
			go a.onLegConnected(e.LegID)
		case *voiceblender.LegDisconnectedEvent:
			go a.onLegDisconnected(e.LegID, e.Cdr.Reason)
		case *voiceblender.LegHoldEvent:
			go a.onLegHold(e.LegID)
		case *voiceblender.LegUnholdEvent:
			go a.onLegUnhold(e.LegID)
		case *voiceblender.LegTransferRequestedEvent:
			go a.onTransferRequested(e)
		case *voiceblender.DTMFReceivedEvent:
			// A dial-plan gather node consumes it; otherwise it's IVR input.
			go func(legID, digit string) {
				if !a.dpOnDTMF(legID, digit) {
					a.ivrOnDTMF(legID, digit)
				}
			}(e.LegID, e.Digit)
		case *voiceblender.TTSFinishedEvent:
			// A dial-plan tts node consumes it; otherwise it's an IVR prompt.
			go func(legID, ttsID string) {
				if !a.dpOnPromptFinished(legID, ttsID) {
					a.ivrOnTTSFinished(legID, ttsID)
				}
			}(e.LegID, e.TTSID)
		case *voiceblender.PlaybackFinishedEvent:
			go a.dpOnPromptFinished(e.LegID, e.PlaybackID)
		case *voiceblender.STTTextEvent:
			go a.dpOnSTT(e.LegID, e.Text, e.IsFinal)
		case *voiceblender.TTSErrorEvent:
			a.log.Error("tts error", "leg_id", e.LegID, "error", e.Error)

		case *voiceblender.SIPRegistrationAttemptEvent:
			go a.onRegistrationAttempt(e)
		case *voiceblender.SIPRegistrationActiveEvent:
			a.onRegistrationActive(e)
		case *voiceblender.SIPRegistrationExpiredEvent:
			a.onRegistrationExpired(e)

		case *voiceblender.SIPOutboundRegistrationActiveEvent:
			a.trunks.setStatusByServerID(e.TrunkID, "registered", "")
		case *voiceblender.SIPOutboundRegistrationFailedEvent:
			reason := e.Reason
			if e.Error != "" {
				reason = e.Error
			}
			a.trunks.setStatusByServerID(e.TrunkID, "failed", reason)
		case *voiceblender.SIPOutboundRegistrationExpiredEvent:
			a.trunks.setStatusByServerID(e.TrunkID, "expired", e.Reason)
		}
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
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

// vsiFrameLogEnabled reports whether raw VSI frame logging is on. Default on;
// VSI_LOG=0/false/off/no disables it.
func vsiFrameLogEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("VSI_LOG"))) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// splitCSV parses a comma-separated list, trimming whitespace and dropping
// empty entries. Returns nil for empty input.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
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

// chooseCodec returns the first codec in prefs the caller offered (matched
// case-insensitively by name), or "" to let VoiceBlender pick its default.
func chooseCodec(prefs []string, offered []voiceblender.OfferedCodec) string {
	if len(prefs) == 0 || len(offered) == 0 {
		return ""
	}
	offeredByName := make(map[string]string, len(offered))
	for _, raw := range offered {
		var c struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &c); err != nil || c.Name == "" {
			continue
		}
		offeredByName[strings.ToLower(c.Name)] = c.Name
	}
	for _, pref := range prefs {
		if name, ok := offeredByName[strings.ToLower(pref)]; ok {
			return name
		}
	}
	return ""
}
