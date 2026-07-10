package main

import (
	"context"
	"strings"
	"sync"
	"time"

	voiceblender "github.com/VoiceBlender/voiceblender-go"
)

// ivrState is the position of an inbound trunk call in the dial-by-extension IVR.
type ivrState int

const (
	ivrGreeting ivrState = iota // answered, greeting about to play
	ivrPrompt                   // accepting extension digits
	ivrGoodbye                  // farewell playing, about to hang up
)

// ivrCall is the per-leg state of a caller in the IVR.
type ivrCall struct {
	mu          sync.Mutex
	legID       string
	state       ivrState
	activeTTSID string
	digits      string // extension digits entered so far
	attempts    int    // invalid/failed attempts this call
	callerID    string // original caller identity (for the onward From header)
	trunkID     string // trunk the call arrived on
	tenantID    string // owning tenant
	startedAt   time.Time
}

const ivrMaxAttempts = 3

// ivrViews projects inbound trunk calls currently in the IVR into the
// live-calls panel.
func (a *app) ivrViews(tenantID string) []callView {
	var out []callView
	a.calls.Range(func(_, v any) bool {
		c := v.(*ivrCall)
		if c.tenantID != tenantID {
			return true
		}
		from := c.callerID
		if from == "" {
			from = "external"
		}
		out = append(out, callView{
			ID: c.legID, From: from, To: "IVR", Kind: "inbound",
			Via: a.trunkName(c.trunkID), State: "ivr",
			Since: c.startedAt.UTC().Format(time.RFC3339),
		})
		return true
	})
	return out
}

// getCall returns the IVR state for a leg, or nil.
func (a *app) getCall(legID string) *ivrCall {
	v, ok := a.calls.Load(legID)
	if !ok {
		return nil
	}
	return v.(*ivrCall)
}

// startIVR drops an inbound trunk call into the dial-by-extension IVR. Normally
// it plays brief UK ringback via early media, answers, and speaks the greeting
// once the leg connects. When alreadyAnswered is true (the dial plan reached an
// IVR node after a play/tts node), the leg is already up, so it skips
// answering and speaks the prompt immediately.
func (a *app) startIVR(ring *voiceblender.LegRingingEvent, trunk Trunk, tenantID string, alreadyAnswered bool) {
	ctx := context.Background()
	legID := ring.LegID
	codec := chooseCodec(a.answerCodecs, ring.OfferedCodecs)

	a.calls.Store(legID, &ivrCall{legID: legID, state: ivrGreeting, callerID: sipUser(ring.From), trunkID: trunk.ID, tenantID: tenantID, startedAt: time.Now()})

	if alreadyAnswered {
		a.ivrOnConnected(legID) // leg already has media; go straight to the prompt
		return
	}

	if _, err := a.vsi().LegEarlyMedia(ctx, voiceblender.EarlyMediaPayload{ID: legID, Codec: codec}); err != nil {
		a.log.Warn("early media not available", "leg_id", legID, "error", err)
	} else if pb, err := a.vsi().LegPlayStart(ctx, voiceblender.PlaybackStartPayload{ID: legID, Tone: "gb_ringback"}); err == nil {
		time.Sleep(2 * time.Second)
		if _, err := a.vsi().LegPlayStop(ctx, voiceblender.PlaybackTargetPayload{ID: legID, PlaybackID: pb.PlaybackID}); err != nil && !isVSINotFound(err) {
			a.log.Warn("stop ringback", "leg_id", legID, "error", err)
		}
	}

	if _, err := a.vsi().AnswerLeg(ctx, voiceblender.AnswerLegPayload{ID: legID, Codec: codec}); err != nil {
		a.log.Error("answer inbound trunk call", "leg_id", legID, "error", err)
		a.calls.Delete(legID)
	}
}

// ivrOnConnected speaks the greeting + dial prompt once the call is answered.
func (a *app) ivrOnConnected(legID string) {
	c := a.getCall(legID)
	if c == nil {
		return
	}
	c.mu.Lock()
	c.state = ivrPrompt
	c.mu.Unlock()
	a.ivrSpeak(legID, "Thank you for calling "+a.companyName+". Please enter the extension number, followed by the hash key.")
}

// ivrOnDTMF accumulates extension digits and submits on '#', on '*' (clear), or
// automatically once the entered digits match a known extension.
func (a *app) ivrOnDTMF(legID, digit string) {
	c := a.getCall(legID)
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.state != ivrPrompt {
		c.mu.Unlock()
		return
	}
	switch digit {
	case "#":
		number := c.digits
		c.digits = ""
		c.mu.Unlock()
		a.ivrSubmit(legID, number)
		return
	case "*":
		c.digits = ""
		c.mu.Unlock()
		a.ivrSpeak(legID, "Cleared. Please enter the extension number, followed by the hash key.")
		return
	default:
		c.digits += digit
		number := c.digits
		c.mu.Unlock()
		// Auto-submit as soon as the entry matches a configured extension.
		if _, ok := a.exts.byNumber(c.tenantID, number); ok {
			a.ivrSubmit(legID, number)
		}
	}
}

// ivrSubmit looks up the entered extension and either bridges to it or
// re-prompts (up to ivrMaxAttempts) on an unknown/offline extension.
func (a *app) ivrSubmit(legID, number string) {
	c := a.getCall(legID)
	if c == nil {
		return
	}
	number = strings.TrimSpace(number)

	callee, ok := a.exts.byNumber(c.tenantID, number)
	if !ok {
		a.ivrRetry(legID, c, "Sorry, extension "+spell(number)+" was not found.")
		return
	}
	aor, reg := a.exts.registeredAOR(callee.Username)
	if !reg {
		a.ivrRetry(legID, c, "Sorry, extension "+spell(number)+" is currently unavailable.")
		return
	}

	a.ivrStopTTS(c)
	a.log.Info("IVR bridging to extension", "leg_id", legID, "extension", callee.Number)
	from := c.callerID
	if from == "" {
		from = "ivr"
	}
	// The caller was already answered by the IVR, so callerAnswered=true; dial
	// the callee's registered AOR so the server resolves it to the phone.
	fromLabel := from
	if fromLabel == "" {
		fromLabel = "external"
	}
	a.startBridge(legID, aor, from, nil, true, callMeta{tenantID: c.tenantID, from: fromLabel, to: callee.Number, kind: "inbound", via: a.trunkName(c.trunkID), codecs: callee.Codecs})
}

// ivrRetry re-prompts after a bad entry, hanging up after too many attempts.
func (a *app) ivrRetry(legID string, c *ivrCall, msg string) {
	c.mu.Lock()
	c.attempts++
	attempts := c.attempts
	c.mu.Unlock()
	if attempts >= ivrMaxAttempts {
		a.ivrGoodbye(legID)
		return
	}
	a.ivrSpeak(legID, msg+" Please try another extension, followed by the hash key.")
}

// ivrGoodbye plays a farewell and transitions to hang up.
func (a *app) ivrGoodbye(legID string) {
	c := a.getCall(legID)
	if c == nil {
		return
	}
	c.mu.Lock()
	c.state = ivrGoodbye
	c.mu.Unlock()
	a.ivrSpeak(legID, "Thank you for calling. Goodbye.")
}

// ivrOnTTSFinished advances the state machine when a prompt finishes.
func (a *app) ivrOnTTSFinished(legID, ttsID string) {
	c := a.getCall(legID)
	if c == nil {
		return
	}
	c.mu.Lock()
	if ttsID != c.activeTTSID {
		c.mu.Unlock()
		return
	}
	c.activeTTSID = ""
	state := c.state
	c.mu.Unlock()

	if state == ivrGoodbye {
		a.hangup(legID, "")
	}
}

// ivrSpeak stops any active prompt and starts a new one, tracking its id so
// ivrOnTTSFinished can ignore events for replaced prompts.
func (a *app) ivrSpeak(legID, text string) {
	c := a.getCall(legID)
	if c == nil {
		return
	}
	ctx := context.Background()

	c.mu.Lock()
	prev := c.activeTTSID
	c.activeTTSID = ""
	c.mu.Unlock()
	if prev != "" {
		if _, err := a.vsi().LegPlayStop(ctx, voiceblender.PlaybackTargetPayload{ID: legID, PlaybackID: prev}); err != nil && !isVSINotFound(err) {
			a.log.Warn("stop previous tts", "leg_id", legID, "error", err)
		}
	}

	resp, err := a.vsi().LegTTS(ctx, voiceblender.TTSStartPayload{
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

// ivrStopTTS stops the current prompt and clears activeTTSID so the resulting
// finished event is ignored (used before bridging the caller out of the IVR).
func (a *app) ivrStopTTS(c *ivrCall) {
	c.mu.Lock()
	prev := c.activeTTSID
	c.activeTTSID = ""
	c.mu.Unlock()
	if prev == "" {
		return
	}
	if _, err := a.vsi().LegPlayStop(context.Background(), voiceblender.PlaybackTargetPayload{ID: c.legID, PlaybackID: prev}); err != nil && !isVSINotFound(err) {
		a.log.Warn("stop tts", "leg_id", c.legID, "error", err)
	}
}

// spell inserts spaces between digits so TTS reads "1001" as "1 0 0 1".
func spell(number string) string {
	return strings.Join(strings.Split(number, ""), " ")
}
