package main

import (
	"context"
	"time"

	voiceblender "github.com/VoiceBlender/voiceblender-go"
)

// Session time limits.
//
// This app spends real money per minute of wall-clock time, not per action. Two
// connected WebRTC legs mean two Deepgram streams billed continuously, whether
// or not anyone is talking — so a tab left open over lunch costs the same as a
// conversation. The limits below are the difference between a demo and a bill:
//
//   - idle    nobody has spoken for a while → end it. The one that matters.
//   - max     a hard ceiling on any one conversation, in case someone leaves a
//             radio playing into the microphone and "activity" never stops.
//   - empty   a link nobody opened, or one everyone has left → drop it.
//
// Any limit set to zero is disabled.

// reapInterval is how often sessions are checked. The limits are minutes-scale,
// so this only has to be fine enough that the overshoot is not noticeable.
const reapInterval = 15 * time.Second

// reapSessions ends sessions that have hit a limit. It runs for the life of the
// process.
func (a *app) reapSessions(ctx context.Context) {
	if a.cfg.maxDuration == 0 && a.cfg.idleTimeout == 0 && a.cfg.emptyTimeout == 0 {
		return // every limit disabled
	}
	ticker := time.NewTicker(reapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		now := time.Now()
		for _, s := range a.sessions.all() {
			if reason := s.expiry(now, a.cfg); reason != "" {
				a.endSession(ctx, s, reason)
			}
		}
	}
}

// endSession tears a conversation down and tells both browsers why.
//
// Order matters for the bill: stop the transcribers first, because they are the
// meter that is running, then release everything else. It is safe to call more
// than once — the first caller wins and the rest return immediately — and safe
// to call on a half-built session.
func (a *app) endSession(ctx context.Context, s *session, reason string) {
	if !s.close() {
		return // already ended by another path
	}
	a.log.Info("ending session", "session", s.id, "reason", reason)

	members := s.members()

	// 1. Stop the meters.
	for _, p := range members {
		a.media.stopSTT(ctx, p)
	}
	// 2. Drop anything staged or playing; none of it can be delivered now.
	for _, p := range members {
		a.interp.discardAllStaged(ctx, s, p)
		a.interp.clearInflight(ctx, p)
	}
	// 3. Tell the browsers before the audio disappears, so the page can explain
	//    itself rather than just going silent.
	s.broadcast(map[string]any{"type": "ended", "reason": reason})

	// 4. Release the media.
	for _, p := range members {
		if legID := a.sessions.unbindLeg(p); legID != "" {
			if _, err := a.vsi().DeleteLeg(ctx, voiceblender.DeleteLegPayload{ID: legID}); err != nil && !isVSINotFound(err) {
				a.log.Warn("delete leg", "leg_id", legID, "error", err)
			}
		}
	}
	if _, err := a.vsi().DeleteRoom(ctx, voiceblender.IDPayload{ID: a.vbRoom(s.id)}); err != nil && !isVSINotFound(err) {
		a.log.Warn("delete room", "session", s.id, "error", err)
	}

	// 5. Retire the link, then close the sockets. Closing last gives the writer
	//    goroutine a moment to flush the "ended" message above.
	a.sessions.drop(s.id)
	for _, p := range members {
		p.hangUp()
	}
}
