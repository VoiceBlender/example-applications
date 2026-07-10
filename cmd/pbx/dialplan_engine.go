package main

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"time"

	voiceblender "github.com/VoiceBlender/voiceblender-go"
)

// dpExec is the per-call execution state of a dial-plan walk. A call suspends at
// a play/tts node (waiting for playback.finished / tts.finished) and resumes
// from nextNode; ring/forward/reject/ext hand the leg off and end the walk.
type dpExec struct {
	ring      *voiceblender.LegRingingEvent
	legID     string
	trunkID   string
	tenantID  string
	did       string
	from      string
	codec     string
	startedAt time.Time

	mu         sync.Mutex
	answered   bool
	resumeNode string // node to (re)enter once the leg connects after we answer it
	pendingID  string // playback_id or tts_id we're waiting to finish
	nextNode   string // node to walk to when the pending prompt finishes

	// gather state (active while a gather node is collecting input)
	gathering      bool
	gatherNode     string
	gatherInput    string // "dtmf" | "speech" | "both"
	gatherSTT      bool   // speech recognition started on the leg
	gatherDigits   string
	gatherPromptID string // prompt playing during collection (barge-in stops it)
	gatherNum      int    // auto-submit after this many digits (0 = wait for # / match)
	gatherGen      int    // generation, to invalidate a stale timeout timer
}

const dpMaxSteps = 50 // guard against a cyclic/malformed graph

// runDialplan starts a dial-plan walk for an inbound call. trunkID is the
// matched trunk ("" if none identified).
func (a *app) runDialplan(ring *voiceblender.LegRingingEvent, trunkID, tenantID string) {
	g := a.dialplan.get(tenantID)
	start := g.start()
	if start == nil {
		a.log.Warn("no dial plan start node; rejecting inbound call", "leg_id", ring.LegID)
		a.hangup(ring.LegID, "declined")
		return
	}
	exec := &dpExec{
		ring:      ring,
		legID:     ring.LegID,
		trunkID:   trunkID,
		tenantID:  tenantID,
		did:       sipUser(ring.To),
		from:      sipUser(ring.From),
		codec:     chooseCodec(a.answerCodecs, ring.OfferedCodecs),
		startedAt: time.Now(),
	}
	a.dpExecs.Store(ring.LegID, exec)
	a.log.Info("dial plan start", "leg_id", ring.LegID, "trunk", trunkID, "did", exec.did, "from", exec.from)
	a.dpWalk(exec, g, g.edgeTo(start.ID, dpPortOut), 0)
}

// dpExecViews projects inbound calls currently walking the dial plan (not yet
// bridged / handed to the IVR) into the live-calls panel.
func (a *app) dpExecViews(tenantID string) []callView {
	var out []callView
	a.dpExecs.Range(func(_, v any) bool {
		e := v.(*dpExec)
		if e.tenantID != tenantID {
			return true
		}
		e.mu.Lock()
		state := "dial plan"
		if e.gathering {
			state = "menu"
		}
		e.mu.Unlock()
		to := e.did
		if to == "" {
			to = "dial plan"
		}
		out = append(out, callView{
			ID: e.legID, From: e.from, To: to, Kind: "inbound",
			Via: a.trunkName(e.trunkID), State: state,
			Since: e.startedAt.UTC().Format(time.RFC3339),
		})
		return true
	})
	return out
}

// dpWalk advances the call from nodeID until it hands the leg off (terminal
// action) or suspends on a prompt (play/tts).
func (a *app) dpWalk(exec *dpExec, g DPGraph, nodeID string, step int) {
	if step > dpMaxSteps {
		a.log.Warn("dial plan step limit reached; rejecting", "leg_id", exec.legID)
		a.dpReject(exec, "declined")
		return
	}
	node := g.node(nodeID)
	if node == nil {
		// Dangling / no edge → default: reject.
		a.log.Info("dial plan reached dead end; rejecting", "leg_id", exec.legID)
		a.dpReject(exec, "declined")
		return
	}

	switch node.Type {
	case dpMatch:
		next := dpPortNoMatch
		if a.dpMatches(node, exec) {
			next = dpPortMatch
		}
		a.dpWalk(exec, g, g.edgeTo(node.ID, next), step+1)

	case dpExt:
		a.dpRouteExtension(exec, g, node)

	case dpIVR:
		a.dpExecs.Delete(exec.legID)
		trunk, _ := a.trunks.get(exec.trunkID)
		a.startIVR(exec.ring, trunk, exec.tenantID, exec.isAnswered())

	case dpAnswerNode:
		if !exec.isAnswered() {
			a.dpAnswer(exec, node.ID) // answer, then re-enter this node on connect
			return
		}
		a.dpWalk(exec, g, g.edgeTo(node.ID, dpPortNext), step+1)

	case dpGatherNode:
		a.dpGather(exec, node)

	case dpWaitNode:
		secs := atoiDefault(node.param("seconds", "1"), 1)
		if secs < 0 {
			secs = 0
		}
		if secs > 300 {
			secs = 300 // sanity cap
		}
		nodeID := node.ID
		if secs == 0 {
			a.dpWalk(exec, g, g.edgeTo(nodeID, dpPortNext), step+1)
			return
		}
		a.log.Info("dial plan wait", "leg_id", exec.legID, "seconds", secs)
		time.AfterFunc(time.Duration(secs)*time.Second, func() {
			g2 := a.dialplan.get(exec.tenantID)
			a.dpWalk(exec, g2, g2.edgeTo(nodeID, dpPortNext), 0)
		})

	case dpReject:
		a.dpReject(exec, node.param("reason", "declined"))

	case dpForward:
		a.dpForward(exec, g, node)

	case dpPlay:
		a.dpPlay(exec, g, node)

	case dpTTS:
		a.dpTTS(exec, g, node)

	default:
		a.log.Warn("unknown dial plan node type", "type", node.Type, "leg_id", exec.legID)
		a.dpReject(exec, "declined")
	}
}

// dpMatches evaluates a match node's trunk + DID condition against the call.
func (a *app) dpMatches(node *DPNode, exec *dpExec) bool {
	if t := node.param("trunk", ""); t != "" && t != exec.trunkID {
		return false
	}
	mode := node.param("did_mode", didAny)
	val := node.param("did", "")
	switch mode {
	case didAny, "":
		return true
	case didExact:
		return exec.did == val
	case didPrefix:
		return strings.HasPrefix(exec.did, val)
	case didRegex:
		ok, err := regexp.MatchString(val, exec.did)
		return err == nil && ok
	}
	return false
}

// dpRouteExtension rings one or more configured extensions. The node's number
// param is a comma-separated list; more than one registered target is forked
// (rung simultaneously, first to answer wins). ring_time bounds how long it
// rings; if nobody answers (offline / timeout / declined) it follows the node's
// "noanswer" output, or hangs up the caller when there's no such edge.
func (a *app) dpRouteExtension(exec *dpExec, g DPGraph, node *DPNode) {
	a.dpExecs.Delete(exec.legID)

	// Continuation for the no-answer outcome: re-enter the dial plan on the
	// caller at the node wired to the "noanswer" output (nil if not wired).
	var onNoAnswer func()
	if g.edgeTo(node.ID, dpPortNoAnswer) != "" {
		onNoAnswer = func() {
			g2 := a.dialplan.get(exec.tenantID)
			a.dpExecs.Store(exec.legID, exec)
			a.dpWalk(exec, g2, g2.edgeTo(node.ID, dpPortNoAnswer), 0)
		}
	}

	var targets []forkTarget
	var labels []string
	for _, n := range splitCSV(node.param("number", "")) {
		ts := a.extensionTargets(exec.tenantID, n)
		if len(ts) == 0 {
			continue
		}
		targets = append(targets, ts...)
		labels = append(labels, n)
	}
	if len(targets) == 0 {
		a.log.Info("dial plan: no reachable extension to ring", "leg_id", exec.legID)
		if onNoAnswer != nil {
			onNoAnswer()
			return
		}
		a.hangup(exec.legID, "unavailable")
		return
	}

	ringTime := atoiDefault(node.param("ring_time", "30"), 30)
	via := a.trunkName(exec.trunkID)
	// A lone registered SIP phone can use the simpler bridge path; anything with a
	// WebRTC device (which has no leg.connected to drive a bridge) forks.
	if len(targets) == 1 && targets[0].sess == nil {
		t := targets[0]
		a.startBridge(exec.legID, t.aor, exec.from, nil, exec.isAnswered(),
			callMeta{tenantID: exec.tenantID, from: exec.from, to: t.number, kind: "inbound", via: via, codecs: t.codecs, ringTime: ringTime, onNoAnswer: onNoAnswer})
		return
	}
	a.startFork(exec.legID, targets, exec.from, exec.isAnswered(),
		callMeta{tenantID: exec.tenantID, from: exec.from, to: strings.Join(labels, ","), kind: "inbound", via: via, ringTime: ringTime, onNoAnswer: onNoAnswer})
}

// extensionTargets returns the ring targets for one extension number: its
// registered SIP AOR (when online) plus every live WebRTC softphone device.
// Empty if the number isn't an extension or has no reachable endpoint. This is
// the shared fan-out used by inbound dial-plan `ext` nodes, extension-to-
// extension calls, and softphone-initiated outbound calls.
func (a *app) extensionTargets(tenantID, number string) []forkTarget {
	callee, ok := a.exts.byNumber(tenantID, number)
	if !ok {
		return nil
	}
	var out []forkTarget
	if aor, reg := a.exts.registeredAOR(callee.Username); reg {
		out = append(out, forkTarget{number: callee.Number, aor: aor, codecs: callee.Codecs})
	}
	for _, s := range a.phones.liveForExt(tenantID, callee.Number) {
		out = append(out, forkTarget{number: callee.Number, sess: s})
	}
	return out
}

// dpForward bridges the inbound call back out via a trunk to an external
// number. ring_time bounds how long it rings; if the far end doesn't answer it
// follows the node's "noanswer" output, or hangs up when there's no such edge.
func (a *app) dpForward(exec *dpExec, g DPGraph, node *DPNode) {
	a.dpExecs.Delete(exec.legID)

	// No-answer continuation: re-enter the dial plan on the caller at the node
	// wired to the "noanswer" output (nil if not wired).
	var onNoAnswer func()
	if g.edgeTo(node.ID, dpPortNoAnswer) != "" {
		onNoAnswer = func() {
			g2 := a.dialplan.get(exec.tenantID)
			a.dpExecs.Store(exec.legID, exec)
			a.dpWalk(exec, g2, g2.edgeTo(node.ID, dpPortNoAnswer), 0)
		}
	}
	fail := func() {
		if onNoAnswer != nil {
			onNoAnswer()
			return
		}
		a.hangup(exec.legID, "unavailable")
	}

	number := node.param("number", "")
	trunk, ok := a.trunks.get(node.param("trunk", ""))
	if ok && trunk.TenantID != exec.tenantID {
		ok = false // a forward node must not reference another tenant's trunk
	}
	if !ok {
		if t, has := a.trunks.outboundTrunk(exec.tenantID); has {
			trunk = t
		} else {
			a.log.Info("dial plan forward: no trunk available", "leg_id", exec.legID)
			fail()
			return
		}
	}
	host := trunk.dialHost()
	if number == "" || host == "" {
		a.log.Warn("dial plan forward: missing number or trunk host", "leg_id", exec.legID)
		fail()
		return
	}
	toURI := "sip:" + number + "@" + host
	from := exec.from
	var auth *voiceblender.SIPAuth
	if trunk.Type == trunkRegister && trunk.Username != "" {
		auth = &voiceblender.SIPAuth{Username: trunk.Username, Password: trunk.Password}
		from = trunk.Username
	}
	ringTime := atoiDefault(node.param("ring_time", "30"), 30)
	a.log.Info("dial plan forward", "to", toURI, "trunk", trunk.Name, "leg_id", exec.legID)
	a.startBridge(exec.legID, toURI, from, auth, exec.isAnswered(),
		callMeta{tenantID: exec.tenantID, from: exec.from, to: number, kind: "forward", via: trunk.Name, ringTime: ringTime, onNoAnswer: onNoAnswer})
}

// dpReject declines the call and ends the walk.
func (a *app) dpReject(exec *dpExec, reason string) {
	a.dpExecs.Delete(exec.legID)
	a.hangup(exec.legID, safeHangupReason(reason))
}

// dpPlay plays an audio file to the (answered) leg, then suspends until
// playback.finished advances to the node's `next`. If the leg isn't answered
// yet it is answered first and this node is re-entered once the leg connects
// (playback needs an established media path, or the server returns 409).
func (a *app) dpPlay(exec *dpExec, g DPGraph, node *DPNode) {
	url := node.param("url", "")
	if url == "" {
		a.dpWalk(exec, g, g.edgeTo(node.ID, dpPortNext), 0)
		return
	}
	if !exec.isAnswered() {
		a.dpAnswer(exec, node.ID)
		return
	}
	resp, err := a.vsi().LegPlayStart(context.Background(), voiceblender.PlaybackStartPayload{
		ID: exec.legID, URL: url, MimeType: "audio/mpeg",
	})
	if err != nil {
		a.log.Warn("dial plan play failed", "leg_id", exec.legID, "error", err)
		a.dpReject(exec, "declined")
		return
	}
	exec.suspend(resp.PlaybackID, g.edgeTo(node.ID, dpPortNext))
	a.log.Info("dial plan play", "leg_id", exec.legID, "url", url)
}

// dpTTS speaks text to the (answered) leg, then suspends until tts.finished
// advances to the node's `next`. Answers first + re-enters on connect if needed.
func (a *app) dpTTS(exec *dpExec, g DPGraph, node *DPNode) {
	text := node.param("text", "")
	if text == "" {
		a.dpWalk(exec, g, g.edgeTo(node.ID, dpPortNext), 0)
		return
	}
	if !exec.isAnswered() {
		a.dpAnswer(exec, node.ID)
		return
	}
	resp, err := a.vsi().LegTTS(context.Background(), voiceblender.TTSStartPayload{
		ID:       exec.legID,
		Text:     text,
		Voice:    node.param("voice", a.ttsVoice),
		Provider: a.ttsProvider,
		APIKey:   a.ttsAPIKey,
	})
	if err != nil {
		a.log.Warn("dial plan tts failed", "leg_id", exec.legID, "error", err)
		a.dpReject(exec, "declined")
		return
	}
	exec.suspend(resp.TTSID, g.edgeTo(node.ID, dpPortNext))
	a.log.Info("dial plan tts", "leg_id", exec.legID)
}

// dpAnswer answers the caller and records the node to re-enter once the leg
// connects (media is only ready after the caller's ACK, signalled by
// leg.connected — playing before then fails).
func (a *app) dpAnswer(exec *dpExec, resumeNodeID string) {
	exec.mu.Lock()
	exec.resumeNode = resumeNodeID
	exec.mu.Unlock()
	if _, err := a.vsi().AnswerLeg(context.Background(), voiceblender.AnswerLegPayload{ID: exec.legID, Codec: exec.codec}); err != nil {
		a.log.Error("dial plan answer failed", "leg_id", exec.legID, "error", err)
		a.dpReject(exec, "declined")
	}
}

// dpOnConnected resumes a walk suspended on an answer, once the leg is up.
func (a *app) dpOnConnected(exec *dpExec) {
	exec.mu.Lock()
	exec.answered = true
	node := exec.resumeNode
	exec.resumeNode = ""
	exec.mu.Unlock()
	if node != "" {
		a.dpWalk(exec, a.dialplan.get(exec.tenantID), node, 0)
	}
}

// dpOnPromptFinished resumes a suspended walk when the pending prompt (play or
// tts) for a leg finishes. Returns true if it handled the event.
func (a *app) dpOnPromptFinished(legID, promptID string) bool {
	v, ok := a.dpExecs.Load(legID)
	if !ok {
		return false
	}
	exec := v.(*dpExec)
	exec.mu.Lock()
	if exec.gathering {
		// A gather prompt finished; keep waiting for DTMF / speech / timeout.
		if exec.gatherPromptID == promptID {
			exec.gatherPromptID = ""
		}
		exec.mu.Unlock()
		return true
	}
	// Only advance when a play/tts NODE is actually suspended on this exact
	// prompt. If nothing is pending (pendingID == ""), this is a stray prompt
	// finish — e.g. a gather prompt whose tts.finished arrived just after the
	// gather already submitted (stopping the prompt triggers its finish). Walking
	// on an empty pendingID would follow an empty nextNode into a dead-end reject
	// and hang up the live call.
	if exec.pendingID == "" || exec.pendingID != promptID {
		exec.mu.Unlock()
		return true
	}
	next := exec.nextNode
	exec.pendingID = ""
	exec.nextNode = ""
	exec.mu.Unlock()

	a.dpWalk(exec, a.dialplan.get(exec.tenantID), next, 0)
	return true
}

func (e *dpExec) suspend(pendingID, nextNode string) {
	e.mu.Lock()
	e.pendingID = pendingID
	e.nextNode = nextNode
	e.mu.Unlock()
}

func (e *dpExec) isAnswered() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.answered
}
