package main

import (
	"regexp"
	"strings"
)

// Caller-number masking modes for the supervisor view.
const (
	maskModeLast4  = "last4"  // "+44 *********0123" — last 4 digits preserved
	maskModeHidden = "hidden" // "caller" — no digits at all
	maskModeFull   = "full"   // unchanged — for development / debugging
)

// sipUserRE matches the user portion of a SIP/SIPS URI. Capture #1 is
// the scheme prefix (e.g. "sip:"), capture #2 is the user.
var sipUserRE = regexp.MustCompile(`(?i)(sips?:)([^@>;]+)`)

// maskCaller hides personally-identifying digits in a SIP caller URI
// for display on the supervisor's panel. The wrapping host part is
// preserved so the UI keeps any non-PII context (carrier domain,
// trunk identifier, etc.).
//
// Empty input and an empty/"full" mode pass through unchanged.
func maskCaller(from, mode string) string {
	if from == "" || mode == maskModeFull {
		return from
	}
	if !sipUserRE.MatchString(from) {
		// Bare phone number or display name — mask in place.
		return maskUser(from, mode)
	}
	return sipUserRE.ReplaceAllStringFunc(from, func(m string) string {
		groups := sipUserRE.FindStringSubmatch(m)
		return groups[1] + maskUser(groups[2], mode)
	})
}

// maskUser hides PII inside a user portion. The leading "+" of an E.164
// number is preserved since it doesn't identify the caller.
func maskUser(user, mode string) string {
	if mode == maskModeHidden {
		return "caller"
	}
	// last4 (default)
	prefix := ""
	if strings.HasPrefix(user, "+") {
		prefix = "+"
		user = user[1:]
	}
	if len(user) <= 4 {
		return prefix + strings.Repeat("*", len(user))
	}
	return prefix + strings.Repeat("*", len(user)-4) + user[len(user)-4:]
}

// resolveMaskMode normalises the env value. Empty / unknown → last4.
func resolveMaskMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case maskModeFull:
		return maskModeFull
	case maskModeHidden:
		return maskModeHidden
	default:
		return maskModeLast4
	}
}
