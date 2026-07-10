package main

import (
	"context"

	voiceblender "github.com/VoiceBlender/voiceblender-go"
)

// onTransferRequested handles an incoming SIP REFER: a phone in a call asked to
// transfer it to another party. With SIP_REFER_AUTO_DIAL=false (the app-driven
// default) VoiceBlender parks the REFER and surfaces it as leg.transfer_requested
// for us to decide. We complete it by re-bridging: the transferor (the leg that
// sent the REFER) wants the OTHER party in its call — the transferee — connected
// to the target, and itself dropped.
//
// Flow: accept the REFER (202 to the transferor's phone, clearing its "transfer
// failed" UI), re-bridge the transferee to the target, then report the outcome
// via complete_transfer (which drives the terminal sipfrag NOTIFY). If we can't
// perform it, we decline instead.
//
// This covers blind transfers cleanly; attended transfers are handled
// "semi-attended" — the target (a registered extension) is rung fresh rather
// than splicing into the transferor's existing consultation call, which the app
// can't identify from the REFER's Replaces Call-ID.
//
// Robustness: accept_transfer/decline_transfer resolve against the server's
// parked REFER. When SIP_REFER_AUTO_DIAL=true the server accepts and dials the
// target itself and parks nothing, so these commands return an error we treat as
// "server is handling it" and don't double-handle.
func (a *app) onTransferRequested(e *voiceblender.LegTransferRequestedEvent) {
	ctx := context.Background()
	transferor := e.LegID

	b, ok := a.bridges.get(transferor)
	if !ok {
		// Not a bridged call we manage — either the server is auto-dialing it
		// (nothing parked) or we can't complete it. Best-effort decline; a
		// failure just means there was nothing parked for us to reject.
		if _, err := a.vsi().DeclineTransfer(ctx, voiceblender.DeclineTransferPayload{ID: transferor}); err != nil {
			a.log.Info("transfer not app-managed (server-handled or unknown leg)", "leg_id", transferor, "error", err)
		}
		return
	}

	target := sipUser(e.Target)
	if target == "" {
		a.log.Warn("transfer requested with empty target", "leg_id", transferor)
		_, _ = a.vsi().DeclineTransfer(ctx, voiceblender.DeclineTransferPayload{ID: transferor, Code: 400, Reason: "Bad Refer-To"})
		return
	}

	// Accept: 202 to the transferor. A failure here means nothing was parked —
	// the server is handling the REFER itself (auto-dial on) or it already timed
	// out — so we must not re-bridge, or we'd double-handle the call.
	if _, err := a.vsi().AcceptTransfer(ctx, voiceblender.AcceptTransferPayload{ID: transferor}); err != nil {
		a.log.Info("transfer accept skipped (server-handled or already decided)", "leg_id", transferor, "error", err)
		return
	}

	transferee := b.peerOf(transferor)
	// Best-effort caller ID for the onward call: the transferee's label.
	fromLabel := b.to
	if transferor == b.bLeg {
		fromLabel = b.from
	}
	tenantID := b.tenantID
	a.log.Info("SIP transfer accepted", "kind", e.Kind, "transferor", transferor, "transferee", transferee, "target", e.Target, "tenant", tenantID)

	a.bridges.remove(b)
	// Stop any hold music the transferor started on the transferee (attended
	// transfers hold the transferee while consulting).
	if pb := b.takeMoH(transferor); pb != "" {
		_, _ = a.vsi().LegPlayStop(ctx, voiceblender.PlaybackTargetPayload{ID: transferee, PlaybackID: pb})
	}
	// Detach the transferee (re-homed into a new room by bridgeFrom).
	_, _ = a.vsi().RemoveLegFromRoom(ctx, voiceblender.RoomLegPayload{RoomID: b.roomID, LegID: transferee})
	_, _ = a.vsi().DeleteRoom(ctx, voiceblender.IDPayload{ID: b.roomID})

	// Route the transferee to the target, then report the outcome. The terminal
	// NOTIFY goes out on the still-live transferor leg, so complete before
	// dropping it.
	msg := a.bridgeFrom(transferee, tenantID, fromLabel, target)
	success := msg == ""
	if !success {
		a.log.Info("SIP transfer target unreachable", "target", target, "reason", msg)
		a.detachOrHangup("", transferee, "unavailable")
	}
	if _, err := a.vsi().CompleteTransfer(ctx, voiceblender.CompleteTransferPayload{ID: transferor, Success: success}); err != nil {
		a.log.Warn("complete_transfer failed", "leg_id", transferor, "error", err)
	}
	// The transferor's REFER is complete; drop its leg.
	a.hangup(transferor, "")
	a.notifyChanged()
}
