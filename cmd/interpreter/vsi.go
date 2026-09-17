package main

import (
	"context"

	voiceblender "github.com/VoiceBlender/voiceblender-go"
)

// vsiClient is the subset of the VSI EventStream this app issues commands
// through.
//
// *voiceblender.EventStream satisfies it as-is; the interface exists so the turn
// state machine — the part with all the interesting edge cases, and the part
// that is hard to exercise against a live media server — can be driven against a
// recorder in tests. Nothing here changes how the app talks to VoiceBlender: it
// is still one VSI WebSocket, and the REST client is still never used.
type vsiClient interface {
	CreateRoom(ctx context.Context, payload voiceblender.CreateRoomRequest) (voiceblender.Room, error)
	DeleteRoom(ctx context.Context, payload voiceblender.IDPayload) (voiceblender.VSIStatusResponse, error)
	AddLegToRoom(ctx context.Context, payload voiceblender.AddLegPayload) (voiceblender.AddLegToRoomResult, error)
	DeleteLeg(ctx context.Context, payload voiceblender.DeleteLegPayload) (voiceblender.VSIStatusResponse, error)
	RoomRoutingSet(ctx context.Context, payload voiceblender.RoomRoutingSetPayload) (voiceblender.RoomRoutingView, error)

	WebRTCOffer(ctx context.Context, payload voiceblender.WebRTCOfferRequest) (voiceblender.WebRTCOfferResult, error)
	WebRTCAddCandidate(ctx context.Context, payload voiceblender.VSIWebRTCAddCandidatePayload) (voiceblender.VSIStatusResponse, error)
	WebRTCGetCandidates(ctx context.Context, payload voiceblender.IDPayload) (voiceblender.WebRTCCandidatesResult, error)

	LegSTTStart(ctx context.Context, payload voiceblender.STTStartPayload) (voiceblender.STTStartLegResult, error)
	LegSTTStop(ctx context.Context, payload voiceblender.IDPayload) (voiceblender.STTStopResult, error)

	LegTTS(ctx context.Context, payload voiceblender.TTSStartPayload) (voiceblender.TTSStartResult, error)
	LegTTSPreflight(ctx context.Context, payload voiceblender.TTSStartPayload) (voiceblender.TTSStartResult, error)
	LegTTSCommit(ctx context.Context, payload voiceblender.TTSTargetPayload) (voiceblender.TTSStartResult, error)
	LegTTSDiscard(ctx context.Context, payload voiceblender.TTSTargetPayload) (voiceblender.TTSDiscardResult, error)
	LegPlayStop(ctx context.Context, payload voiceblender.PlaybackTargetPayload) (voiceblender.PlaybackStopResult, error)
}

// compile-time proof that the real stream still satisfies the interface, so a
// future SDK bump that changes a signature fails here rather than at a call site.
var _ vsiClient = (*voiceblender.EventStream)(nil)
