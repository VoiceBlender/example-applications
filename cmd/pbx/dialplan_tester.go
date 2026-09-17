package main

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	voiceblender "github.com/VoiceBlender/voiceblender-go"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// Dial-plan tester: the console's Dial plan page can place a *test call* from
// the browser. The browser establishes a WebRTC leg (same signalling as the
// softphone), then asks to start: the app runs the tenant's saved dial plan on
// that leg exactly as if it were an inbound trunk call, with an operator-chosen
// caller, DID and trunk (so match nodes can be exercised). The page's keypad
// sends DTMF over the tester WS, which is fed into the same handlers as a
// dtmf.received event — gather nodes and the IVR can be driven without a phone.
// Each step of the walk is traced back to the page so it can highlight the
// active node.

// dpTestSession is one browser's test call.
type dpTestSession struct {
	tenantID string
	outbox   chan any // → the tester WS

	mu      sync.Mutex
	legID   string
	started bool
}

func (s *dpTestSession) send(msg any) {
	select {
	case s.outbox <- msg:
	default:
	}
}

func (s *dpTestSession) leg() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.legID
}

// dpTest returns the test session driving legID, if any.
func (a *app) dpTest(legID string) (*dpTestSession, bool) {
	v, ok := a.dpTests.Load(legID)
	if !ok {
		return nil, false
	}
	return v.(*dpTestSession), true
}

// dpTrace reports a dial-plan step to the tester page watching legID (no-op for
// real calls). node is the graph node involved ("" if none).
func (a *app) dpTrace(legID, node, text string) {
	if s, ok := a.dpTest(legID); ok {
		s.send(map[string]any{"type": "trace", "node": node, "text": text, "at": time.Now().UTC().Format(time.RFC3339Nano)})
	}
}

// dpTestEnded tells the tester its call leg is gone and forgets the leg.
func (a *app) dpTestEnded(legID, reason string) {
	if s, ok := a.dpTest(legID); ok {
		a.dpTests.Delete(legID)
		s.mu.Lock()
		if s.legID == legID {
			s.legID, s.started = "", false
		}
		s.mu.Unlock()
		s.send(map[string]any{"type": "ended", "reason": reason})
	}
}

// handleDialplanTest is the tester's signalling WebSocket: WebRTC offer/ICE,
// then start / dtmf / hangup. The test leg is deleted when the call ends or the
// WS closes.
func (a *app) handleDialplanTest(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromCtx(r)
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if err != nil {
		return
	}
	defer c.CloseNow()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	sess := &dpTestSession{tenantID: tenantID, outbox: make(chan any, 64)}
	defer func() {
		if leg := sess.leg(); leg != "" {
			a.hangup(leg, "")
			a.dpTests.Delete(leg)
		}
	}()

	go func() {
		ping := time.NewTicker(20 * time.Second)
		defer ping.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-sess.outbox:
				if err := wsjson.Write(ctx, c, msg); err != nil {
					cancel()
					return
				}
			case <-ping.C:
				pctx, pcancel := context.WithTimeout(ctx, 10*time.Second)
				err := c.Ping(pctx)
				pcancel()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()

	type testMsg struct {
		Type      string                        `json:"type"`
		SDP       string                        `json:"sdp"`
		Candidate voiceblender.ICECandidateInit `json:"candidate"`
		From      string                        `json:"from"`
		DID       string                        `json:"did"`
		Trunk     string                        `json:"trunk"`
		Digit     string                        `json:"digit"`
	}
	for {
		var msg testMsg
		if err := wsjson.Read(ctx, c, &msg); err != nil {
			return
		}
		switch msg.Type {
		case "webrtc.offer":
			a.dpTestOffer(ctx, sess, msg.SDP)
		case "webrtc.candidate":
			if leg := sess.leg(); leg != "" {
				if _, err := a.vsi().WebRTCAddCandidate(ctx, voiceblender.VSIWebRTCAddCandidatePayload{ID: leg, Candidate: msg.Candidate}); err != nil && !isVSINotFound(err) {
					a.log.Warn("dial plan test: add ice candidate", "leg_id", leg, "error", err)
				}
			}
		case "start":
			a.dpTestStart(sess, msg.From, msg.DID, msg.Trunk)
		case "dtmf":
			a.dpTestDTMF(sess, msg.Digit)
		case "hangup":
			if leg := sess.leg(); leg != "" {
				a.hangup(leg, "")
			}
		}
	}
}

// dpTestOffer creates the browser's WebRTC test leg.
func (a *app) dpTestOffer(ctx context.Context, sess *dpTestSession, sdp string) {
	if sdp == "" {
		sess.send(map[string]any{"type": "error", "message": "sdp required"})
		return
	}
	if prev := sess.leg(); prev != "" {
		sess.send(map[string]any{"type": "error", "message": "a test call is already active"})
		return
	}
	resp, err := a.vsi().WebRTCOffer(ctx, voiceblender.WebRTCOfferRequest{SDP: sdp, AppID: a.appID})
	if err != nil {
		a.log.Error("dial plan test: webrtc offer", "error", err)
		sess.send(map[string]any{"type": "error", "message": "offer failed"})
		return
	}
	sess.mu.Lock()
	sess.legID = resp.LegID
	sess.mu.Unlock()
	a.dpTests.Store(resp.LegID, sess)
	a.log.Info("dial plan test leg created", "tenant", sess.tenantID, "leg_id", resp.LegID)
	sess.send(map[string]any{"type": "webrtc.answer", "leg_id": resp.LegID, "sdp": resp.SDP})
	go a.pushCandidates(ctx, resp.LegID, sess.send)
}

// dpTestStart runs the saved dial plan on the (media-connected) test leg as a
// simulated inbound call from `from` to `did`, arriving on `trunkID`.
func (a *app) dpTestStart(sess *dpTestSession, from, did, trunkID string) {
	leg := sess.leg()
	if leg == "" {
		sess.send(map[string]any{"type": "error", "message": "audio not ready"})
		return
	}
	sess.mu.Lock()
	already := sess.started
	sess.started = true
	sess.mu.Unlock()
	if already {
		return
	}
	if trunkID != "" {
		if t, ok := a.trunks.get(trunkID); !ok || t.TenantID != sess.tenantID {
			trunkID = "" // never let a tester claim another tenant's trunk
		}
	}
	from = strings.TrimSpace(from)
	if from == "" {
		from = "tester"
	}
	did = strings.TrimSpace(did)
	ring := &voiceblender.LegRingingEvent{
		LegID:   leg,
		AppID:   a.appID,
		LegType: "webrtc",
		From:    "sip:" + from + "@dialplan.test",
		To:      "sip:" + did + "@dialplan.test",
		TrunkID: trunkID,
	}
	a.log.Info("dial plan test start", "tenant", sess.tenantID, "leg_id", leg, "from", from, "did", did, "trunk", trunkID)
	sess.send(map[string]any{"type": "started", "leg_id": leg})
	a.startDialplan(ring, trunkID, sess.tenantID, true)
}

// dpTestDTMF injects a keypad digit into the test call, routed exactly like a
// dtmf.received event (dial-plan gather first, then the IVR).
func (a *app) dpTestDTMF(sess *dpTestSession, digit string) {
	leg := sess.leg()
	if leg == "" || len(digit) != 1 || !strings.Contains("0123456789*#", digit) {
		return
	}
	a.dpTrace(leg, "", "DTMF "+digit)
	if !a.dpOnDTMF(leg, digit) {
		a.ivrOnDTMF(leg, digit)
	}
}
