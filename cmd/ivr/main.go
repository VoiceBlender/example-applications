// Command ivr is a company IVR (Interactive Voice Response) built on VoiceBlender.
//
// Call flow:
//
//	Inbound call
//	  → Early media: UK ringback tone plays for 3 s
//	  → Answer → Welcome greeting → Main menu
//	    1 → Sales queue (room: sales)
//	    2 → Support queue (room: support)
//	    3 → Billing queue (room: billing)
//	    0 → Deepgram AI agent (room: operator)
//	    9 → Repeat menu
//	    * → Goodbye
//	    invalid/timeout → Re-prompt (up to 3 times then goodbye)
//
// # TTS sequencing
//
// Each call tracks its active TTS ID. Starting a new prompt stops the
// previous one first. tts.finished events for replaced prompts are discarded
// so they cannot accidentally advance the state machine.
//
// # Event delivery and commands
//
// Events and commands both travel over a single VoiceBlender VSI WebSocket
// (Client.Events). Inbound frames are piped into the client's hub so the
// Subscribe API works; outbound commands (answer_leg, leg_tts, add_leg_to_room,
// room_play_start, …) are sent through the same *EventStream's typed methods
// instead of the REST API. The VoiceBlender instance does not need any per-leg
// or per-room webhook URL configured.
//
// Environment variables:
//
//	VOICEBLENDER_URL    VoiceBlender base URL (default: http://localhost:8080/v1)
//	ELEVENLABS_API_KEY  ElevenLabs API key (optional if pre-configured in VoiceBlender)
//	TTS_VOICE           TTS voice name (default: Rachel)
//	TTS_PROVIDER        TTS provider name (default: elevenlabs)
//	DEEPGRAM_API_KEY    Deepgram API key for the AI agent (operator queue)
//	COMPANY_NAME        Name spoken in greeting (default: Acme Corp)
//	ANSWER_CODECS       Comma-separated codec preference order for inbound early-media and
//	                    answer SDP, e.g. "opus,PCMA,PCMU". The first one the caller offered
//	                    wins; if none match (or unset), VoiceBlender's default order is used.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	voiceblender "github.com/VoiceBlender/voiceblender-go"
)

// ivrState is the current menu position for a call.
type ivrState int

const (
	stateGreeting ivrState = iota // playing the welcome message
	stateMenu                     // main menu prompt is playing or we are waiting for a digit
	stateRouted                   // caller has been sent to a department queue
	stateGoodbye                  // playing goodbye, about to hang up
)

// call tracks per-leg IVR state.
type call struct {
	mu             sync.Mutex
	legID          string
	state          ivrState
	activeTTSID    string // tts_id of the currently playing prompt; "" when idle
	attempts       int    // invalid DTMF attempts on the current menu cycle
	pendingMenu    bool   // re-play the main menu once the current TTS finishes
	roomID         string // set once the leg is placed in a department room
	holdPlaybackID string // playback_id of the looping hold music in the room
	holdMessage    string // TTS text repeated every 15 s while waiting in the room
}

// app holds shared IVR resources.
type app struct {
	client       *voiceblender.Client
	stream       *voiceblender.EventStream // VSI WebSocket for both events and commands
	log          *slog.Logger
	ttsVoice     string
	ttsProvider  string
	ttsAPIKey    string
	companyName  string
	answerCodecs []string // codec preference order for inbound answer/early media
	calls        sync.Map // legID → *call
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	baseURL := envOr("VOICEBLENDER_URL", "http://localhost:8080/v1")

	a := &app{
		client:       voiceblender.New(voiceblender.WithBaseURL(baseURL)),
		log:          log,
		ttsVoice:     envOr("TTS_VOICE", "Rachel"),
		ttsProvider:  envOr("TTS_PROVIDER", "elevenlabs"),
		ttsAPIKey:    os.Getenv("TTS_API_KEY"),
		companyName:  envOr("COMPANY_NAME", "Acme Corp"),
		answerCodecs: splitCSV(os.Getenv("ANSWER_CODECS")),
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// Open the VSI WebSocket once and use it for both directions: events flow
	// in via PipeTo (into the client's hub, so Subscribe works) and commands
	// flow out via the *EventStream's typed methods.
	stream, err := a.client.Events(ctx)
	if err != nil {
		log.Error("event stream open", "error", err)
		os.Exit(1)
	}
	defer stream.Close()
	a.stream = stream
	go func() {
		if err := stream.PipeTo(ctx, a.client); err != nil && ctx.Err() == nil {
			log.Error("event stream", "error", err)
			cancel()
		}
	}()

	// Ensure the department rooms exist before any calls arrive.
	for _, dept := range []string{"sales", "support", "billing", "operator"} {
		_, err := a.stream.CreateRoom(ctx, voiceblender.CreateRoomRequest{ID: dept})
		if err != nil && !isVSIConflict(err) {
			log.Error("create room", "room", dept, "error", err)
			os.Exit(1)
		}
		log.Info("room ready", "room", dept)
	}

	log.Info("IVR ready", "base_url", baseURL)

	// Drive the IVR state machine from the VSI event stream until interrupted.
	a.runEventLoop(ctx)
}

// runEventLoop consumes every event from the client's hub and dispatches the
// types the IVR cares about. Closing the subscription on return drains it
// cleanly so the dispatcher goroutine inside the hub doesn't keep matching
// against it after we stop reading.
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
			if e.LegType != string(voiceblender.LegTypeSIPInbound) {
				continue
			}
			a.log.Info("event", "type", e.Type, "leg_id", e.LegID)
			go a.onRinging(e)

		case *voiceblender.LegConnectedEvent:
			a.log.Info("event", "type", e.Type, "leg_id", e.LegID)
			go a.onConnected(e.LegID)

		case *voiceblender.LegDisconnectedEvent:
			a.log.Info("event", "type", e.Type, "leg_id", e.LegID)
			a.calls.Delete(e.LegID)

		case *voiceblender.LegLeftRoomEvent:
			// Caller left the room (e.g. agent picked up and moved them). Stop hold-message repeats.
			a.log.Info("event", "type", e.Type, "leg_id", e.LegID)
			if c := a.getCall(e.LegID); c != nil {
				c.mu.Lock()
				c.holdMessage = ""
				c.mu.Unlock()
			}

		case *voiceblender.DTMFReceivedEvent:
			a.log.Info("event", "type", e.Type, "leg_id", e.LegID, "digit", e.Digit)
			go a.onDTMF(e.LegID, e.Digit)

		case *voiceblender.TTSFinishedEvent:
			a.log.Info("event", "type", e.Type, "leg_id", e.LegID, "tts_id", e.TTSID)
			go a.onTTSFinished(e.LegID, e.TTSID)

		case *voiceblender.TTSErrorEvent:
			a.log.Error("tts error", "leg_id", e.LegID, "tts_id", e.TTSID, "error", e.Error)
		}
	}
}

// onRinging plays UK ringback via early media for 3 seconds, then answers.
//
// The same codec is locked in for both the 183 (early media) and 200 (answer)
// SDPs by walking ANSWER_CODECS in preference order and picking the first
// match in the caller's offered_codecs. An empty string falls back to
// VoiceBlender's default order.
func (a *app) onRinging(ring *voiceblender.LegRingingEvent) {
	ctx := context.Background()

	legID := ring.LegID
	codec := chooseCodec(a.answerCodecs, ring.OfferedCodecs)

	c := &call{legID: legID, state: stateGreeting}
	a.calls.Store(legID, c)

	// Enable early media so we can play audio before answering.
	a.log.Info("cmd", "action", "early_media", "leg_id", legID, "codec", codec)
	if _, err := a.stream.LegEarlyMedia(ctx, voiceblender.EarlyMediaPayload{ID: legID, Codec: codec}); err != nil {
		a.log.Warn("early media not available, answering immediately", "leg_id", legID, "error", err)
	} else {
		// Play UK ringback for 3 seconds then stop it before answering.
		a.log.Info("cmd", "action", "play_leg", "leg_id", legID, "tone", "gb_ringback")
		pb, err := a.stream.LegPlayStart(ctx, voiceblender.PlaybackStartPayload{ID: legID, Tone: "gb_ringback"})
		if err != nil {
			a.log.Warn("play ringback", "leg_id", legID, "error", err)
		} else {
			time.Sleep(3 * time.Second)
			a.log.Info("cmd", "action", "stop_play_leg", "leg_id", legID, "playback_id", pb.PlaybackID)
			if _, err := a.stream.LegPlayStop(ctx, voiceblender.PlaybackTargetPayload{ID: legID, PlaybackID: pb.PlaybackID}); err != nil && !isVSINotFound(err) {
				a.log.Warn("stop ringback", "leg_id", legID, "error", err)
			}
		}
	}

	// Reuse the codec negotiated above. If early media succeeded the codec
	// is already locked; if it failed Answer locks it on the 200 OK instead.
	a.log.Info("cmd", "action", "answer_leg", "leg_id", legID, "codec", codec)
	if _, err := a.stream.AnswerLeg(ctx, voiceblender.AnswerLegPayload{ID: legID, Codec: codec}); err != nil {
		a.log.Error("answer leg", "leg_id", legID, "error", err)
		a.calls.Delete(legID)
	}
}

// onConnected plays the welcome greeting once the call is answered.
func (a *app) onConnected(legID string) {
	c := a.getCall(legID)
	if c == nil {
		return
	}

	c.mu.Lock()
	c.state = stateGreeting
	c.mu.Unlock()

	a.speak(legID, "Thank you for calling "+a.companyName+". Please hold while we connect your call.")
}

// onTTSFinished advances the IVR state machine when a prompt finishes playing.
// All sequencing of back-to-back TTS is driven from here to prevent overlap.
//
// ttsID is matched against the call's activeTTSID so that tts.finished events
// fired for prompts that were stopped early (replaced by a newer prompt) are
// silently discarded and do not incorrectly advance the state machine.
func (a *app) onTTSFinished(legID, ttsID string) {
	c := a.getCall(legID)
	if c == nil {
		return
	}

	c.mu.Lock()
	if ttsID != c.activeTTSID {
		// This event is for a prompt that was replaced; ignore it.
		c.mu.Unlock()
		return
	}
	c.activeTTSID = ""
	state := c.state
	pending := c.pendingMenu
	c.pendingMenu = false
	roomID := c.roomID
	holdPlaybackID := c.holdPlaybackID
	c.mu.Unlock()

	switch state {
	case stateGreeting:
		// Greeting done — play the main menu.
		c.mu.Lock()
		c.state = stateMenu
		c.attempts = 0
		c.mu.Unlock()
		a.playMenu(legID)

	case stateMenu:
		// An error or retry prompt just finished — re-play the menu.
		if pending {
			a.playMenu(legID)
		}

	case stateRouted:
		// Hold message done — restore music volume, then repeat after 15 s.
		if roomID != "" && holdPlaybackID != "" {
			ctx := context.Background()
			a.log.Info("cmd", "action", "volume_play_room", "room", roomID, "playback_id", holdPlaybackID, "volume", 0)
			if _, err := a.stream.RoomPlayVolume(ctx, voiceblender.PlaybackVolumePayload{ID: roomID, PlaybackID: holdPlaybackID, Volume: 0}); err != nil && !isVSINotFound(err) {
				a.log.Warn("restore hold music volume", "room", roomID, "error", err)
			}
		}
		c.mu.Lock()
		holdMsg := c.holdMessage
		c.mu.Unlock()
		if holdMsg != "" {
			go func() {
				time.Sleep(15 * time.Second)
				c.mu.Lock()
				stillWaiting := c.state == stateRouted
				c.mu.Unlock()
				if stillWaiting {
					a.speak(legID, holdMsg)
				}
			}()
		}

	case stateGoodbye:
		// Goodbye done — hang up.
		ctx := context.Background()
		a.log.Info("cmd", "action", "delete_leg", "leg_id", legID)
		if _, err := a.stream.DeleteLeg(ctx, voiceblender.DeleteLegPayload{ID: legID}); err != nil && !isVSINotFound(err) {
			a.log.Error("delete leg", "leg_id", legID, "error", err)
		}
	}
}

// onDTMF handles a DTMF digit press from the caller.
func (a *app) onDTMF(legID, digit string) {
	c := a.getCall(legID)
	if c == nil {
		return
	}

	c.mu.Lock()
	state := c.state
	c.mu.Unlock()

	if state != stateMenu {
		return // ignore stray digits during greeting/routing/goodbye
	}

	ctx := context.Background()

	switch digit {
	case "1":
		a.routeToDepartment(ctx, legID, "sales", "Sales")
	case "2":
		a.routeToDepartment(ctx, legID, "support", "Support")
	case "3":
		a.routeToDepartment(ctx, legID, "billing", "Billing")
	case "0":
		a.routeToAgent(ctx, legID)
	case "9":
		a.playMenu(legID)
	case "*":
		a.goodbye(legID)
	default:
		c.mu.Lock()
		c.attempts++
		attempts := c.attempts
		c.pendingMenu = true // re-play menu once the error prompt finishes
		c.mu.Unlock()

		if attempts >= 3 {
			a.log.Info("too many invalid inputs, hanging up", "leg_id", legID)
			a.goodbye(legID)
			return
		}
		a.speak(legID, "I'm sorry, that's not a valid option. Please try again.")
		// onTTSFinished sees pendingMenu=true and plays the menu when this ends.
	}
}

// routeToDepartment moves the caller into a department room.
func (a *app) routeToDepartment(ctx context.Context, legID, roomID, displayName string) {
	c := a.getCall(legID)
	if c == nil {
		return
	}

	// Stop the menu prompt before joining the room so it doesn't keep
	// playing on top of the hold music.
	a.stopActiveTTS(ctx, c)

	c.mu.Lock()
	c.state = stateRouted
	c.mu.Unlock()

	a.log.Info("cmd", "action", "add_leg_to_room", "leg_id", legID, "room", roomID)
	resp, err := a.stream.AddLegToRoom(ctx, voiceblender.AddLegPayload{RoomID: roomID, LegID: legID})
	if err != nil {
		a.log.Error("add leg to room", "leg_id", legID, "room", roomID, "error", err)
		c.mu.Lock()
		c.state = stateMenu
		c.pendingMenu = true // re-play menu once the error prompt finishes
		c.mu.Unlock()
		a.speak(legID, "I'm sorry, that queue is not available right now. Please try another option.")
		return
	}

	a.log.Info("caller routed", "leg_id", legID, "room", roomID, "status", resp.Status)

	c.mu.Lock()
	c.roomID = roomID
	c.holdMessage = "Please hold while I connect you to " + displayName + "."
	c.mu.Unlock()

	// Start hold music before speaking so speak() can duck its volume.
	const holdMusicURL = "http://localhost/moh/new_music.mp3"
	a.log.Info("cmd", "action", "play_room", "room", roomID, "url", holdMusicURL)
	holdPB, err := a.stream.RoomPlayStart(ctx, voiceblender.PlaybackStartPayload{
		ID:       roomID,
		URL:      holdMusicURL,
		MimeType: "audio/mpeg",
		Repeat:   -1, // loop indefinitely
	})
	if err != nil {
		a.log.Warn("play hold music", "room", roomID, "error", err)
	} else {
		c.mu.Lock()
		c.holdPlaybackID = holdPB.PlaybackID
		c.mu.Unlock()
	}

	// Routing message plays after hold music starts so speak() can duck it.
	a.speak(legID, "Please hold while I connect you to "+displayName+".")
}

// routeToAgent places the caller in the operator room and attaches a Deepgram
// AI agent to handle the conversation.
func (a *app) routeToAgent(ctx context.Context, legID string) {
	c := a.getCall(legID)
	if c == nil {
		return
	}

	// Stop the menu prompt before joining the room so it doesn't keep
	// playing on top of the Deepgram agent's greeting.
	a.stopActiveTTS(ctx, c)

	c.mu.Lock()
	c.state = stateRouted
	c.mu.Unlock()

	const roomID = "operator"

	a.log.Info("cmd", "action", "add_leg_to_room", "leg_id", legID, "room", roomID)
	resp, err := a.stream.AddLegToRoom(ctx, voiceblender.AddLegPayload{RoomID: roomID, LegID: legID})
	if err != nil {
		a.log.Error("add leg to room", "leg_id", legID, "room", roomID, "error", err)
		c.mu.Lock()
		c.state = stateMenu
		c.pendingMenu = true
		c.mu.Unlock()
		a.speak(legID, "I'm sorry, the operator is not available right now. Please try another option.")
		return
	}

	a.log.Info("caller routed to agent", "leg_id", legID, "room", roomID, "status", resp.Status)

	// No "please hold" TTS here — the Deepgram agent's own greeting plays
	// as soon as it attaches and any IVR-side prompt would overlap it.

	// Attach a Deepgram voice agent to the room.
	//
	// The settings field is the full Deepgram agent configuration object
	// (agent.listen, agent.think, agent.speak, audio, etc.).
	// See https://developers.deepgram.com/docs/voice-agent for details.
	//
	// TODO: edit the Deepgram agent settings below.
	agentSettings := map[string]any{
		"type": "Settings",
		"audio": map[string]any{
			"input":  map[string]any{"encoding": "linear16", "sample_rate": 48000},
			"output": map[string]any{"encoding": "linear16", "sample_rate": 24000, "container": "none"},
		},
		"agent": map[string]any{
			"language": "en",
			"speak":    map[string]any{"provider": map[string]any{"type": "deepgram", "model": "aura-2-odysseus-en"}},
			"listen":   map[string]any{"provider": map[string]any{"type": "deepgram", "version": "v2", "model": "flux-general-en"}},
			"think": map[string]any{
				"provider": map[string]any{"type": "google", "model": "gemini-2.5-flash"},
				"prompt":   "You are a helpful virtual assistant speaking to callers on the phone for Acme Corp. Be warm, concise, and professional. Keep responses to 1-2 sentences.",
			},
			"greeting": "Hello! How may I help you?",
		},
	}

	a.log.Info("cmd", "action", "deepgram_agent_room", "room", roomID)
	if _, err := a.stream.RoomAgentDeepgram(ctx, voiceblender.AgentDeepgramPayload{
		ID:       roomID,
		Settings: agentSettings,
		APIKey:   os.Getenv("DEEPGRAM_API_KEY"),
	}); err != nil {
		a.log.Error("attach agent", "room", roomID, "error", err)
	}
}

// playMenu speaks the main menu options.
func (a *app) playMenu(legID string) {
	text := "For Sales, press 1. " +
		"For Support, press 2. " +
		"For Billing, press 3. " +
		"For an operator, press 0. " +
		"To repeat this menu, press 9. " +
		"To end the call, press star."
	a.speak(legID, text)
}

// goodbye plays a farewell and transitions to goodbye state.
func (a *app) goodbye(legID string) {
	c := a.getCall(legID)
	if c == nil {
		return
	}
	c.mu.Lock()
	c.state = stateGoodbye
	c.pendingMenu = false
	c.mu.Unlock()

	a.speak(legID, "Thank you for calling "+a.companyName+". Goodbye.")
	// onTTSFinished will hang up once this completes.
}

// stopActiveTTS stops the prompt currently playing on the leg, if any, and
// clears activeTTSID so the resulting tts.finished event is ignored by the
// state machine. Used before routing the leg into a room — otherwise the
// menu prompt continues playing on top of the room audio.
func (a *app) stopActiveTTS(ctx context.Context, c *call) {
	c.mu.Lock()
	prev := c.activeTTSID
	c.activeTTSID = ""
	c.mu.Unlock()

	if prev == "" {
		return
	}
	a.log.Info("cmd", "action", "stop_play_leg", "leg_id", c.legID, "tts_id", prev)
	if _, err := a.stream.LegPlayStop(ctx, voiceblender.PlaybackTargetPayload{ID: c.legID, PlaybackID: prev}); err != nil && !isVSINotFound(err) {
		a.log.Warn("stop previous tts", "leg_id", c.legID, "tts_id", prev, "error", err)
	}
}

// speak stops any active TTS prompt, then starts a new one.
// The new tts_id is stored on the call so that onTTSFinished can
// discard events for prompts that were replaced.
func (a *app) speak(legID, text string) {
	c := a.getCall(legID)
	if c == nil {
		return
	}

	ctx := context.Background()

	// Stop the prompt currently playing, if any.
	c.mu.Lock()
	prev := c.activeTTSID
	c.activeTTSID = ""
	roomID := c.roomID
	holdPlaybackID := c.holdPlaybackID
	c.mu.Unlock()

	if prev != "" {
		a.log.Info("cmd", "action", "stop_play_leg", "leg_id", legID, "tts_id", prev)
		if _, err := a.stream.LegPlayStop(ctx, voiceblender.PlaybackTargetPayload{ID: legID, PlaybackID: prev}); err != nil && !isVSINotFound(err) {
			a.log.Warn("stop previous tts", "leg_id", legID, "tts_id", prev, "error", err)
		}
	}

	// Duck hold music while TTS plays (-3 steps ≈ -9 dB).
	if roomID != "" && holdPlaybackID != "" {
		a.log.Info("cmd", "action", "volume_play_room", "room", roomID, "playback_id", holdPlaybackID, "volume", -6)
		if _, err := a.stream.RoomPlayVolume(ctx, voiceblender.PlaybackVolumePayload{ID: roomID, PlaybackID: holdPlaybackID, Volume: -6}); err != nil && !isVSINotFound(err) {
			a.log.Warn("duck hold music", "room", roomID, "error", err)
		}
	}

	a.log.Info("cmd", "action", "tts_leg", "leg_id", legID, "text", text)
	resp, err := a.stream.LegTTS(ctx, voiceblender.TTSStartPayload{
		ID:       legID,
		Text:     text,
		Voice:    a.ttsVoice,
		Provider: a.ttsProvider,
		APIKey:   a.ttsAPIKey,
	})
	if err != nil {
		a.log.Error("tts", "leg_id", legID, "error", err)
		return
	}

	c.mu.Lock()
	c.activeTTSID = resp.TTSID
	c.mu.Unlock()
}

// getCall retrieves the call state for a leg, or nil if not found.
func (a *app) getCall(legID string) *call {
	v, ok := a.calls.Load(legID)
	if !ok {
		return nil
	}
	return v.(*call)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// isVSINotFound reports whether err is a VSI error with code 404 (e.g.
// stopping a playback that has already ended, hanging up a leg that's
// already gone).
func isVSINotFound(err error) bool {
	e, ok := err.(*voiceblender.VSIError)
	return ok && e.Code == 404
}

// isVSIConflict reports whether err is a VSI error with code 409 (used on
// startup so an existing department room is treated as success).
func isVSIConflict(err error) bool {
	e, ok := err.(*voiceblender.VSIError)
	return ok && e.Code == 409
}

// splitCSV parses a comma-separated list, trimming whitespace and dropping
// empty entries. Returns nil for an empty input so callers can length-check.
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

// chooseCodec returns the first codec in prefs (preference order) that the
// caller actually offered, using the codec name as the server spells it in
// offered_codecs. Matching is case-insensitive. Returns "" when prefs is
// empty or none of them were offered, letting VoiceBlender fall back to its
// own default preference order rather than answering with a codec the
// caller can't decode.
func chooseCodec(prefs []string, offered []voiceblender.OfferedCodec) string {
	if len(prefs) == 0 || len(offered) == 0 {
		return ""
	}
	offeredByName := make(map[string]string, len(offered))
	for _, c := range offered {
		if c.Name == "" {
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
