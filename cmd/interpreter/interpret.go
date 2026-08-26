package main

import (
	"context"
	"sort"
	"strings"
	"time"

	voiceblender "github.com/VoiceBlender/voiceblender-go"
)

// The hot path: turning one person's speech into the other person's language,
// fast enough that the conversation still feels like a conversation.
//
// The naive cascade waits for the speaker to stop, then transcribes, translates
// and synthesizes — a second or more of dead air after every sentence. This one
// runs the expensive half speculatively:
//
//	speaker still talking
//	  │
//	  ├─ eager_end_of_turn ──► translate ──► leg_tts_preflight   (audio buffered,
//	  │   ("probably done")                                       nothing played)
//	  │
//	  ├─ turn_resumed ───────► leg_tts_discard   (they carried on; throw it away)
//	  │
//	  └─ end_of_turn ────────► leg_tts_commit    (plays immediately — the audio
//	                                              already exists, so this costs
//	                                              no round trip to the TTS vendor)
//
// Everything expensive therefore happens during the speaker's final few hundred
// milliseconds, and what remains on the critical path is a commit and a mixer
// tick. When the speculation is wrong — the speaker resumed, or revised their
// wording — the staged audio is discarded and the slow path runs; correctness
// never depends on the guess being right.
type interpreter struct {
	a *app
}

func newInterpreter(a *app) *interpreter { return &interpreter{a: a} }

const (
	// stagedMax mirrors VoiceBlender's TTS_PREFLIGHT_MAX_PER_LEG default. The
	// server REFUSES a fourth staged utterance rather than evicting one, so we
	// evict our own oldest first — otherwise a burst of eager turns would start
	// failing to stage at exactly the moment speculation is most useful.
	stagedMax = 3

	// stagedTTL expires staged entries locally a little before VoiceBlender's
	// own 30 s preflight TTL, so a commit is never attempted against an id the
	// server has already dropped.
	stagedTTL = 25 * time.Second

	// eagerWait is how long end_of_turn will wait for an in-flight speculative
	// translation to finish before giving up and taking the slow path. Waiting
	// briefly is faster than restarting the work: the eager pass is usually
	// nearly done by the time the turn actually ends.
	eagerWait = 400 * time.Millisecond

	// readyWait is how long a commit will wait for synthesis to finish when the
	// turn ended sooner than the TTS vendor could produce audio.
	readyWait = 300 * time.Millisecond
)

// ── VoiceBlender events in ────────────────────────────────────────────────────

// onTurn handles a turn boundary from the speaker's transcriber. This is the
// state machine described above.
func (i *interpreter) onTurn(e *voiceblender.STTTurnEvent) {
	s, speaker, ok := i.a.sessions.forLeg(e.LegID)
	if !ok {
		return
	}
	listener := s.peerOf(speaker)
	if listener == nil {
		return // nobody to interpret for
	}
	// Someone is talking, so the idle timeout starts again from here.
	s.touch()
	ctx := context.Background()

	switch e.TurnEvent {
	case "start_of_turn":
		s.broadcast(map[string]any{"type": "speaking", "who": speaker.id, "on": true})

	case "eager_end_of_turn":
		// Only Deepgram Flux emits this, and only when eager_eot_threshold is
		// set. Everything below it is the speculative path.
		i.preflight(ctx, s, speaker, listener, e.TurnIndex, e.Text)

	case "turn_resumed":
		// The speaker carried on: the guess was wrong, throw the audio away.
		i.discardStaged(ctx, s, speaker, e.TurnIndex)

	case "end_of_turn":
		s.broadcast(map[string]any{"type": "speaking", "who": speaker.id, "on": false})
		i.finishTurn(ctx, s, speaker, listener, e.TurnIndex, e.Text)
	}
}

// onText handles a transcript fragment. Its main job is the live caption track;
// it only drives speech for providers that have no turn events of their own.
func (i *interpreter) onText(e *voiceblender.STTTextEvent) {
	s, speaker, ok := i.a.sessions.forLeg(e.LegID)
	if !ok {
		return
	}
	text := strings.TrimSpace(e.Text)
	if text == "" {
		return
	}
	s.touch()
	s.broadcast(map[string]any{
		"type": "caption", "who": speaker.id, "original": text, "final": e.IsFinal,
	})

	// Flux reports end_of_turn as BOTH a turn event and a final transcript.
	// Speaking here as well would say everything twice, so for a leg on Flux
	// this handler is captions only and onTurn owns the speech.
	//
	// This is per SPEAKER, not per deployment: with per-language routing one
	// participant can be on Flux while the other is on Speechmatics.
	if speaker.usesTurnEvents() {
		return
	}
	if !e.IsFinal {
		return
	}
	listener := s.peerOf(speaker)
	if listener == nil {
		return
	}
	i.translateAndSpeak(context.Background(), s, speaker, listener, text)
}

// onStaged marks a speculative utterance as synthesized and ready to commit.
func (i *interpreter) onStaged(e *voiceblender.TTSStagedEvent) {
	// The event names the LISTENER's leg (that is where the audio is staged),
	// but the bookkeeping lives on the SPEAKER whose words they are.
	s, listener, ok := i.a.sessions.forLeg(e.LegID)
	if !ok {
		return
	}
	speaker := s.peerOf(listener)
	if speaker == nil {
		return
	}
	// Look up by tts id, not by turn: this event can arrive AFTER end_of_turn
	// has already pulled the entry out of the turn map, and a commit blocked on
	// synthesis is waiting to be woken by exactly this.
	speaker.markStagedReady(e.TTSID)
}

// onFinished clears the listener's in-flight slot when playback ends naturally.
func (i *interpreter) onFinished(e *voiceblender.TTSFinishedEvent) {
	_, listener, ok := i.a.sessions.forLeg(e.LegID)
	if !ok {
		return
	}
	listener.mu.Lock()
	if listener.inflight == e.TTSID {
		listener.inflight = ""
	}
	listener.mu.Unlock()
}

// onTTSError clears the in-flight slot and tells the room, so a silent failure
// does not look like the other person simply going quiet.
func (i *interpreter) onTTSError(e *voiceblender.TTSErrorEvent) {
	s, listener, ok := i.a.sessions.forLeg(e.LegID)
	if !ok {
		return
	}
	listener.mu.Lock()
	if listener.inflight == e.TTSID {
		listener.inflight = ""
	}
	listener.mu.Unlock()

	i.a.log.Error("tts failed", "session", s.id, "leg_id", e.LegID,
		"category", e.Category, "error", e.Error)
	listener.send(map[string]any{"type": "warning", "message": "could not speak the translation"})
}

// ── the speculative path ──────────────────────────────────────────────────────

// preflight translates a probably-finished turn and stages the audio on the
// listener's leg, ready to be committed the moment the turn really ends.
func (i *interpreter) preflight(ctx context.Context, s *session, speaker, listener *participant, turnIndex int, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	listenerLeg := listener.leg()
	if listenerLeg == "" {
		return
	}

	// Publish a barrier before doing any slow work, so an end_of_turn arriving
	// mid-translation waits for this instead of racing it and duplicating the
	// whole cascade.
	done := make(chan struct{})
	speaker.mu.Lock()
	if _, busy := speaker.pending[turnIndex]; busy {
		speaker.mu.Unlock()
		return // already speculating on this turn
	}
	if speaker.pending == nil {
		speaker.pending = make(map[int]chan struct{})
	}
	speaker.pending[turnIndex] = done
	speaker.mu.Unlock()

	defer func() {
		speaker.mu.Lock()
		delete(speaker.pending, turnIndex)
		speaker.mu.Unlock()
		close(done)
	}()

	from, to := speaker.getLang(), listener.getLang()
	translated, err := i.a.tr.Translate(ctx, text, from, to)
	if err != nil {
		i.a.log.Warn("speculative translate failed", "session", s.id, "error", err)
		return // end_of_turn will take the slow path
	}

	// If the turn ended while we were translating, this audio is already stale —
	// finishTurn has moved on, so staging it now would only leak a preflight.
	if i.turnIsDone(speaker, turnIndex) {
		return
	}

	i.evictOldestStaged(ctx, s, speaker, listener)

	res, err := i.a.vsi().LegTTSPreflight(ctx, i.ttsPayload(listenerLeg, translated, speaker))
	if err != nil {
		i.a.log.Warn("preflight tts", "session", s.id, "leg_id", listenerLeg, "error", err)
		return
	}
	st := &staged{ttsID: res.TTSID, src: text, text: translated, at: time.Now(), readyCh: make(chan struct{})}

	speaker.mu.Lock()
	stale := turnIndex <= speaker.maxDone
	speaker.mu.Unlock()
	if !stale {
		speaker.putStaged(turnIndex, st)
	}

	if stale {
		// The turn finished during the preflight round trip. Drop it rather than
		// leave it occupying one of the three per-leg staging slots.
		i.discardID(ctx, listenerLeg, res.TTSID)
		return
	}
	s.broadcast(map[string]any{
		"type": "caption", "who": speaker.id, "original": text, "translated": translated, "final": false,
	})
}

// finishTurn is the end of a turn: commit the speculative audio if it still
// matches what was actually said, otherwise fall back to translating for real.
func (i *interpreter) finishTurn(ctx context.Context, s *session, speaker, listener *participant, turnIndex int, finalText string) {
	finalText = strings.TrimSpace(finalText)

	speaker.mu.Lock()
	if turnIndex > speaker.maxDone {
		speaker.maxDone = turnIndex
	}
	wait := speaker.pending[turnIndex]
	speaker.mu.Unlock()

	// Give an in-flight speculative pass a moment to land. It is usually almost
	// finished, and waiting for it is faster than redoing it.
	if wait != nil {
		select {
		case <-wait:
		case <-time.After(eagerWait):
		}
	}

	st := speaker.takeStaged(turnIndex)
	defer speaker.releaseStaged(st)

	listenerLeg := listener.leg()
	if listenerLeg == "" {
		if st != nil {
			i.a.log.Debug("listener leg gone, dropping staged", "session", s.id)
		}
		return
	}

	if st != nil && st.src == finalText {
		if i.commitStaged(ctx, s, speaker, listener, listenerLeg, st) {
			return
		}
		// Commit failed (expired or still synthesizing) — fall through and say
		// it the slow way rather than dropping the sentence.
	} else if st != nil {
		// The transcript was revised after the eager guess: the staged audio is
		// the wrong words.
		i.a.log.Debug("eager transcript revised, re-translating",
			"session", s.id, "eager", st.src, "final", finalText)
		i.discardID(ctx, listenerLeg, st.ttsID)
	}

	if finalText == "" {
		return
	}
	i.translateAndSpeak(ctx, s, speaker, listener, finalText)
}

// commitStaged plays an already-synthesized utterance. Reports whether it
// actually started.
func (i *interpreter) commitStaged(ctx context.Context, s *session, speaker, listener *participant, listenerLeg string, st *staged) bool {
	// The turn can end before the vendor finishes synthesizing. Committing then
	// would be rejected, so wait briefly for tts.staged.
	if !waitReady(st) {
		i.discardID(ctx, listenerLeg, st.ttsID)
		return false
	}
	// One voice at a time on a leg: concurrent leg_tts overlap audibly.
	i.stopInflight(ctx, listener)

	if _, err := i.a.vsi().LegTTSCommit(ctx, voiceblender.TTSTargetPayload{ID: listenerLeg, TTSID: st.ttsID}); err != nil {
		i.a.log.Warn("commit staged tts", "session", s.id, "tts_id", st.ttsID, "error", err)
		return false
	}
	listener.mu.Lock()
	listener.inflight = st.ttsID
	listener.mu.Unlock()

	s.broadcast(map[string]any{
		"type": "caption", "who": speaker.id, "original": st.src, "translated": st.text, "final": true,
	})
	return true
}

// waitReady blocks until an utterance has finished synthesizing, up to readyWait.
//
// The readiness handshake is the closing of readyCh, NOT the ready field: that
// field is written under the participant's lock by markStagedReady, so reading
// it from here would be an unsynchronized read of shared state. A closed channel
// makes the first case fire immediately, so the already-synthesized path — which
// is the common one — costs nothing.
func waitReady(st *staged) bool {
	select {
	case <-st.readyCh:
		return true
	default:
	}
	t := time.NewTimer(readyWait)
	defer t.Stop()
	select {
	case <-st.readyCh:
		return true
	case <-t.C:
		return false
	}
}

// ── the plain path ────────────────────────────────────────────────────────────

// translateAndSpeak is the unspeculative cascade: translate now, synthesize now,
// play now. It runs when there was no eager guess, when the guess was wrong, or
// when the STT provider has no eager end-of-turn at all.
func (i *interpreter) translateAndSpeak(ctx context.Context, s *session, speaker, listener *participant, text string) {
	from, to := speaker.getLang(), listener.getLang()
	translated, err := i.a.tr.Translate(ctx, text, from, to)
	if err != nil {
		i.a.log.Error("translate", "session", s.id, "from", from, "to", to, "error", err)
		speaker.send(map[string]any{"type": "warning", "message": "translation failed"})
		return
	}
	listenerLeg := listener.leg()
	if listenerLeg == "" {
		return
	}
	i.stopInflight(ctx, listener)

	res, err := i.a.vsi().LegTTS(ctx, i.ttsPayload(listenerLeg, translated, speaker))
	if err != nil {
		i.a.log.Error("speak translation", "session", s.id, "leg_id", listenerLeg, "error", err)
		return
	}
	listener.mu.Lock()
	listener.inflight = res.TTSID
	listener.mu.Unlock()

	s.broadcast(map[string]any{
		"type": "caption", "who": speaker.id, "original": text, "translated": translated, "final": true,
	})
}

// ── helpers ───────────────────────────────────────────────────────────────────

// ttsPayload builds the synthesis request.
//
// The voice follows the SPEAKER, not the listener: Bob hears Alice's words in a
// voice matching the gender Alice selected. eleven_flash_v2_5 is multilingual,
// so that voice carries across every language we offer and does not change when
// someone switches language.
func (i *interpreter) ttsPayload(legID, text string, speaker *participant) voiceblender.TTSStartPayload {
	return voiceblender.TTSStartPayload{
		ID:       legID,
		Text:     text,
		Voice:    i.a.voiceFor(speaker.getGender()),
		ModelID:  i.a.cfg.ttsModelID,
		Provider: i.a.cfg.ttsProvider,
		APIKey:   i.a.cfg.ttsAPIKey,
	}
}

// stopInflight cuts any translation currently playing to this listener.
//
// VoiceBlender neither queues nor interrupts concurrent leg_tts on one leg — it
// mixes them, and you hear both at once. Serializing is the app's job, and
// leg_play_stop is how: a tts_id is a valid playback_id.
func (i *interpreter) stopInflight(ctx context.Context, listener *participant) {
	listener.mu.Lock()
	ttsID, legID := listener.inflight, listener.legID
	listener.inflight = ""
	listener.mu.Unlock()
	if ttsID == "" || legID == "" {
		return
	}
	if _, err := i.a.vsi().LegPlayStop(ctx, voiceblender.PlaybackTargetPayload{ID: legID, PlaybackID: ttsID}); err != nil && !isVSINotFound(err) {
		i.a.log.Debug("stop in-flight tts", "leg_id", legID, "tts_id", ttsID, "error", err)
	}
}

// clearInflight forgets (and stops) whatever was playing to a participant.
func (i *interpreter) clearInflight(ctx context.Context, listener *participant) {
	i.stopInflight(ctx, listener)
}

// turnIsDone reports whether a turn has already been finished by finishTurn.
func (i *interpreter) turnIsDone(speaker *participant, turnIndex int) bool {
	speaker.mu.Lock()
	defer speaker.mu.Unlock()
	return turnIndex <= speaker.maxDone
}

// evictOldestStaged makes room for another speculative utterance.
//
// VoiceBlender caps staged utterances per leg and returns 409 on the one that
// would exceed the cap — it refuses rather than evicting. So we evict, oldest
// turn first, before asking.
func (i *interpreter) evictOldestStaged(ctx context.Context, s *session, speaker, listener *participant) {
	speaker.mu.Lock()
	if len(speaker.staged) < stagedMax {
		speaker.mu.Unlock()
		return
	}
	idxs := make([]int, 0, len(speaker.staged))
	for idx := range speaker.staged {
		idxs = append(idxs, idx)
	}
	sort.Ints(idxs)
	oldest := idxs[0]
	speaker.mu.Unlock()

	st := speaker.takeStaged(oldest)
	if st == nil {
		return
	}
	defer speaker.releaseStaged(st)
	i.a.log.Debug("evicting oldest staged utterance", "session", s.id, "turn", oldest)
	i.discardID(ctx, listener.leg(), st.ttsID)
}

// discardStaged drops one staged utterance for a turn.
func (i *interpreter) discardStaged(ctx context.Context, s *session, speaker *participant, turnIndex int) {
	st := speaker.takeStaged(turnIndex)
	if st == nil {
		return
	}
	defer speaker.releaseStaged(st)
	listener := s.peerOf(speaker)
	if listener == nil {
		return
	}
	i.discardID(ctx, listener.leg(), st.ttsID)
}

// discardAllStaged drops every utterance staged for a participant's peer. Used
// when a language changes, a leg drops, or someone leaves — in each case the
// staged audio is either the wrong language or has nowhere to play.
func (i *interpreter) discardAllStaged(ctx context.Context, s *session, speaker *participant) {
	all := speaker.dropAllStaged()
	if len(all) == 0 {
		return
	}
	listener := s.peerOf(speaker)
	if listener == nil {
		return
	}
	legID := listener.leg()
	for _, st := range all {
		i.discardID(ctx, legID, st.ttsID)
	}
}

// discardID drops one staged tts id, tolerating one already gone.
func (i *interpreter) discardID(ctx context.Context, legID, ttsID string) {
	if legID == "" || ttsID == "" {
		return
	}
	if _, err := i.a.vsi().LegTTSDiscard(ctx, voiceblender.TTSTargetPayload{ID: legID, TTSID: ttsID}); err != nil && !isVSINotFound(err) {
		i.a.log.Debug("discard staged tts", "leg_id", legID, "tts_id", ttsID, "error", err)
	}
}

// sweepStaged expires staged utterances that were never committed — a turn that
// never ended because the speaker's leg dropped mid-sentence, say. Without this
// they would sit in the per-leg staging slots until VoiceBlender's own TTL, and
// three stale entries block all further speculation on that leg.
func (i *interpreter) sweepStaged(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		cutoff := time.Now().Add(-stagedTTL)
		for _, s := range i.a.sessions.all() {
			for _, speaker := range s.members() {
				listener := s.peerOf(speaker)
				if listener == nil {
					continue
				}
				speaker.mu.Lock()
				var expired []*staged
				for idx, st := range speaker.staged {
					if st.at.Before(cutoff) {
						expired = append(expired, st)
						delete(speaker.staged, idx)
						delete(speaker.stagedByID, st.ttsID)
					}
				}
				speaker.mu.Unlock()
				for _, st := range expired {
					i.discardID(ctx, listener.leg(), st.ttsID)
				}
			}
		}
	}
}
