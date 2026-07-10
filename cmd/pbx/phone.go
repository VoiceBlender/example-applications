package main

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"html/template"
	"net/http"
	"strings"
	"time"

	voiceblender "github.com/VoiceBlender/voiceblender-go"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

//go:embed web/phone_login.html
var phoneLoginHTML string

var phoneLoginTemplate = template.Must(template.New("phone_login").Parse(phoneLoginHTML))

//go:embed web/phone.html
var phoneHTML []byte

// A WebRTC softphone signs in with an extension's WebRTCAccount credentials and
// gets a browser phone that acts as an extra device for that extension. The
// signalling protocol (webrtc.offer/answer/candidate, the 250 ms ICE poll, the
// <audio> sink) mirrors the contact-centre example's agent stream.

// handlePhoneLogin renders the softphone login form (GET) and verifies a WebRTC
// account credential (POST).
func (a *app) handlePhoneLogin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = phoneLoginTemplate.Execute(w, loginPageData{
			Next:   "/phone",
			Failed: r.URL.Query().Get("err") == "1",
		})
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		user := r.Form.Get("username")
		password := r.Form.Get("password")
		ext, acct, ok := a.exts.byWebRTCAccount(user)
		valid := ok && acct.Password != "" && subtle.ConstantTimeCompare([]byte(password), []byte(acct.Password)) == 1
		if !valid {
			time.Sleep(250 * time.Millisecond)
			http.Redirect(w, r, "/phone/login?err=1", http.StatusSeeOther)
			return
		}
		setSessionCookie(w, r, cookiePhone, a.phoneSessions.create(acct.Username, ext.Number, ext.TenantID))
		a.log.Info("softphone login", "account", acct.Username, "extension", ext.Number, "remote", r.RemoteAddr)
		http.Redirect(w, r, "/phone", http.StatusSeeOther)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *app) handlePhoneLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookiePhone); err == nil {
		a.phoneSessions.delete(c.Value)
	}
	clearSessionCookie(w, r, cookiePhone)
	if wantsHTML(r) {
		http.Redirect(w, r, "/phone/login", http.StatusSeeOther)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) handlePhoneIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(phoneHTML)
}

// handlePhoneStream is the softphone's signalling WebSocket: WebRTC offer/answer/
// ICE plus call control (answer / reject / dial / hangup / mute). One live
// browser leg is created per connection and reused for every call; it is deleted
// only when this WS closes.
func (a *app) handlePhoneStream(w http.ResponseWriter, r *http.Request) {
	id, ok := a.phoneSessions.get(a.phoneCookie(r))
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if err != nil {
		return
	}
	defer c.CloseNow()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	sess := &phoneSession{account: id.account, extNumber: id.extNumber, tenantID: id.tenantID, outbox: make(chan any, 32), state: phoneIdle}
	if prev := a.phones.add(sess); prev != nil {
		if leg := prev.leg(); leg != "" {
			a.hangup(leg, "")
		}
	}
	a.log.Info("softphone connected", "account", sess.account, "extension", sess.extNumber)
	defer func() {
		a.phones.remove(sess)
		if leg := sess.leg(); leg != "" {
			a.hangup(leg, "") // deleting the leg tears down any active call peer
		}
		a.log.Info("softphone disconnected", "account", sess.account)
	}()

	// Writer: pump the session outbox to the browser, and keepalive-ping so a
	// dead client is detected and its leg/session reaped instead of lingering.
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

	// Greet with the account/extension so the UI can label itself, then send the
	// recent-calls list and saved phonebook.
	sess.send(map[string]any{"type": "hello", "account": sess.account, "extension": sess.extNumber})
	a.pushHistory(sess)
	a.pushPhonebook(sess)

	type phoneMsg struct {
		Type      string                        `json:"type"`
		SDP       string                        `json:"sdp"`
		Candidate voiceblender.ICECandidateInit `json:"candidate"`
		Number    string                        `json:"number"`
		On        bool                          `json:"on"`
		Contacts  []contact                     `json:"contacts"`
		Index     int                           `json:"index"`
	}
	for {
		var msg phoneMsg
		if err := wsjson.Read(ctx, c, &msg); err != nil {
			return
		}
		switch msg.Type {
		case "ping":
			sess.send(map[string]any{"type": "pong"})
		case "webrtc.offer":
			a.phoneOffer(ctx, sess, msg.SDP)
		case "webrtc.candidate":
			a.phoneRemoteCandidate(ctx, sess, msg.Candidate)
		case "answer":
			a.phoneAnswer(sess)
		case "reject", "hangup":
			a.endSoftphoneCall(sess)
		case "dial":
			a.phoneDial(sess, msg.Number)
		case "mute":
			a.phoneMute(sess, msg.On)
		case "dnd":
			sess.setDnd(msg.On)
			a.notifyChanged() // refresh the console presence badge
		case "hold":
			a.phoneHold(sess, msg.On)
		case "transfer":
			a.blindTransfer(sess, msg.Number)
		case "transfer-desk":
			a.blindTransferToDesk(sess)
		case "contacts.save":
			a.savePhonebook(sess, msg.Contacts)
		case "history.remove":
			if err := a.store.RemoveCallHistoryAt(context.Background(), sess.account, msg.Index); err != nil {
				a.log.Warn("remove call history", "account", sess.account, "error", err)
			}
			a.pushHistory(sess)
		}
	}
}

// pushPhonebook sends the account's saved contacts to its browser.
func (a *app) pushPhonebook(sess *phoneSession) {
	items, err := a.store.LoadPhonebook(context.Background(), sess.account)
	if err != nil {
		a.log.Warn("load phonebook", "account", sess.account, "error", err)
		return
	}
	sess.send(map[string]any{"type": "phonebook", "items": items})
}

// savePhonebook persists the account's contacts (trimmed; entries without a
// number dropped) and echoes the cleaned list back to the browser.
func (a *app) savePhonebook(sess *phoneSession, items []contact) {
	clean := make([]contact, 0, len(items))
	for _, c := range items {
		c.Name = strings.TrimSpace(c.Name)
		c.Number = normNumber(c.Number)
		if c.Number == "" {
			continue
		}
		if c.Name == "" {
			c.Name = c.Number
		}
		clean = append(clean, c)
	}
	if err := a.store.SavePhonebook(context.Background(), sess.account, clean); err != nil {
		a.log.Warn("save phonebook", "account", sess.account, "error", err)
	}
	a.pushPhonebook(sess)
}

// recordCall appends an entry to the account's recent-calls history and pushes
// the refreshed list to the browser (for the re-dial list).
func (a *app) recordCall(sess *phoneSession, number, dir string) {
	if number == "" {
		return
	}
	rec := callRecord{Number: number, Dir: dir, At: time.Now().UTC().Format(time.RFC3339)}
	if err := a.store.AppendCallHistory(context.Background(), sess.account, rec); err != nil {
		a.log.Warn("append call history", "account", sess.account, "error", err)
	}
	a.pushHistory(sess)
}

// pushHistory sends the account's recent-calls list to its browser.
func (a *app) pushHistory(sess *phoneSession) {
	items, err := a.store.LoadCallHistory(context.Background(), sess.account)
	if err != nil {
		a.log.Warn("load call history", "account", sess.account, "error", err)
		return
	}
	sess.send(map[string]any{"type": "history", "items": items})
}

// softphoneStopRing stops a browser's incoming ring (leg stays alive) and, if a
// ring was pending, records it as a missed call.
func (a *app) softphoneStopRing(sess *phoneSession, reason string) {
	sess.send(map[string]any{"type": "stop-ring", "reason": reason})
	if from := sess.takeRing(); from != "" {
		a.recordCall(sess, from, "missed")
	}
	sess.setIdle()
}

// normNumber trims a dialed number to its significant characters.
func normNumber(s string) string {
	return strings.TrimSpace(s)
}

// phoneCookie returns the softphone session token from the request, if present.
func (a *app) phoneCookie(r *http.Request) string {
	if c, err := r.Cookie(cookiePhone); err == nil {
		return c.Value
	}
	return ""
}

// phoneOffer establishes the browser's WebRTC leg and starts pushing ICE.
func (a *app) phoneOffer(ctx context.Context, sess *phoneSession, sdp string) {
	if sdp == "" {
		sess.send(map[string]any{"type": "webrtc.error", "message": "sdp required"})
		return
	}
	resp, err := a.vsi().WebRTCOffer(ctx, voiceblender.WebRTCOfferRequest{SDP: sdp})
	if err != nil {
		a.log.Error("softphone webrtc offer", "account", sess.account, "error", err)
		sess.send(map[string]any{"type": "webrtc.error", "message": "offer failed"})
		return
	}
	// A re-offer (page reload) replaces the old leg.
	if prev := sess.leg(); prev != "" && prev != resp.LegID {
		a.hangup(prev, "")
	}
	a.phones.bindLeg(sess, resp.LegID)
	a.log.Info("softphone leg created", "account", sess.account, "leg_id", resp.LegID)
	sess.send(map[string]any{"type": "webrtc.answer", "leg_id": resp.LegID, "sdp": resp.SDP})
	go a.pushPhoneCandidates(ctx, sess.leg(), sess)
}

func (a *app) phoneRemoteCandidate(ctx context.Context, sess *phoneSession, cand voiceblender.ICECandidateInit) {
	leg := sess.leg()
	if leg == "" {
		return // browser raced its first ICE candidate ahead of the answer
	}
	if _, err := a.vsi().WebRTCAddCandidate(ctx, voiceblender.VSIWebRTCAddCandidatePayload{ID: leg, Candidate: cand}); err != nil && !isVSINotFound(err) {
		a.log.Warn("softphone add ice candidate", "leg_id", leg, "error", err)
	}
}

// pushPhoneCandidates polls VoiceBlender for the leg's server-gathered ICE
// candidates and forwards each to the browser. Exits when gathering completes or
// the WS ends.
func (a *app) pushPhoneCandidates(ctx context.Context, legID string, sess *phoneSession) {
	if legID == "" {
		return
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		resp, err := a.vsi().WebRTCGetCandidates(ctx, voiceblender.IDPayload{ID: legID})
		if err != nil {
			if ctx.Err() != nil || isVSINotFound(err) {
				return
			}
			a.log.Warn("softphone poll ice", "leg_id", legID, "error", err)
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

// phoneAnswer accepts an incoming ring: the browser's leg wins the fork it's a
// candidate in and is bridged to the caller.
func (a *app) phoneAnswer(sess *phoneSession) {
	leg := sess.leg()
	if leg == "" {
		return
	}
	if g, ok := a.forks.get(leg); ok && leg != g.aLeg {
		a.onForkConnected(g, leg)
	}
}

// phoneMute mutes/unmutes the softphone's leg.
func (a *app) phoneMute(sess *phoneSession, on bool) {
	leg := sess.leg()
	if leg == "" {
		return
	}
	ctx := context.Background()
	var err error
	if on {
		_, err = a.vsi().MuteLeg(ctx, voiceblender.IDPayload{ID: leg})
	} else {
		_, err = a.vsi().UnmuteLeg(ctx, voiceblender.IDPayload{ID: leg})
	}
	if err != nil && !isVSINotFound(err) {
		a.log.Warn("softphone mute", "leg_id", leg, "on", on, "error", err)
	}
}

// phoneHold puts the softphone's active call on/off hold. A browser leg can't
// signal SIP hold, so it's done at the app level: detach the browser leg from
// the call's room (so it neither hears nor feeds the far party) and play the
// configured hold music to the peer; resuming stops the music and re-joins.
func (a *app) phoneHold(sess *phoneSession, on bool) {
	leg := sess.leg()
	if leg == "" {
		return
	}
	b, ok := a.bridges.get(leg)
	if !ok {
		return // not in an active call
	}
	ctx := context.Background()
	peer := b.peerOf(leg)
	if on {
		if _, err := a.vsi().RemoveLegFromRoom(ctx, voiceblender.RoomLegPayload{RoomID: b.roomID, LegID: leg}); err != nil && !isVSINotFound(err) {
			a.log.Warn("hold: detach softphone leg", "leg_id", leg, "error", err)
		}
		if url := a.config.holdMusicURL(sess.tenantID); url != "" {
			if pb, err := a.vsi().LegPlayStart(ctx, voiceblender.PlaybackStartPayload{ID: peer, URL: url, MimeType: "audio/mpeg", Repeat: -1}); err != nil {
				a.log.Warn("hold: start music", "leg_id", peer, "error", err)
			} else {
				b.setMoH(leg, pb.PlaybackID)
			}
		}
		sess.send(map[string]any{"type": "hold", "on": true})
		a.log.Info("softphone hold", "account", sess.account, "leg_id", leg)
		return
	}
	if pb := b.takeMoH(leg); pb != "" {
		if _, err := a.vsi().LegPlayStop(ctx, voiceblender.PlaybackTargetPayload{ID: peer, PlaybackID: pb}); err != nil && !isVSINotFound(err) {
			a.log.Warn("hold: stop music", "leg_id", peer, "error", err)
		}
	}
	if _, err := a.vsi().AddLegToRoom(ctx, voiceblender.AddLegPayload{RoomID: b.roomID, LegID: leg}); err != nil {
		a.log.Warn("hold: rejoin softphone leg", "leg_id", leg, "error", err)
	}
	sess.send(map[string]any{"type": "hold", "on": false})
	a.log.Info("softphone resume", "account", sess.account, "leg_id", leg)
}

// phoneDial places an outbound call as the extension. The softphone's live leg
// is the (already-answered) caller; the dialed number is routed exactly like an
// extension call — ringing another extension's endpoints, or out via a trunk.
func (a *app) phoneDial(sess *phoneSession, number string) {
	number = normNumber(number)
	if number == "" {
		sess.send(map[string]any{"type": "call.error", "message": "no number"})
		return
	}
	leg := sess.leg()
	if leg == "" {
		sess.send(map[string]any{"type": "webrtc.error", "message": "audio not ready"})
		return
	}
	if st, _ := sess.snapshot(); st != phoneIdle {
		sess.send(map[string]any{"type": "call.error", "message": "already on a call"})
		return
	}
	// Reserve the device for this call (one call per softphone leg — the room id
	// is derived from the leg). detachOrHangup resets it on any failure/teardown.
	sess.setState(phoneInCall, "call-"+leg)
	a.recordCall(sess, number, "out")
	if msg := a.bridgeFrom(leg, sess.tenantID, sess.extNumber, number); msg != "" {
		sess.send(map[string]any{"type": "call.error", "message": msg})
		sess.setIdle()
	}
}

// bridgeFrom routes number as an outbound call from an already-answered caller
// leg (which hears ringback while the far end rings): an extension number rings
// that extension's endpoints, anything else dials out a trunk. Shared by the
// softphone dialer and blind transfer. Returns "" on success or a short error.
func (a *app) bridgeFrom(callerLeg, tenantID, fromID, number string) string {
	if targets := a.extensionTargets(tenantID, number); len(targets) > 0 {
		if len(targets) == 1 && targets[0].sess == nil {
			a.startBridge(callerLeg, targets[0].aor, fromID, nil, true, callMeta{tenantID: tenantID, from: fromID, to: number, kind: "internal"})
		} else {
			a.startFork(callerLeg, targets, fromID, true, callMeta{tenantID: tenantID, from: fromID, to: number, kind: "internal"})
		}
		return ""
	}
	trunk, ok := a.trunks.outboundTrunk(tenantID)
	if !ok {
		return "no outbound trunk"
	}
	host := trunk.dialHost()
	if host == "" {
		return "trunk has no host"
	}
	toURI := "sip:" + number + "@" + host
	callFrom := fromID
	var auth *voiceblender.SIPAuth
	if trunk.Type == trunkRegister && trunk.Username != "" {
		auth = &voiceblender.SIPAuth{Username: trunk.Username, Password: trunk.Password}
		callFrom = trunk.Username
	}
	a.startBridge(callerLeg, toURI, callFrom, auth, true, callMeta{tenantID: tenantID, from: fromID, to: number, kind: "external", via: trunk.Name})
	return ""
}

// blindTransfer hands the connected party off to another extension/number and
// drops the softphone out of the call (its leg stays alive). The surviving peer
// becomes the caller of a fresh call to the target, hearing ringback until it
// answers — so the transfer needs no SIP REFER.
func (a *app) blindTransfer(sess *phoneSession, target string) {
	target = normNumber(target)
	if target == "" {
		sess.send(map[string]any{"type": "call.error", "message": "no transfer target"})
		return
	}
	leg := sess.leg()
	if leg == "" {
		return
	}
	b, ok := a.bridges.get(leg)
	if !ok {
		sess.send(map[string]any{"type": "call.error", "message": "not in a call"})
		return
	}
	peer := a.releaseForTransfer(sess, b, leg)
	if msg := a.bridgeFrom(peer, sess.tenantID, sess.extNumber, target); msg != "" {
		a.hangup(peer, "unavailable")
		a.log.Info("blind transfer failed", "account", sess.account, "target", target, "reason", msg)
	} else {
		a.log.Info("blind transfer", "account", sess.account, "target", target, "peer", peer)
	}
	a.notifyChanged()
}

// blindTransferToDesk moves the call to the SIP (desk) phone registered on the
// softphone's own extension — only the SIP registration, not the extension's
// other WebRTC devices. Registration is checked BEFORE dropping the softphone,
// so if the desk phone isn't registered the call simply stays put.
func (a *app) blindTransferToDesk(sess *phoneSession) {
	leg := sess.leg()
	if leg == "" {
		return
	}
	b, ok := a.bridges.get(leg)
	if !ok {
		sess.send(map[string]any{"type": "call.error", "message": "not in a call"})
		return
	}
	ext, ok := a.exts.byNumber(sess.tenantID, sess.extNumber)
	if !ok {
		sess.send(map[string]any{"type": "call.error", "message": "unknown extension"})
		return
	}
	aor, reg := a.exts.registeredAOR(ext.Username)
	if !reg {
		sess.send(map[string]any{"type": "call.error", "message": "desk phone not registered"})
		return
	}
	peer := a.releaseForTransfer(sess, b, leg)
	a.startBridge(peer, aor, sess.extNumber, nil, true,
		callMeta{tenantID: sess.tenantID, from: sess.extNumber, to: ext.Number, kind: "internal", codecs: ext.Codecs})
	a.notifyChanged()
	a.log.Info("transfer to desk phone", "account", sess.account, "extension", ext.Number, "peer", peer)
}

// releaseForTransfer drops the softphone out of its bridge (its leg stays alive)
// and frees the room, returning the peer leg so a fresh call can re-home it. The
// caller must have validated the transfer target first.
func (a *app) releaseForTransfer(sess *phoneSession, b *bridge, leg string) string {
	ctx := context.Background()
	peer := b.peerOf(leg)
	a.bridges.remove(b)
	// If the call was on hold, stop the hold music before re-using the peer.
	if pb := b.takeMoH(leg); pb != "" {
		_, _ = a.vsi().LegPlayStop(ctx, voiceblender.PlaybackTargetPayload{ID: peer, PlaybackID: pb})
	}
	// Detach both legs (softphone kept alive) and free the old room; the peer is
	// re-homed into a new room by the fresh call the caller starts next.
	_, _ = a.vsi().RemoveLegFromRoom(ctx, voiceblender.RoomLegPayload{RoomID: b.roomID, LegID: leg})
	_, _ = a.vsi().RemoveLegFromRoom(ctx, voiceblender.RoomLegPayload{RoomID: b.roomID, LegID: peer})
	_, _ = a.vsi().DeleteRoom(ctx, voiceblender.IDPayload{ID: b.roomID})
	sess.setIdle()
	sess.send(map[string]any{"type": "call.ended", "reason": "transferred"})
	return peer
}

// endSoftphoneCall ends whatever the softphone leg is currently doing (ringing
// as a candidate, ringing a callee, or in a bridge) WITHOUT deleting the browser
// leg, so the device stays ready for the next call.
func (a *app) endSoftphoneCall(sess *phoneSession) {
	leg := sess.leg()
	if leg == "" {
		sess.setIdle()
		return
	}
	if g, ok := a.forks.get(leg); ok {
		if leg == g.aLeg {
			a.forkCallerGaveUp(g) // outbound call the softphone placed
		} else {
			a.forkCandidateGone(g, leg, "declined") // incoming ring rejected
		}
		return
	}
	if b, ok := a.bridges.get(leg); ok {
		ctx := context.Background()
		a.bridges.remove(b)
		peer := b.peerOf(leg)
		// Stop the ringback tone playing into the softphone (outbound call the
		// caller is cancelling before the callee answered).
		if pb := b.takeRingback(); pb != "" {
			_, _ = a.vsi().LegPlayStop(ctx, voiceblender.PlaybackTargetPayload{ID: leg, PlaybackID: pb})
		}
		a.hangup(peer, "")
		if _, err := a.vsi().RemoveLegFromRoom(ctx, voiceblender.RoomLegPayload{RoomID: b.roomID, LegID: leg}); err != nil && !isVSINotFound(err) {
			a.log.Warn("detach softphone leg", "leg_id", leg, "error", err)
		}
		_, _ = a.vsi().DeleteRoom(ctx, voiceblender.IDPayload{ID: b.roomID})
		sess.setIdle()
		sess.send(map[string]any{"type": "call.ended", "reason": ""})
		a.notifyChanged()
		return
	}
	sess.setIdle()
}
