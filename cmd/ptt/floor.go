package main

import (
	"context"
	"sync"
	"time"

	voiceblender "github.com/VoiceBlender/voiceblender-go"
)

// A burst is one active transmission in a room: exactly one speaker holds the
// floor while everyone else present listens. It exists only between a PTT press
// and its release; when there is no burst, the room has no VoiceBlender room and
// no media legs allocated.
type burst struct {
	roomID string
	holder *pttSession
}

// floorManager enforces single-speaker floor control per room and orchestrates
// the on-demand VoiceBlender media: on a granted press it creates the room and
// has each participant's browser offer a WebRTC leg (speaker un-muted, listeners
// muted); on release it deletes every leg and the room. Everything is driven
// over the VSI EventStream (a.vsi()).
type floorManager struct {
	a      *app
	mu     sync.Mutex
	active map[string]*burst // room id → current burst
}

func newFloorManager(a *app) *floorManager {
	return &floorManager{a: a, active: make(map[string]*burst)}
}

// speaker returns the username currently holding a room's floor ("" if idle).
func (f *floorManager) speaker(roomID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b := f.active[roomID]; b != nil {
		return b.holder.username
	}
	return ""
}

// press attempts to grant sess the floor in its room. If the floor is already
// held it replies floor.denied; otherwise it opens the VoiceBlender room and
// tells sess (speaker) and every other present member (listeners) to start
// their media.
func (f *floorManager) press(sess *pttSession) {
	roomID := sess.roomID
	f.mu.Lock()
	if b := f.active[roomID]; b != nil {
		holder := b.holder.username
		f.mu.Unlock()
		sess.send(map[string]any{"type": "floor.denied", "by": holder})
		return
	}
	f.active[roomID] = &burst{roomID: roomID, holder: sess}
	f.mu.Unlock()

	ctx := context.Background()
	// One VoiceBlender room per active burst. Tagged with our app_id and
	// namespaced by it, so a VoiceBlender shared with the other examples can
	// neither confuse our events nor collide on a room id. Tolerate a leftover
	// room from an unclean teardown.
	if _, err := f.a.vsi().CreateRoom(ctx, voiceblender.CreateRoomRequest{
		ID:    f.a.vbRoom(roomID),
		AppID: f.a.appID,
	}); err != nil && !isVSIConflict(err) {
		f.a.log.Error("open channel", "room", roomID, "error", err)
		f.mu.Lock()
		delete(f.active, roomID)
		f.mu.Unlock()
		sess.send(map[string]any{"type": "floor.error", "message": "could not open channel"})
		return
	}

	sess.setRole(roleSpeaker)
	sess.send(map[string]any{"type": "floor.granted"})
	for _, m := range f.a.presence.membersInRoom(roomID) {
		if m == sess {
			continue
		}
		m.setRole(roleListener)
		m.send(map[string]any{"type": "listen.start"})
	}
	f.broadcastSpeaker(roomID, sess.username)
	f.a.log.Info("floor granted", "room", roomID, "speaker", sess.username)
}

// release ends the burst if sess is the current holder (no-op otherwise).
func (f *floorManager) release(sess *pttSession) {
	f.endBurstIfHolder(sess)
}

// endBurstIfHolder tears the burst down when sess holds the floor. Returns true
// if a burst was ended.
func (f *floorManager) endBurstIfHolder(sess *pttSession) bool {
	roomID := sess.roomID
	f.mu.Lock()
	b := f.active[roomID]
	if b == nil || b.holder != sess {
		f.mu.Unlock()
		return false
	}
	delete(f.active, roomID)
	f.mu.Unlock()
	f.teardown(roomID)
	f.a.log.Info("floor released", "room", roomID, "speaker", sess.username)
	return true
}

// teardown deletes every participant's media leg and the VoiceBlender room, and
// tells every browser to stop. Called exactly once per burst.
func (f *floorManager) teardown(roomID string) {
	ctx := context.Background()
	for _, m := range f.a.presence.membersInRoom(roomID) {
		if leg := m.leg(); leg != "" {
			f.a.presence.unbindLeg(m)
			if _, err := f.a.vsi().DeleteLeg(ctx, voiceblender.DeleteLegPayload{ID: leg}); err != nil && !isVSINotFound(err) {
				f.a.log.Warn("delete leg", "leg_id", leg, "error", err)
			}
		}
		m.setRole(roleNone)
		m.send(map[string]any{"type": "stop"})
	}
	if _, err := f.a.vsi().DeleteRoom(ctx, voiceblender.IDPayload{ID: f.a.vbRoom(roomID)}); err != nil && !isVSINotFound(err) {
		f.a.log.Warn("delete room", "room", roomID, "error", err)
	}
	f.broadcastSpeaker(roomID, "")
}

// offer handles a browser's WebRTC offer during a burst: it establishes the
// media leg and joins it to the room (muted for listeners, un-muted for the
// speaker), then streams server ICE candidates back.
func (f *floorManager) offer(ctx context.Context, sess *pttSession, sdp string) {
	if sdp == "" {
		sess.send(map[string]any{"type": "webrtc.error", "message": "sdp required"})
		return
	}
	role := sess.getRole()
	if role == roleNone {
		// The burst ended before the offer arrived — nothing to join.
		sess.send(map[string]any{"type": "stop"})
		return
	}
	// Tag the browser leg with our app_id so its events carry it. This is what
	// lets VoiceBlender filter the VSI stream for us (see manageStream): an
	// untagged leg's events would be dropped by the filter and we'd never learn
	// the leg had gone.
	resp, err := f.a.vsi().WebRTCOffer(ctx, voiceblender.WebRTCOfferRequest{SDP: sdp, AppID: f.a.appID})
	if err != nil {
		f.a.log.Error("webrtc offer", "user", sess.username, "error", err)
		sess.send(map[string]any{"type": "webrtc.error", "message": "offer failed"})
		return
	}
	// If the burst ended while we were negotiating, discard the fresh leg so it
	// doesn't linger against a deleted room.
	if sess.getRole() == roleNone {
		if _, derr := f.a.vsi().DeleteLeg(ctx, voiceblender.DeleteLegPayload{ID: resp.LegID}); derr != nil && !isVSINotFound(derr) {
			f.a.log.Warn("discard stale leg", "leg_id", resp.LegID, "error", derr)
		}
		sess.send(map[string]any{"type": "stop"})
		return
	}
	f.a.presence.bindLeg(sess, resp.LegID)
	mute := role == roleListener
	if _, err := f.a.vsi().AddLegToRoom(ctx, voiceblender.AddLegPayload{RoomID: f.a.vbRoom(sess.roomID), LegID: resp.LegID, Mute: mute}); err != nil {
		f.a.log.Warn("add leg to room", "leg_id", resp.LegID, "room", sess.roomID, "error", err)
	}
	sess.send(map[string]any{"type": "webrtc.answer", "leg_id": resp.LegID, "sdp": resp.SDP})
	go f.pushCandidates(ctx, resp.LegID, sess)
}

// remoteCandidate forwards a browser ICE candidate to its media leg.
func (f *floorManager) remoteCandidate(ctx context.Context, sess *pttSession, cand voiceblender.ICECandidateInit) {
	leg := sess.leg()
	if leg == "" {
		return // browser raced its first candidate ahead of the answer
	}
	if _, err := f.a.vsi().WebRTCAddCandidate(ctx, voiceblender.VSIWebRTCAddCandidatePayload{ID: leg, Candidate: cand}); err != nil && !isVSINotFound(err) {
		f.a.log.Warn("add ice candidate", "leg_id", leg, "error", err)
	}
}

// pushCandidates polls VoiceBlender for the leg's server-gathered ICE candidates
// and forwards each to the browser. Exits when gathering completes, the leg is
// gone, or ctx ends.
func (f *floorManager) pushCandidates(ctx context.Context, legID string, sess *pttSession) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		// Stop once the leg has been torn down (burst ended / re-offered).
		if sess.leg() != legID {
			return
		}
		resp, err := f.a.vsi().WebRTCGetCandidates(ctx, voiceblender.IDPayload{ID: legID})
		if err != nil {
			if isVSINotFound(err) {
				return
			}
			continue
		}
		for _, cand := range resp.Candidates {
			sess.send(map[string]any{"type": "webrtc.candidate", "candidate": cand})
		}
		if resp.Done {
			return
		}
	}
}

// onJoin brings a session that entered a room mid-burst into the current
// transmission as a listener.
func (f *floorManager) onJoin(sess *pttSession) {
	f.mu.Lock()
	b := f.active[sess.roomID]
	f.mu.Unlock()
	if b == nil || b.holder == sess {
		return
	}
	sess.setRole(roleListener)
	sess.send(map[string]any{"type": "listen.start"})
	sess.send(map[string]any{"type": "speaker", "user": b.holder.username})
}

// leave cleans up a departing session: if it holds the floor the whole burst
// ends; otherwise its listener leg (if any) is removed. The caller removes it
// from presence afterwards.
func (f *floorManager) leave(sess *pttSession) {
	if f.endBurstIfHolder(sess) {
		return
	}
	if leg := sess.leg(); leg != "" {
		f.a.presence.unbindLeg(sess)
		if _, err := f.a.vsi().DeleteLeg(context.Background(), voiceblender.DeleteLegPayload{ID: leg}); err != nil && !isVSINotFound(err) {
			f.a.log.Warn("delete leg", "leg_id", leg, "error", err)
		}
	}
	sess.setRole(roleNone)
}

// legGone handles a media leg dropping unexpectedly (VoiceBlender LegDisconnected
// event): if it was the speaker's leg the burst ends, otherwise the listener is
// simply left without media until the burst ends on its own.
func (f *floorManager) legGone(legID string) {
	sess, ok := f.a.presence.sessionForLeg(legID)
	if !ok {
		return
	}
	f.a.presence.unbindLeg(sess)
	f.endBurstIfHolder(sess)
}

// broadcastSpeaker tells every session in a room who currently holds the floor
// ("" when the floor is free), for the "who is speaking" indicator.
func (f *floorManager) broadcastSpeaker(roomID, username string) {
	for _, m := range f.a.presence.membersInRoom(roomID) {
		m.send(map[string]any{"type": "speaker", "user": username})
	}
}
