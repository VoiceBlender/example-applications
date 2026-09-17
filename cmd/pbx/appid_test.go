package main

import (
	"regexp"
	"testing"
)

// Calls and phone registrations that reach VoiceBlender without going through
// something the PBX created are untagged, so the default filter must accept the
// empty app_id alongside the PBX's own.
func TestAppFilterDefaultAcceptsUntagged(t *testing.T) {
	a := &app{appID: "pbx"}
	re := mustCompileFilter(t, a.appFilter())

	tests := []struct {
		name  string
		appID string
		match bool
	}{
		{"an untagged inbound call or REGISTER", "", true},
		{"something the PBX created (room, leg, trunk)", "pbx", true},
		{"another example's traffic", "contact-centre", false},
		{"a neighbouring app id", "pbx-staging", false},
	}
	for _, tt := range tests {
		if got := re.MatchString(tt.appID); got != tt.match {
			t.Errorf("%s: filter %q matched app_id %q = %v, want %v",
				tt.name, a.appFilter(), tt.appID, got, tt.match)
		}
	}
}

func TestAppFilterStrictAndLiteral(t *testing.T) {
	a := &app{appID: "pbx.eu", appStrict: true}
	re := mustCompileFilter(t, a.appFilter())

	if re.MatchString("") {
		t.Error("strict filter must not accept untagged events")
	}
	if !re.MatchString("pbx.eu") {
		t.Error("strict filter must accept its own app id")
	}
	if re.MatchString("pbxXeu") {
		t.Error("'.' was treated as a wildcard; the app id must be matched literally")
	}
}

func mustCompileFilter(t *testing.T, pattern string) *regexp.Regexp {
	t.Helper()
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("appFilter() produced an invalid regex %q: %v", pattern, err)
	}
	return re
}
