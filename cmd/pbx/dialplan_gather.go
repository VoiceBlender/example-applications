package main

import (
	"context"
	"strconv"
	"strings"
	"time"

	voiceblender "github.com/VoiceBlender/voiceblender-go"
)

// A gather node plays an optional prompt (TTS or audio URL) and collects DTMF,
// then branches on what the caller entered: the output port named after the
// entered digits is followed, falling back to the `default` port on no match,
// no input, or timeout. Each wired output port IS an accepted option, so an
// entry is auto-submitted as soon as it matches a wired port (or reaches
// num_digits, or the caller presses #). `*` clears the current entry.

// dpGather answers the leg if needed, plays the prompt, and begins collecting.
func (a *app) dpGather(exec *dpExec, node *DPNode) {
	if !exec.isAnswered() {
		a.dpAnswer(exec, node.ID) // answer, then re-enter this node on connect
		return
	}
	input := node.param("input", dpInputDTMF)
	num := atoiDefault(node.param("num_digits", "0"), 0)
	timeout := atoiDefault(node.param("timeout", "7"), 7)
	if timeout <= 0 {
		timeout = 7
	}

	exec.mu.Lock()
	exec.gathering = true
	exec.gatherNode = node.ID
	exec.gatherInput = input
	exec.gatherDigits = ""
	exec.gatherNum = num
	exec.gatherGen++
	gen := exec.gatherGen
	exec.mu.Unlock()

	// Optional prompt (barge-in: the first keypress stops it).
	ctx := context.Background()
	if text := node.param("text", ""); text != "" {
		if resp, err := a.vsi().LegTTS(ctx, voiceblender.TTSStartPayload{
			ID: exec.legID, Text: text, Voice: node.param("voice", a.ttsVoice),
			Provider: a.ttsProvider, APIKey: a.ttsAPIKey,
		}); err == nil {
			exec.setGatherPrompt(resp.TTSID)
		} else {
			a.log.Warn("gather tts failed", "leg_id", exec.legID, "error", err)
		}
	} else if url := node.param("url", ""); url != "" {
		if resp, err := a.vsi().LegPlayStart(ctx, voiceblender.PlaybackStartPayload{
			ID: exec.legID, URL: url, MimeType: "audio/mpeg",
		}); err == nil {
			exec.setGatherPrompt(resp.PlaybackID)
		} else {
			a.log.Warn("gather play failed", "leg_id", exec.legID, "error", err)
		}
	}

	// Speech recognition (word/close-word matched against the option ports).
	if input == dpInputSpeech || input == dpInputBoth {
		if _, err := a.vsi().LegSTTStart(ctx, voiceblender.STTStartPayload{
			ID:       exec.legID,
			Language: node.param("language", a.sttLanguage),
			Provider: a.sttProvider,
			APIKey:   a.sttAPIKey,
		}); err != nil {
			a.log.Warn("gather stt start failed", "leg_id", exec.legID, "error", err)
		} else {
			exec.mu.Lock()
			exec.gatherSTT = true
			exec.mu.Unlock()
		}
	}

	// No-input / timeout → default branch.
	time.AfterFunc(time.Duration(timeout)*time.Second, func() {
		exec.mu.Lock()
		expired := exec.gathering && exec.gatherGen == gen
		digits := exec.gatherDigits
		exec.mu.Unlock()
		if expired {
			a.log.Info("dial plan gather timeout", "leg_id", exec.legID)
			a.dpGatherSubmit(exec, digits)
		}
	})
	a.log.Info("dial plan gather", "leg_id", exec.legID, "node", node.ID, "input", input)
}

// dpOnDTMF feeds a digit into an active gather. Returns true if the leg is
// gathering (so the caller doesn't also route it to the IVR).
func (a *app) dpOnDTMF(legID, digit string) bool {
	v, ok := a.dpExecs.Load(legID)
	if !ok {
		return false
	}
	exec := v.(*dpExec)

	exec.mu.Lock()
	if !exec.gathering {
		exec.mu.Unlock()
		return false
	}
	if exec.gatherInput == dpInputSpeech {
		exec.mu.Unlock()
		return true // speech-only gather; consume the digit but ignore it
	}
	prompt := exec.gatherPromptID
	exec.gatherPromptID = ""
	node := exec.gatherNode
	var value string
	submit := false
	switch digit {
	case "#":
		value, submit = exec.gatherDigits, true
	case "*":
		exec.gatherDigits = ""
	default:
		exec.gatherDigits += digit
		if a.dpGatherHasOption(exec.tenantID, node, exec.gatherDigits) {
			value, submit = exec.gatherDigits, true
		} else if exec.gatherNum > 0 && len(exec.gatherDigits) >= exec.gatherNum {
			value, submit = exec.gatherDigits, true
		}
	}
	exec.mu.Unlock()

	// Barge-in: stop the prompt on the first keypress.
	if prompt != "" {
		if _, err := a.vsi().LegPlayStop(context.Background(), voiceblender.PlaybackTargetPayload{ID: legID, PlaybackID: prompt}); err != nil && !isVSINotFound(err) {
			a.log.Warn("gather stop prompt", "leg_id", legID, "error", err)
		}
	}
	if submit {
		a.dpGatherSubmit(exec, value)
	}
	return true
}

// dpGatherSubmit routes to the output port matching the entered value, or the
// `default` port. Stops the prompt and STT first, and clears the gather state
// (idempotent so the timeout timer and a real entry can't both fire).
func (a *app) dpGatherSubmit(exec *dpExec, value string) {
	exec.mu.Lock()
	if !exec.gathering {
		exec.mu.Unlock()
		return
	}
	node := exec.gatherNode
	prompt := exec.gatherPromptID
	stt := exec.gatherSTT
	exec.gathering = false
	exec.gatherNode = ""
	exec.gatherDigits = ""
	exec.gatherPromptID = ""
	exec.gatherSTT = false
	exec.mu.Unlock()

	ctx := context.Background()
	if prompt != "" {
		if _, err := a.vsi().LegPlayStop(ctx, voiceblender.PlaybackTargetPayload{ID: exec.legID, PlaybackID: prompt}); err != nil && !isVSINotFound(err) {
			a.log.Warn("gather stop prompt", "leg_id", exec.legID, "error", err)
		}
	}
	if stt {
		if _, err := a.vsi().LegSTTStop(ctx, voiceblender.IDPayload{ID: exec.legID}); err != nil && !isVSINotFound(err) {
			a.log.Warn("gather stop stt", "leg_id", exec.legID, "error", err)
		}
	}

	g := a.dialplan.get(exec.tenantID)
	target := g.edgeTo(node, value)
	if target == "" {
		target = g.edgeTo(node, dpPortDefault)
	}
	a.log.Info("dial plan gather result", "leg_id", exec.legID, "entered", value, "target", target)
	if target == "" {
		a.dpReject(exec, "declined")
		return
	}
	a.dpWalk(exec, g, target, 0)
}

// dpOnSTT feeds a speech transcript into an active speech/both gather. On a
// final transcript it fuzzy-matches the words against the option ports and
// submits on a hit; a non-match keeps listening until the timeout. Returns true
// if the leg is a speech gather (so it isn't handled elsewhere).
func (a *app) dpOnSTT(legID, text string, isFinal bool) bool {
	v, ok := a.dpExecs.Load(legID)
	if !ok {
		return false
	}
	exec := v.(*dpExec)
	exec.mu.Lock()
	if !exec.gathering || (exec.gatherInput != dpInputSpeech && exec.gatherInput != dpInputBoth) {
		exec.mu.Unlock()
		return false
	}
	node := exec.gatherNode
	exec.mu.Unlock()

	if !isFinal || strings.TrimSpace(text) == "" {
		return true // wait for a final utterance
	}
	opts := a.dpGatherOptions(exec.tenantID, node)
	if val, ok := matchSpeech(text, opts); ok {
		a.log.Info("dial plan gather speech match", "leg_id", legID, "text", text, "option", val)
		a.dpGatherSubmit(exec, val)
	} else {
		a.log.Info("dial plan gather speech no match", "leg_id", legID, "text", text)
	}
	return true
}

// dpGatherOptions returns the wired output ports of a gather node (excluding
// `default`) — these are the accepted keywords/digits.
func (a *app) dpGatherOptions(tenantID, nodeID string) []string {
	var opts []string
	for _, e := range a.dialplan.get(tenantID).Edges {
		if e.From == nodeID && e.Port != dpPortDefault {
			opts = append(opts, e.Port)
		}
	}
	return opts
}

// dpGatherHasOption reports whether the gather node has a wired output for value
// (i.e. value is an accepted menu option).
func (a *app) dpGatherHasOption(tenantID, nodeID, value string) bool {
	for _, e := range a.dialplan.get(tenantID).Edges {
		if e.From == nodeID && e.Port == value {
			return true
		}
	}
	return false
}

func (e *dpExec) setGatherPrompt(id string) {
	e.mu.Lock()
	e.gatherPromptID = id
	e.mu.Unlock()
}

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}
