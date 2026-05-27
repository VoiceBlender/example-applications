package main

import (
	"context"
	"log/slog"
	"strings"
	"time"

	voiceblender "github.com/VoiceBlender/voiceblender-go"
)

// Transfer failure reasons, sent in `transfer.failed`.
const (
	transferReasonRejected           = "rejected"
	transferReasonCancelled          = "cancelled"
	transferReasonTargetDisconnected = "target_disconnected"
	transferReasonTargetUnavailable  = "target_unavailable"
	transferReasonCustomerDropped    = "customer_dropped"
	transferReasonTimeout            = "timeout"
	transferReasonSetupFailed        = "setup_failed"
	transferReasonAgentDisconnected  = "agent_disconnected"
)

// fanOutTransferTarget routes msg to whoever's being rung (supervisor
// username — many tabs — or single agent id).
func (a *app) fanOutTransferTarget(t *transfer, msg any) {
	if t == nil {
		return
	}
	switch t.TargetKind {
	case intercomTargetSupervisor:
		a.fanOutToSupervisors(t.Target, msg)
	case intercomTargetAgent:
		a.fanOutToAgentByID(t.Target, msg)
	}
}

// fanOutToOriginator routes msg to whoever STARTED the transfer. A
// FromAgentID prefixed with "supervisor:" is a supervisor-initiated
// transfer — fan out to every session of that username; anything else
// is an agent id.
func (a *app) fanOutToOriginator(fromID string, msg any) {
	if username, ok := strings.CutPrefix(fromID, "supervisor:"); ok {
		a.fanOutToSupervisors(username, msg)
		return
	}
	a.fanOutToAgentByID(fromID, msg)
}

// ----------- agent (caller) handlers -----------------------------------

// handleAgentTransferStart kicks off a blind or attended transfer
// initiated by an agent who's currently on a customer call.
func (a *app) handleAgentTransferStart(ctx context.Context, ag *agent, kind, target string, attended bool, outbox chan<- any, log *slog.Logger) {
	call := a.reg.callAnsweredBy(ag.ID)
	if call == nil {
		sendOrDrop(outbox, errorMsg("transfer.error", "not on a call"))
		return
	}
	a.startTransfer(ctx, call, ag.ID, ag.Name, call.AnsweredAgentLeg, kind, target, attended, outbox, log)
}

// startTransfer is the shared transfer-start core. Caller must already
// have looked up the call view and the originator's WebRTC leg. The
// originator is identified by (fromID, fromName) — agent.ID for an
// agent or "supervisor:<username>" for a supervisor. Customer is put
// on hold (or kept on hold if already there), the target's incoming-
// modal is pushed, and the originator is acked with the ringing state.
// For attended transfers the target's `transfer.incoming` notification
// carries `attended: true` so their UI shows a "consulting" pill on
// Answer rather than the regular transferred-call UI.
func (a *app) startTransfer(ctx context.Context, call *callView, fromID, fromName, fromLegID, kind, target string, attended bool, outbox chan<- any, log *slog.Logger) {
	if target == "" {
		sendOrDrop(outbox, errorMsg("transfer.error", "target required"))
		return
	}
	if kind == "" {
		kind = intercomTargetSupervisor
	}
	if a.transfers.ByCall(call.LegID) != nil {
		sendOrDrop(outbox, errorMsg("transfer.error", "transfer already in flight"))
		return
	}

	// Target validation, mirrors the intercom validation.
	var targetName string
	switch kind {
	case intercomTargetSupervisor:
		// Self-check: a supervisor can't transfer back to themselves.
		if fromUser, ok := strings.CutPrefix(fromID, "supervisor:"); ok && fromUser == target {
			sendOrDrop(outbox, errorMsg("transfer.error", "cannot transfer to yourself"))
			return
		}
		if len(a.supervisors.Sessions(target)) == 0 {
			sendOrDrop(outbox, errorMsg("transfer.error", "supervisor not available"))
			return
		}
		targetName = target
	case intercomTargetAgent:
		if target == fromID {
			sendOrDrop(outbox, errorMsg("transfer.error", "cannot transfer to yourself"))
			return
		}
		callee, ok := a.agents.get(target)
		if !ok {
			sendOrDrop(outbox, errorMsg("transfer.error", "agent not available"))
			return
		}
		if _, online := a.agentOutboxes.Load(target); !online {
			sendOrDrop(outbox, errorMsg("transfer.error", "agent offline"))
			return
		}
		if a.reg.callAnsweredBy(target) != nil {
			sendOrDrop(outbox, errorMsg("transfer.error", "agent is on a call"))
			return
		}
		if a.intercoms.ByAgent(target) != nil {
			sendOrDrop(outbox, errorMsg("transfer.error", "agent is busy"))
			return
		}
		if a.transfers.ByCall(target) != nil {
			// Cheap extra check; an agent receiving a transfer right now
			// is also unavailable.
			sendOrDrop(outbox, errorMsg("transfer.error", "agent is busy"))
			return
		}
		targetName = callee.Name
	default:
		sendOrDrop(outbox, errorMsg("transfer.error", "unknown target kind"))
		return
	}

	t, err := a.transfers.Create(call.LegID, call.RoomID, fromID, fromName, kind, target, targetName, fromLegID, attended)
	if err != nil {
		sendOrDrop(outbox, errorMsg("transfer.error", err.Error()))
		return
	}

	// Put the customer on hold (or accept that they already are).
	if _, err := a.holdCaller(ctx, call, fromLegID, log); err != nil {
		log.Warn("transfer: hold failed", "error", err)
		a.transfers.Cancel(t.ID)
		a.transfers.Settle(t.ID)
		sendOrDrop(outbox, errorMsg("transfer.error", "hold failed"))
		return
	}

	log.Info("transfer: ringing", "transfer_id", t.ID, "kind", kind, "target", target, "from", fromID)

	// Push the incoming-call notification.
	a.fanOutTransferTarget(t, map[string]any{
		"type":          "transfer.incoming",
		"transfer_id":   t.ID,
		"from_agent_id": t.FromAgentID,
		"from_name":     t.FromName,
		"caller_from":   maskCaller(call.From, a.supervisorMask),
		"attended":      t.Attended,
		"started_at":    t.StartedAt,
	})
	// Ack the originator with the ringing state.
	sendOrDrop(outbox, map[string]any{
		"type":        "transfer.state",
		"transfer_id": t.ID,
		"state":       t.State,
		"target_kind": t.TargetKind,
		"target":      t.Target,
		"target_name": t.TargetName,
		"attended":    t.Attended,
	})

	// Auto-fail after the ring timeout. The watcher fires whether the
	// transfer is still ringing or already settled (no-op on settled).
	go a.watchTransferTimeout(t.ID, a.transferTimeout)
}

// watchTransferTimeout sleeps for d, then fails the transfer if it's
// still ringing. Idempotent under races (Fail is atomic).
func (a *app) watchTransferTimeout(transferID string, d time.Duration) {
	if d <= 0 {
		return
	}
	time.Sleep(d)
	t := a.transfers.Get(transferID)
	if t == nil || t.State != transferRinging {
		return
	}
	failed, ok := a.transfers.Fail(transferID)
	if !ok {
		return
	}
	a.log.Info("transfer: timeout", "transfer_id", failed.ID)
	a.afterTransferFailed(failed, transferReasonTimeout)
}

// handleAgentTransferCancel is the original agent calling off the
// transfer before any target answers (or aborting an attended consult).
func (a *app) handleAgentTransferCancel(ctx context.Context, ag *agent, transferID string, log *slog.Logger) {
	a.cancelOwnTransfer(ctx, ag.ID, transferID, log)
}

// cancelOwnTransfer is the shared cancel core — fromID is the agent's
// id or "supervisor:<username>" depending on who started the transfer.
func (a *app) cancelOwnTransfer(_ context.Context, fromID, transferID string, log *slog.Logger) {
	if transferID == "" {
		if t := a.transfers.ByFromAgent(fromID); t != nil {
			transferID = t.ID
		}
	}
	if transferID == "" {
		return
	}
	t := a.transfers.Get(transferID)
	if t == nil || t.FromAgentID != fromID {
		return
	}
	cancelled, ok := a.transfers.Cancel(transferID)
	if !ok {
		return
	}
	log.Info("transfer: cancelled by originator", "transfer_id", cancelled.ID, "from", fromID)
	a.afterTransferFailed(cancelled, transferReasonCancelled)
}

// ----------- target handlers (agent or supervisor) ---------------------

// answerTransfer is the shared answer path. Two branches:
//   - blind:     immediately finalize (target → customer room, etc.).
//   - attended:  spin up a private consult room, bridge original + target
//                in it; the customer stays on hold. The actual swap to
//                the customer room happens later in `finalizeTransfer`,
//                fired either by an explicit `transfer.complete` from the
//                original agent or by their WS-close defer.
// `newAgentID`/`newAgentName` are the answering side's identity (the
// agent's id+name, or "supervisor:<username>"+username). `legID` is the
// answerer's WebRTC leg.
func (a *app) answerTransfer(ctx context.Context, t *transfer, newAgentID, newAgentName, legID string, log *slog.Logger) (claimed *transfer, ok bool) {
	claimed, ok = a.transfers.ClaimAnswer(t.ID, legID)
	if !ok {
		return nil, false
	}
	call := a.reg.lookup(claimed.CallLegID)
	if call == nil {
		// Call disappeared between ring and answer.
		a.transfers.Fail(claimed.ID)
		a.transfers.Settle(claimed.ID)
		a.notifyOriginalAgentTransferEnded(claimed, transferReasonCustomerDropped)
		return nil, false
	}

	if !claimed.Attended {
		if !a.finalizeTransfer(ctx, claimed, newAgentID, newAgentName, call, log) {
			return nil, false
		}
		return claimed, true
	}

	// Attended: create the consult room and add both legs.
	room, err := a.bringUpConsultRoom(ctx, claimed, log)
	if err != nil {
		log.Error("transfer: consult room setup", "transfer_id", claimed.ID, "error", err)
		a.transfers.Fail(claimed.ID)
		a.transfers.Settle(claimed.ID)
		a.notifyOriginalAgentTransferEnded(claimed, transferReasonSetupFailed)
		a.fanOutTransferTarget(claimed, map[string]any{
			"type":        "transfer.cleared",
			"transfer_id": claimed.ID,
		})
		return nil, false
	}
	_ = room
	log.Info("transfer: consulting",
		"transfer_id", claimed.ID,
		"from", claimed.FromName,
		"to_kind", claimed.TargetKind, "to", claimed.Target, "to_name", claimed.TargetName)

	// Push consulting state to both sides so they can swap UI.
	consultingMsg := map[string]any{
		"type":         "transfer.consulting",
		"transfer_id":  claimed.ID,
		"from_agent_id": claimed.FromAgentID,
		"from_name":    claimed.FromName,
		"target_kind":  claimed.TargetKind,
		"target":       claimed.Target,
		"target_name":  claimed.TargetName,
		"caller_from":  maskCaller(call.From, a.supervisorMask),
	}
	a.fanOutToOriginator(claimed.FromAgentID, consultingMsg)
	a.fanOutTransferTarget(claimed, consultingMsg)
	a.reg.NotifyChanged()
	return claimed, true
}

// bringUpConsultRoom creates a small private room for the consult and
// adds the original agent's leg + the target's leg to it. Reuses the
// default routing matrix: original joins as "agent", target joins as
// "supervisor" — the matrix has agent ↔ supervisor wired both ways so
// they hear each other (no "customer" leg is in the room).
func (a *app) bringUpConsultRoom(ctx context.Context, t *transfer, log *slog.Logger) (*voiceblender.Room, error) {
	if _, err := a.client.CreateRoom(ctx, voiceblender.CreateRoomRequest{ID: t.ConsultRoomID}); err != nil && !voiceblender.IsConflict(err) {
		return nil, err
	}
	room := a.client.Room(t.ConsultRoomID)
	if _, err := room.SetRouting(ctx, voiceblender.RoomRoutingRequest{Matrix: defaultCallRoomMatrix()}); err != nil {
		log.Warn("consult: set routing", "room", t.ConsultRoomID, "error", err)
	}
	if _, err := room.AddLeg(ctx, voiceblender.AddLegRequest{LegID: t.FromLegID, Role: "agent"}); err != nil {
		_, _ = room.Delete(context.Background())
		return nil, err
	}
	if _, err := room.AddLeg(ctx, voiceblender.AddLegRequest{LegID: t.TargetLegID, Role: "supervisor"}); err != nil {
		_, _ = room.Delete(context.Background())
		return nil, err
	}
	return room, nil
}

// finalizeTransfer is the actual hand-off: stop the customer's hold
// music, add the target's leg to the customer room as "agent", swap
// the call's ownership in the registry, signal the per-call goroutine
// to watch the new leg. Shared by blind-answer and attended-complete.
//
// Ordering matters: VoiceBlender hangs up every leg still in a room
// when the room is Deleted. The original agent's leg sits in the
// consult room during attended consult — so we must (a) call
// `reg.transferTo` BEFORE touching the consult room, so the bridge
// loop's safety check sees the new agent leg and ignores the original
// leg's eventual disconnect; and (b) explicitly RemoveLeg both sides
// from the consult room before Delete so the room is empty when
// deleted (no surprise hangups).
func (a *app) finalizeTransfer(ctx context.Context, t *transfer, newAgentID, newAgentName string, call *callView, log *slog.Logger) bool {
	room := a.client.Room(t.RoomID)
	if call.holdPlaybackID != "" {
		if _, err := room.StopPlay(ctx, call.holdPlaybackID); err != nil && !voiceblender.IsNotFound(err) {
			log.Warn("transfer finalize: stop hold music", "room", t.RoomID, "error", err)
		}
	}
	if _, err := room.AddLeg(ctx, voiceblender.AddLegRequest{LegID: t.TargetLegID, Role: "agent"}); err != nil {
		log.Error("transfer finalize: add target leg", "room", t.RoomID, "error", err)
		// Best-effort roll back so the customer isn't left in silence.
		_ = a.restoreCaller(context.Background(), call, t.FromLegID, log)
		a.notifyOriginalAgentTransferEnded(t, transferReasonSetupFailed)
		a.fanOutTransferTarget(t, map[string]any{
			"type":        "transfer.cleared",
			"transfer_id": t.ID,
		})
		a.transfers.Settle(t.ID)
		return false
	}

	// Supervisor target only: hook the session up to the customer call
	// BEFORE reg.transferTo (which broadcasts a fresh snapshot). If we
	// did this after, the snapshot would briefly lack `self.transferred_call`
	// and the supervisor's "on call" pill (with the End button) would
	// flicker off then back on, sometimes leaving them with no End button
	// until the next snapshot.
	if t.TargetKind == intercomTargetSupervisor {
		for _, p := range a.supervisors.Sessions(t.Target) {
			if legID, _ := p.Session.get(); legID == t.TargetLegID {
				p.Session.setRoom(t.RoomID)
				a.supervisorActiveCalls.Store(p.Session, t.CallLegID)
				break
			}
		}
	}
	// Supervisor originator only: the supervisor's leg is no longer on
	// the customer call (they're being handed off). Free their slot in
	// supervisorActiveCalls and clear the session's room pointer so the
	// "on call" panel disappears on the next snapshot.
	if fromUsername, ok := strings.CutPrefix(t.FromAgentID, "supervisor:"); ok {
		for _, p := range a.supervisors.Sessions(fromUsername) {
			if legID, _ := p.Session.get(); legID == t.FromLegID {
				p.Session.setRoom("")
				a.supervisorActiveCalls.Delete(p.Session)
				break
			}
		}
	}

	// Atomic registry update + signal the per-call goroutine. Must run
	// BEFORE the consult-room teardown so the bridge loop's safety
	// check ("AnsweredAgentLeg has been swapped") catches the eventual
	// original-agent-leg disconnect.
	a.reg.transferTo(t.CallLegID, newAgentID, newAgentName, t.TargetLegID)

	// Tear down the consult room (attended only). RemoveLeg both sides
	// first so the room is empty when Deleted — Room.Delete hangs up
	// any legs still in the room, and the original agent's leg is one
	// of them.
	if t.ConsultRoomID != "" {
		if _, err := a.client.Room(t.ConsultRoomID).RemoveLeg(ctx, t.TargetLegID); err != nil && !voiceblender.IsNotFound(err) {
			log.Warn("transfer finalize: remove target from consult", "room", t.ConsultRoomID, "error", err)
		}
		if _, err := a.client.Room(t.ConsultRoomID).RemoveLeg(ctx, t.FromLegID); err != nil && !voiceblender.IsNotFound(err) {
			log.Warn("transfer finalize: remove origin from consult", "room", t.ConsultRoomID, "error", err)
		}
		if _, err := a.client.Room(t.ConsultRoomID).Delete(context.Background()); err != nil && !voiceblender.IsNotFound(err) {
			log.Warn("transfer finalize: delete consult room", "room", t.ConsultRoomID, "error", err)
		}
	}

	log.Info("transfer: completed",
		"transfer_id", t.ID,
		"attended", t.Attended,
		"from", t.FromName,
		"to_kind", t.TargetKind, "to", t.Target, "to_name", t.TargetName)

	a.fanOutToOriginator(t.FromAgentID, map[string]any{
		"type":        "transfer.completed",
		"transfer_id": t.ID,
	})
	if t.TargetKind == intercomTargetSupervisor {
		a.fanOutToSupervisors(t.Target, map[string]any{
			"type":        "transfer.active",
			"transfer_id": t.ID,
			"call_leg_id": t.CallLegID,
			"caller_from": maskCaller(call.From, a.supervisorMask),
		})
	}
	a.transfers.Settle(t.ID)
	a.reg.NotifyChanged()
	return true
}

// handleAgentTransferAnswer is the called agent's "Answer" action.
// For blind transfers this completes the hand-off; for attended it
// enters the consult phase (server pushes `transfer.consulting`).
func (a *app) handleAgentTransferAnswer(ctx context.Context, ag *agent, transferID string, outbox chan<- any, log *slog.Logger) {
	if transferID == "" {
		sendOrDrop(outbox, errorMsg("transfer.error", "transfer_id required"))
		return
	}
	legID := a.agents.webRTCLeg(ag.ID)
	if legID == "" {
		sendOrDrop(outbox, errorMsg("transfer.error", "audio not ready"))
		return
	}
	t := a.transfers.Get(transferID)
	if t == nil || t.TargetKind != intercomTargetAgent || t.Target != ag.ID {
		sendOrDrop(outbox, errorMsg("transfer.error", "no longer ringing"))
		return
	}
	claimed, ok := a.answerTransfer(ctx, t, ag.ID, ag.Name, legID, log)
	if !ok {
		sendOrDrop(outbox, errorMsg("transfer.error", "no longer available"))
		return
	}
	if !claimed.Attended {
		// Blind: snapshot already shows the customer call via
		// self.current_call. The transfer.active ping is just a hint
		// for the UI to flush the modal.
		sendOrDrop(outbox, map[string]any{
			"type":        "transfer.active",
			"transfer_id": t.ID,
		})
	}
	// Attended: the consultingMsg push from answerTransfer drives the UI.
}

// handleAgentTransferComplete is the original agent's explicit
// "Complete transfer" action at the end of a consult — the customer
// is moved to the target and this agent goes idle.
func (a *app) handleAgentTransferComplete(ctx context.Context, ag *agent, transferID string, log *slog.Logger) {
	a.completeOwnTransfer(ctx, ag.ID, transferID, log)
}

// completeOwnTransfer is the shared complete core for an attended
// consult — fromID is "supervisor:<username>" for a supervisor.
func (a *app) completeOwnTransfer(ctx context.Context, fromID, transferID string, log *slog.Logger) {
	if transferID == "" {
		if t := a.transfers.ByFromAgent(fromID); t != nil {
			transferID = t.ID
		}
	}
	if transferID == "" {
		return
	}
	t := a.transfers.Get(transferID)
	if t == nil || t.FromAgentID != fromID || t.State != transferConsulting {
		return
	}
	a.completeAttendedTransfer(ctx, t.ID, log)
}

// completeAttendedTransfer atomically advances a consulting transfer
// to completed and runs the finalize hand-off. Used by the explicit
// `transfer.complete` message AND by the Agent-A WS-disconnect path.
func (a *app) completeAttendedTransfer(ctx context.Context, transferID string, log *slog.Logger) {
	t, ok := a.transfers.Complete(transferID)
	if !ok {
		return
	}
	call := a.reg.lookup(t.CallLegID)
	if call == nil {
		a.notifyOriginalAgentTransferEnded(t, transferReasonCustomerDropped)
		a.fanOutTransferTarget(t, map[string]any{
			"type":        "transfer.cleared",
			"transfer_id": t.ID,
		})
		return
	}
	newAgentID := t.Target
	newAgentName := t.TargetName
	if t.TargetKind == intercomTargetSupervisor {
		newAgentID = "supervisor:" + t.Target
		newAgentName = t.Target
	}
	a.finalizeTransfer(ctx, t, newAgentID, newAgentName, call, log)
}

// handleAgentTransferReject is the called agent's "Reject".
func (a *app) handleAgentTransferReject(_ context.Context, ag *agent, transferID string, log *slog.Logger) {
	if transferID == "" {
		return
	}
	t := a.transfers.Get(transferID)
	if t == nil || t.TargetKind != intercomTargetAgent || t.Target != ag.ID {
		return
	}
	failed, ok := a.transfers.Fail(transferID)
	if !ok {
		return
	}
	log.Info("transfer: rejected by agent", "transfer_id", failed.ID, "by", ag.ID)
	a.afterTransferFailed(failed, transferReasonRejected)
}

// handleSupervisorTransferStart kicks off a blind or attended transfer
// initiated by a supervisor who's currently on a customer call (the
// call having been transferred TO them earlier by an agent).
func (a *app) handleSupervisorTransferStart(ctx context.Context, sess *supervisorSession, presence *supervisorPresence, kind, target string, attended bool, outbox chan<- any, log *slog.Logger) {
	fromID := "supervisor:" + presence.Username
	call := a.reg.callAnsweredBy(fromID)
	if call == nil {
		sendOrDrop(outbox, errorMsg("transfer.error", "not on a call"))
		return
	}
	legID, _ := sess.get()
	if legID == "" || legID != call.AnsweredAgentLeg {
		sendOrDrop(outbox, errorMsg("transfer.error", "audio not ready"))
		return
	}
	a.startTransfer(ctx, call, fromID, presence.Username, legID, kind, target, attended, outbox, log)
}

// handleSupervisorTransferCancel is the supervisor calling off a
// transfer they initiated (ringing or attended-consult phase).
func (a *app) handleSupervisorTransferCancel(ctx context.Context, presence *supervisorPresence, transferID string, log *slog.Logger) {
	a.cancelOwnTransfer(ctx, "supervisor:"+presence.Username, transferID, log)
}

// handleSupervisorTransferComplete is the supervisor's explicit
// "Complete transfer" at the end of an attended consult.
func (a *app) handleSupervisorTransferComplete(ctx context.Context, presence *supervisorPresence, transferID string, log *slog.Logger) {
	a.completeOwnTransfer(ctx, "supervisor:"+presence.Username, transferID, log)
}

// handleSupervisorTransferAnswer is the supervisor's "Answer" action.
func (a *app) handleSupervisorTransferAnswer(ctx context.Context, sess *supervisorSession, presence *supervisorPresence, transferID string, outbox chan<- any, log *slog.Logger) {
	legID, prevRoom := sess.get()
	if legID == "" {
		sendOrDrop(outbox, errorMsg("transfer.error", "audio not ready"))
		return
	}
	if transferID == "" {
		sendOrDrop(outbox, errorMsg("transfer.error", "transfer_id required"))
		return
	}
	t := a.transfers.Get(transferID)
	if t == nil || t.TargetKind != intercomTargetSupervisor || t.Target != presence.Username {
		sendOrDrop(outbox, errorMsg("transfer.error", "no longer ringing"))
		return
	}
	// If they were listening to a different room, drop the listen so
	// the leg is free for the transfer.
	if prevRoom != "" && prevRoom != t.RoomID {
		if _, err := a.client.Room(prevRoom).RemoveLeg(ctx, legID); err != nil && !voiceblender.IsNotFound(err) {
			log.Warn("transfer answer: leave prior listen", "room", prevRoom, "error", err)
		}
	}
	claimed, ok := a.answerTransfer(ctx, t, "supervisor:"+presence.Username, presence.Username, legID, log)
	if !ok {
		sendOrDrop(outbox, errorMsg("transfer.error", "no longer available"))
		return
	}
	// Close any sibling sessions' incoming modal.
	for _, p := range a.supervisors.Sessions(presence.Username) {
		if p.Session == sess {
			continue
		}
		sendOrDrop(p.Outbox, map[string]any{
			"type":        "transfer.cleared",
			"transfer_id": t.ID,
		})
	}
	if claimed.Attended {
		// Consulting: the supervisor's leg is in the consult room, not
		// the customer's. Don't register them as on a customer call yet.
		sess.setRoom(claimed.ConsultRoomID)
		return
	}
	// Blind: bridged with the customer immediately.
	sess.setRoom(claimed.RoomID)
	a.supervisorActiveCalls.Store(sess, claimed.CallLegID)
	a.reg.NotifyChanged()
}

// handleSupervisorTransferReject is the supervisor's "Reject".
func (a *app) handleSupervisorTransferReject(_ context.Context, presence *supervisorPresence, transferID string, log *slog.Logger) {
	if transferID == "" {
		return
	}
	t := a.transfers.Get(transferID)
	if t == nil || t.TargetKind != intercomTargetSupervisor || t.Target != presence.Username {
		return
	}
	failed, ok := a.transfers.Fail(transferID)
	if !ok {
		return
	}
	log.Info("transfer: rejected by supervisor", "transfer_id", failed.ID, "by", presence.Username)
	a.afterTransferFailed(failed, transferReasonRejected)
}

// handleSupervisorTransferEnd is the supervisor hanging up a customer
// call they accepted via transfer. Hangs up the customer leg; the
// per-call goroutine then tears down naturally.
func (a *app) handleSupervisorTransferEnd(ctx context.Context, sess *supervisorSession, _ string, log *slog.Logger) {
	v, ok := a.supervisorActiveCalls.Load(sess)
	if !ok {
		return
	}
	callLegID, _ := v.(string)
	a.supervisorActiveCalls.Delete(sess)
	if rm, _ := sess.get(); rm != "" {
		sess.setRoom("")
	}
	if callLegID == "" {
		return
	}
	if _, err := a.client.Leg(callLegID).Hangup(ctx, voiceblender.DeleteLegRequest{}); err != nil && !voiceblender.IsNotFound(err) {
		log.Warn("supervisor: hangup transferred call", "leg_id", callLegID, "error", err)
	}
}

// ----------- shared helpers --------------------------------------------

// afterTransferFailed runs the failure cleanup: tear down the consult
// room (attended only), restore the customer to the original agent
// (resume from hold), and notify both sides. RemoveLeg-then-Delete on
// the consult room mirrors finalizeTransfer's ordering — Room.Delete
// would otherwise hang up the legs still in it and the original
// agent's leg-disconnect would be misread by the per-call bridge loop
// as the agent dropping (drops the customer).
func (a *app) afterTransferFailed(t *transfer, reason string) {
	if t == nil {
		return
	}
	if t.ConsultRoomID != "" {
		if t.TargetLegID != "" {
			if _, err := a.client.Room(t.ConsultRoomID).RemoveLeg(context.Background(), t.TargetLegID); err != nil && !voiceblender.IsNotFound(err) {
				a.log.Warn("transfer fail: remove target leg from consult", "room", t.ConsultRoomID, "error", err)
			}
		}
		if t.FromLegID != "" {
			if _, err := a.client.Room(t.ConsultRoomID).RemoveLeg(context.Background(), t.FromLegID); err != nil && !voiceblender.IsNotFound(err) {
				a.log.Warn("transfer fail: remove origin leg from consult", "room", t.ConsultRoomID, "error", err)
			}
		}
		if _, err := a.client.Room(t.ConsultRoomID).Delete(context.Background()); err != nil && !voiceblender.IsNotFound(err) {
			a.log.Warn("transfer fail: delete consult room", "room", t.ConsultRoomID, "error", err)
		}
	}
	call := a.reg.lookup(t.CallLegID)
	if call != nil {
		// Re-add the originator's leg if it still exists. For a
		// supervisor originator we use the recorded FromLegID (which is
		// the supervisor's WebRTC leg captured at Create time); for an
		// agent we look it up by agent id so a fresh leg from a
		// reconnect during the transfer is still picked up.
		originalLeg := t.FromLegID
		if _, ok := strings.CutPrefix(t.FromAgentID, "supervisor:"); !ok {
			if cur := a.agents.webRTCLeg(t.FromAgentID); cur != "" {
				originalLeg = cur
			} else {
				originalLeg = ""
			}
		}
		_ = a.restoreCaller(context.Background(), call, originalLeg, a.log)
	}
	a.notifyOriginalAgentTransferEnded(t, reason)
	a.fanOutTransferTarget(t, map[string]any{
		"type":        "transfer.cleared",
		"transfer_id": t.ID,
	})
	a.transfers.Settle(t.ID)
	a.reg.NotifyChanged()
}

// notifyOriginalAgentTransferEnded pushes transfer.failed to the
// agent (or supervisor) who started the transfer.
func (a *app) notifyOriginalAgentTransferEnded(t *transfer, reason string) {
	if t == nil {
		return
	}
	a.fanOutToOriginator(t.FromAgentID, map[string]any{
		"type":        "transfer.failed",
		"transfer_id": t.ID,
		"reason":      reason,
	})
}

// failTransferOnCustomerHangup is called from bridgeAndMonitor when the
// customer drops. Closes any in-flight transfer cleanly.
func (a *app) failTransferOnCustomerHangup(callLegID string, log *slog.Logger) {
	t := a.transfers.ByCall(callLegID)
	if t == nil {
		return
	}
	if _, ok := a.transfers.Fail(t.ID); !ok {
		return
	}
	log.Info("transfer: customer dropped", "transfer_id", t.ID)
	// Tear down the consult room, if any. The call is dead so there's
	// nothing to restore — but we still need to evict the legs before
	// Room.Delete (otherwise the original agent's leg gets hung up too,
	// which would prevent them from taking new calls).
	if t.ConsultRoomID != "" {
		if t.TargetLegID != "" {
			if _, err := a.client.Room(t.ConsultRoomID).RemoveLeg(context.Background(), t.TargetLegID); err != nil && !voiceblender.IsNotFound(err) {
				log.Warn("transfer customer-drop: remove target leg from consult", "room", t.ConsultRoomID, "error", err)
			}
		}
		if t.FromLegID != "" {
			if _, err := a.client.Room(t.ConsultRoomID).RemoveLeg(context.Background(), t.FromLegID); err != nil && !voiceblender.IsNotFound(err) {
				log.Warn("transfer customer-drop: remove origin leg from consult", "room", t.ConsultRoomID, "error", err)
			}
		}
		if _, err := a.client.Room(t.ConsultRoomID).Delete(context.Background()); err != nil && !voiceblender.IsNotFound(err) {
			log.Warn("transfer customer-drop: delete consult room", "room", t.ConsultRoomID, "error", err)
		}
	}
	a.notifyOriginalAgentTransferEnded(t, transferReasonCustomerDropped)
	a.fanOutTransferTarget(t, map[string]any{
		"type":        "transfer.cleared",
		"transfer_id": t.ID,
	})
	a.transfers.Settle(t.ID)
}

// cleanupTransfersOnAgentDisconnect handles WS disconnects from either
// side of a transfer. If the disconnecting agent is the ORIGINATOR:
//   - ringing  → cancel (drops the ringing modal, restoreCaller is
//                a no-op since their leg is gone)
//   - consulting → COMPLETE the transfer (the user's explicit ask:
//                  "transfer should happen when the first agent
//                  disconnects"). Customer goes to the target.
// If the disconnecting agent is the TARGET of any ringing or consulting
// transfer: fail it and restore the customer to the originator.
func (a *app) cleanupTransfersOnAgentDisconnect(agentID string) {
	if t := a.transfers.ByFromAgent(agentID); t != nil {
		switch t.State {
		case transferConsulting:
			// Agent A left the consult — complete the hand-off to target.
			a.completeAttendedTransfer(context.Background(), t.ID, a.log)
		case transferRinging:
			if cancelled, ok := a.transfers.Cancel(t.ID); ok {
				// Agent's leg is gone — restoreCaller has no one to
				// re-add; the call effectively ends when the customer's
				// leg gets cleaned up by the WS-close defer in
				// handleAgentStream.
				a.fanOutTransferTarget(cancelled, map[string]any{
					"type":        "transfer.cleared",
					"transfer_id": cancelled.ID,
				})
				a.transfers.Settle(cancelled.ID)
				a.reg.NotifyChanged()
			}
		}
	}
	for _, t := range a.transfers.ByTarget(intercomTargetAgent, agentID) {
		if failed, ok := a.transfers.Fail(t.ID); ok {
			a.afterTransferFailed(failed, transferReasonTargetDisconnected)
		}
	}
}

// cleanupTransfersOnSupervisorDisconnect handles WS disconnects for a
// supervisor session involved in transfers in either direction.
//
//   - ORIGINATOR (this supervisor started a transfer):
//     - ringing  → cancel (drops the target's modal). Their leg is
//                  gone, so restoreCaller can't put them back; the
//                  customer-hangup step below ends the call.
//     - consulting → COMPLETE (same "transfer happens when the
//                    first party disconnects" rule we use for agents).
//
//   - TARGET (this supervisor was being rung): fail the transfer and
//     restore the customer to the originator — but only if no other
//     tab of the same username is still up to answer.
//
// Finally, any active transferred customer call on this session is
// hung up (only if it's still in supervisorActiveCalls — finalize will
// have already cleared it for a consulting completion above).
func (a *app) cleanupTransfersOnSupervisorDisconnect(p *supervisorPresence) {
	if p == nil || p.Username == "" {
		return
	}
	fromID := "supervisor:" + p.Username
	if t := a.transfers.ByFromAgent(fromID); t != nil {
		switch t.State {
		case transferConsulting:
			a.completeAttendedTransfer(context.Background(), t.ID, a.log)
		case transferRinging:
			if cancelled, ok := a.transfers.Cancel(t.ID); ok {
				a.fanOutTransferTarget(cancelled, map[string]any{
					"type":        "transfer.cleared",
					"transfer_id": cancelled.ID,
				})
				a.transfers.Settle(cancelled.ID)
				a.reg.NotifyChanged()
			}
		}
	}
	for _, t := range a.transfers.ByTarget(intercomTargetSupervisor, p.Username) {
		if len(a.supervisors.Sessions(p.Username)) > 0 {
			continue // other tabs of same username still up
		}
		if failed, ok := a.transfers.Fail(t.ID); ok {
			a.afterTransferFailed(failed, transferReasonTargetDisconnected)
		}
	}
	// Active transferred call → hang up customer.
	if v, ok := a.supervisorActiveCalls.LoadAndDelete(p.Session); ok {
		if callLegID, _ := v.(string); callLegID != "" {
			if _, err := a.client.Leg(callLegID).Hangup(context.Background(), voiceblender.DeleteLegRequest{}); err != nil && !voiceblender.IsNotFound(err) {
				a.log.Warn("supervisor disconnect: hangup transferred call", "leg_id", callLegID, "error", err)
			}
		}
	}
}
