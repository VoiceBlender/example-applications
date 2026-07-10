package main

import (
	"context"
	"sync"
	"time"

	voiceblender "github.com/VoiceBlender/voiceblender-go"
)

// A fork rings several extensions simultaneously and connects the caller to the
// first one that answers, cancelling the rest. Each target is its own outbound
// leg (no room auto-join); when one connects it becomes a normal bridge so hold,
// teardown, and the live-calls panel work uniformly.

type forkTarget struct {
	number string
	aor    string
	codecs []string
	// sess is set for a WebRTC "virtual device" target: instead of originating a
	// SIP leg we ring the already-live browser leg (sess.leg()) and wait for the
	// browser to accept over its phone WS. nil ⇒ ordinary SIP originate.
	sess *phoneSession
}

// forkCand is one ringing candidate in a fork. sess non-nil marks it a WebRTC
// device (a pre-existing browser leg that must be detached, never deleted).
type forkCand struct {
	number string
	sess   *phoneSession
}

func (c forkCand) webrtc() bool { return c.sess != nil }

type forkGroup struct {
	roomID         string
	tenantID       string
	aLeg           string
	callerAnswered bool
	from           string
	to             string // label, e.g. "1001,1002"
	via            string
	kind           string
	ringbackPB     string
	startedAt      time.Time
	onNoAnswer     func() // resume the dial plan on the caller if nobody answers

	mu         sync.Mutex
	candidates map[string]forkCand // candidate legID → candidate
	won        bool
	ringTimer  *time.Timer // bounds WebRTC-device ringing (SIP legs self-timeout)
}

type forkRegistry struct {
	mu    sync.Mutex
	byLeg map[string]*forkGroup // caller leg + each candidate leg → group
}

func newForkRegistry() *forkRegistry { return &forkRegistry{byLeg: make(map[string]*forkGroup)} }

func (r *forkRegistry) add(g *forkGroup) {
	r.mu.Lock()
	r.byLeg[g.aLeg] = g
	for leg := range g.candidates {
		r.byLeg[leg] = g
	}
	r.mu.Unlock()
}

func (r *forkRegistry) get(legID string) (*forkGroup, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.byLeg[legID]
	return g, ok
}

// removeLeg drops a single candidate leg key (the group stays live for the rest).
func (r *forkRegistry) removeLeg(legID string) {
	r.mu.Lock()
	delete(r.byLeg, legID)
	r.mu.Unlock()
}

// remove drops every leg key pointing at g.
func (r *forkRegistry) remove(g *forkGroup) {
	r.mu.Lock()
	for k, v := range r.byLeg {
		if v == g {
			delete(r.byLeg, k)
		}
	}
	r.mu.Unlock()
}

// views projects each active fork (still ringing) into the live-calls panel.
func (r *forkRegistry) views(tenantID string) []callView {
	r.mu.Lock()
	seen := make(map[*forkGroup]struct{})
	groups := make([]*forkGroup, 0)
	for _, g := range r.byLeg {
		if _, ok := seen[g]; ok || g.tenantID != tenantID {
			continue
		}
		seen[g] = struct{}{}
		groups = append(groups, g)
	}
	r.mu.Unlock()
	out := make([]callView, 0, len(groups))
	for _, g := range groups {
		out = append(out, callView{
			ID: g.roomID, From: g.from, To: g.to, Kind: g.kind, Via: g.via,
			State: "ringing", Since: g.startedAt.UTC().Format(time.RFC3339),
		})
	}
	return out
}

func (a *app) forkViews(tenantID string) []callView { return a.forks.views(tenantID) }

// startFork rings all targets at once and waits for the first to answer.
func (a *app) startFork(aLeg string, targets []forkTarget, fromCLI string, callerAnswered bool, meta callMeta) {
	ctx := context.Background()
	roomID := "call-" + aLeg

	if _, err := a.vsi().CreateRoom(ctx, voiceblender.CreateRoomRequest{ID: roomID}); err != nil && !isVSIConflict(err) {
		a.log.Error("create room", "room", roomID, "error", err)
		a.hangup(aLeg, "unavailable")
		return
	}

	// Ring the caller while all targets ring.
	var ringbackPB string
	if callerAnswered {
		if pb, err := a.vsi().LegPlayStart(ctx, voiceblender.PlaybackStartPayload{ID: aLeg, Tone: "gb_ringback", Repeat: -1}); err != nil {
			a.log.Warn("play ringback", "leg_id", aLeg, "error", err)
		} else {
			ringbackPB = pb.PlaybackID
		}
	} else if _, err := a.vsi().LegRing(ctx, voiceblender.IDPayload{ID: aLeg}); err != nil {
		a.log.Warn("ring caller (180)", "leg_id", aLeg, "error", err)
	}

	ringTime := meta.ringTime
	if ringTime <= 0 {
		ringTime = 60
	}
	g := &forkGroup{
		roomID: roomID, tenantID: meta.tenantID, aLeg: aLeg, callerAnswered: callerAnswered,
		from: meta.from, to: meta.to, via: meta.via, kind: meta.kind,
		ringbackPB: ringbackPB, startedAt: time.Now(), onNoAnswer: meta.onNoAnswer,
		candidates: make(map[string]forkCand),
	}

	// Each target either originates a SIP leg (no RoomID, so none auto-bridges;
	// the winner is joined into the room explicitly when it answers) or, for a
	// WebRTC device, rings its already-live browser leg via a phone-WS push.
	hasWebRTC := false
	var dndLegs []string // WebRTC devices in do-not-disturb → declined immediately
	for _, t := range targets {
		if t.sess != nil {
			leg := t.sess.leg()
			if leg == "" {
				continue // device signed out between target-build and now
			}
			g.candidates[leg] = forkCand{number: t.number, sess: t.sess}
			t.sess.setRing(meta.from) // remember caller (logged as missed if declined)
			if t.sess.isDnd() {
				// Do-not-disturb: decline this candidate like a SIP phone would.
				dndLegs = append(dndLegs, leg)
				continue
			}
			t.sess.setState(phoneRinging, "")
			t.sess.send(map[string]any{"type": "ring", "from": meta.from, "to": t.number, "call_id": roomID})
			hasWebRTC = true
			continue
		}
		raw, err := a.vsi().CreateLeg(ctx, voiceblender.CreateLegRequest{
			Type: "sip", To: t.aor, From: fromCLI, Codecs: t.codecs, RingTimeout: ringTime,
		})
		if err != nil {
			a.log.Warn("fork originate leg", "to", t.aor, "error", err)
			continue
		}
		if legID := parseLegID(raw); legID != "" {
			g.candidates[legID] = forkCand{number: t.number}
		}
	}

	if len(g.candidates) == 0 {
		if ringbackPB != "" {
			_, _ = a.vsi().LegPlayStop(ctx, voiceblender.PlaybackTargetPayload{ID: aLeg, PlaybackID: ringbackPB})
		}
		_, _ = a.vsi().DeleteRoom(ctx, voiceblender.IDPayload{ID: roomID})
		a.detachOrHangup(roomID, aLeg, "unavailable")
		return
	}

	a.forks.add(g)
	// WebRTC devices don't self-time-out like a ringing SIP leg, so bound the
	// whole fork with a timer that stops any still-ringing browsers.
	if hasWebRTC {
		g.ringTimer = time.AfterFunc(time.Duration(ringTime)*time.Second, func() { a.onForkRingTimeout(g) })
	}
	a.calls.Delete(aLeg) // if the caller was in the IVR, it's a live call now
	a.notifyChanged()
	a.log.Info("forking to extensions", "a_leg", aLeg, "targets", len(g.candidates), "room", roomID)
	// Decline any do-not-disturb devices now (busy). If they were the only
	// candidates, this takes the caller down the no-answer/reject path.
	for _, leg := range dndLegs {
		a.forkCandidateGone(g, leg, "declined")
	}
}

// onForkRingTimeout fires when a fork with WebRTC devices reaches its ring time:
// stop every still-ringing browser (SIP legs already self-timed-out). Dropping
// the last candidate takes the caller down the no-answer path.
func (a *app) onForkRingTimeout(g *forkGroup) {
	g.mu.Lock()
	if g.won {
		g.mu.Unlock()
		return
	}
	var webLegs []string
	for leg, c := range g.candidates {
		if c.webrtc() {
			webLegs = append(webLegs, leg)
		}
	}
	g.mu.Unlock()
	for _, leg := range webLegs {
		a.forkCandidateGone(g, leg, "timeout")
	}
}

// onForkConnected runs when a candidate answers. The first winner is bridged to
// the caller; the losers are cancelled and the fork becomes a normal bridge. For
// a WebRTC device this is driven by the browser's phone-WS "answer" (its leg is
// already media-live, so there's no leg.connected to react to).
func (a *app) onForkConnected(g *forkGroup, winner string) {
	ctx := context.Background()

	g.mu.Lock()
	winCand, isCand := g.candidates[winner]
	if g.won || !isCand {
		g.mu.Unlock()
		// A second answer raced in (or an unknown leg). Drop just this one:
		// stop a browser's ring (leg survives) or CANCEL a SIP leg.
		if winCand.webrtc() {
			a.softphoneStopRing(winCand.sess, "taken")
		} else if isCand {
			a.hangup(winner, "")
		}
		return
	}
	g.won = true
	if g.ringTimer != nil {
		g.ringTimer.Stop()
	}
	winNum := winCand.number
	type loser struct {
		leg  string
		cand forkCand
	}
	losers := make([]loser, 0, len(g.candidates))
	for leg, c := range g.candidates {
		if leg != winner {
			losers = append(losers, loser{leg, c})
		}
	}
	ringbackPB := g.ringbackPB
	g.mu.Unlock()

	dropLosers := func() {
		for _, l := range losers {
			if l.cand.webrtc() {
				a.softphoneStopRing(l.cand.sess, "taken")
			} else {
				a.hangup(l.leg, "")
			}
		}
	}

	if ringbackPB != "" {
		if _, err := a.vsi().LegPlayStop(ctx, voiceblender.PlaybackTargetPayload{ID: g.aLeg, PlaybackID: ringbackPB}); err != nil && !isVSINotFound(err) {
			a.log.Warn("stop ringback", "leg_id", g.aLeg, "error", err)
		}
	}
	if !g.callerAnswered {
		if _, err := a.vsi().AnswerLeg(ctx, voiceblender.AnswerLegPayload{ID: g.aLeg}); err != nil {
			a.log.Error("answer caller on fork connect", "leg_id", g.aLeg, "error", err)
			a.forks.remove(g)
			// winner is a fresh candidate leg — for a browser, keep it alive.
			if winCand.webrtc() {
				a.softphoneStopRing(winCand.sess, "failed")
			} else {
				a.hangup(winner, "")
			}
			dropLosers()
			_, _ = a.vsi().DeleteRoom(ctx, voiceblender.IDPayload{ID: g.roomID})
			a.detachOrHangup(g.roomID, g.aLeg, "")
			return
		}
	}
	// Bridge caller + winner in the room.
	_, _ = a.vsi().AddLegToRoom(ctx, voiceblender.AddLegPayload{RoomID: g.roomID, LegID: g.aLeg})
	if _, err := a.vsi().AddLegToRoom(ctx, voiceblender.AddLegPayload{RoomID: g.roomID, LegID: winner}); err != nil {
		a.log.Error("add winner to room", "leg_id", winner, "error", err)
	}
	if winCand.webrtc() {
		winCand.sess.setState(phoneInCall, g.roomID)
		winCand.sess.send(map[string]any{"type": "call.connected", "peer": g.from})
		if from := winCand.sess.takeRing(); from != "" {
			a.recordCall(winCand.sess, from, "in") // answered incoming call
		}
	}
	// If the caller is itself a softphone (an outbound call it placed), tell it
	// the far end answered so its UI leaves the "calling" state.
	a.notifySoftphoneCaller(g.roomID, g.aLeg, winNum)
	dropLosers()

	// Hand off to a normal (already-connected) bridge so hold/teardown/panel work.
	a.forks.remove(g)
	a.bridges.add(&bridge{
		roomID: g.roomID, tenantID: g.tenantID, aLeg: g.aLeg, bLeg: winner,
		from: g.from, to: winNum, kind: g.kind, via: g.via, startedAt: g.startedAt,
		callerAnswered: true, connected: true, connectedAt: time.Now(),
	})
	a.notifyChanged()
	a.log.Info("fork answered", "winner_leg", winner, "extension", winNum, "a_leg", g.aLeg)
}

// onForkDisconnected handles a SIP candidate leg (or the caller) leaving a fork.
// WebRTC-device drops (reject / timeout / browser close) route through
// forkCandidateGone instead, since a live browser leg has no CANCEL.
func (a *app) onForkDisconnected(g *forkGroup, legID, reason string) {
	if legID == g.aLeg {
		a.forkCallerGaveUp(g)
		return
	}
	a.forkCandidateGone(g, legID, reason)
}

// forkCallerGaveUp tears down a fork when the caller hangs up mid-ring: cancel
// every SIP candidate and stop every ringing browser (leaving its leg alive).
func (a *app) forkCallerGaveUp(g *forkGroup) {
	ctx := context.Background()
	g.mu.Lock()
	if g.ringTimer != nil {
		g.ringTimer.Stop()
	}
	cands := make(map[string]forkCand, len(g.candidates))
	for leg, c := range g.candidates {
		cands[leg] = c
	}
	ringbackPB := g.ringbackPB
	g.mu.Unlock()
	a.forks.remove(g)
	// Stop the ringback tone if the caller heard one (softphone outbound call).
	if ringbackPB != "" {
		_, _ = a.vsi().LegPlayStop(ctx, voiceblender.PlaybackTargetPayload{ID: g.aLeg, PlaybackID: ringbackPB})
	}
	for leg, c := range cands {
		if c.webrtc() {
			a.softphoneStopRing(c.sess, "cancelled")
		} else {
			a.hangup(leg, "")
		}
	}
	_, _ = a.vsi().DeleteRoom(ctx, voiceblender.IDPayload{ID: g.roomID})
	a.notifyChanged()
	a.log.Info("fork caller gave up", "a_leg", g.aLeg)
}

// forkCandidateGone drops one candidate that stopped ringing (SIP declined /
// timed out, browser rejected / timed out / closed). A WebRTC candidate's ring
// is stopped and its leg left alive; SIP legs are already gone. When the last
// candidate leaves, the caller follows the no-answer path.
func (a *app) forkCandidateGone(g *forkGroup, legID, reason string) {
	ctx := context.Background()
	g.mu.Lock()
	cand, ok := g.candidates[legID]
	if !ok {
		g.mu.Unlock()
		return
	}
	delete(g.candidates, legID)
	remaining := len(g.candidates)
	won := g.won
	ringbackPB := g.ringbackPB
	if remaining == 0 && g.ringTimer != nil {
		g.ringTimer.Stop()
	}
	g.mu.Unlock()

	a.forks.removeLeg(legID)
	if won {
		// A winner was already chosen (this is a ring-timeout racing the answer):
		// leave the winning session's in-call state untouched.
		return
	}
	if cand.webrtc() {
		a.softphoneStopRing(cand.sess, reason)
	}
	if remaining > 0 {
		return // others still ringing
	}
	// Nobody answered.
	if ringbackPB != "" {
		_, _ = a.vsi().LegPlayStop(ctx, voiceblender.PlaybackTargetPayload{ID: g.aLeg, PlaybackID: ringbackPB})
	}
	a.forks.remove(g)
	_, _ = a.vsi().DeleteRoom(ctx, voiceblender.IDPayload{ID: g.roomID})
	a.notifyChanged()
	if g.onNoAnswer != nil {
		a.log.Info("fork no answer → dial plan continues", "a_leg", g.aLeg, "reason", reason)
		g.onNoAnswer()
		return
	}
	a.detachOrHangup(g.roomID, g.aLeg, safeHangupReason(reason))
	a.log.Info("fork unanswered", "a_leg", g.aLeg, "reason", reason)
}
