package main

import (
	"context"
	"strings"
	"time"

	voiceblender "github.com/VoiceBlender/voiceblender-go"
)

// registrationReconciler periodically re-syncs registration status from the
// server until the context is cancelled.
func (a *app) registrationReconciler(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.reconcileRegistrations(ctx)
		}
	}
}

// reconcileRegistrations queries the server for the authoritative set of active
// AOR bindings and updates every extension's live status accordingly. Run once
// at startup (to pick up phones that registered before the app came up) and on
// a ticker (to self-heal any missed active/expired event).
func (a *app) reconcileRegistrations(ctx context.Context) {
	resp, err := a.vsi().ListSIPRegistrations(ctx)
	if err != nil {
		a.log.Warn("list registrations", "error", err)
		return
	}
	present := make(map[string]regStatus, len(resp.Bindings))
	for _, b := range resp.Bindings {
		user := strings.ToLower(sipUser(b.Aor))
		if user == "" {
			continue
		}
		var expires time.Time
		if b.ExpiresAt != "" {
			expires, _ = time.Parse(time.RFC3339, b.ExpiresAt)
		}
		present[user] = regStatus{
			AOR:       b.Aor,
			Contact:   b.Contact,
			Socket:    b.Socket,
			UserAgent: b.UserAgent,
			ExpiresAt: expires,
		}
	}
	a.exts.reconcile(present)
}

// onRegistrationAttempt handles an inbound REGISTER surfaced for a decision.
//
// The flow is a standard digest handshake:
//   - unknown AOR            → reject (403)
//   - known, no Authorization → challenge (401) with the extension's password
//   - known, has Authorization → accept (bind + 200 OK)
//
// The server validates the digest against the password supplied in the
// challenge, so by the time an authorized attempt reaches us the credentials
// have already checked out. The server auto-accepts if we don't respond before
// its consult timeout, so respond promptly.
func (a *app) onRegistrationAttempt(e *voiceblender.SIPRegistrationAttemptEvent) {
	ctx := context.Background()
	user := sipUser(e.Aor)

	ext, ok := a.exts.byUsername(user)
	if !ok {
		a.log.Info("register rejected: unknown extension", "aor", e.Aor, "source", e.SourceAddress)
		if _, err := a.vsi().RejectRegistration(ctx, voiceblender.RejectRegistrationPayload{
			ID: e.AttemptID, Code: 403, Reason: "Unknown extension",
		}); err != nil {
			a.log.Warn("reject registration", "aor", e.Aor, "error", err)
		}
		return
	}

	if !e.HasAuthorization {
		a.log.Info("register challenge", "aor", e.Aor, "username", ext.Username, "max_expires", ext.regExpiry())
		if _, err := a.vsi().ChallengeRegistration(ctx, voiceblender.ChallengeRegistrationPayload{
			ID:         e.AttemptID,
			Realm:      a.realm,
			Username:   ext.Username,
			Password:   ext.Password,
			MaxExpires: ext.regExpiry(),
		}); err != nil {
			a.log.Warn("challenge registration", "aor", e.Aor, "error", err)
		}
		return
	}

	a.log.Info("register accept", "aor", e.Aor, "username", ext.Username, "contact", e.Contact, "max_expires", ext.regExpiry())
	if _, err := a.vsi().AcceptRegistration(ctx, voiceblender.AcceptRegistrationPayload{ID: e.AttemptID, MaxExpires: ext.regExpiry()}); err != nil {
		a.log.Warn("accept registration", "aor", e.Aor, "error", err)
	}
}

// onRegistrationActive records a new or refreshed AOR binding.
func (a *app) onRegistrationActive(e *voiceblender.SIPRegistrationActiveEvent) {
	user := sipUser(e.Aor)
	var expires time.Time
	if e.ExpiresAt != "" {
		expires, _ = time.Parse(time.RFC3339, e.ExpiresAt)
	}
	a.exts.setRegistered(user, regStatus{
		AOR:       e.Aor,
		Contact:   e.Contact,
		Socket:    e.Socket,
		UserAgent: e.UserAgent,
		ExpiresAt: expires,
	})
	a.log.Info("registration active", "aor", e.Aor, "contact", e.Contact, "expires", e.ExpiresAt)
}

// onRegistrationExpired clears an AOR binding.
func (a *app) onRegistrationExpired(e *voiceblender.SIPRegistrationExpiredEvent) {
	user := sipUser(e.Aor)
	a.exts.setUnregistered(user)
	a.log.Info("registration expired", "aor", e.Aor, "reason", e.Reason)
}
