package main

import (
	"context"
	"time"

	voiceblender "github.com/VoiceBlender/voiceblender-go"
)

// mediaManager owns everything on the VoiceBlender side of a session: the room,
// the two WebRTC legs, the routing matrix that silences the direct path between
// them, and the per-leg speech-to-text.
//
// Unlike the push-to-talk example, media here is NOT on demand. Both legs come
// up when the participants join and stay up for the whole conversation: the
// interpreter has to be listening continuously, and re-negotiating WebRTC per
// utterance would cost far more than it saved.
type mediaManager struct {
	a *app
}

func newMediaManager(a *app) *mediaManager { return &mediaManager{a: a} }

// ensureRoom creates the session's VoiceBlender room, tolerating one left over
// from an unclean teardown.
func (m *mediaManager) ensureRoom(ctx context.Context, s *session) error {
	s.mu.Lock()
	if s.roomUp {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	// Tagged with our app_id and namespaced by it, so a VoiceBlender shared with
	// the other examples can neither confuse our events nor collide on a room id.
	if _, err := m.a.vsi().CreateRoom(ctx, voiceblender.CreateRoomRequest{
		ID:    m.a.vbRoom(s.id),
		AppID: m.a.appID,
	}); err != nil && !isVSIConflict(err) {
		return err
	}
	s.mu.Lock()
	s.roomUp = true
	s.mu.Unlock()
	return nil
}

// offer handles a browser's WebRTC offer: it establishes the media leg, joins it
// to the session room under this participant's role, and streams the server's
// ICE candidates back.
func (m *mediaManager) offer(ctx context.Context, s *session, p *participant, sdp string) {
	if sdp == "" {
		p.send(map[string]any{"type": "webrtc.error", "message": "sdp required"})
		return
	}
	if err := m.ensureRoom(ctx, s); err != nil {
		m.a.log.Error("create room", "session", s.id, "error", err)
		p.send(map[string]any{"type": "webrtc.error", "message": "could not open session"})
		return
	}

	// Tag the browser leg with our app_id so its events carry it. This is what
	// lets VoiceBlender filter the VSI stream for us (see manageStream): an
	// untagged leg's events would be dropped by the filter and we'd never learn
	// the leg had connected — which is the signal that starts transcription.
	resp, err := m.a.vsi().WebRTCOffer(ctx, voiceblender.WebRTCOfferRequest{SDP: sdp, AppID: m.a.appID})
	if err != nil {
		m.a.log.Error("webrtc offer", "session", s.id, "participant", p.name, "error", err)
		p.send(map[string]any{"type": "webrtc.error", "message": "offer failed"})
		return
	}
	m.a.sessions.bindLeg(s, p, resp.LegID)

	// The role is what the routing matrix keys on. Passing an empty one here
	// would silently give this participant the full mix — i.e. they would hear
	// their peer's untranslated voice — so it is asserted rather than assumed.
	role := p.role()
	if role == "" {
		m.a.log.Error("participant has no role", "session", s.id, "participant", p.name)
		p.send(map[string]any{"type": "webrtc.error", "message": "internal role error"})
		return
	}
	if _, err := m.a.vsi().AddLegToRoom(ctx, voiceblender.AddLegPayload{
		RoomID: m.a.vbRoom(s.id),
		LegID:  resp.LegID,
		Role:   role,
	}); err != nil {
		m.a.log.Warn("add leg to room", "leg_id", resp.LegID, "session", s.id, "error", err)
	}
	// Re-apply on every join: the matrix is per-room, and applying it again is
	// both cheap and idempotent, which is cheaper than reasoning about whether
	// the second participant arrived before or after the first one set it.
	m.applyRouting(ctx, s)

	p.send(map[string]any{"type": "webrtc.answer", "leg_id": resp.LegID, "sdp": resp.SDP})
	go m.pushCandidates(ctx, resp.LegID, p)
}

// applyRouting silences the direct path between the two participants.
//
// This is the load-bearing call of the whole app. The routing matrix maps a
// listener's role to the set of source roles it may hear; an empty (but present)
// list means that listener hears nothing from the mix at all. Both roles get an
// empty list, so neither participant ever hears the other's raw voice — the only
// audio that reaches them is what leg_tts injects privately into their own leg,
// which is the translation.
//
// A role missing from the matrix means "full mesh", so both rows must always be
// written together.
func (m *mediaManager) applyRouting(ctx context.Context, s *session) {
	if _, err := m.a.vsi().RoomRoutingSet(ctx, voiceblender.RoomRoutingSetPayload{
		RoomID: m.a.vbRoom(s.id),
		Matrix: map[string][]string{
			roleA: {},
			roleB: {},
		},
	}); err != nil {
		// Worth shouting about: if this fails the participants can hear each
		// other untranslated, which is the one outcome the design forbids.
		m.a.log.Error("apply routing matrix — participants may hear each other untranslated",
			"session", s.id, "error", err)
		s.broadcast(map[string]any{"type": "warning", "message": "audio isolation failed — you may hear the other speaker directly"})
		return
	}
	s.mu.Lock()
	s.routing = true
	s.mu.Unlock()
}

// remoteCandidate forwards a browser ICE candidate to its media leg.
func (m *mediaManager) remoteCandidate(ctx context.Context, p *participant, cand voiceblender.ICECandidateInit) {
	leg := p.leg()
	if leg == "" {
		return // browser raced its first candidate ahead of the answer
	}
	if _, err := m.a.vsi().WebRTCAddCandidate(ctx, voiceblender.VSIWebRTCAddCandidatePayload{ID: leg, Candidate: cand}); err != nil && !isVSINotFound(err) {
		m.a.log.Warn("add ice candidate", "leg_id", leg, "error", err)
	}
}

// pushCandidates polls VoiceBlender for the leg's server-gathered ICE candidates
// and forwards each to the browser. Exits when gathering completes, the leg is
// gone, or ctx ends.
func (m *mediaManager) pushCandidates(ctx context.Context, legID string, p *participant) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if p.leg() != legID {
			return // leg replaced or torn down
		}
		resp, err := m.a.vsi().WebRTCGetCandidates(ctx, voiceblender.IDPayload{ID: legID})
		if err != nil {
			if isVSINotFound(err) {
				return
			}
			continue
		}
		for _, cand := range resp.Candidates {
			p.send(map[string]any{"type": "webrtc.candidate", "candidate": cand})
		}
		if resp.Done {
			return
		}
	}
}

// legConnected marks a participant's media as live and, once BOTH sides are up,
// starts transcription.
//
// The wait matters twice over: leg_stt_start returns 409 on a leg that has not
// connected yet, and transcribing before there is a peer to translate for would
// burn STT minutes producing text nobody can hear.
func (m *mediaManager) legConnected(legID string) {
	s, p, ok := m.a.sessions.forLeg(legID)
	if !ok {
		return
	}
	p.mu.Lock()
	p.live = true
	p.mu.Unlock()

	p.send(map[string]any{"type": "state", "media": "live"})
	m.a.log.Info("leg connected", "session", s.id, "participant", p.name, "leg_id", legID)
	m.maybeStartSTT(context.Background(), s)
}

// maybeStartSTT starts transcription on both legs once both are connected.
func (m *mediaManager) maybeStartSTT(ctx context.Context, s *session) {
	a, b, ok := s.both()
	if !ok {
		return
	}
	for _, p := range []*participant{a, b} {
		_, _, live := p.snapshot()
		if !live {
			return // wait for the other side
		}
	}
	for _, p := range []*participant{a, b} {
		m.startSTT(ctx, s, p)
	}
	// The maximum-duration cap runs from here — when the meters actually start —
	// not from when the link was created.
	s.markStarted()
	s.broadcast(map[string]any{"type": "state", "interpreting": true})
}

// startSTT brings this leg's transcriber into line with the participant's
// currently selected language, starting or restarting it as needed.
//
// It is a reconciler rather than a one-shot "start", because the desired
// language can change while a previous attempt is still in flight. sttMu
// serializes the attempts; sttLang records what is actually running, so a drift
// between selected and running is detectable instead of silent.
//
// Per-leg STT taps only that leg's incoming audio — before mixing and before
// routing — so a speaker's transcript can never contain their peer's voice,
// even though both legs share a room.
func (m *mediaManager) startSTT(ctx context.Context, s *session, p *participant) {
	p.sttMu.Lock()
	defer p.sttMu.Unlock()

	// Up to two passes: if the selection changed while we were dialling, the
	// second pass reconciles to the newer value rather than leaving the older
	// one running.
	for attempt := 0; attempt < 2; attempt++ {
		p.mu.Lock()
		legID, want, live := p.legID, p.lang, p.live
		running, runningLang := p.sttOn, p.sttLang
		p.mu.Unlock()

		if legID == "" || !live {
			return // nothing to transcribe yet; legConnected will come back
		}
		if running && runningLang == want {
			return // already transcribing the right language
		}
		if running {
			// Wrong language running — stop it before starting the right one.
			m.stopSTTOnLeg(ctx, p, legID)
		}
		if !m.dialSTT(ctx, s, p, legID, want) {
			return
		}
		p.mu.Lock()
		settled := p.lang == want
		p.mu.Unlock()
		if settled {
			return
		}
		// The participant changed language mid-dial; go round again.
		m.a.log.Info("language changed while starting transcription, reconciling",
			"session", s.id, "participant", p.name)
	}
}

// dialSTT starts the transcriber for one language. Reports whether the leg is
// now transcribing that language.
func (m *mediaManager) dialSTT(ctx context.Context, s *session, p *participant, legID, lang string) bool {
	payload, ok := m.sttPayload(legID, lang)
	if !ok {
		m.a.log.Error("no configured transcriber can handle this language",
			"session", s.id, "participant", p.name, "lang", lang)
		p.send(map[string]any{"type": "warning",
			"message": "no transcriber here handles that language — nothing you say will be interpreted"})
		return false
	}
	provider, _ := m.a.cfg.providerForLang(lang)

	if _, err := m.a.vsi().LegSTTStart(ctx, payload); err != nil {
		if isVSIConflict(err) {
			// A transcriber is already attached to this leg — from a racing
			// start, or a stop that has not landed. Whatever it is, it is not
			// the language we were asked for, so clear it and try once more.
			// Returning here (as an earlier version did) left the WRONG
			// language transcribing with nothing to notice.
			m.a.log.Warn("transcriber already attached, replacing it",
				"session", s.id, "participant", p.name, "leg_id", legID)
			m.stopSTTOnLeg(ctx, p, legID)
			if _, err = m.a.vsi().LegSTTStart(ctx, payload); err == nil {
				m.markSTT(p, lang, provider)
				m.a.log.Info("stt started", "session", s.id, "participant", p.name,
					"lang", lang, "provider", provider, "leg_id", legID)
				return true
			}
		}
		m.clearSTT(p)
		m.a.log.Error("start stt", "session", s.id, "leg_id", legID, "error", err)
		p.send(map[string]any{"type": "warning", "message": "transcription failed to start"})
		return false
	}
	m.markSTT(p, lang, provider)
	m.a.log.Info("stt started", "session", s.id, "participant", p.name,
		"lang", lang, "provider", provider, "leg_id", legID)
	return true
}

// markSTT records what is now running on this leg.
func (m *mediaManager) markSTT(p *participant, lang, provider string) {
	p.mu.Lock()
	p.sttOn, p.sttLang, p.sttProvider = true, lang, provider
	p.mu.Unlock()
}

// clearSTT records that nothing is transcribing this leg.
func (m *mediaManager) clearSTT(p *participant) {
	p.mu.Lock()
	p.sttOn, p.sttLang, p.sttProvider = false, "", ""
	p.mu.Unlock()
}

// stopSTTOnLeg tells VoiceBlender to drop the transcriber on a leg and records
// it locally. Tolerates there being none.
func (m *mediaManager) stopSTTOnLeg(ctx context.Context, p *participant, legID string) {
	m.clearSTT(p)
	if legID == "" {
		return
	}
	if _, err := m.a.vsi().LegSTTStop(ctx, voiceblender.IDPayload{ID: legID}); err != nil && !isVSINotFound(err) {
		m.a.log.Warn("stop stt", "leg_id", legID, "error", err)
	}
}

// stopSTT ends transcription on a participant's leg.
func (m *mediaManager) stopSTT(ctx context.Context, p *participant) {
	p.sttMu.Lock()
	defer p.sttMu.Unlock()
	p.mu.Lock()
	legID, on := p.legID, p.sttOn
	p.mu.Unlock()
	if !on {
		return
	}
	m.stopSTTOnLeg(ctx, p, legID)
}

// sttPayload builds the provider-specific STT options.
//
// The two supported providers want the language in different fields, and only
// one of them can do the speculative path:
//
//   - deepgram_flux takes language_hint (never `language`), and is the only
//     provider that emits eager_end_of_turn — the event the whole low-latency
//     design hangs off. Leaving eager_eot_threshold unset silently disables
//     those events, so a zero here means "run the slow path deliberately".
//   - speechmatics takes `language` and reports only real turn ends, so it
//     translates a full turn late. It is here because it covers languages Flux
//     does not.
func (m *mediaManager) sttPayload(legID, langCode string) (voiceblender.STTStartPayload, bool) {
	l := lookupLang(langCode)
	provider, ok := m.a.cfg.providerForLang(langCode)
	if !ok {
		// Dialling anyway would be rejected by the provider and the only symptom
		// would be silence from this participant, so refuse loudly instead.
		return voiceblender.STTStartPayload{}, false
	}
	code, _ := sttCode(provider, l)
	p := voiceblender.STTStartPayload{
		ID:       legID,
		Provider: provider,
		APIKey:   m.a.cfg.sttKeys[provider],
		Partial:  true, // partials drive the live captions
	}
	if provider == "deepgram_flux" {
		// No explicit Model: with hints present and no model, the server selects
		// flux-general-multi, which is the only Flux model that accepts hints.
		p.LanguageHints = []string{code}
		p.EagerEotThreshold = m.a.cfg.eagerEOT
	} else {
		p.Language = code
	}
	return p, true
}

// setLanguage changes a participant's language mid-session.
//
// The STT session is pinned to a language at start, so this restarts it. Any
// utterance already staged for the peer was synthesized from the old language
// and is dropped rather than committed.
func (m *mediaManager) setLanguage(ctx context.Context, s *session, p *participant, code string) {
	if !m.a.cfg.langSupported(code) {
		m.a.log.Warn("rejected unsupported language",
			"session", s.id, "participant", p.name, "lang", code)
		p.send(map[string]any{"type": "warning", "message": "that language is not available on this deployment"})
		// Send the authoritative value back, or the selector goes on showing a
		// language that was refused — the browser updates optimistically.
		p.send(map[string]any{"type": "lang", "who": p.id, "lang": p.getLang()})
		return
	}
	p.mu.Lock()
	if p.lang == code {
		p.mu.Unlock()
		return
	}
	p.lang = code
	p.mu.Unlock()

	m.a.interp.discardAllStaged(ctx, s, p)
	// startSTT reconciles: it notices the running language no longer matches and
	// replaces the transcriber. Doing an explicit stop first would open a window
	// for a concurrent start to re-attach the OLD language.
	m.startSTT(ctx, s, p)

	m.a.log.Info("language changed", "session", s.id, "participant", p.name, "lang", code)
	s.broadcast(map[string]any{"type": "lang", "who": p.id, "lang": code})
}

// setGender changes the voice a participant's words are spoken in on the other
// side.
//
// Unlike a language change this does NOT restart transcription — the speaker's
// own audio is unaffected, only the synthesis of their translated words. But
// anything already staged for the peer was synthesized in the old voice, so it
// is dropped rather than committed: hearing one sentence in a different voice is
// exactly the jarring effect the setting exists to avoid.
func (m *mediaManager) setGender(ctx context.Context, s *session, p *participant, code string) {
	if !knownGender(code) {
		return
	}
	p.mu.Lock()
	if p.gender == code {
		p.mu.Unlock()
		return
	}
	p.gender = code
	p.mu.Unlock()

	m.a.interp.discardAllStaged(ctx, s, p)
	m.a.log.Info("voice changed", "session", s.id, "participant", p.name, "gender", code)
	s.broadcast(map[string]any{"type": "gender", "who": p.id, "gender": code})
}

// legGone handles a media leg dropping unexpectedly (a VoiceBlender
// leg.disconnected event, e.g. an ICE failure). The participant keeps their
// browser socket — the page will re-offer — but everything hanging off the old
// leg is cleared, including any translation mid-flight towards them.
func (m *mediaManager) legGone(legID string) {
	s, p, ok := m.a.sessions.forLeg(legID)
	if !ok {
		return
	}
	m.a.log.Info("leg gone", "session", s.id, "participant", p.name, "leg_id", legID)
	m.a.sessions.unbindLeg(p)

	ctx := context.Background()
	// Anything this participant said that is still staged on the peer's leg is
	// now unspeakable-for; and anything playing to them has nowhere to play.
	m.a.interp.discardAllStaged(ctx, s, p)
	m.a.interp.clearInflight(ctx, p)

	p.send(map[string]any{"type": "state", "media": "down"})
	s.broadcast(map[string]any{"type": "state", "interpreting": false})
}

// leave tears down one participant's media and, when the session empties, the
// room itself.
func (m *mediaManager) leave(ctx context.Context, s *session, p *participant) {
	m.stopSTT(ctx, p)
	m.a.interp.discardAllStaged(ctx, s, p)
	m.a.interp.clearInflight(ctx, p)

	if legID := m.a.sessions.unbindLeg(p); legID != "" {
		if _, err := m.a.vsi().DeleteLeg(ctx, voiceblender.DeleteLegPayload{ID: legID}); err != nil && !isVSINotFound(err) {
			m.a.log.Warn("delete leg", "leg_id", legID, "error", err)
		}
	}

	nowEmpty := m.a.sessions.leave(s, p)
	if nowEmpty {
		if _, err := m.a.vsi().DeleteRoom(ctx, voiceblender.IDPayload{ID: m.a.vbRoom(s.id)}); err != nil && !isVSINotFound(err) {
			m.a.log.Warn("delete room", "session", s.id, "error", err)
		}
		m.a.log.Info("session ended", "session", s.id)
		return
	}
	// The remaining participant has nobody to be interpreted for: stop their STT
	// so we are not paying to transcribe into the void.
	for _, other := range s.members() {
		m.stopSTT(ctx, other)
		other.send(map[string]any{"type": "peer", "state": "left", "name": p.name})
		other.send(map[string]any{"type": "state", "interpreting": false})
	}
}
