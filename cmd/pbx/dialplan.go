package main

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	voiceblender "github.com/VoiceBlender/voiceblender-go"
)

// callMeta describes a call for the live-calls panel and carries the codec
// preference for the outbound (B) leg.
type callMeta struct {
	tenantID   string   // owning tenant (for console call isolation)
	from       string   // caller label (e.g. "1001")
	to         string   // callee label (e.g. "1002" or a dialed number)
	kind       string   // internal | external | inbound | forward
	via        string   // trunk name, when the call is from/to a trunk
	codecs     []string // codec preference order for the outbound leg (nil = default)
	ringTime   int      // seconds to ring the callee (0 = default 60)
	onNoAnswer func()   // called instead of hanging up the caller when nobody answers (nil = hang up)
}

// bridge is one active two-party call: the caller leg (A) and the outbound
// callee/trunk leg (B). The caller is only answered (or, if already answered,
// joined into the room) once B connects — until then B is ringing and A hears
// ringback. ringbackPB is set only when the caller was already answered (IVR
// path) and we play a ringback tone to them while B rings.
//
// Immutable fields are set once at creation and safe to read unlocked; mutable
// state (guarded by mu) is written as the call progresses and read by the
// snapshot goroutine.
type bridge struct {
	roomID    string
	tenantID  string
	aLeg      string
	bLeg      string
	from      string
	to        string
	kind      string
	via       string
	startedAt time.Time

	onNoAnswer func() // resume the dial plan on the caller if the callee never answers

	mu             sync.Mutex
	callerAnswered bool
	ringbackPB     string
	connected      bool
	connectedAt    time.Time
	moh            map[string]string // heldLegID → hold-music playback id on the peer leg
}

func (b *bridge) isConnected() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.connected
}

// peerOf returns the other leg in the bridge.
func (b *bridge) peerOf(legID string) string {
	if legID == b.aLeg {
		return b.bLeg
	}
	return b.aLeg
}

func (b *bridge) setMoH(heldLeg, pb string) {
	b.mu.Lock()
	if b.moh == nil {
		b.moh = make(map[string]string)
	}
	b.moh[heldLeg] = pb
	b.mu.Unlock()
}

// takeMoH returns and clears the hold-music playback id for a held leg.
func (b *bridge) takeMoH(heldLeg string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	pb := b.moh[heldLeg]
	delete(b.moh, heldLeg)
	return pb
}

// takeRingback returns and clears the ringback playback id.
func (b *bridge) takeRingback() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	pb := b.ringbackPB
	b.ringbackPB = ""
	return pb
}

func (b *bridge) answered() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.callerAnswered
}

func (b *bridge) markAnswered() {
	b.mu.Lock()
	b.callerAnswered = true
	b.mu.Unlock()
}

func (b *bridge) markConnected() {
	b.mu.Lock()
	b.connected = true
	b.connectedAt = time.Now()
	b.mu.Unlock()
}

// view projects the bridge into the live-calls panel shape.
func (b *bridge) view() callView {
	b.mu.Lock()
	defer b.mu.Unlock()
	state, since := "ringing", b.startedAt
	if b.connected {
		state, since = "connected", b.connectedAt
	}
	return callView{
		ID:    b.roomID,
		From:  b.from,
		To:    b.to,
		Kind:  b.kind,
		Via:   b.via,
		State: state,
		Since: since.UTC().Format(time.RFC3339),
	}
}

// bridgeRegistry tracks active bridges, indexed by both leg IDs so either
// side's disconnect can find and tear down the whole call.
type bridgeRegistry struct {
	mu    sync.Mutex
	byLeg map[string]*bridge
}

func newBridgeRegistry() *bridgeRegistry {
	return &bridgeRegistry{byLeg: make(map[string]*bridge)}
}

func (r *bridgeRegistry) add(b *bridge) {
	r.mu.Lock()
	r.byLeg[b.aLeg] = b
	r.byLeg[b.bLeg] = b
	r.mu.Unlock()
}

func (r *bridgeRegistry) get(legID string) (*bridge, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.byLeg[legID]
	return b, ok
}

func (r *bridgeRegistry) remove(b *bridge) {
	r.mu.Lock()
	delete(r.byLeg, b.aLeg)
	delete(r.byLeg, b.bLeg)
	r.mu.Unlock()
}

// count returns the number of active bridged calls (two leg entries each).
func (r *bridgeRegistry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byLeg) / 2
}

// views returns the tenant's live-calls projection, oldest first.
func (r *bridgeRegistry) views(tenantID string) []callView {
	r.mu.Lock()
	seen := make(map[*bridge]struct{})
	bs := make([]*bridge, 0, len(r.byLeg)/2)
	for _, b := range r.byLeg {
		if _, ok := seen[b]; ok || b.tenantID != tenantID {
			continue
		}
		seen[b] = struct{}{}
		bs = append(bs, b)
	}
	r.mu.Unlock()

	out := make([]callView, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.view())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since < out[j].Since })
	return out
}

// onRinging is the dialplan entry point for every inbound leg. Authentication
// is decided here purely from the ringing event:
//
//   - source IP matches a configured trunk → IP-authenticated, trusted → IVR
//   - otherwise it's an extension INVITE → digest-challenge, then route
func (a *app) onRinging(ring *voiceblender.LegRingingEvent) {
	if ring.LegType != string(voiceblender.LegTypeSIPInbound) {
		return // outbound leg progress; the caller already hears ringback
	}

	// Identify the trunk (and hence the tenant whose dial plan handles the call).
	// The server may tag register-trunk inbound calls via TrunkID; otherwise we
	// resolve by source IP or by the dialed number matching a register trunk's
	// DID/username (see trunkForInbound).
	var trunk Trunk
	haveTrunk := false
	if ring.TrunkID != "" {
		trunk, haveTrunk = a.trunks.get(ring.TrunkID)
	}
	if !haveTrunk {
		trunk, haveTrunk = a.trunks.trunkForInbound(hostIP(ring.SourceAddress), sipUser(ring.To))
	}
	if haveTrunk {
		a.log.Info("inbound trunk call → dial plan", "leg_id", ring.LegID, "trunk", trunk.ID, "tenant", trunk.TenantID, "did", sipUser(ring.To), "source", ring.SourceAddress)
		a.runDialplan(ring, trunk.ID, trunk.TenantID)
		return
	}

	// Not on a known trunk. If it's an authenticated extension, route it below;
	// if the From matches a known extension, challenge it; otherwise hand it to
	// the dial plan (its default/no-match action decides — no blind reject).
	if !ring.Authenticated {
		fromUser := sipUser(ring.From)
		ext, ok := a.exts.byUsername(fromUser)
		if !ok {
			// No trunk and no known extension → no tenant to route within.
			a.log.Info("unauthenticated INVITE, no trunk/extension match → reject", "leg_id", ring.LegID, "from", ring.From, "source", ring.SourceAddress)
			a.hangup(ring.LegID, "declined")
			return
		}
		a.log.Info("INVITE challenge", "leg_id", ring.LegID, "username", ext.Username)
		if _, err := a.vsi().ChallengeLeg(context.Background(), voiceblender.ChallengeLegPayload{
			ID:       ring.LegID,
			Realm:    a.realm,
			Username: ext.Username,
			Password: ext.Password,
		}); err != nil {
			a.log.Warn("challenge leg", "leg_id", ring.LegID, "error", err)
			a.hangup(ring.LegID, "declined")
		}
		return
	}

	// Authenticated extension call. Trust AuthUsername as the caller identity.
	caller, ok := a.exts.byUsername(ring.AuthUsername)
	if !ok {
		a.log.Warn("authenticated INVITE from unknown extension", "leg_id", ring.LegID, "auth", ring.AuthUsername)
		a.hangup(ring.LegID, "declined")
		return
	}
	dialed := sipUser(ring.To)
	a.log.Info("extension call", "leg_id", ring.LegID, "tenant", caller.TenantID, "from", caller.Number, "dialed", dialed)

	// Resolve the dialed number within the caller's tenant (numbers repeat across
	// tenants); anything unmatched goes out the caller's trunk.
	if callee, ok := a.exts.byNumber(caller.TenantID, dialed); ok {
		a.callInternal(ring.LegID, caller, callee)
		return
	}
	a.callExternal(ring.LegID, caller, dialed)
}

// callInternal rings another extension's endpoints — its registered SIP phone
// and any live WebRTC softphone devices — and bridges the caller to the first to
// answer. We dial a SIP callee's registered AOR so the server resolves it to the
// bound contact.
func (a *app) callInternal(aLeg string, caller, callee Extension) {
	targets := a.extensionTargets(callee.TenantID, callee.Number)
	if len(targets) == 0 {
		a.log.Info("callee offline", "callee", callee.Number)
		a.hangup(aLeg, "unavailable")
		return
	}
	meta := callMeta{tenantID: callee.TenantID, from: caller.Number, to: callee.Number, kind: "internal"}
	if len(targets) == 1 && targets[0].sess == nil {
		meta.codecs = callee.Codecs
		a.startBridge(aLeg, targets[0].aor, caller.Number, nil, false, meta)
		return
	}
	a.startFork(aLeg, targets, caller.Number, false, meta)
}

// callExternal routes the caller out via the caller's tenant's trunk.
func (a *app) callExternal(aLeg string, caller Extension, dialed string) {
	trunk, ok := a.trunks.outboundTrunk(caller.TenantID)
	if !ok {
		a.log.Info("no outbound trunk configured", "dialed", dialed)
		a.hangup(aLeg, "unavailable")
		return
	}
	host := trunk.dialHost()
	if host == "" {
		a.log.Warn("outbound trunk has no dial host", "trunk", trunk.Name)
		a.hangup(aLeg, "unavailable")
		return
	}
	toURI := "sip:" + dialed + "@" + host
	// For a register trunk the provider may digest-challenge the INVITE; pass
	// the trunk credentials so sipgo can answer a 401/407. IP trunks need none.
	// The From is set to the trunk's AOR user so the server's implicit trunk
	// match routes the INVITE through the trunk's upstream and presents the
	// trunk's number as caller ID.
	from := caller.Number
	var auth *voiceblender.SIPAuth
	if trunk.Type == trunkRegister && trunk.Username != "" {
		auth = &voiceblender.SIPAuth{Username: trunk.Username, Password: trunk.Password}
		from = trunk.Username
	}
	a.log.Info("external call", "dialed", dialed, "trunk", trunk.Name, "to", toURI)
	a.startBridge(aLeg, toURI, from, auth, false, callMeta{tenantID: caller.TenantID, from: caller.Number, to: dialed, kind: "external", via: trunk.Name})
}

// startBridge rings the callee and only connects the caller through once the
// callee answers, so the caller's call is NOT answered prematurely:
//
//   - The caller hears ringing while B rings: an unanswered caller gets 180
//     Ringing (their phone plays local ringback); an already-answered caller
//     (IVR path) gets a ringback tone.
//   - B is originated into the room; it auto-joins once it has media.
//   - When B connects (onLegConnected → connectCaller) we answer the caller (if
//     needed) and join it into the room, mixing the two legs.
func (a *app) startBridge(aLeg, toURI, fromCLI string, auth *voiceblender.SIPAuth, callerAnswered bool, meta callMeta) {
	ctx := context.Background()
	roomID := "call-" + aLeg

	if _, err := a.vsi().CreateRoom(ctx, voiceblender.CreateRoomRequest{ID: roomID}); err != nil && !isVSIConflict(err) {
		a.log.Error("create room", "room", roomID, "error", err)
		a.hangup(aLeg, "unavailable")
		return
	}

	// Tell the caller we're ringing.
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
	raw, err := a.vsi().CreateLeg(ctx, voiceblender.CreateLegRequest{
		Type:        "sip",
		To:          toURI,
		From:        fromCLI,
		RoomID:      roomID,
		Auth:        auth,
		Codecs:      meta.codecs,
		RingTimeout: ringTime,
	})
	if err != nil {
		a.log.Error("originate outbound leg", "to", toURI, "error", err)
		if ringbackPB != "" {
			_, _ = a.vsi().LegPlayStop(ctx, voiceblender.PlaybackTargetPayload{ID: aLeg, PlaybackID: ringbackPB})
		}
		_, _ = a.vsi().DeleteRoom(ctx, voiceblender.IDPayload{ID: roomID})
		a.hangup(aLeg, "unavailable")
		return
	}
	bLeg := parseLegID(raw)
	if bLeg == "" {
		a.log.Error("outbound leg has no id", "to", toURI)
		a.hangup(aLeg, "unavailable")
		return
	}

	a.bridges.add(&bridge{
		roomID: roomID, tenantID: meta.tenantID, aLeg: aLeg, bLeg: bLeg,
		from: meta.from, to: meta.to, kind: meta.kind, via: meta.via, startedAt: time.Now(),
		onNoAnswer:     meta.onNoAnswer,
		callerAnswered: callerAnswered, ringbackPB: ringbackPB,
	})
	a.calls.Delete(aLeg) // if the caller was in the IVR, it's a live call now
	a.notifyChanged()
	a.log.Info("ringing callee", "a_leg", aLeg, "b_leg", bLeg, "room", roomID, "to", toURI)
}

// onLegConnected fires when any leg answers/connects.
func (a *app) onLegConnected(legID string) {
	// A forked candidate answered → it wins; bridge it, cancel the rest.
	if g, ok := a.forks.get(legID); ok && legID != g.aLeg {
		a.onForkConnected(g, legID)
		return
	}
	// Dial-plan caller connected after we answered it for a play/tts node.
	if v, ok := a.dpExecs.Load(legID); ok {
		a.dpOnConnected(v.(*dpExec))
		return
	}
	// IVR caller answered → start the greeting/menu.
	if c := a.getCall(legID); c != nil {
		a.ivrOnConnected(legID)
		return
	}
	// Callee (B) answered → connect the caller through.
	if b, ok := a.bridges.get(legID); ok && legID == b.bLeg {
		a.connectCaller(b)
	}
}

// connectCaller runs when the callee answers: stop ringback, answer the caller
// (if it wasn't already), and join the caller into the room to mix the audio.
func (a *app) connectCaller(b *bridge) {
	ctx := context.Background()

	if pb := b.takeRingback(); pb != "" {
		if _, err := a.vsi().LegPlayStop(ctx, voiceblender.PlaybackTargetPayload{ID: b.aLeg, PlaybackID: pb}); err != nil && !isVSINotFound(err) {
			a.log.Warn("stop ringback", "leg_id", b.aLeg, "error", err)
		}
	}

	if !b.answered() {
		if _, err := a.vsi().AnswerLeg(ctx, voiceblender.AnswerLegPayload{ID: b.aLeg}); err != nil {
			a.log.Error("answer caller on connect", "leg_id", b.aLeg, "error", err)
			a.hangup(b.bLeg, "")
			return
		}
		b.markAnswered()
	}

	if _, err := a.vsi().AddLegToRoom(ctx, voiceblender.AddLegPayload{RoomID: b.roomID, LegID: b.aLeg}); err != nil {
		a.log.Error("add caller to room", "leg_id", b.aLeg, "room", b.roomID, "error", err)
		a.detachOrHangup(b.roomID, b.aLeg, "")
		a.hangup(b.bLeg, "")
		return
	}
	b.markConnected()
	a.notifySoftphoneCaller(b.roomID, b.aLeg, b.to)
	a.notifyChanged()
	a.log.Info("call connected", "a_leg", b.aLeg, "b_leg", b.bLeg)
}

// onLegDisconnected tears down a bridged call when either side hangs up (or the
// callee's leg fails), relaying the disconnect reason to the surviving leg, and
// cleans up any IVR state.
func (a *app) onLegDisconnected(legID, reason string) {
	// If a softphone's own media leg dropped (browser closed / crashed), retire
	// it from the phone registry so it's no longer a ring target. Its call
	// teardown (fork/bridge peer) continues below.
	if s, ok := a.phones.sessionForLeg(legID); ok {
		a.phones.bindLeg(s, "")
		s.setIdle()
		a.log.Info("softphone leg dropped", "leg_id", legID, "account", s.account)
	}
	// A leg leaving a still-ringing fork (caller gave up, or a target declined).
	if g, ok := a.forks.get(legID); ok {
		a.onForkDisconnected(g, legID, reason)
		return
	}
	if b, ok := a.bridges.get(legID); ok {
		a.bridges.remove(b)
		ctx := context.Background()
		if pb := b.takeRingback(); pb != "" {
			_, _ = a.vsi().LegPlayStop(ctx, voiceblender.PlaybackTargetPayload{ID: b.aLeg, PlaybackID: pb})
		}
		// No answer: the callee (B) failed before the call connected, and the ext
		// node has a "noanswer" continuation → resume the dial plan on the caller
		// instead of hanging it up.
		if legID == b.bLeg && !b.isConnected() && b.onNoAnswer != nil {
			_, _ = a.vsi().DeleteRoom(ctx, voiceblender.IDPayload{ID: b.roomID})
			a.notifyChanged()
			a.log.Info("callee no answer → dial plan continues", "a_leg", b.aLeg, "reason", reason)
			b.onNoAnswer()
			return
		}
		peer := b.aLeg
		if legID == b.aLeg {
			peer = b.bLeg
		}
		// Relay a recognised rejection reason (busy/declined/…) so the caller
		// gets a proper code; otherwise (e.g. the caller simply cancelled) fall
		// back to a plain hangup, which CANCELs the peer's ringing leg. If the
		// peer is a softphone, detach it (keep the browser leg alive) instead.
		a.detachOrHangup(b.roomID, peer, safeHangupReason(reason))
		if _, err := a.vsi().DeleteRoom(ctx, voiceblender.IDPayload{ID: b.roomID}); err != nil && !isVSINotFound(err) {
			a.log.Warn("delete room", "room", b.roomID, "error", err)
		}
		a.notifyChanged()
		a.log.Info("call ended", "a_leg", b.aLeg, "b_leg", b.bLeg, "reason", reason)
	}
	a.calls.Delete(legID)
	a.dpExecs.Delete(legID)
}

// onLegHold plays on-hold music to the far party when one side of a bridged
// call puts it on hold. The held leg (e.g. the extension that pressed hold)
// isn't listening, so the music goes to its peer.
func (a *app) onLegHold(legID string) {
	b, ok := a.bridges.get(legID)
	if !ok {
		return
	}
	url := a.config.holdMusicURL(b.tenantID)
	if url == "" {
		a.log.Info("leg on hold but no hold-music URL configured", "leg_id", legID)
		return
	}
	peer := b.peerOf(legID)
	pb, err := a.vsi().LegPlayStart(context.Background(), voiceblender.PlaybackStartPayload{
		ID:       peer,
		URL:      url,
		MimeType: "audio/mpeg",
		Repeat:   -1, // loop until unhold
	})
	if err != nil {
		a.log.Warn("start hold music", "leg_id", peer, "error", err)
		return
	}
	b.setMoH(legID, pb.PlaybackID)
	a.log.Info("hold music started", "held_leg", legID, "peer", peer)
}

// onLegUnhold stops the on-hold music started for a held leg.
func (a *app) onLegUnhold(legID string) {
	b, ok := a.bridges.get(legID)
	if !ok {
		return
	}
	pb := b.takeMoH(legID)
	if pb == "" {
		return
	}
	peer := b.peerOf(legID)
	if _, err := a.vsi().LegPlayStop(context.Background(), voiceblender.PlaybackTargetPayload{ID: peer, PlaybackID: pb}); err != nil && !isVSINotFound(err) {
		a.log.Warn("stop hold music", "leg_id", peer, "error", err)
	}
	a.log.Info("hold music stopped", "held_leg", legID, "peer", peer)
}

// validHangupReasons are the DeleteLeg reasons the server maps to a SIP
// rejection code. Relaying any OTHER reason to a still-ringing leg is rejected
// with 400 (and the leg is left ringing), so we only pass these through and
// fall back to "" — a plain hangup, which CANCELs a ringing leg — otherwise.
var validHangupReasons = map[string]bool{
	"busy": true, "declined": true, "rejected": true, "unavailable": true,
	"not_found": true, "forbidden": true, "server_error": true,
}

func safeHangupReason(reason string) string {
	if validHangupReasons[reason] {
		return reason
	}
	return ""
}

// notifySoftphoneCaller tells a softphone that placed an outbound call that the
// far end has answered, so its UI leaves the "calling" state. No-op when the
// caller leg isn't a softphone (an ordinary SIP caller hears the media directly).
func (a *app) notifySoftphoneCaller(roomID, legID, peer string) {
	if s, ok := a.phones.sessionForLeg(legID); ok {
		s.setState(phoneInCall, roomID)
		s.send(map[string]any{"type": "call.connected", "peer": peer})
	}
}

// detachOrHangup ends a leg's participation in a call. A live WebRTC softphone
// leg is *detached* from the room and kept alive (so the browser phone survives
// for the next call) and told the call ended; any other leg is hung up. This is
// the single choke point every call-teardown site must route through so a
// persistent softphone leg is never accidentally deleted.
func (a *app) detachOrHangup(roomID, legID, reason string) {
	if s, ok := a.phones.sessionForLeg(legID); ok {
		if roomID != "" {
			if _, err := a.vsi().RemoveLegFromRoom(context.Background(), voiceblender.RoomLegPayload{RoomID: roomID, LegID: legID}); err != nil && !isVSINotFound(err) {
				a.log.Warn("detach webrtc leg from room", "leg_id", legID, "room", roomID, "error", err)
			}
		}
		s.setIdle()
		s.send(map[string]any{"type": "call.ended", "reason": reason})
		return
	}
	a.hangup(legID, reason)
}

// hangup deletes a leg, ignoring "already gone" errors.
func (a *app) hangup(legID, reason string) {
	if _, err := a.vsi().DeleteLeg(context.Background(), voiceblender.DeleteLegPayload{ID: legID, Reason: reason}); err != nil && !isVSINotFound(err) {
		a.log.Warn("hangup", "leg_id", legID, "error", err)
	}
}

// parseLegID extracts the "id" field from a create_leg response.
func parseLegID(raw json.RawMessage) string {
	var v struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	return v.ID
}
