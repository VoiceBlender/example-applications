package main

import (
	"context"
	"net/http"
	"time"

	voiceblender "github.com/VoiceBlender/voiceblender-go"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// handlePTTStream is a room's signalling WebSocket. The browser connects with
// ?room=<id>; the socket then carries floor control (ptt.press / ptt.release),
// on-demand WebRTC signalling (webrtc.offer / webrtc.candidate), and presence.
//
// The media is fully on-demand: a session has no leg while the room is quiet.
// When the floor is granted, the speaker and every present listener are asked to
// offer a leg; when it's released, all legs and the VoiceBlender room are torn
// down. Mirrors the pbx softphone signalling (offer → WebRTCOffer → answer, the
// 250 ms ICE poll) but with floor control instead of per-call bridging.
func (a *app) handlePTTStream(w http.ResponseWriter, r *http.Request) {
	username := userFromCtx(r)
	roomID := r.URL.Query().Get("room")
	room, ok := a.rooms.get(roomID)
	if !ok || !a.rooms.canAccess(room, username) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if err != nil {
		return
	}
	defer c.CloseNow()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	sess := &pttSession{username: username, roomID: roomID, outbox: make(chan any, 32)}
	// Log a join only on the user's first session in the room, so extra tabs (or
	// watching the same channel twice) don't spam the activity feed.
	firstSession := !a.presence.userPresent(roomID, username)
	a.presence.add(sess)
	a.log.Info("room joined", "user", username, "room", roomID)
	if firstSession {
		a.recordEvent(roomID, username, eventJoin)
	}
	defer func() {
		a.floor.leave(sess)     // end the burst if speaker, else drop a listener leg
		a.presence.remove(sess) // then leave presence
		if !a.presence.userPresent(roomID, username) {
			a.recordEvent(roomID, username, eventLeave) // last session gone → they left
		}
		a.pushPresence(roomID) // tell the room the roster changed
		a.notifyChanged()      // refresh lobby headcounts
		a.log.Info("room left", "user", username, "room", roomID)
	}()

	// Writer: pump the session outbox to the browser, keepalive-ping so a dead
	// client is detected and its media reaped instead of lingering.
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

	// Greet, publish the roster, and if a transmission is already in progress
	// bring this browser in as a listener.
	owned := room.Owner == username
	sess.send(map[string]any{"type": "hello", "user": username, "room": room.ID, "name": room.Name, "owner": owned, "invite": inviteFor(room, username), "roger": room.Roger,
		"radio": room.Radio, "radio_low": room.RadioLow, "radio_high": room.RadioHigh})
	sess.send(map[string]any{"type": "history", "events": a.recentEvents(roomID)})
	a.pushPresence(roomID)
	a.notifyChanged()
	a.floor.onJoin(sess)

	type inMsg struct {
		Type      string                        `json:"type"`
		SDP       string                        `json:"sdp"`
		Candidate voiceblender.ICECandidateInit `json:"candidate"`
	}
	for {
		var msg inMsg
		if err := wsjson.Read(ctx, c, &msg); err != nil {
			return
		}
		switch msg.Type {
		case "ping":
			sess.send(map[string]any{"type": "pong"})
		case "ptt.press":
			a.floor.press(sess)
		case "ptt.release":
			a.floor.release(sess)
		case "ring":
			a.ringChannel(sess)
		case "webrtc.offer":
			a.floor.offer(ctx, sess, msg.SDP)
		case "webrtc.candidate":
			a.floor.remoteCandidate(ctx, sess, msg.Candidate)
		}
	}
}

// pushPresence sends the current room roster and speaker to every session in the
// room, so each browser can render the participant list and talk indicator.
func (a *app) pushPresence(roomID string) {
	users := a.presence.usernamesInRoom(roomID)
	speaker := a.floor.speaker(roomID)
	msg := map[string]any{"type": "presence", "users": users, "speaker": speaker}
	for _, m := range a.presence.membersInRoom(roomID) {
		m.send(msg)
	}
}

// ringChannel alerts everyone in the room — the radio "call alert": it pulls
// people back to the tab when you need them. It is rate-limited per session
// (ringCooldown) because it is loud and interrupts everyone. The ringer is
// included in the fan-out so their own UI can confirm the alert went out.
func (a *app) ringChannel(sess *pttSession) {
	ok, wait := sess.takeRing(time.Now())
	if !ok {
		sess.send(map[string]any{"type": "ring.throttled", "wait": int(wait.Seconds()) + 1})
		return
	}
	msg := map[string]any{"type": "ring", "from": sess.username}
	for _, m := range a.presence.membersInRoom(sess.roomID) {
		m.send(msg)
	}
	a.recordEvent(sess.roomID, sess.username, eventRing)
	a.log.Info("channel ring", "user", sess.username, "room", sess.roomID)
}

// pushRoomConfig broadcasts changed room settings (the roger tone and the
// comms-radio band-limit filter) to every session in the room so each browser
// updates live.
func (a *app) pushRoomConfig(room Room) {
	msg := map[string]any{"type": "config", "roger": room.Roger,
		"radio": room.Radio, "radio_low": room.RadioLow, "radio_high": room.RadioHigh}
	for _, m := range a.presence.membersInRoom(room.ID) {
		m.send(msg)
	}
}

// inviteFor returns the room's invite code, but only to its owner (only the
// owner needs to share it).
func inviteFor(room Room, username string) string {
	if room.Owner == username {
		return room.InviteCode
	}
	return ""
}
