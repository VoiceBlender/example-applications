package main

import (
	"regexp"
	"testing"
)

// The contact centre answers INBOUND calls. VoiceBlender creates those legs, and
// they only carry an app_id when the INVITE arrives with an X-App-ID header — so
// an ordinary call is untagged. The default filter must therefore accept the
// empty app_id, or the contact centre would never ring.
func TestAppFilterDefaultAcceptsUnattributedInboundCalls(t *testing.T) {
	a := &app{appID: "contact-centre"}
	re := mustCompile(t, a.appFilter())

	tests := []struct {
		name  string
		appID string
		match bool
	}{
		{"an unattributed inbound call", "", true},
		{"a call tagged for us via X-App-ID", "contact-centre", true},
		{"something we created (room, agent leg)", "contact-centre", true},
		{"another example's traffic", "ptt", false},
		{"the pbx's traffic", "pbx", false},
	}
	for _, tt := range tests {
		if got := re.MatchString(tt.appID); got != tt.match {
			t.Errorf("%s: filter %q matched app_id %q = %v, want %v",
				tt.name, a.appFilter(), tt.appID, got, tt.match)
		}
	}
}

// With APP_ID_STRICT the operator is asserting that calls for this app arrive
// stamped with X-App-ID, so untagged events are somebody else's.
func TestAppFilterStrictRejectsUntagged(t *testing.T) {
	a := &app{appID: "contact-centre", appStrict: true}
	re := mustCompile(t, a.appFilter())

	if re.MatchString("") {
		t.Error("strict filter must not accept untagged events")
	}
	if !re.MatchString("contact-centre") {
		t.Error("strict filter must accept our own events")
	}
	if re.MatchString("ptt") {
		t.Error("strict filter must not accept another example's events")
	}
}

// The filter is anchored and escaped, so a neighbouring app id can neither be
// swept in by a shared prefix/suffix nor smuggle a regex metacharacter through.
func TestAppFilterIsAnchoredAndLiteral(t *testing.T) {
	a := &app{appID: "contact-centre"}
	re := mustCompile(t, a.appFilter())
	for _, id := range []string{"contact-centre-staging", "my-contact-centre"} {
		if re.MatchString(id) {
			t.Errorf("filter should not match neighbouring app id %q", id)
		}
	}

	dotted := &app{appID: "cc.eu", appStrict: true}
	re = mustCompile(t, dotted.appFilter())
	if !re.MatchString("cc.eu") {
		t.Error("filter should match its own app id")
	}
	if re.MatchString("ccXeu") {
		t.Error("'.' was treated as a wildcard; the app id must be matched literally")
	}
}

func mustCompile(t *testing.T, pattern string) *regexp.Regexp {
	t.Helper()
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("appFilter() produced an invalid regex %q: %v", pattern, err)
	}
	return re
}
