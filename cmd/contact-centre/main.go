// Command contact-centre is the VoiceBlender example contact-centre.
//
// Stage 1 — waiting-room front door with a live supervisor panel:
//
//	Inbound SIP call
//	  → Early media: UK ringback tone (gb_ringback) plays for 3 s
//	  → Answer → welcome TTS → place caller in their own waiting room
//	  → Loop hold music in the room
//	  → Every ANNOUNCEMENT_INTERVAL, speak the caller's live position in the queue
//
// Each caller is in a dedicated VoiceBlender room (waiting-<leg_id>), so the
// mixed-minus-self mixer guarantees callers cannot hear each other or each
// other's announcements.
//
// Events are received over VSI (WebSocket); there is no HTTP webhook server.
// The same process serves:
//   - GET  /                     the supervisor panel (embedded HTML)
//   - GET  /api/calls            JSON snapshot of currently active calls
//   - GET  /api/calls/stream     WebSocket — bidirectional supervisor channel
//     server → client: snapshot / webrtc.answer / webrtc.candidate / webrtc.error / listen.error
//     client → server: webrtc.offer / webrtc.candidate / webrtc.hangup / listen.start {room_id} / listen.stop
//   - GET  /agent                the agent panel: name-only login + queue dashboard
//   - POST /api/agents/login     {name} → {agent_id, name}
//   - POST /api/agents/logout    {agent_id}
//   - GET  /api/agents/whoami    ?agent_id=… → {name} or 404
//   - GET  /api/agent/stream     ?agent_id=… → WebSocket: bidirectional
//     server → client: snapshot / webrtc.answer / webrtc.candidate / webrtc.error / call.error
//     client → server: webrtc.offer / webrtc.candidate / webrtc.hangup / call.answer / call.hangup
//   - GET  /moh/new_music.mp3    hold-music MP3 (VoiceBlender fetches this)
//
// Call log:
//
//	Every completed call is appended to the store selected by CALL_LOG_BACKEND
//	("memory" — default, in-process ring buffer of CALL_LOG_MAX entries; or
//	"redis" — any Redis-compatible server via CALL_LOG_REDIS_URL). The
//	supervisor snapshot includes the most-recent CALL_LOG_LIMIT entries.
//
// Environment variables (see .env.example):
//
//	VOICEBLENDER_URL       VoiceBlender base URL (default: http://localhost:8080/v1)
//	LISTEN_ADDR            HTTP server bind address (default: :8090)
//	HOLD_MUSIC_URL         URL VoiceBlender fetches hold music from (default: http://localhost:8090/moh/new_music.mp3)
//	ANNOUNCEMENT_INTERVAL  Time between queue-position announcements (default: 20s)
//	TTS_VOICE              TTS voice name (default: Rachel)
//	TTS_PROVIDER           TTS provider name (default: elevenlabs)
//	TTS_API_KEY            TTS API key (optional if pre-configured in VoiceBlender)
//	STT_LANGUAGE           STT language code (default: en)
//	STT_PROVIDER           STT provider name (default: elevenlabs)
//	STT_API_KEY            STT API key (optional if pre-configured in VoiceBlender)
//	ANSWER_CODECS          Comma-separated codec preference order for inbound early-media + answer SDP, e.g. "opus,PCMA,PCMU". The first one the caller offered wins; if none match, the server's default order is used. (default: unset)
//	SUPERVISOR_CALLER_MASK Privacy mask for caller numbers on the supervisor view. "last4" (default) preserves only the last 4 digits; "hidden" replaces with "caller"; "full" disables masking. Agent panel always sees the real number.
//	SLA_THRESHOLD          Service-level threshold for KPI tile (default: 20s)
//	METRICS_WINDOW         Rolling window for KPI averages and counts (default: 30m)
//	TRANSFER_RING_TIMEOUT  How long a blind transfer rings before auto-fail (default: 30s)
//	SUPERVISOR_PASSWORD    Static password to access the supervisor panel (default: unset, no auth)
//	AGENT_PASSWORD         Static password to access the agent panel (default: unset, no auth)
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	voiceblender "github.com/VoiceBlender/voiceblender-go"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

//go:embed web/index.html
var indexHTML []byte

//go:embed web/agent.html
var agentHTML []byte

type app struct {
	client         *voiceblender.Client
	log            *slog.Logger
	holdMusicURL   string
	announceEvery  time.Duration
	ttsVoice       string
	ttsProvider    string
	ttsAPIKey      string
	sttLanguage    string
	sttProvider    string
	sttAPIKey      string
	answerCodecs   []string // codec preference order for inbound answer
	supervisorMask string   // caller-number mask mode for the supervisor view
	slaThreshold   time.Duration
	metricsWindow  time.Duration

	reg                   *registry
	agents                *agentRegistry
	callLog               LogStore
	transcripts           *transcriptStore
	supervisorLegs        sync.Map // legID (string) -> struct{}
	supervisors           *supervisorRegistry
	intercoms             *intercomRegistry
	transfers             *transferRegistry
	agentOutboxes         sync.Map      // agentID (string) -> chan<- any (latest live WS only)
	supervisorActiveCalls sync.Map      // *supervisorSession -> string (customer leg id) for transferred-to-supervisor calls
	transferTimeout       time.Duration // TRANSFER_RING_TIMEOUT

	auth     authConfig
	sessions *sessionStore
}

// defaultCallRoomMatrix returns the role-routing matrix applied to every
// per-caller waiting/call room when it's created. The mixer enforces it:
//   - customer hears only agent — never any supervisor
//   - agent hears customer + any supervisor in the room (whisper is just
//     the supervisor un-muting their mic; the matrix is always permissive)
//   - supervisor hears caller + agent (silent monitor unless they un-mute)
//
// Whisper is enforced purely by the supervisor's mic mute state: while
// muted they send silent frames (browser-side track.enabled = false), so
// the mixer adds 0 to the agent's stream and the agent hears nothing
// extra. Un-muting → audio flows to the agent only.
//
// Multiple supervisors in the same room each get the same view; none of them
// hear each other (their own role is excluded from their row).
func defaultCallRoomMatrix() map[string][]string {
	return map[string][]string{
		"customer":   {"agent"},
		"agent":      {"customer", "supervisor"},
		"supervisor": {"customer", "agent"},
	}
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	baseURL := envOr("VOICEBLENDER_URL", "http://localhost:8080/v1")
	listenAddr := envOr("LISTEN_ADDR", ":8090")
	holdMusicURL := envOr("HOLD_MUSIC_URL", "http://localhost"+listenAddr+"/moh/new_music.mp3")

	interval, err := time.ParseDuration(envOr("ANNOUNCEMENT_INTERVAL", "20s"))
	if err != nil {
		log.Error("invalid ANNOUNCEMENT_INTERVAL", "error", err)
		os.Exit(1)
	}

	slaThreshold := parseDurationOr(log, "SLA_THRESHOLD", "20s", 20*time.Second)
	metricsWindow := parseDurationOr(log, "METRICS_WINDOW", "30m", 30*time.Minute)
	transferRingTimeout := parseDurationOr(log, "TRANSFER_RING_TIMEOUT", "30s", 30*time.Second)

	callLogMax := envInt("CALL_LOG_MAX", 200)
	callLog, callLogBackend, err := makeCallLog(context.Background(), callLogMax)
	if err != nil {
		log.Error("call log backend", "error", err)
		os.Exit(1)
	}
	defer callLog.Close()

	a := &app{
		client:          voiceblender.New(voiceblender.WithBaseURL(baseURL)),
		log:             log,
		holdMusicURL:    holdMusicURL,
		announceEvery:   interval,
		ttsVoice:        envOr("TTS_VOICE", "Rachel"),
		ttsProvider:     envOr("TTS_PROVIDER", "elevenlabs"),
		ttsAPIKey:       os.Getenv("TTS_API_KEY"),
		sttLanguage:     envOr("STT_LANGUAGE", "en"),
		sttProvider:     envOr("STT_PROVIDER", "elevenlabs"),
		sttAPIKey:       os.Getenv("STT_API_KEY"),
		answerCodecs:    splitCSV(os.Getenv("ANSWER_CODECS")),
		supervisorMask:  resolveMaskMode(os.Getenv("SUPERVISOR_CALLER_MASK")),
		slaThreshold:    slaThreshold,
		metricsWindow:   metricsWindow,
		reg:             newRegistry(),
		agents:          newAgentRegistry(),
		callLog:         callLog,
		transcripts:     newTranscriptStore(),
		supervisors:     newSupervisorRegistry(),
		intercoms:       newIntercomRegistry(),
		transfers:       newTransferRegistry(),
		transferTimeout: transferRingTimeout,
		auth: authConfig{
			SupervisorPassword: os.Getenv("SUPERVISOR_PASSWORD"),
			AgentPassword:      os.Getenv("AGENT_PASSWORD"),
		},
		sessions: newSessionStore(),
	}
	// Agent login/logout/leg-changes should also wake supervisor panels.
	a.agents.onChange(a.reg.NotifyChanged)
	// Supervisor presence changes refresh the agent panels (so the
	// "Call supervisor" dropdown gains/loses entries live) and supervisor
	// panels (in case they ever display a presence list themselves).
	a.supervisors.onChange(a.reg.NotifyChanged)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	go a.serveHTTP(ctx, listenAddr)

	// Pump VSI events into the client's event hub.
	go func() {
		if err := a.client.RunEventStream(ctx); err != nil && ctx.Err() == nil {
			log.Error("event stream", "error", err)
			cancel()
		}
	}()

	ringings := a.client.Subscribe(voiceblender.EventLegRinging)
	defer ringings.Close()

	sttEvents := a.client.Subscribe(voiceblender.EventSTTText)
	defer sttEvents.Close()
	go a.pumpSTT(ctx, sttEvents)

	log.Info("contact centre ready",
		"voiceblender_url", baseURL,
		"operator_panel", "http://localhost"+listenAddr+"/",
		"agent_panel", "http://localhost"+listenAddr+"/agent",
		"hold_music_url", holdMusicURL,
		"announcement_interval", interval,
		"call_log_backend", callLogBackend,
		"call_log_max", callLogMax,
		"sla_threshold", slaThreshold,
		"metrics_window", metricsWindow,
		"auth_supervisor", a.auth.requiresAuth(roleSupervisor),
		"auth_agent", a.auth.requiresAuth(roleAgent),
	)

	for {
		ev, err := ringings.Next(ctx)
		if err != nil {
			return
		}
		ring := ev.(*voiceblender.LegRingingEvent)
		if ring.LegType != string(voiceblender.LegTypeSIPInbound) {
			log.Debug("skipping non-inbound leg", "leg_id", ring.LegID, "leg_type", ring.LegType)
			continue
		}
		log.Info("ringing", "leg_id", ring.LegID, "from", ring.From, "to", ring.To)
		answer := a.reg.addRinging(ring.LegID, ring.From, ring.To)
		go a.handle(ctx, ring, answer)
	}
}

// pumpSTT consumes stt.text events for every active call's room. For each
// final transcript it resolves the speaker label (customer / agent name /
// supervisor), appends to the per-call store, and wakes supervisor panels
// so the next snapshot carries the new line.
//
// Trade-off accepted: every transcript event causes a full snapshot to
// every supervisor, with the running transcript baked into every in_call
// row. Fine for the example's call volume; a production version would
// push incremental transcript deltas.
func (a *app) pumpSTT(ctx context.Context, sub *voiceblender.Subscription) {
	for {
		ev, err := sub.Next(ctx)
		if err != nil {
			return
		}
		stt, ok := ev.(*voiceblender.STTTextEvent)
		if !ok || !stt.IsFinal || stt.Text == "" {
			continue
		}
		call := a.reg.callByRoom(stt.RoomID)
		if call == nil {
			continue
		}
		speaker := ""
		switch {
		case stt.LegID == call.LegID:
			speaker = "customer"
		case stt.LegID == call.AnsweredAgentLeg:
			speaker = call.AnsweredByName
			if speaker == "" {
				speaker = "agent"
			}
		default:
			if _, ok := a.supervisorLegs.Load(stt.LegID); ok {
				speaker = "Supervisor"
			}
		}
		if speaker == "" {
			continue // unrecognised leg — defensive drop
		}
		a.transcripts.Append(call.LegID, TranscriptLine{
			LegID:   stt.LegID,
			Speaker: speaker,
			Text:    stt.Text,
			At:      time.Now(),
		})
		a.reg.NotifyChanged()
	}
}

// handle drives one inbound call: ringback → answer → welcome → waiting room
// with looping hold music and periodic queue-position announcements. If an
// agent claims the call via call.answer, the waiting-room phase ends and the
// goroutine transitions into bridge-monitor mode until either party
// disconnects.
func (a *app) handle(ctx context.Context, ring *voiceblender.LegRingingEvent, answerCh <-chan struct{}) {
	leg := a.client.Leg(ring.LegID)
	log := a.log.With("leg_id", leg.ID)
	// Order matters: defers run LIFO, so we register reg.remove FIRST (it
	// runs last) and the log-append SECOND (it runs first, while the call
	// is still in the registry and lookup can find it).
	defer a.reg.remove(leg.ID)
	defer a.transcripts.Drop(leg.ID)
	defer func() {
		if call := a.reg.lookup(leg.ID); call != nil {
			entry := newLogEntry(call, a.transcripts.Get(leg.ID))
			if err := a.callLog.Append(context.Background(), entry); err != nil {
				log.Warn("call log append", "error", err)
			} else {
				a.reg.NotifyChanged() // refresh supervisor panels
			}
		}
	}()

	sub := leg.Subscribe(
		voiceblender.EventLegConnected,
		voiceblender.EventLegDisconnected,
		voiceblender.EventTTSFinished,
		voiceblender.EventTTSError,
	)
	defer sub.Close()

	// --- 1. Ringback: early media + UK ringback tone for 3 s. -------------
	// Negotiate the media codec: walk our ANSWER_CODECS preference order
	// and pick the first one the caller actually offered (offered_codecs
	// on the ringing event). The same codec is locked in for both the
	// 183 early-media SDP and the 200 OK answer SDP. Empty (no preference
	// matched, or none configured) leaves the server's default order.
	codec := chooseCodec(a.answerCodecs, ring.OfferedCodecs)
	log.Info("cmd", "action", "early_media", "codec", codec)
	if _, err := leg.EarlyMedia(ctx, voiceblender.EarlyMediaLegRequest{Codec: codec}); err != nil {
		log.Warn("early media not available, answering immediately", "error", err)
	} else {
		log.Info("cmd", "action", "play_leg", "tone", "gb_ringback")
		pb, err := leg.Play(ctx, voiceblender.PlayTone("gb_ringback"))
		if err != nil {
			log.Warn("play ringback", "error", err)
		} else {
			if disc := waitOrDisconnect(ctx, sub, 3*time.Second); disc != nil {
				log.Info("caller hung up during ringback", "reason", disc.Cdr.Reason)
				return
			}
			log.Info("cmd", "action", "stop_play_leg", "playback_id", pb.PlaybackID)
			if _, err := leg.StopPlay(context.Background(), pb.PlaybackID); err != nil && !voiceblender.IsNotFound(err) {
				log.Warn("stop ringback", "error", err)
			}
		}
	}

	// --- 2. Answer. --------------------------------------------------------
	// Reuse the codec negotiated above. If early media succeeded the codec
	// is already locked in at the 183; passing it again is harmless and
	// keeps the answer correct on the path where early media was skipped.
	log.Info("cmd", "action", "answer_leg", "codec", codec)
	if _, err := leg.Answer(ctx, voiceblender.AnswerLegRequest{Codec: codec}); err != nil {
		log.Error("answer leg", "error", err)
		return
	}

	// --- 3. Wait for leg.connected (media fully up). -----------------------
	if !waitForConnected(ctx, sub) {
		log.Info("caller hung up before connected")
		return
	}

	// --- 4. Welcome message. -----------------------------------------------
	if !a.playTTSAndWait(ctx, log, leg, sub, "Welcome to the Voiceblender's contact centre example.") {
		return
	}

	// --- 5. Per-caller waiting room. ---------------------------------------
	roomID := "waiting-" + leg.ID

	log.Info("cmd", "action", "create_room", "room", roomID)
	if _, err := a.client.CreateRoom(ctx, voiceblender.CreateRoomRequest{ID: roomID}); err != nil && !voiceblender.IsConflict(err) {
		log.Error("create room", "room", roomID, "error", err)
		return
	}
	room := a.client.Room(roomID)
	defer func() {
		if _, err := room.Delete(context.Background()); err != nil && !voiceblender.IsNotFound(err) {
			log.Warn("delete room", "room", roomID, "error", err)
		}
	}()

	// Install the default role-based routing matrix for this call. Roles are
	// assigned per-leg as legs join (Role on AddLegRequest). The matrix
	// controls who hears whom — see defaultCallRoomMatrix for the semantics.
	if _, err := room.SetRouting(ctx, voiceblender.RoomRoutingRequest{Matrix: defaultCallRoomMatrix()}); err != nil {
		log.Warn("set room routing", "room", roomID, "error", err)
	}

	log.Info("cmd", "action", "add_leg_to_room", "room", roomID, "role", "customer")
	if _, err := room.AddLeg(ctx, voiceblender.AddLegRequest{LegID: leg.ID, Role: "customer"}); err != nil {
		log.Error("add leg to room", "room", roomID, "error", err)
		return
	}
	a.reg.markQueued(leg.ID, roomID)

	// --- 6. Looping hold music in the room. --------------------------------
	holdReq := voiceblender.PlayURL(a.holdMusicURL, "audio/mpeg")
	holdReq.Repeat = -1
	log.Info("cmd", "action", "play_room", "room", roomID, "url", a.holdMusicURL)
	holdPB, err := room.Play(ctx, holdReq)
	if err != nil {
		log.Warn("play hold music", "room", roomID, "error", err)
	}
	holdPlaybackID := ""
	if holdPB != nil {
		holdPlaybackID = holdPB.PlaybackID
	}

	// --- 7. Waiting room: announce on a timer, watch for disconnect, and
	//        listen for an agent claiming the call.
	announce := time.NewTimer(0) // fire immediately for the first announcement
	defer announce.Stop()
	for {
		select {
		case <-ctx.Done():
			return

		case ev := <-sub.Events():
			if d, ok := ev.(*voiceblender.LegDisconnectedEvent); ok {
				log.Info("caller hung up while holding", "reason", d.Cdr.Reason)
				return
			}

		case <-announce.C:
			if !a.announceAndWait(ctx, log, leg, room, holdPlaybackID, sub) {
				return
			}
			announce.Reset(a.announceEvery)

		case <-answerCh:
			a.bridgeAndMonitor(ctx, log, leg, sub, room, holdPlaybackID)
			return
		}
	}
}

// bridgeAndMonitor runs once an agent has claimed the call. It tears down
// the waiting-room experience, adds the agent's WebRTC leg to the room, and
// blocks until either party disconnects. The caller's per-leg cleanup
// (room.Delete, registry.remove) runs in handle's defers after this returns.
func (a *app) bridgeAndMonitor(ctx context.Context, log *slog.Logger, leg *voiceblender.Leg, sub *voiceblender.Subscription, room *voiceblender.Room, holdPlaybackID string) {
	bridge := a.reg.lookup(leg.ID)
	if bridge == nil || bridge.AnsweredAgentLeg == "" {
		log.Warn("bridge wake without bridge info")
		return
	}
	log = log.With("agent", bridge.AnsweredByName, "agent_leg_id", bridge.AnsweredAgentLeg)
	log.Info("agent answered, bridging")

	// Stop the hold music; from now on the agent is the audio source.
	if holdPlaybackID != "" {
		if _, err := room.StopPlay(ctx, holdPlaybackID); err != nil && !voiceblender.IsNotFound(err) {
			log.Warn("stop hold music on bridge", "playback_id", holdPlaybackID, "error", err)
		}
	}

	// Add the agent's WebRTC leg to the caller's room. The mixer now bridges
	// both legs through the routing matrix (customer↔agent by default; the
	// matrix already excludes any supervisor's audio from reaching the
	// customer, even before whisper is involved).
	if _, err := room.AddLeg(ctx, voiceblender.AddLegRequest{LegID: bridge.AnsweredAgentLeg, Role: "agent"}); err != nil {
		log.Error("add agent leg to room", "error", err)
		// Best-effort: hang up the caller so the agent isn't left dangling.
		_, _ = leg.Hangup(context.Background(), voiceblender.DeleteLegRequest{})
		return
	}

	// Start room-wide STT so the supervisor can see a live transcript. The
	// server spins up one transcriber per leg in the room, so customer +
	// agent + any later-joining supervisor each get individually attributed
	// stt.text events. Best-effort: if STT can't start (e.g. no API key),
	// log and continue with the call.
	if _, err := room.STT(ctx, voiceblender.STTRequest{
		Language: a.sttLanguage,
		Provider: a.sttProvider,
		APIKey:   a.sttAPIKey,
		Partial:  false,
	}); err != nil {
		log.Warn("start room stt", "room", room.ID, "error", err)
	} else {
		defer func() {
			if _, err := room.StopSTT(context.Background()); err != nil && !voiceblender.IsNotFound(err) {
				log.Warn("stop room stt", "room", room.ID, "error", err)
			}
		}()
	}
	// On exit, take the agent's leg back out of the room so a subsequent
	// room.Delete doesn't kill it. The leg itself stays alive for the next
	// call.
	defer func() {
		if _, err := room.RemoveLeg(context.Background(), bridge.AnsweredAgentLeg); err != nil && !voiceblender.IsNotFound(err) {
			log.Warn("remove agent leg from room on bridge end", "error", err)
		}
	}()

	// Watch the agent's WebRTC leg for disconnect. The subscription is
	// swappable: a successful transfer pushes a transferOutcome on the
	// call's transferSignal, after which we close the old sub and open
	// one for the new agent leg without restarting the goroutine.
	agentSub := a.client.Leg(bridge.AnsweredAgentLeg).Subscribe(voiceblender.EventLegDisconnected)
	transferSig := a.reg.transferChan(leg.ID)

	for {
		select {
		case <-ctx.Done():
			agentSub.Close()
			return

		case ev := <-sub.Events():
			if d, ok := ev.(*voiceblender.LegDisconnectedEvent); ok {
				log.Info("caller hung up on bridged call", "reason", d.Cdr.Reason)
				agentSub.Close()
				// Fail any in-flight transfer so the target's modal closes
				// and the original agent sees a clear end-reason.
				a.failTransferOnCustomerHangup(leg.ID, log)
				return
			}

		case ev := <-agentSub.Events():
			if _, ok := ev.(*voiceblender.LegDisconnectedEvent); ok {
				// Attended-transfer race #1: a transfer is in flight for
				// this call. If we're still consulting, this disconnect
				// means the original agent left the consult — that's our
				// completion trigger. Otherwise (cancelled / completed /
				// failed mid-finalize) the disconnect is just consult-
				// room teardown noise; the post-finalize bridge-end
				// cleanup will run via transferSignal or the failure
				// path is already restoring the caller.
				if t := a.transfers.ByCall(leg.ID); t != nil {
					if t.State == transferConsulting {
						go a.completeAttendedTransfer(context.Background(), t.ID, log)
					}
					continue
				}
				// Attended-transfer race #2: a transfer already swapped
				// the agent under us — this disconnect is for the former
				// leg. Wait for the transferSignal that's already in
				// flight (or just on its way).
				if cur := a.reg.lookup(leg.ID); cur != nil && cur.AnsweredAgentLeg != bridge.AnsweredAgentLeg {
					continue
				}
				log.Info("agent dropped on bridged call — hanging up caller")
				if _, err := leg.Hangup(context.Background(), voiceblender.DeleteLegRequest{}); err != nil && !voiceblender.IsNotFound(err) {
					log.Warn("hangup caller after agent drop", "error", err)
				}
				agentSub.Close()
				return
			}

		case out, ok := <-transferSig:
			if !ok {
				transferSig = nil
				continue
			}
			// Transfer completed — swap our agent-leg subscription.
			log.Info("transfer completed; swapping agent leg watcher",
				"old_leg", bridge.AnsweredAgentLeg, "new_leg", out.NewAgentLeg, "new_agent", out.NewAgentName)
			agentSub.Close()
			agentSub = a.client.Leg(out.NewAgentLeg).Subscribe(voiceblender.EventLegDisconnected)
			// Update local view so logs reflect the new agent.
			bridge.AnsweredAgentLeg = out.NewAgentLeg
			bridge.AnsweredByName = out.NewAgentName
			bridge.AnsweredByAgentID = out.NewAgentID
			log = log.With("agent", out.NewAgentName, "agent_leg_id", out.NewAgentLeg)
		}
	}
}

// announceAndWait speaks the caller's current queue position, ducking and
// restoring the hold music around it. Returns true if we should keep looping,
// false if the call ended.
func (a *app) announceAndWait(ctx context.Context, log *slog.Logger, leg *voiceblender.Leg, room *voiceblender.Room, holdPlaybackID string, sub *voiceblender.Subscription) bool {
	pos := a.reg.queuePosition(leg.ID)
	if pos == 0 {
		return false
	}

	var text string
	if pos == 1 {
		text = "You are next in the queue. Thank you for holding."
	} else {
		text = fmt.Sprintf("You are number %d in the queue. Thank you for holding.", pos)
	}

	if holdPlaybackID != "" {
		log.Info("cmd", "action", "volume_play_room", "room", room.ID, "playback_id", holdPlaybackID, "volume", -6)
		if _, err := room.VolumePlay(ctx, holdPlaybackID, voiceblender.VolumeRequest{Volume: -6}); err != nil && !voiceblender.IsNotFound(err) {
			log.Warn("duck hold music", "room", room.ID, "error", err)
		}
		defer func() {
			log.Info("cmd", "action", "volume_play_room", "room", room.ID, "playback_id", holdPlaybackID, "volume", 0)
			if _, err := room.VolumePlay(context.Background(), holdPlaybackID, voiceblender.VolumeRequest{Volume: 0}); err != nil && !voiceblender.IsNotFound(err) {
				log.Warn("restore hold music volume", "room", room.ID, "error", err)
			}
		}()
	}

	return a.playTTSAndWait(ctx, log, leg, sub, text)
}

// playTTSAndWait plays a TTS prompt and blocks until the matching tts.finished
// arrives, or a tts.error for the same id, or the caller disconnects. Returns
// false if the call ended (disconnect or ctx cancel); true otherwise.
func (a *app) playTTSAndWait(ctx context.Context, log *slog.Logger, leg *voiceblender.Leg, sub *voiceblender.Subscription, text string) bool {
	log.Info("cmd", "action", "tts_leg", "text", text)
	resp, err := leg.PlayTTS(ctx, voiceblender.TTSRequest{
		Text:     text,
		Voice:    a.ttsVoice,
		Provider: a.ttsProvider,
		APIKey:   a.ttsAPIKey,
	})
	if err != nil {
		log.Error("tts", "error", err)
		return true
	}
	ttsID := resp.TTSID

	for {
		select {
		case <-ctx.Done():
			return false
		case ev := <-sub.Events():
			switch e := ev.(type) {
			case *voiceblender.LegDisconnectedEvent:
				log.Info("caller hung up during tts", "reason", e.Cdr.Reason)
				return false
			case *voiceblender.TTSFinishedEvent:
				if e.TTSID != ttsID {
					continue
				}
				return true
			case *voiceblender.TTSErrorEvent:
				if e.TTSID != ttsID {
					continue
				}
				log.Warn("tts error", "error", e.Error)
				return true
			}
		}
	}
}

func waitForConnected(ctx context.Context, sub *voiceblender.Subscription) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case ev := <-sub.Events():
			switch ev.(type) {
			case *voiceblender.LegConnectedEvent:
				return true
			case *voiceblender.LegDisconnectedEvent:
				return false
			}
		}
	}
}

func waitOrDisconnect(ctx context.Context, sub *voiceblender.Subscription, d time.Duration) *voiceblender.LegDisconnectedEvent {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			return nil
		case ev := <-sub.Events():
			if disc, ok := ev.(*voiceblender.LegDisconnectedEvent); ok {
				return disc
			}
		}
	}
}

// serveHTTP runs the panel + API + hold-music HTTP server.
func (a *app) serveHTTP(ctx context.Context, addr string) {
	mux := http.NewServeMux()
	// Public routes — login form + logout + hold-music for the VoiceBlender server.
	mux.HandleFunc("/login", a.handleLogin)
	mux.HandleFunc("/logout", a.handleLogout)
	mux.HandleFunc("/api/auth/whoami", a.handleAuthWhoami)
	mux.Handle("/moh/", http.StripPrefix("/moh/", http.FileServer(http.Dir("cmd/contact-centre/assets"))))
	// Supervisor-gated routes (no-op gate if SUPERVISOR_PASSWORD is unset).
	mux.Handle("/", a.requireRole(roleSupervisor, http.HandlerFunc(a.handleIndex)))
	mux.Handle("/api/calls", a.requireRole(roleSupervisor, http.HandlerFunc(a.handleCallsSnapshot)))
	mux.Handle("/api/calls/stream", a.requireRole(roleSupervisor, http.HandlerFunc(a.handleCallsStream)))
	// Agent-gated routes (no-op gate if AGENT_PASSWORD is unset).
	mux.Handle("/agent", a.requireRole(roleAgent, http.HandlerFunc(a.handleAgentIndex)))
	mux.Handle("/api/agents/login", a.requireRole(roleAgent, http.HandlerFunc(a.handleAgentLogin)))
	mux.Handle("/api/agents/logout", a.requireRole(roleAgent, http.HandlerFunc(a.handleAgentLogout)))
	mux.Handle("/api/agents/whoami", a.requireRole(roleAgent, http.HandlerFunc(a.handleAgentWhoami)))
	mux.Handle("/api/agent/stream", a.requireRole(roleAgent, http.HandlerFunc(a.handleAgentStream)))

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		a.log.Error("listen", "addr", addr, "error", err)
		os.Exit(1)
	}
	a.log.Info("http listening", "addr", ln.Addr())

	srv := &http.Server{Handler: mux}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		a.log.Error("http server", "error", err)
		os.Exit(1)
	}
}

func (a *app) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(indexHTML)
}

func (a *app) handleCallsSnapshot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(a.reg.snapshot())
}

// supervisorSession holds the per-WS state for a supervisor: their WebRTC
// "listen" leg id (if they've offered) and the room they're currently
// listening to (if any). Mutated by the reader, read by the writer; mu
// guards both.
type supervisorSession struct {
	mu              sync.Mutex
	webrtcLegID     string
	listeningRoomID string
}

func (s *supervisorSession) get() (legID, roomID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.webrtcLegID, s.listeningRoomID
}

func (s *supervisorSession) setLeg(legID string) (prev string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev = s.webrtcLegID
	s.webrtcLegID = legID
	return prev
}

func (s *supervisorSession) setRoom(roomID string) (prev string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev = s.listeningRoomID
	s.listeningRoomID = roomID
	return prev
}

// handleCallsStream is the supervisor's bidirectional channel: snapshot push
// plus WebRTC signaling for silent-monitor listening (one shared protocol
// shape with the agent stream so the client code stays small).
func (a *app) handleCallsStream(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		a.log.Warn("ws accept (supervisor)", "error", err)
		return
	}
	defer c.CloseNow()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Derive the supervisor's display identity from the auth session,
	// when one exists. Falls back to "supervisor" so the intercom
	// feature stays usable when SUPERVISOR_PASSWORD is unset.
	username := supervisorUsernameFromRequest(a, r)

	log := a.log.With("kind", "supervisor", "username", username)
	log.Info("supervisor stream connected")

	sess := &supervisorSession{}
	outbox := make(chan any, 16)

	// Register presence so the agent panel's dropdown picks us up and
	// so the intercom pump can fan messages out to us by username.
	presence := &supervisorPresence{Session: sess, Username: username, Outbox: outbox}
	a.supervisors.Add(presence)
	defer a.supervisors.Remove(sess)

	sub := a.reg.subscribe()
	defer a.reg.unsubscribe(sub)

	// On WS close, end any intercom this session was involved in
	// (incoming-modal targets get cleared; an answered intercom drops).
	defer a.cleanupIntercomsOnSupervisorDisconnect(presence)
	// Same for transfers: fail ringing-to-this-username with no other
	// tabs left; hang up any answered transferred customer call.
	defer a.cleanupTransfersOnSupervisorDisconnect(presence)

	go a.readSupervisorMessages(ctx, cancel, c, sess, presence, outbox, log)

	// Tear down any leg this session created when the WS closes.
	defer func() {
		legID, roomID := sess.get()
		if roomID != "" && legID != "" {
			_, _ = a.client.Room(roomID).RemoveLeg(context.Background(), legID)
		}
		if legID != "" {
			a.supervisorLegs.Delete(legID)
			if _, err := a.client.Leg(legID).Hangup(context.Background(), voiceblender.DeleteLegRequest{}); err != nil && !voiceblender.IsNotFound(err) {
				log.Warn("hangup supervisor leg on ws close", "leg_id", legID, "error", err)
			} else {
				log.Info("supervisor leg hung up", "leg_id", legID)
			}
		}
	}()

	if err := writeAgentMsg(ctx, c, a.supervisorSnapshot(sess, presence)); err != nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.trigger:
			if err := writeAgentMsg(ctx, c, a.supervisorSnapshot(sess, presence)); err != nil {
				return
			}
		case msg := <-outbox:
			if err := writeAgentMsg(ctx, c, msg); err != nil {
				return
			}
		}
	}
}

// roomStillExists reports whether roomID corresponds to a live customer
// call (in `calls`) or an active intercom. Used by the supervisor
// snapshot to scrub a stale session room reference when the call /
// intercom it pointed at has ended.
func (a *app) roomStillExists(roomID string, calls []callView) bool {
	if roomID == "" {
		return false
	}
	for _, c := range calls {
		if c.RoomID == roomID {
			return true
		}
	}
	a.intercoms.mu.Lock()
	defer a.intercoms.mu.Unlock()
	for _, ic := range a.intercoms.byID {
		if ic.RoomID == roomID {
			return true
		}
	}
	return false
}

// supervisorSnapshot builds the supervisor view: every active call, every
// logged-in agent (with their current call denormalized in), and the
// listening state of this particular supervisor session.
func (a *app) supervisorSnapshot(sess *supervisorSession, presence *supervisorPresence) map[string]any {
	snap := a.reg.snapshot()
	agents := a.agents.list()
	enriched := make([]map[string]any, len(agents))
	for i, ag := range agents {
		entry := map[string]any{
			"agent_id":     ag.ID,
			"name":         ag.Name,
			"logged_in_at": ag.LoggedInAt,
		}
		if call := a.reg.callAnsweredBy(ag.ID); call != nil {
			// Mask the caller number in the agent's "current call" column
			// the same way as the active-call list — privacy applies to
			// every supervisor-side surface uniformly.
			masked := *call
			masked.From = maskCaller(masked.From, a.supervisorMask)
			entry["current_call"] = masked
		}
		enriched[i] = entry
	}
	_, roomID := sess.get()
	// If the room the session believes it's in no longer exists (the call
	// ended, or the intercom was torn down), clear the stale id so the
	// client's listen pill returns to "not listening" instead of churning
	// through "connecting" → "audio failed" when the PC eventually notices.
	if roomID != "" && !a.roomStillExists(roomID, snap.Calls) {
		sess.setRoom("")
		roomID = ""
	}
	logEntries, err := a.callLog.List(context.Background(), 100)
	if err != nil {
		a.log.Warn("call log list", "error", err)
		logEntries = nil
	}
	// Mask historical caller numbers. callLog.List returns a fresh slice
	// for both backends, so mutating it in place is safe.
	for i := range logEntries {
		logEntries[i].From = maskCaller(logEntries[i].From, a.supervisorMask)
	}
	// Decorate active in_call rows with their running transcript so the
	// supervisor's modal can re-render straight off the snapshot. Other
	// states have no transcript yet, so they pass through unchanged.
	calls := make([]map[string]any, len(snap.Calls))
	for i, c := range snap.Calls {
		row := map[string]any{
			"leg_id":               c.LegID,
			"from":                 maskCaller(c.From, a.supervisorMask),
			"to":                   c.To,
			"state":                c.State,
			"position":             c.Position,
			"started_at":           c.StartedAt,
			"room_id":              c.RoomID,
			"answered_by_agent_id": c.AnsweredByAgentID,
			"answered_by_name":     c.AnsweredByName,
			"answered_agent_leg":   c.AnsweredAgentLeg,
			"answered_at":          c.AnsweredAt,
			"on_hold":              c.OnHold,
		}
		if c.State == "in_call" {
			row["transcript"] = a.transcripts.Get(c.LegID)
		}
		calls[i] = row
	}
	metrics := computeMetrics(snap.Calls, logEntries, snap.At, a.metricsWindow, a.slaThreshold)
	self := map[string]any{"listening_room_id": roomID}
	// Surface an active transferred customer call (if any) so the
	// supervisor's topbar pill repaints after a WS reconnect. Also
	// reports whether the customer is on hold (the panel toggles its
	// Hold/Resume button from that flag).
	if v, ok := a.supervisorActiveCalls.Load(sess); ok {
		if callLegID, _ := v.(string); callLegID != "" {
			if c := a.reg.lookup(callLegID); c != nil {
				self["transferred_call"] = map[string]any{
					"call_leg_id": callLegID,
					"caller_from": maskCaller(c.From, a.supervisorMask),
					"on_hold":     c.OnHold,
				}
			}
		}
	}
	// In-flight transfer this supervisor initiated — surfaces ringing /
	// consulting state so reconnect rebuilds their picker / status UI.
	if t := a.transfers.ByFromAgent("supervisor:" + presence.Username); t != nil {
		self["transfer"] = map[string]any{
			"transfer_id": t.ID,
			"state":       t.State,
			"target_kind": t.TargetKind,
			"target":      t.Target,
			"target_name": t.TargetName,
			"attended":    t.Attended,
			"started_at":  t.StartedAt,
		}
	}
	// Surface this session's active attended-transfer consult (if any)
	// so the topbar pill re-paints after a WS reconnect. The session is
	// in a consult when its leg matches a consulting transfer's TargetLegID.
	if legID, _ := sess.get(); legID != "" {
		a.transfers.mu.Lock()
		for _, t := range a.transfers.byID {
			if t.State == transferConsulting && t.TargetKind == intercomTargetSupervisor && t.TargetLegID == legID {
				var callerFrom string
				if c := a.reg.lookup(t.CallLegID); c != nil {
					callerFrom = maskCaller(c.From, a.supervisorMask)
				}
				self["consulting"] = map[string]any{
					"transfer_id":   t.ID,
					"from_agent_id": t.FromAgentID,
					"from_name":     t.FromName,
					"caller_from":   callerFrom,
				}
				break
			}
		}
		a.transfers.mu.Unlock()
	}
	// Surface this session's active intercom (if any) so the topbar
	// pill re-paints correctly after a WS reconnect. Looked up by the
	// session's WebRTC leg id — that's the supervisor side of the
	// intercom-room bridge.
	if legID, _ := sess.get(); legID != "" {
		// Walk all intercoms (small set) once to find the one whose
		// CalleeLeg matches this session — that's the supervisor side
		// of the intercom bridge.
		a.intercoms.mu.Lock()
		for _, ic := range a.intercoms.byID {
			if ic.State == intercomActive && ic.CalleeLeg == legID {
				self["intercom"] = map[string]any{
					"intercom_id": ic.ID,
					"agent_id":    ic.AgentID,
					"agent_name":  ic.AgentName,
					"state":       ic.State,
					"started_at":  ic.StartedAt,
					"answered_at": ic.AnsweredAt,
				}
				break
			}
		}
		a.intercoms.mu.Unlock()
	}
	self["username"] = presence.Username
	return map[string]any{
		"type":        "snapshot",
		"calls":       calls,
		"stats":       snap.Stats,
		"at":          snap.At,
		"agents":      enriched,
		"supervisors": a.supervisors.UsernamesOnline(),
		"call_log":    logEntries,
		"metrics":     metrics,
		"self":        self,
	}
}

// readSupervisorMessages drives the supervisor WS reader: WebRTC signaling
// plus listen.start / listen.stop plus intercom answer / reject / hangup.
func (a *app) readSupervisorMessages(ctx context.Context, cancel context.CancelFunc, c *websocket.Conn, sess *supervisorSession, presence *supervisorPresence, outbox chan<- any, log *slog.Logger) {
	defer cancel()
	for {
		var msg struct {
			Type       string                        `json:"type"`
			SDP        string                        `json:"sdp,omitempty"`
			Candidate  voiceblender.ICECandidateInit `json:"candidate,omitempty"`
			RoomID     string                        `json:"room_id,omitempty"`
			IntercomID string                        `json:"intercom_id,omitempty"`
			TransferID string                        `json:"transfer_id,omitempty"`
			TargetKind string                        `json:"target_kind,omitempty"`
			Target     string                        `json:"target,omitempty"`
			Attended   bool                          `json:"attended,omitempty"`
		}
		if err := wsjson.Read(ctx, c, &msg); err != nil {
			return
		}
		switch msg.Type {
		case "webrtc.offer":
			a.handleSupervisorOffer(ctx, sess, msg.SDP, outbox, log)
		case "webrtc.candidate":
			a.handleSupervisorRemoteCandidate(ctx, sess, msg.Candidate, log)
		case "webrtc.hangup":
			a.handleSupervisorRTCHangup(sess, log)
		case "listen.start":
			a.handleSupervisorListenStart(ctx, sess, msg.RoomID, outbox, log)
		case "listen.stop":
			a.handleSupervisorListenStop(ctx, sess, log)
		case "intercom.answer":
			a.handleSupervisorIntercomAnswer(ctx, sess, presence, msg.IntercomID, outbox, log)
		case "intercom.reject":
			a.handleSupervisorIntercomReject(ctx, presence, msg.IntercomID, log)
		case "intercom.hangup":
			a.handleSupervisorIntercomHangup(ctx, sess, msg.IntercomID, log)
		case "transfer.start":
			a.handleSupervisorTransferStart(ctx, sess, presence, msg.TargetKind, msg.Target, msg.Attended, outbox, log)
		case "transfer.cancel":
			a.handleSupervisorTransferCancel(ctx, presence, msg.TransferID, log)
		case "transfer.complete":
			a.handleSupervisorTransferComplete(ctx, presence, msg.TransferID, log)
		case "transfer.answer":
			a.handleSupervisorTransferAnswer(ctx, sess, presence, msg.TransferID, outbox, log)
		case "transfer.reject":
			a.handleSupervisorTransferReject(ctx, presence, msg.TransferID, log)
		case "transfer.hangup":
			a.handleSupervisorTransferEnd(ctx, sess, msg.TransferID, log)
		case "call.hold":
			a.handleSupervisorCallHold(ctx, sess, presence, outbox, log)
		case "call.resume":
			a.handleSupervisorCallResume(ctx, sess, presence, outbox, log)
		default:
			log.Debug("ignored supervisor ws message", "type", msg.Type)
		}
	}
}

func (a *app) handleSupervisorOffer(ctx context.Context, sess *supervisorSession, sdp string, outbox chan<- any, log *slog.Logger) {
	if sdp == "" {
		sendOrDrop(outbox, map[string]any{"type": "webrtc.error", "message": "sdp required"})
		return
	}
	resp, err := a.client.WebRTCOffer(ctx, voiceblender.WebRTCOfferRequest{SDP: sdp})
	if err != nil {
		log.Error("supervisor webrtc offer", "error", err)
		sendOrDrop(outbox, map[string]any{"type": "webrtc.error", "message": "offer failed"})
		return
	}
	prev := sess.setLeg(resp.LegID)
	a.supervisorLegs.Store(resp.LegID, struct{}{})
	if prev != "" {
		a.supervisorLegs.Delete(prev)
		go func() {
			if _, err := a.client.Leg(prev).Hangup(context.Background(), voiceblender.DeleteLegRequest{}); err != nil && !voiceblender.IsNotFound(err) {
				log.Warn("hangup stale supervisor leg", "leg_id", prev, "error", err)
			}
		}()
	}
	log.Info("supervisor webrtc leg created", "leg_id", resp.LegID)
	sendOrDrop(outbox, map[string]any{
		"type":   "webrtc.answer",
		"leg_id": resp.LegID,
		"sdp":    resp.SDP,
	})
	go a.pushAgentCandidates(ctx, resp.LegID, outbox, log)
}

func (a *app) handleSupervisorRemoteCandidate(ctx context.Context, sess *supervisorSession, cand voiceblender.ICECandidateInit, log *slog.Logger) {
	legID, _ := sess.get()
	if legID == "" {
		return
	}
	if _, err := a.client.Leg(legID).AddICECandidate(ctx, cand); err != nil && !voiceblender.IsNotFound(err) {
		log.Warn("add remote ice candidate (supervisor)", "leg_id", legID, "error", err)
	}
}

func (a *app) handleSupervisorRTCHangup(sess *supervisorSession, log *slog.Logger) {
	legID := sess.setLeg("")
	if legID == "" {
		return
	}
	a.supervisorLegs.Delete(legID)
	if roomID := sess.setRoom(""); roomID != "" {
		_, _ = a.client.Room(roomID).RemoveLeg(context.Background(), legID)
	}
	if _, err := a.client.Leg(legID).Hangup(context.Background(), voiceblender.DeleteLegRequest{}); err != nil && !voiceblender.IsNotFound(err) {
		log.Warn("hangup supervisor leg", "leg_id", legID, "error", err)
	} else {
		log.Info("supervisor webrtc leg hung up by client", "leg_id", legID)
	}
}

// handleSupervisorListenStart adds the supervisor's WebRTC leg to the named
// room. The mixer is mixed-minus-self, so the supervisor hears caller +
// agent without being audible to either (their inbound audio is silent
// because the browser sends a silent stream).
func (a *app) handleSupervisorListenStart(ctx context.Context, sess *supervisorSession, roomID string, outbox chan<- any, log *slog.Logger) {
	legID, prevRoom := sess.get()
	if legID == "" {
		sendOrDrop(outbox, map[string]any{"type": "listen.error", "message": "audio not ready"})
		return
	}
	if roomID == "" {
		sendOrDrop(outbox, map[string]any{"type": "listen.error", "message": "room_id required"})
		return
	}
	// Find the call sitting in that room; refuse if it isn't an active call.
	if !a.roomIsListenable(roomID) {
		sendOrDrop(outbox, map[string]any{"type": "listen.error", "message": "call not available"})
		return
	}
	if prevRoom == roomID {
		return // already listening to this one
	}
	if prevRoom != "" {
		if _, err := a.client.Room(prevRoom).RemoveLeg(ctx, legID); err != nil && !voiceblender.IsNotFound(err) {
			log.Warn("listen.start: leave previous room", "room", prevRoom, "error", err)
		}
	}
	if _, err := a.client.Room(roomID).AddLeg(ctx, voiceblender.AddLegRequest{LegID: legID, Role: "supervisor"}); err != nil {
		log.Error("listen.start: add to room", "room", roomID, "error", err)
		sendOrDrop(outbox, map[string]any{"type": "listen.error", "message": "join room failed"})
		return
	}
	sess.setRoom(roomID)
	log.Info("supervisor listening", "room", roomID, "leg_id", legID)
	a.reg.NotifyChanged() // refresh this WS so the UI re-renders self.listening_room_id
}

func (a *app) handleSupervisorListenStop(ctx context.Context, sess *supervisorSession, log *slog.Logger) {
	legID, _ := sess.get()
	roomID := sess.setRoom("")
	if legID == "" || roomID == "" {
		return
	}
	if _, err := a.client.Room(roomID).RemoveLeg(ctx, legID); err != nil && !voiceblender.IsNotFound(err) {
		log.Warn("listen.stop: leave room", "room", roomID, "error", err)
	}
	log.Info("supervisor stopped listening", "room", roomID, "leg_id", legID)
	a.reg.NotifyChanged()
}

// roomIsListenable returns true if there's a call currently in_call (or
// on_hold) whose room matches the given id.
func (a *app) roomIsListenable(roomID string) bool {
	snap := a.reg.snapshot()
	for _, c := range snap.Calls {
		if c.RoomID == roomID && c.State == "in_call" {
			return true
		}
	}
	return false
}

// --- agent endpoints --------------------------------------------------------

func (a *app) handleAgentIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/agent" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(agentHTML)
}

func (a *app) handleAgentLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ag, ok := a.agents.login(body.Name)
	if !ok {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	a.log.Info("agent logged in", "agent_id", ag.ID, "name", ag.Name)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ag)
}

func (a *app) handleAgentLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		AgentID string `json:"agent_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.AgentID != "" {
		if ag, ok := a.agents.get(body.AgentID); ok {
			// setWebRTCLeg("") atomically swaps and returns any prior leg
			// so the SDK call happens outside the mutex.
			if legID := a.agents.setWebRTCLeg(ag.ID, ""); legID != "" {
				if _, err := a.client.Leg(legID).Hangup(context.Background(), voiceblender.DeleteLegRequest{}); err != nil && !voiceblender.IsNotFound(err) {
					a.log.Warn("hangup webrtc leg on logout", "agent_id", ag.ID, "leg_id", legID, "error", err)
				}
			}
			a.log.Info("agent logged out", "agent_id", ag.ID, "name", ag.Name)
		}
		a.agents.logout(body.AgentID)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) handleAgentWhoami(w http.ResponseWriter, r *http.Request) {
	ag, ok := a.agents.get(r.URL.Query().Get("agent_id"))
	if !ok {
		http.Error(w, "unknown agent", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ag)
}

// handleAgentStream is the agent's single bidirectional channel. The server
// pushes:
//
//	{type: "snapshot", calls, stats, at}     queue updates
//	{type: "webrtc.answer", leg_id, sdp}     in reply to a webrtc.offer
//	{type: "webrtc.candidate", candidate}    a server-gathered ICE candidate
//	{type: "webrtc.error",  message}         offer/candidate handling failed
//
// The client sends:
//
//	{type: "webrtc.offer", sdp}              start an audio leg
//	{type: "webrtc.candidate", candidate}    a browser-gathered ICE candidate
//	{type: "webrtc.hangup"}                  drop the audio leg
//
// The agent's WebRTC leg lifetime is bound to this WS session: when the WS
// closes, we hang the leg up automatically.
func (a *app) handleAgentStream(w http.ResponseWriter, r *http.Request) {
	ag, ok := a.agents.get(r.URL.Query().Get("agent_id"))
	if !ok {
		http.Error(w, "unknown agent", http.StatusUnauthorized)
		return
	}

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		a.log.Warn("ws accept (agent)", "error", err)
		return
	}
	defer c.CloseNow()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	log := a.log.With("agent_id", ag.ID, "name", ag.Name)
	log.Info("agent stream connected")

	// Cancel any pending grace-period removal from a previous WS — the
	// agent is back. On exit, schedule a fresh removal; if the browser
	// reconnects in time the timer is cancelled, otherwise the agent
	// drops off the supervisor's roster automatically.
	a.agents.cancelRemove(ag.ID)
	defer func() {
		a.agents.scheduleRemove(ag.ID)
		log.Info("agent stream disconnected; scheduled removal", "grace", agentRemoveGrace)
	}()

	sub := a.reg.subscribe()
	defer a.reg.unsubscribe(sub)

	// All writes happen on this single goroutine via outbox; the reader
	// goroutine sends messages here. coder/websocket forbids concurrent
	// writers, and one writer keeps message ordering deterministic.
	outbox := make(chan any, 16)

	// Make the outbox addressable from outside this handler so the
	// intercom flow can push ad-hoc events to this agent. Latest WS
	// wins on reconnect; cleared on disconnect. NotifyChanged so
	// other agents' intercom dropdowns add/remove this agent live.
	a.agentOutboxes.Store(ag.ID, (chan<- any)(outbox))
	a.reg.NotifyChanged()
	defer func() {
		a.agentOutboxes.Delete(ag.ID)
		a.reg.NotifyChanged()
	}()

	// On WS close, end any intercom this agent is involved in so the
	// supervisor side sees `intercom.ended` with `agent_disconnected`.
	defer a.cleanupIntercomsOnAgentDisconnect(ag.ID)
	// Same for transfers: cancel any they own; fail any they're the target of.
	defer a.cleanupTransfersOnAgentDisconnect(ag.ID)

	// Reader: parse incoming WS messages and turn them into actions.
	go a.readAgentMessages(ctx, cancel, c, ag, outbox, log)

	// Hang up any WebRTC leg the session created when the WS closes.
	defer func() {
		if legID := a.agents.setWebRTCLeg(ag.ID, ""); legID != "" {
			if _, err := a.client.Leg(legID).Hangup(context.Background(), voiceblender.DeleteLegRequest{}); err != nil && !voiceblender.IsNotFound(err) {
				log.Warn("hangup webrtc leg on ws close", "leg_id", legID, "error", err)
			} else {
				log.Info("agent webrtc leg hung up", "leg_id", legID)
			}
		}
	}()

	// Initial snapshot so the panel paints immediately.
	if err := writeAgentMsg(ctx, c, a.agentSnapshotFor(ag.ID)); err != nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.trigger:
			if err := writeAgentMsg(ctx, c, a.agentSnapshotFor(ag.ID)); err != nil {
				return
			}
		case msg := <-outbox:
			if err := writeAgentMsg(ctx, c, msg); err != nil {
				return
			}
		}
	}
}

// agentSnapshotFor builds the per-agent snapshot envelope. The self field
// carries the agent's current call (if any) so the panel can show the
// "on call" card without needing a separate message type.
func (a *app) agentSnapshotFor(agentID string) map[string]any {
	snap := a.reg.queueSnapshot()
	self := map[string]any{"current_call": a.reg.callAnsweredBy(agentID)}
	// Surface this agent's active intercom (if any) so the panel's
	// intercom pill re-paints after a WS reconnect without waiting for
	// the next push.
	if ic := a.intercoms.ByAgent(agentID); ic != nil {
		self["intercom"] = map[string]any{
			"intercom_id": ic.ID,
			"target_kind": ic.TargetKind,
			"target":      ic.Target,
			"target_name": ic.TargetName,
			"state":       ic.State,
			"started_at":  ic.StartedAt,
			"answered_at": ic.AnsweredAt,
		}
	}
	// In-flight transfer this agent initiated — survives reconnect.
	if t := a.transfers.ByFromAgent(agentID); t != nil {
		self["transfer"] = map[string]any{
			"transfer_id": t.ID,
			"state":       t.State,
			"target_kind": t.TargetKind,
			"target":      t.Target,
			"target_name": t.TargetName,
			"attended":    t.Attended,
			"started_at":  t.StartedAt,
		}
	}
	// Attended-transfer consult this agent is the target of — survives reconnect.
	for _, t := range a.transfers.ByTarget(intercomTargetAgent, agentID) {
		if t.State != transferConsulting {
			continue
		}
		var callerFrom string
		if c := a.reg.lookup(t.CallLegID); c != nil {
			callerFrom = c.From
		}
		self["consulting"] = map[string]any{
			"transfer_id":   t.ID,
			"from_agent_id": t.FromAgentID,
			"from_name":     t.FromName,
			"caller_from":   callerFrom,
		}
		break
	}
	// Build the "other agents currently connected" list for the
	// intercom dropdown. We include only agents with an active WS
	// (i.e. an outbox in a.agentOutboxes) so calls don't ring an
	// agent who's already gone, and we exclude self.
	others := make([]map[string]any, 0)
	for _, peer := range a.agents.list() {
		if peer.ID == agentID {
			continue
		}
		if _, online := a.agentOutboxes.Load(peer.ID); !online {
			continue
		}
		// Also exclude agents currently on a customer call — they
		// can't accept an intercom anyway.
		if a.reg.callAnsweredBy(peer.ID) != nil {
			continue
		}
		others = append(others, map[string]any{
			"agent_id": peer.ID,
			"name":     peer.Name,
		})
	}
	env := map[string]any{
		"type":        "snapshot",
		"calls":       snap.Calls,
		"stats":       snap.Stats,
		"at":          snap.At,
		"self":        self,
		"supervisors": a.supervisors.UsernamesOnline(),
		"agents":      others,
	}
	return env
}

func writeAgentMsg(ctx context.Context, c *websocket.Conn, v any) error {
	return wsjson.Write(ctx, c, v)
}

// readAgentMessages is the per-session reader goroutine. It drives the
// WebRTC handshake (offer → answer + start candidate pusher; trickled
// browser candidates → SDK) and the call.answer / call.hangup actions.
func (a *app) readAgentMessages(ctx context.Context, cancel context.CancelFunc, c *websocket.Conn, ag *agent, outbox chan<- any, log *slog.Logger) {
	defer cancel() // a read error tears down the whole session
	for {
		var msg struct {
			Type       string                        `json:"type"`
			SDP        string                        `json:"sdp,omitempty"`
			Candidate  voiceblender.ICECandidateInit `json:"candidate,omitempty"`
			LegID      string                        `json:"leg_id,omitempty"`
			TargetKind string                        `json:"target_kind,omitempty"`
			Target     string                        `json:"target,omitempty"`
			IntercomID string                        `json:"intercom_id,omitempty"`
			TransferID string                        `json:"transfer_id,omitempty"`
			Attended   bool                          `json:"attended,omitempty"`
		}
		if err := wsjson.Read(ctx, c, &msg); err != nil {
			return
		}
		switch msg.Type {
		case "webrtc.offer":
			a.handleAgentOffer(ctx, ag, msg.SDP, outbox, log)
		case "webrtc.candidate":
			a.handleAgentRemoteCandidate(ctx, ag, msg.Candidate, log)
		case "webrtc.hangup":
			a.handleAgentRTCHangup(ag, log)
		case "call.answer":
			a.handleAgentCallAnswer(ag, msg.LegID, outbox, log)
		case "call.hangup":
			a.handleAgentCallHangup(ctx, ag, log)
		case "call.hold":
			a.handleAgentCallHold(ctx, ag, outbox, log)
		case "call.resume":
			a.handleAgentCallResume(ctx, ag, outbox, log)
		case "intercom.call":
			a.handleAgentIntercomCall(ctx, ag, msg.TargetKind, msg.Target, outbox, log)
		case "intercom.answer":
			a.handleAgentIntercomAnswer(ctx, ag, msg.IntercomID, outbox, log)
		case "intercom.reject":
			a.handleAgentIntercomReject(ctx, ag, msg.IntercomID, log)
		case "intercom.hangup":
			a.handleAgentIntercomHangup(ctx, ag, msg.IntercomID, log)
		case "transfer.start":
			a.handleAgentTransferStart(ctx, ag, msg.TargetKind, msg.Target, msg.Attended, outbox, log)
		case "transfer.cancel":
			a.handleAgentTransferCancel(ctx, ag, msg.TransferID, log)
		case "transfer.complete":
			a.handleAgentTransferComplete(ctx, ag, msg.TransferID, log)
		case "transfer.answer":
			a.handleAgentTransferAnswer(ctx, ag, msg.TransferID, outbox, log)
		case "transfer.reject":
			a.handleAgentTransferReject(ctx, ag, msg.TransferID, log)
		default:
			log.Debug("ignored ws message", "type", msg.Type)
		}
	}
}

func (a *app) handleAgentCallAnswer(ag *agent, callerLegID string, outbox chan<- any, log *slog.Logger) {
	if callerLegID == "" {
		sendOrDrop(outbox, map[string]any{"type": "call.error", "message": "leg_id required"})
		return
	}
	agentLegID := a.agents.webRTCLeg(ag.ID)
	if agentLegID == "" {
		sendOrDrop(outbox, map[string]any{"type": "call.error", "message": "audio not ready"})
		return
	}
	if existing := a.reg.callAnsweredBy(ag.ID); existing != nil {
		sendOrDrop(outbox, map[string]any{"type": "call.error", "message": "already on a call"})
		return
	}
	if !a.reg.claim(callerLegID, ag.ID, ag.Name, agentLegID) {
		sendOrDrop(outbox, map[string]any{"type": "call.error", "message": "call no longer available"})
		return
	}
	log.Info("agent claimed call", "caller_leg_id", callerLegID)
	// The caller's handle goroutine wakes on the answer channel and performs
	// the actual room.AddLeg there. No further action needed here.
}

func (a *app) handleAgentCallHangup(ctx context.Context, ag *agent, log *slog.Logger) {
	call := a.reg.callAnsweredBy(ag.ID)
	if call == nil {
		return
	}
	if _, err := a.client.Leg(call.LegID).Hangup(ctx, voiceblender.DeleteLegRequest{}); err != nil && !voiceblender.IsNotFound(err) {
		log.Warn("hangup caller leg", "leg_id", call.LegID, "error", err)
		return
	}
	log.Info("agent hung up caller", "caller_leg_id", call.LegID)
}

// holdCaller takes the named agent leg out of the call's room and
// starts looping hold music. On success stores the playback id on the
// live callView via setHold(true). Idempotent: if the call is already
// on hold, returns the existing playback id without re-playing music.
// Shared by handleAgentCallHold and the transfer flow.
func (a *app) holdCaller(ctx context.Context, call *callView, agentLeg string, log *slog.Logger) (string, error) {
	if call == nil || call.RoomID == "" {
		return "", fmt.Errorf("bridge not ready")
	}
	if call.OnHold {
		return call.holdPlaybackID, nil
	}
	if agentLeg == "" {
		return "", fmt.Errorf("bridge not ready")
	}
	room := a.client.Room(call.RoomID)
	if _, err := room.RemoveLeg(ctx, agentLeg); err != nil && !voiceblender.IsNotFound(err) {
		return "", fmt.Errorf("remove agent leg: %w", err)
	}
	holdReq := voiceblender.PlayURL(a.holdMusicURL, "audio/mpeg")
	holdReq.Repeat = -1
	pb, err := room.Play(ctx, holdReq)
	if err != nil {
		// Best-effort: put agent back in so caller doesn't sit in silence.
		_, _ = room.AddLeg(context.Background(), voiceblender.AddLegRequest{LegID: agentLeg, Role: "agent"})
		return "", fmt.Errorf("play hold music: %w", err)
	}
	a.reg.setHold(call.LegID, true, pb.PlaybackID)
	log.Info("call placed on hold", "caller_leg_id", call.LegID, "playback_id", pb.PlaybackID)
	return pb.PlaybackID, nil
}

// restoreCaller reverses holdCaller: stop the music, put the named
// agent leg back in the room as "agent". Idempotent. Shared by
// handleAgentCallResume and the transfer-failed / cancelled paths.
func (a *app) restoreCaller(ctx context.Context, call *callView, agentLeg string, log *slog.Logger) error {
	if call == nil || call.RoomID == "" {
		return fmt.Errorf("bridge not ready")
	}
	room := a.client.Room(call.RoomID)
	if call.holdPlaybackID != "" {
		if _, err := room.StopPlay(ctx, call.holdPlaybackID); err != nil && !voiceblender.IsNotFound(err) {
			log.Warn("restore: stop hold music", "room", call.RoomID, "error", err)
		}
	}
	if agentLeg != "" {
		if _, err := room.AddLeg(ctx, voiceblender.AddLegRequest{LegID: agentLeg, Role: "agent"}); err != nil && !voiceblender.IsNotFound(err) {
			return fmt.Errorf("re-add agent leg: %w", err)
		}
	}
	a.reg.setHold(call.LegID, false, "")
	log.Info("call restored from hold", "caller_leg_id", call.LegID)
	return nil
}

// handleAgentCallHold removes the agent's WebRTC leg from the caller's room
// and starts hold music in the room. The caller hears only the music; the
// agent hears nothing (they're no longer in any mixer). The agent's own
// leg stays alive and ready to be re-added on resume.
func (a *app) handleAgentCallHold(ctx context.Context, ag *agent, outbox chan<- any, log *slog.Logger) {
	call := a.reg.callAnsweredBy(ag.ID)
	if call == nil {
		sendOrDrop(outbox, map[string]any{"type": "call.error", "message": "not on a call"})
		return
	}
	if call.OnHold {
		return
	}
	if _, err := a.holdCaller(ctx, call, call.AnsweredAgentLeg, log); err != nil {
		log.Warn("hold failed", "error", err)
		sendOrDrop(outbox, map[string]any{"type": "call.error", "message": "hold failed"})
		return
	}
}

// handleAgentCallResume reverses hold: stop the music, put the agent back
// in the room. Bridge resumes immediately.
func (a *app) handleAgentCallResume(ctx context.Context, ag *agent, outbox chan<- any, log *slog.Logger) {
	call := a.reg.callAnsweredBy(ag.ID)
	if call == nil {
		sendOrDrop(outbox, map[string]any{"type": "call.error", "message": "not on a call"})
		return
	}
	if !call.OnHold {
		return
	}
	if err := a.restoreCaller(ctx, call, call.AnsweredAgentLeg, log); err != nil {
		log.Warn("resume failed", "error", err)
		sendOrDrop(outbox, map[string]any{"type": "call.error", "message": "resume failed"})
		return
	}
}

// handleSupervisorCallHold / Resume mirror the agent versions for a
// supervisor who's on a transferred customer call. Lookup is by the
// supervisor's id ("supervisor:<username>") in the call registry.
func (a *app) handleSupervisorCallHold(ctx context.Context, _ *supervisorSession, presence *supervisorPresence, outbox chan<- any, log *slog.Logger) {
	fromID := "supervisor:" + presence.Username
	call := a.reg.callAnsweredBy(fromID)
	if call == nil {
		sendOrDrop(outbox, map[string]any{"type": "call.error", "message": "not on a call"})
		return
	}
	if call.OnHold {
		return
	}
	if _, err := a.holdCaller(ctx, call, call.AnsweredAgentLeg, log); err != nil {
		log.Warn("supervisor hold failed", "error", err)
		sendOrDrop(outbox, map[string]any{"type": "call.error", "message": "hold failed"})
		return
	}
}

func (a *app) handleSupervisorCallResume(ctx context.Context, _ *supervisorSession, presence *supervisorPresence, outbox chan<- any, log *slog.Logger) {
	fromID := "supervisor:" + presence.Username
	call := a.reg.callAnsweredBy(fromID)
	if call == nil {
		sendOrDrop(outbox, map[string]any{"type": "call.error", "message": "not on a call"})
		return
	}
	if !call.OnHold {
		return
	}
	if err := a.restoreCaller(ctx, call, call.AnsweredAgentLeg, log); err != nil {
		log.Warn("supervisor resume failed", "error", err)
		sendOrDrop(outbox, map[string]any{"type": "call.error", "message": "resume failed"})
		return
	}
}

func (a *app) handleAgentOffer(ctx context.Context, ag *agent, sdp string, outbox chan<- any, log *slog.Logger) {
	if sdp == "" {
		sendOrDrop(outbox, map[string]any{"type": "webrtc.error", "message": "sdp required"})
		return
	}
	resp, err := a.client.WebRTCOffer(ctx, voiceblender.WebRTCOfferRequest{SDP: sdp})
	if err != nil {
		log.Error("webrtc offer", "error", err)
		sendOrDrop(outbox, map[string]any{"type": "webrtc.error", "message": "offer failed"})
		return
	}

	prev := a.agents.setWebRTCLeg(ag.ID, resp.LegID)
	if prev != "" {
		// Page reload or retry — drop the old leg.
		go func() {
			if _, err := a.client.Leg(prev).Hangup(context.Background(), voiceblender.DeleteLegRequest{}); err != nil && !voiceblender.IsNotFound(err) {
				log.Warn("hangup stale webrtc leg", "leg_id", prev, "error", err)
			}
		}()
	}

	log.Info("agent webrtc leg created", "leg_id", resp.LegID)
	sendOrDrop(outbox, map[string]any{
		"type":   "webrtc.answer",
		"leg_id": resp.LegID,
		"sdp":    resp.SDP,
	})

	// Start pushing server-gathered ICE candidates as they arrive.
	go a.pushAgentCandidates(ctx, resp.LegID, outbox, log)
}

func (a *app) handleAgentRemoteCandidate(ctx context.Context, ag *agent, cand voiceblender.ICECandidateInit, log *slog.Logger) {
	legID := a.agents.webRTCLeg(ag.ID)
	if legID == "" {
		return // no leg yet — browser raced its first ICE candidate ahead of the answer
	}
	if _, err := a.client.Leg(legID).AddICECandidate(ctx, cand); err != nil && !voiceblender.IsNotFound(err) {
		log.Warn("add remote ice candidate", "leg_id", legID, "error", err)
	}
}

func (a *app) handleAgentRTCHangup(ag *agent, log *slog.Logger) {
	if legID := a.agents.setWebRTCLeg(ag.ID, ""); legID != "" {
		if _, err := a.client.Leg(legID).Hangup(context.Background(), voiceblender.DeleteLegRequest{}); err != nil && !voiceblender.IsNotFound(err) {
			log.Warn("hangup webrtc leg", "leg_id", legID, "error", err)
		} else {
			log.Info("agent webrtc leg hung up by client", "leg_id", legID)
		}
	}
}

// pushAgentCandidates polls VoiceBlender for the leg's ICE candidates (the
// only API surface the SDK exposes for this) and forwards each new one to
// the browser over the WS. Exits when gathering is complete or the session
// ends. The poll is hidden from the client — they just receive candidates.
func (a *app) pushAgentCandidates(ctx context.Context, legID string, outbox chan<- any, log *slog.Logger) {
	leg := a.client.Leg(legID)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		resp, err := leg.GetICECandidates(ctx)
		if err != nil {
			if ctx.Err() != nil || voiceblender.IsNotFound(err) {
				return
			}
			log.Warn("poll ice candidates", "leg_id", legID, "error", err)
			continue
		}
		for _, cand := range resp.Candidates {
			sendOrDrop(outbox, map[string]any{
				"type":      "webrtc.candidate",
				"candidate": cand,
			})
		}
		if resp.Done {
			return
		}
	}
}

func sendOrDrop(outbox chan<- any, msg any) {
	select {
	case outbox <- msg:
	default:
		// Outbox is full — drop the message rather than block the producer.
		// In practice this only happens if the WS writer has stalled.
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// parseDurationOr reads a duration from env, falling back to def with a
// warning if the value can't be parsed. Used for non-critical durations
// (KPI window, SLA threshold) where the example should keep running with
// sensible defaults rather than refusing to start.
func parseDurationOr(log *slog.Logger, key, defStr string, def time.Duration) time.Duration {
	raw := envOr(key, defStr)
	d, err := time.ParseDuration(raw)
	if err != nil {
		log.Warn("invalid duration env var; using default", "key", key, "value", raw, "default", def, "error", err)
		return def
	}
	return d
}

// splitCSV parses a comma-separated env value into a trimmed, non-empty
// slice. "opus, PCMA ,," → ["opus", "PCMA"].
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// chooseCodec returns the first codec in prefs (preference order) that
// the caller actually offered, using the codec name as the server spells
// it in offered_codecs. Matching is case-insensitive. Returns "" when
// prefs is empty or none of them were offered — letting VoiceBlender
// fall back to its own default preference order rather than answering
// with a codec the caller can't decode.
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

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// makeCallLog selects the call-log backend based on env. Returns the store,
// a human-readable backend name (for the startup log), and any error.
//
//	CALL_LOG_BACKEND      "memory" (default) | "redis"
//	CALL_LOG_REDIS_URL    redis:// or rediss:// connection URL (required for redis)
//	CALL_LOG_REDIS_KEY    Redis list key (default "contactcentre:call_log")
func makeCallLog(ctx context.Context, capacity int) (LogStore, string, error) {
	backend := envOr("CALL_LOG_BACKEND", "memory")
	switch backend {
	case "memory":
		return newMemStore(capacity), "memory", nil
	case "redis":
		url := os.Getenv("CALL_LOG_REDIS_URL")
		if url == "" {
			return nil, "", errors.New("CALL_LOG_BACKEND=redis requires CALL_LOG_REDIS_URL")
		}
		store, err := newRedisStore(ctx, url, os.Getenv("CALL_LOG_REDIS_KEY"), capacity)
		if err != nil {
			return nil, "", err
		}
		return store, "redis (" + url + ")", nil
	default:
		return nil, "", fmt.Errorf("unknown CALL_LOG_BACKEND %q (want memory|redis)", backend)
	}
}
