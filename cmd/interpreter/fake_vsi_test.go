package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"

	voiceblender "github.com/VoiceBlender/voiceblender-go"
)

// fakeVSI records every command the app issues and lets a test dictate what
// comes back. It stands in for the VSI EventStream so the turn state machine can
// be driven deterministically — no media server, no STT vendor, no clock.
type fakeVSI struct {
	mu    sync.Mutex
	calls []call

	// nextTTSID names the id returned by the next preflight or leg_tts.
	ttsSeq int
	// failPreflight makes the next preflight fail, to exercise the fallback.
	failPreflight bool
	// failCommit makes commits fail, as an expired staging would.
	failCommit bool

	// voices records the voice id of every synthesis request, in order.
	voices []string
}

func (f *fakeVSI) noteVoice(v string) {
	f.mu.Lock()
	f.voices = append(f.voices, v)
	f.mu.Unlock()
}

// lastVoice returns the voice used for the most recent synthesis.
func (f *fakeVSI) lastVoice() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.voices) == 0 {
		return ""
	}
	return f.voices[len(f.voices)-1]
}

type call struct {
	op   string
	id   string // leg or room id
	arg  string // tts id, text, or role, depending on op
	text string
}

func (f *fakeVSI) record(op, id, arg, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{op: op, id: id, arg: arg, text: text})
}

// ops returns just the operation names, in order — the usual assertion.
func (f *fakeVSI) ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.op)
	}
	return out
}

func (f *fakeVSI) find(op string) (call, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.op == op {
			return c, true
		}
	}
	return call{}, false
}

func (f *fakeVSI) count(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.op == op {
			n++
		}
	}
	return n
}

func (f *fakeVSI) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

// ── the interesting half ──────────────────────────────────────────────────────

func (f *fakeVSI) LegTTSPreflight(_ context.Context, p voiceblender.TTSStartPayload) (voiceblender.TTSStartResult, error) {
	f.mu.Lock()
	if f.failPreflight {
		f.mu.Unlock()
		f.record("preflight_failed", p.ID, "", p.Text)
		return voiceblender.TTSStartResult{}, fmt.Errorf("preflight refused")
	}
	f.ttsSeq++
	id := fmt.Sprintf("tts-%d", f.ttsSeq)
	f.mu.Unlock()
	f.noteVoice(p.Voice)
	f.record("preflight", p.ID, id, p.Text)
	return voiceblender.TTSStartResult{TTSID: id, Status: "staged"}, nil
}

func (f *fakeVSI) LegTTSCommit(_ context.Context, p voiceblender.TTSTargetPayload) (voiceblender.TTSStartResult, error) {
	f.mu.Lock()
	fail := f.failCommit
	f.mu.Unlock()
	if fail {
		f.record("commit_failed", p.ID, p.TTSID, "")
		return voiceblender.TTSStartResult{}, fmt.Errorf("staged utterance expired")
	}
	f.record("commit", p.ID, p.TTSID, "")
	return voiceblender.TTSStartResult{TTSID: p.TTSID, Status: "committed"}, nil
}

func (f *fakeVSI) LegTTSDiscard(_ context.Context, p voiceblender.TTSTargetPayload) (voiceblender.TTSDiscardResult, error) {
	f.record("discard", p.ID, p.TTSID, "")
	return voiceblender.TTSDiscardResult{Status: "discarded"}, nil
}

func (f *fakeVSI) LegTTS(_ context.Context, p voiceblender.TTSStartPayload) (voiceblender.TTSStartResult, error) {
	f.mu.Lock()
	f.ttsSeq++
	id := fmt.Sprintf("tts-%d", f.ttsSeq)
	f.mu.Unlock()
	f.noteVoice(p.Voice)
	f.record("tts", p.ID, id, p.Text)
	return voiceblender.TTSStartResult{TTSID: id, Status: "playing"}, nil
}

func (f *fakeVSI) LegPlayStop(_ context.Context, p voiceblender.PlaybackTargetPayload) (voiceblender.PlaybackStopResult, error) {
	f.record("play_stop", p.ID, p.PlaybackID, "")
	return voiceblender.PlaybackStopResult{}, nil
}

func (f *fakeVSI) LegSTTStart(_ context.Context, p voiceblender.STTStartPayload) (voiceblender.STTStartLegResult, error) {
	hint := ""
	if len(p.LanguageHints) > 0 {
		hint = p.LanguageHints[0]
	}
	f.record("stt_start", p.ID, hint, p.Language)
	return voiceblender.STTStartLegResult{Status: "started", LegID: p.ID}, nil
}

func (f *fakeVSI) LegSTTStop(_ context.Context, p voiceblender.IDPayload) (voiceblender.STTStopResult, error) {
	f.record("stt_stop", p.ID, "", "")
	return voiceblender.STTStopResult{}, nil
}

func (f *fakeVSI) RoomRoutingSet(_ context.Context, p voiceblender.RoomRoutingSetPayload) (voiceblender.RoomRoutingView, error) {
	// Record the matrix in a form a test can assert on.
	desc := ""
	for _, role := range []string{roleA, roleB} {
		srcs, present := p.Matrix[role]
		desc += fmt.Sprintf("%s=%v/%v;", role, present, len(srcs))
	}
	f.record("routing_set", p.RoomID, desc, "")
	return voiceblender.RoomRoutingView{Matrix: p.Matrix}, nil
}

func (f *fakeVSI) AddLegToRoom(_ context.Context, p voiceblender.AddLegPayload) (voiceblender.AddLegToRoomResult, error) {
	f.record("add_leg", p.LegID, p.Role, p.RoomID)
	return voiceblender.AddLegToRoomResult{RoomID: p.RoomID, LegID: p.LegID}, nil
}

// ── the boring half ───────────────────────────────────────────────────────────

func (f *fakeVSI) CreateRoom(_ context.Context, p voiceblender.CreateRoomRequest) (voiceblender.Room, error) {
	f.record("create_room", p.ID, p.AppID, "")
	return voiceblender.Room{ID: p.ID}, nil
}
func (f *fakeVSI) DeleteRoom(_ context.Context, p voiceblender.IDPayload) (voiceblender.VSIStatusResponse, error) {
	f.record("delete_room", p.ID, "", "")
	return voiceblender.VSIStatusResponse{}, nil
}
func (f *fakeVSI) DeleteLeg(_ context.Context, p voiceblender.DeleteLegPayload) (voiceblender.VSIStatusResponse, error) {
	f.record("delete_leg", p.ID, "", "")
	return voiceblender.VSIStatusResponse{}, nil
}
func (f *fakeVSI) WebRTCOffer(_ context.Context, p voiceblender.WebRTCOfferRequest) (voiceblender.WebRTCOfferResult, error) {
	f.mu.Lock()
	f.ttsSeq++
	leg := fmt.Sprintf("leg-%d", f.ttsSeq)
	f.mu.Unlock()
	f.record("webrtc_offer", leg, p.AppID, "")
	return voiceblender.WebRTCOfferResult{LegID: leg, SDP: "v=0"}, nil
}
func (f *fakeVSI) WebRTCAddCandidate(_ context.Context, p voiceblender.VSIWebRTCAddCandidatePayload) (voiceblender.VSIStatusResponse, error) {
	return voiceblender.VSIStatusResponse{}, nil
}
func (f *fakeVSI) WebRTCGetCandidates(_ context.Context, p voiceblender.IDPayload) (voiceblender.WebRTCCandidatesResult, error) {
	return voiceblender.WebRTCCandidatesResult{Done: true}, nil
}

// ── harness ───────────────────────────────────────────────────────────────────

// upperTranslator stands in for a real MT service: it "translates" by upper-casing,
// which makes it obvious in an assertion whether a string went through the hop.
type upperTranslator struct {
	mu    sync.Mutex
	calls int
}

func (u *upperTranslator) Translate(_ context.Context, text, _, _ string) (string, error) {
	u.mu.Lock()
	u.calls++
	u.mu.Unlock()
	return "<" + text + ">", nil
}
func (u *upperTranslator) Name() string { return "upper" }
func (u *upperTranslator) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

// testConfig is a two-engine setup: Flux preferred, Speechmatics covering the
// languages it cannot do, both with keys so both are considered available.
func testConfig() config {
	return config{
		sttProvider: "deepgram_flux",
		sttFallback: "speechmatics",
		sttKeys: map[string]string{
			"deepgram_flux": "dg-key",
			"speechmatics":  "sm-key",
		},
		eagerEOT:    0.7,
		ttsProvider: "elevenlabs",
		ttsModelID:  "eleven_flash_v2_5",
		ttsVoices:   defaultGenderVoices,
	}
}

// fluxOnlyConfig has no Speechmatics key, so only Flux's ten are available.
func fluxOnlyConfig() config {
	c := testConfig()
	c.sttKeys = map[string]string{"deepgram_flux": "dg-key"}
	return c
}

// newTestApp wires an app with a fake VSI and a fake translator, and seats two
// participants with connected legs — the state a real session reaches once both
// browsers have negotiated.
func newTestApp(t *testing.T) (*app, *fakeVSI, *upperTranslator, *session, *participant, *participant) {
	t.Helper()
	fake := &fakeVSI{}
	tr := &upperTranslator{}
	a := &app{
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		appID:   "interpreter-test",
		vsiFake: fake,
		tr:      tr,
		cfg:     testConfig(),
	}
	a.sessions = newSessionRegistry()
	a.media = newMediaManager(a)
	a.interp = newInterpreter(a)

	s := a.sessions.create()
	_, alice, err := a.sessions.join(s.id, "alice", "en", defaultGender, a.cfg)
	if err != nil {
		t.Fatalf("join alice: %v", err)
	}
	_, bob, err := a.sessions.join(s.id, "bob", "es", defaultGender, a.cfg)
	if err != nil {
		t.Fatalf("join bob: %v", err)
	}
	alice.lang, bob.lang = "en", "es"
	a.sessions.bindLeg(s, alice, "leg-alice")
	a.sessions.bindLeg(s, bob, "leg-bob")
	alice.live, bob.live = true, true

	fake.reset()
	return a, fake, tr, s, alice, bob
}

// turn synthesizes one stt.turn event from a speaker.
func turn(legID, event string, index int, text string) *voiceblender.STTTurnEvent {
	e := &voiceblender.STTTurnEvent{LegID: legID, TurnEvent: event, TurnIndex: index, Text: text}
	return e
}

// stagedEvent synthesizes the tts.staged event VoiceBlender emits once a
// preflighted utterance has finished synthesizing and is ready to commit.
func stagedEvent(listenerLeg, ttsID string) *voiceblender.TTSStagedEvent {
	return &voiceblender.TTSStagedEvent{LegID: listenerLeg, TTSID: ttsID, Bytes: 4096, DurationMs: 800}
}
