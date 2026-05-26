package main

import (
	"context"
	"log/slog"
	"net/http"

	voiceblender "github.com/VoiceBlender/voiceblender-go"
)

// Intercom end reasons sent in `intercom.ended`.
const (
	icEndedRejected               = "rejected"
	icEndedAgentHangup            = "agent_hangup"
	icEndedSupervisorHangup       = "supervisor_hangup"
	icEndedCalleeHangup           = "callee_hangup"
	icEndedAgentDisconnected      = "agent_disconnected"
	icEndedCalleeDisconnected     = "callee_disconnected"
	icEndedSupervisorDisconnected = "supervisor_disconnected"
	icEndedCalleeBusy             = "callee_busy"
	icEndedSetupFailed            = "setup_failed"
)

// supervisorUsernameFromRequest returns the auth username for the
// supervisor making this WS request, or "supervisor" when auth is off
// (no cookie / empty username). Mirrors the lookup used by
// handleAuthWhoami so identities line up across the app.
func supervisorUsernameFromRequest(a *app, r *http.Request) string {
	if c, err := r.Cookie(cookieSupervisor); err == nil {
		if sess, ok := a.sessions.Get(c.Value); ok && sess.Role == roleSupervisor && sess.Username != "" {
			return sess.Username
		}
	}
	return "supervisor"
}

// fanOutToSupervisors pushes msg to every session matching username
// (one logical supervisor often has multiple browser tabs open).
func (a *app) fanOutToSupervisors(username string, msg any) {
	for _, p := range a.supervisors.Sessions(username) {
		sendOrDrop(p.Outbox, msg)
	}
}

// fanOutToAgentByID pushes msg to a specific agent's WS outbox.
func (a *app) fanOutToAgentByID(agentID string, msg any) {
	if v, ok := a.agentOutboxes.Load(agentID); ok {
		sendOrDrop(v.(chan<- any), msg)
	}
}

// fanOutToTarget routes msg to whoever the intercom is ringing — a
// supervisor username (potentially many tabs) or a single agent.
func (a *app) fanOutToTarget(ic *intercom, msg any) {
	if ic == nil {
		return
	}
	switch ic.TargetKind {
	case intercomTargetSupervisor:
		a.fanOutToSupervisors(ic.Target, msg)
	case intercomTargetAgent:
		a.fanOutToAgentByID(ic.Target, msg)
	}
}

// ----------- agent-side intercom handlers -------------------------------

// handleAgentIntercomCall is the agent's "call supervisor / agent"
// action. We require the agent to be idle (no customer call) and to
// have an active WebRTC leg (the client preps it via ensureAudioReady
// before sending). The room is created here, the agent leg is added
// as the "agent" role; the called side joins as "supervisor" on
// answer (we keep the role name for matrix simplicity — it just means
// "the other party" inside the intercom room).
func (a *app) handleAgentIntercomCall(ctx context.Context, ag *agent, kind, target string, outbox chan<- any, log *slog.Logger) {
	if target == "" {
		sendOrDrop(outbox, errorMsg("intercom.error", "target required"))
		return
	}
	if kind == "" {
		kind = intercomTargetSupervisor // back-compat with old clients
	}
	if a.reg.callAnsweredBy(ag.ID) != nil {
		sendOrDrop(outbox, errorMsg("intercom.error", "end your customer call first"))
		return
	}
	agentLeg := a.agents.webRTCLeg(ag.ID)
	if agentLeg == "" {
		sendOrDrop(outbox, errorMsg("intercom.error", "audio not ready"))
		return
	}

	// Validate target presence + derive a friendly display name.
	var targetName string
	switch kind {
	case intercomTargetSupervisor:
		if len(a.supervisors.Sessions(target)) == 0 {
			sendOrDrop(outbox, errorMsg("intercom.error", "supervisor not available"))
			return
		}
		targetName = target // username doubles as display label
	case intercomTargetAgent:
		if target == ag.ID {
			sendOrDrop(outbox, errorMsg("intercom.error", "cannot call yourself"))
			return
		}
		callee, ok := a.agents.get(target)
		if !ok {
			sendOrDrop(outbox, errorMsg("intercom.error", "agent not available"))
			return
		}
		if _, online := a.agentOutboxes.Load(target); !online {
			sendOrDrop(outbox, errorMsg("intercom.error", "agent offline"))
			return
		}
		if a.reg.callAnsweredBy(target) != nil {
			sendOrDrop(outbox, errorMsg("intercom.error", "agent is on a call"))
			return
		}
		if existing := a.intercoms.ByAgent(target); existing != nil {
			sendOrDrop(outbox, errorMsg("intercom.error", "agent is busy"))
			return
		}
		targetName = callee.Name
	default:
		sendOrDrop(outbox, errorMsg("intercom.error", "unknown target kind"))
		return
	}

	ic, err := a.intercoms.Create(ag, kind, target, targetName, agentLeg)
	if err != nil {
		sendOrDrop(outbox, errorMsg("intercom.error", err.Error()))
		return
	}

	// Create the room and add the calling agent's leg. Best-effort
	// cleanup on failure leaves no dangling registry entry.
	if _, err := a.client.CreateRoom(ctx, voiceblender.CreateRoomRequest{ID: ic.RoomID}); err != nil && !voiceblender.IsConflict(err) {
		log.Error("intercom: create room", "room", ic.RoomID, "error", err)
		a.intercoms.End(ic.ID)
		sendOrDrop(outbox, errorMsg("intercom.error", "room setup failed"))
		return
	}
	room := a.client.Room(ic.RoomID)
	if _, err := room.SetRouting(ctx, voiceblender.RoomRoutingRequest{Matrix: defaultCallRoomMatrix()}); err != nil {
		log.Warn("intercom: set routing", "room", ic.RoomID, "error", err)
	}
	if _, err := room.AddLeg(ctx, voiceblender.AddLegRequest{LegID: agentLeg, Role: "agent"}); err != nil {
		log.Error("intercom: add agent leg", "room", ic.RoomID, "error", err)
		_, _ = room.Delete(context.Background())
		a.intercoms.End(ic.ID)
		sendOrDrop(outbox, errorMsg("intercom.error", "could not bridge agent audio"))
		return
	}

	log.Info("intercom: ringing", "intercom_id", ic.ID, "kind", kind, "target", target)

	// Push the incoming-call notification to whichever party is being rung.
	a.fanOutToTarget(ic, map[string]any{
		"type":        "intercom.incoming",
		"intercom_id": ic.ID,
		"agent_id":    ic.AgentID,
		"agent_name":  ic.AgentName,
		"started_at":  ic.StartedAt,
	})
	// Ack the caller with the ringing state (includes target metadata).
	sendOrDrop(outbox, map[string]any{
		"type":        "intercom.state",
		"intercom_id": ic.ID,
		"state":       ic.State,
		"target_kind": ic.TargetKind,
		"target":      ic.Target,
		"target_name": ic.TargetName,
	})
}

// handleAgentIntercomHangup is the agent (either side) cancelling
// (ringing) or hanging up (active).
func (a *app) handleAgentIntercomHangup(_ context.Context, ag *agent, intercomID string, log *slog.Logger) {
	id := intercomID
	if id == "" {
		// Convenience: when omitted, use whatever intercom this agent has.
		if ic := a.intercoms.ByAgent(ag.ID); ic != nil {
			id = ic.ID
		}
	}
	if id == "" {
		return
	}
	// Determine whether this agent is the caller or the callee, so we
	// can choose a sensible end-reason for the other side's notification.
	current := a.intercoms.Get(id)
	ic, ok := a.intercoms.End(id)
	if !ok {
		return
	}
	a.teardownIntercomRoom(ic, log)
	reason := icEndedAgentHangup
	if current != nil && current.TargetKind == intercomTargetAgent && current.Target == ag.ID {
		reason = icEndedCalleeHangup
	}
	a.notifyIntercomEnded(ic, reason)
}

// handleAgentIntercomAnswer is the called agent's "Answer" action.
// Same dance as the supervisor answer: claim atomically, bridge the
// callee's WebRTC leg into the intercom room as "supervisor" (so the
// routing matrix wires the two sides together symmetrically), notify
// both parties.
func (a *app) handleAgentIntercomAnswer(ctx context.Context, ag *agent, intercomID string, outbox chan<- any, log *slog.Logger) {
	if intercomID == "" {
		sendOrDrop(outbox, errorMsg("intercom.error", "intercom_id required"))
		return
	}
	calleeLeg := a.agents.webRTCLeg(ag.ID)
	if calleeLeg == "" {
		sendOrDrop(outbox, errorMsg("intercom.error", "audio not ready"))
		return
	}
	// Defensive: the called agent must own this intercom as the target.
	ic := a.intercoms.Get(intercomID)
	if ic == nil || ic.TargetKind != intercomTargetAgent || ic.Target != ag.ID {
		sendOrDrop(outbox, errorMsg("intercom.error", "no longer ringing"))
		return
	}
	if a.reg.callAnsweredBy(ag.ID) != nil {
		// Agent took a customer call between ring and answer.
		a.handleAgentIntercomReject(ctx, ag, intercomID, log)
		return
	}

	icClaimed, ok := a.intercoms.ClaimAnswer(intercomID, calleeLeg)
	if !ok {
		sendOrDrop(outbox, errorMsg("intercom.error", "no longer ringing"))
		return
	}

	if _, err := a.client.Room(icClaimed.RoomID).AddLeg(ctx, voiceblender.AddLegRequest{LegID: calleeLeg, Role: "supervisor"}); err != nil {
		log.Error("intercom answer: add callee leg", "room", icClaimed.RoomID, "error", err)
		a.intercoms.End(icClaimed.ID)
		a.teardownIntercomRoom(icClaimed, log)
		a.notifyIntercomEnded(icClaimed, icEndedSetupFailed)
		return
	}

	log.Info("intercom: answered", "intercom_id", icClaimed.ID, "by_agent", ag.ID)

	active := map[string]any{
		"type":        "intercom.active",
		"intercom_id": icClaimed.ID,
		"agent_id":    icClaimed.AgentID,
		"agent_name":  icClaimed.AgentName,
		"target_kind": icClaimed.TargetKind,
		"target":      icClaimed.Target,
		"target_name": icClaimed.TargetName,
		"started_at":  icClaimed.StartedAt,
		"answered_at": icClaimed.AnsweredAt,
	}
	sendOrDrop(outbox, active)               // to the callee
	a.notifyAgentIntercom(icClaimed, active) // to the caller
	a.reg.NotifyChanged()
}

// handleAgentIntercomReject is the called agent's "Reject" action.
// Mirrors handleSupervisorIntercomReject — ends the intercom for both
// sides immediately.
func (a *app) handleAgentIntercomReject(_ context.Context, ag *agent, intercomID string, log *slog.Logger) {
	if intercomID == "" {
		return
	}
	ic := a.intercoms.Get(intercomID)
	if ic == nil || ic.TargetKind != intercomTargetAgent || ic.Target != ag.ID {
		return
	}
	ended, ok := a.intercoms.Reject(intercomID)
	if !ok {
		return
	}
	a.teardownIntercomRoom(ended, log)
	log.Info("intercom: rejected", "intercom_id", ended.ID, "by_agent", ag.ID)
	a.notifyIntercomEnded(ended, icEndedRejected)
}

// ----------- supervisor-side intercom handlers --------------------------

// handleSupervisorIntercomAnswer claims the ringing intercom and
// bridges the supervisor's WebRTC leg into the room. The supervisor's
// client must have prepped its leg before sending this message — same
// expectation as listen.start. If the supervisor is currently listening
// to a different room, the listen is silently dropped to free their leg.
func (a *app) handleSupervisorIntercomAnswer(ctx context.Context, sess *supervisorSession, presence *supervisorPresence, intercomID string, outbox chan<- any, log *slog.Logger) {
	legID, prevRoom := sess.get()
	if legID == "" {
		sendOrDrop(outbox, errorMsg("intercom.error", "audio not ready"))
		return
	}
	if intercomID == "" {
		sendOrDrop(outbox, errorMsg("intercom.error", "intercom_id required"))
		return
	}

	// Defensive: must be a supervisor-targeted intercom whose target
	// matches this presence's username.
	ic := a.intercoms.Get(intercomID)
	if ic == nil || ic.TargetKind != intercomTargetSupervisor || ic.Target != presence.Username {
		sendOrDrop(outbox, errorMsg("intercom.error", "no longer ringing"))
		return
	}

	// Drop any active listen so we can repurpose the leg for the intercom.
	if prevRoom != "" {
		if _, err := a.client.Room(prevRoom).RemoveLeg(ctx, legID); err != nil && !voiceblender.IsNotFound(err) {
			log.Warn("intercom answer: leave prior listen", "room", prevRoom, "error", err)
		}
		sess.setRoom("")
	}

	icClaimed, ok := a.intercoms.ClaimAnswer(intercomID, legID)
	if !ok {
		sendOrDrop(outbox, errorMsg("intercom.error", "no longer ringing"))
		return
	}

	if _, err := a.client.Room(icClaimed.RoomID).AddLeg(ctx, voiceblender.AddLegRequest{LegID: legID, Role: "supervisor"}); err != nil {
		log.Error("intercom answer: add supervisor leg", "room", icClaimed.RoomID, "error", err)
		a.intercoms.End(icClaimed.ID)
		a.teardownIntercomRoom(icClaimed, log)
		a.notifyIntercomEnded(icClaimed, icEndedSetupFailed)
		return
	}
	sess.setRoom(icClaimed.RoomID)

	log.Info("intercom: answered", "intercom_id", icClaimed.ID, "by_supervisor", presence.Username)

	// Close the modal on every other session of the answering username.
	for _, p := range a.supervisors.Sessions(presence.Username) {
		if p.Session == sess {
			continue
		}
		sendOrDrop(p.Outbox, map[string]any{
			"type":        "intercom.cleared",
			"intercom_id": icClaimed.ID,
		})
	}

	active := map[string]any{
		"type":        "intercom.active",
		"intercom_id": icClaimed.ID,
		"agent_id":    icClaimed.AgentID,
		"agent_name":  icClaimed.AgentName,
		"target_kind": icClaimed.TargetKind,
		"target":      icClaimed.Target,
		"target_name": icClaimed.TargetName,
		"supervisor":  presence.Username,
		"started_at":  icClaimed.StartedAt,
		"answered_at": icClaimed.AnsweredAt,
	}
	sendOrDrop(outbox, active)
	a.notifyAgentIntercom(icClaimed, active)
	a.reg.NotifyChanged()
}

// handleSupervisorIntercomReject ends a still-ringing supervisor-target
// intercom for every session of the target username.
func (a *app) handleSupervisorIntercomReject(_ context.Context, presence *supervisorPresence, intercomID string, log *slog.Logger) {
	if intercomID == "" {
		return
	}
	ic := a.intercoms.Get(intercomID)
	if ic == nil || ic.TargetKind != intercomTargetSupervisor || ic.Target != presence.Username {
		return
	}
	ended, ok := a.intercoms.Reject(intercomID)
	if !ok {
		return
	}
	a.teardownIntercomRoom(ended, log)
	log.Info("intercom: rejected", "intercom_id", ended.ID, "by_supervisor", presence.Username)
	a.notifyIntercomEnded(ended, icEndedRejected)
}

// handleSupervisorIntercomHangup ends an answered intercom from the
// supervisor's side.
func (a *app) handleSupervisorIntercomHangup(_ context.Context, sess *supervisorSession, intercomID string, log *slog.Logger) {
	if intercomID == "" {
		return
	}
	ic, ok := a.intercoms.End(intercomID)
	if !ok {
		return
	}
	a.teardownIntercomRoom(ic, log)
	if _, rm := sess.get(); rm == ic.RoomID {
		sess.setRoom("")
	}
	a.notifyIntercomEnded(ic, icEndedSupervisorHangup)
}

// ----------- shared helpers --------------------------------------------

// teardownIntercomRoom deletes the room. The WebRTC legs themselves
// keep their own lifecycle (agent-side per-call teardown via the
// browser; supervisor-side persists for the session) so we don't hang
// them up here.
func (a *app) teardownIntercomRoom(ic *intercom, log *slog.Logger) {
	if ic == nil || ic.RoomID == "" {
		return
	}
	if _, err := a.client.Room(ic.RoomID).Delete(context.Background()); err != nil && !voiceblender.IsNotFound(err) {
		log.Warn("intercom: delete room", "room", ic.RoomID, "error", err)
	}
}

// notifyAgentIntercom pushes a message to the calling agent's WS
// outbox if they're currently connected.
func (a *app) notifyAgentIntercom(ic *intercom, msg any) {
	if ic == nil {
		return
	}
	a.fanOutToAgentByID(ic.AgentID, msg)
}

// notifyIntercomEnded pushes intercom.ended to both sides involved.
func (a *app) notifyIntercomEnded(ic *intercom, reason string) {
	if ic == nil {
		return
	}
	end := map[string]any{
		"type":        "intercom.ended",
		"intercom_id": ic.ID,
		"reason":      reason,
	}
	a.notifyAgentIntercom(ic, end) // caller side
	a.fanOutToTarget(ic, end)      // callee side (supervisor or agent)
	a.reg.NotifyChanged()
}

// cleanupIntercomsOnSupervisorDisconnect ends any intercom (ringing or
// active) targeted at the disconnecting supervisor's username when no
// other sessions of that username are still connected. Ringing
// intercoms with other live sessions are kept alive so the agent isn't
// dropped just because one browser tab closed.
func (a *app) cleanupIntercomsOnSupervisorDisconnect(p *supervisorPresence) {
	if p == nil || p.Username == "" {
		return
	}
	sessLeg, _ := p.Session.get()
	for _, ic := range a.intercoms.activeForTarget(intercomTargetSupervisor, p.Username) {
		// Active intercoms tied to this session's leg always end —
		// the supervisor leg is gone with the WS.
		if ic.State == intercomActive && ic.CalleeLeg == sessLeg {
			if _, ok := a.intercoms.End(ic.ID); ok {
				a.teardownIntercomRoom(&ic, a.log)
				a.notifyIntercomEnded(&ic, icEndedSupervisorDisconnected)
			}
			continue
		}
		// Ringing: drop the call only when no sessions of this username remain.
		if ic.State == intercomRinging && len(a.supervisors.Sessions(p.Username)) == 0 {
			if _, ok := a.intercoms.Reject(ic.ID); ok {
				a.teardownIntercomRoom(&ic, a.log)
				a.notifyIntercomEnded(&ic, icEndedSupervisorDisconnected)
			}
		}
	}
}

// cleanupIntercomsOnAgentDisconnect handles the agent leaving from
// either the caller or the callee side. Caller: every intercom they
// own ends with `agent_disconnected`. Callee: every intercom rung
// against this agent ends with `callee_disconnected`.
func (a *app) cleanupIntercomsOnAgentDisconnect(agentID string) {
	// Caller side.
	if ic := a.intercoms.ByAgent(agentID); ic != nil {
		if _, ok := a.intercoms.End(ic.ID); ok {
			a.teardownIntercomRoom(ic, a.log)
			a.notifyIntercomEnded(ic, icEndedAgentDisconnected)
		}
	}
	// Callee side.
	for _, ic := range a.intercoms.activeForTarget(intercomTargetAgent, agentID) {
		if _, ok := a.intercoms.End(ic.ID); ok {
			a.teardownIntercomRoom(&ic, a.log)
			a.notifyIntercomEnded(&ic, icEndedCalleeDisconnected)
		}
	}
}

// errorMsg builds a small {type, message} payload.
func errorMsg(typ, message string) map[string]any {
	return map[string]any{"type": typ, "message": message}
}
