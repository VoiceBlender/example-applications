package main

import (
	"context"
	"net/http"
	"time"

	voiceblender "github.com/VoiceBlender/voiceblender-go"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// handleStream is a session's signalling WebSocket. The browser connects with
// ?session=<id>&name=<display name>; the socket then carries WebRTC negotiation
// (webrtc.offer / webrtc.answer / webrtc.candidate), the language selector
// (lang.set), and everything the page renders — presence, speaking indicators
// and the live caption track.
//
// Media comes up on connect and stays up: an interpreter has to be listening the
// whole time, so unlike the push-to-talk example there is no per-utterance
// negotiation.
func (a *app) handleStream(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sessionID := q.Get("session")
	name := cleanName(q.Get("name"))

	s, p, err := a.sessions.join(sessionID, name, q.Get("lang"), q.Get("gender"), a.cfg)
	if err != nil {
		switch err {
		case errSessionFull:
			http.Error(w, "session is full", http.StatusConflict)
		default:
			http.Error(w, "session not found", http.StatusNotFound)
		}
		return
	}

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if err != nil {
		a.sessions.leave(s, p)
		return
	}
	defer c.CloseNow()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	a.log.Info("participant joined", "session", s.id, "participant", name, "slot", p.slot)
	defer func() {
		a.media.leave(context.Background(), s, p)
		a.log.Info("participant left", "session", s.id, "participant", name)
	}()

	// Writer: pump this participant's outbox to the browser, keepalive-ping so a
	// dead client is detected and its media reaped instead of lingering.
	go func() {
		ping := time.NewTicker(20 * time.Second)
		defer ping.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-p.c.quit:
				// The session was ended out from under us (a timeout, usually).
				// Drain anything already queued — the "ended" message is in
				// there — then close.
				for {
					select {
					case msg := <-p.c.outbox:
						_ = wsjson.Write(ctx, c, msg)
						continue
					default:
					}
					break
				}
				cancel()
				return
			case msg := <-p.c.outbox:
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

	// Greet, then tell both sides who is present. The browser starts its WebRTC
	// offer as soon as it has the hello.
	p.send(map[string]any{
		"type": "hello", "session": s.id, "you": p.id, "name": p.name,
		"lang": p.getLang(), "gender": p.getGender(), "seat": p.slot,
		// So the page can say up front that it will end itself, rather than
		// appearing to break when it does.
		"idle_timeout_s": int(a.cfg.idleTimeout.Seconds()),
		"max_duration_s": int(a.cfg.maxDuration.Seconds()),
	})
	a.pushPresence(s)

	type inMsg struct {
		Type      string                        `json:"type"`
		SDP       string                        `json:"sdp"`
		Lang      string                        `json:"lang"`
		Gender    string                        `json:"gender"`
		Candidate voiceblender.ICECandidateInit `json:"candidate"`
	}
	for {
		var msg inMsg
		if err := wsjson.Read(ctx, c, &msg); err != nil {
			return
		}
		switch msg.Type {
		case "ping":
			p.send(map[string]any{"type": "pong"})
		case "webrtc.offer":
			a.media.offer(ctx, s, p, msg.SDP)
		case "webrtc.candidate":
			a.media.remoteCandidate(ctx, p, msg.Candidate)
		case "lang.set":
			a.media.setLanguage(ctx, s, p, msg.Lang)
			a.pushPresence(s)
		case "gender.set":
			a.media.setGender(ctx, s, p, msg.Gender)
			a.pushPresence(s)
		}
	}
}

// pushPresence tells both browsers who is in the session and which language each
// has chosen, so each side can label the caption track and show what the other
// person will hear.
func (a *app) pushPresence(s *session) {
	members := s.members()
	people := make([]map[string]any, 0, len(members))
	for _, m := range members {
		lang, _, live := m.snapshot()
		people = append(people, map[string]any{
			"id": m.id, "name": m.name, "seat": m.slot,
			"lang": lang, "gender": m.getGender(), "live": live,
		})
	}
	s.broadcast(map[string]any{"type": "presence", "people": people})
}
