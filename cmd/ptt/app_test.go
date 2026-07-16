package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

// Several examples (contact-centre, ivr, pbx, ptt) can share one VoiceBlender.
// Isolation rests on app_id: everything this app creates is tagged with it, and
// the VSI stream is filtered server-side to it. These tests pin the filter down.
func TestAppFilter(t *testing.T) {
	a := &app{appID: "ptt"}

	re, err := regexp.Compile(a.appFilter())
	if err != nil {
		t.Fatalf("appFilter() produced an invalid regex %q: %v", a.appFilter(), err)
	}

	tests := []struct {
		name  string
		appID string
		match bool
	}{
		{"our own events", "ptt", true},
		{"another example", "pbx", false},
		{"an untagged event", "", false},
		// Anchored, so a neighbouring app whose id merely contains ours is not
		// swept in — this is why the filter is built with anchors + QuoteMeta
		// rather than the bare app id.
		{"a longer app id sharing our prefix", "ptt-staging", false},
		{"a longer app id sharing our suffix", "my-ptt", false},
	}
	for _, tt := range tests {
		if got := re.MatchString(tt.appID); got != tt.match {
			t.Errorf("%s: filter %q matched %q = %v, want %v", tt.name, a.appFilter(), tt.appID, got, tt.match)
		}
	}
}

// An app id with regex metacharacters must still produce a literal match rather
// than a pattern the server would interpret.
func TestAppFilterEscapesMetacharacters(t *testing.T) {
	a := &app{appID: "ptt.demo"}
	re, err := regexp.Compile(a.appFilter())
	if err != nil {
		t.Fatalf("invalid regex: %v", err)
	}
	if !re.MatchString("ptt.demo") {
		t.Error("filter should match its own app id")
	}
	if re.MatchString("pttXdemo") {
		t.Error("'.' was treated as a wildcard; app id must be matched literally")
	}
}

// handleRoomsList backs the room page's "add a channel to watch" picker. It must
// return the same visibility-filtered set the lobby shows: public rooms and the
// viewer's own/admitted private rooms, but not other people's private rooms.
func TestHandleRoomsList(t *testing.T) {
	rooms := newRoomRegistry(nil)
	rooms.rooms = map[string]Room{
		"pub":  {ID: "pub", Name: "Public", Owner: "bob", Visibility: visibilityPublic},
		"mine": {ID: "mine", Name: "Mine", Owner: "alice", Visibility: visibilityPrivate},
		"hers": {ID: "hers", Name: "Hers", Owner: "carol", Visibility: visibilityPrivate},
	}
	rooms.members = map[string]map[string]bool{
		"mine": {"alice": true},
		"hers": {"carol": true},
	}
	a := &app{rooms: rooms, presence: newPresenceRegistry()}

	req := httptest.NewRequest(http.MethodGet, "/api/rooms", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxUser, "alice"))
	rec := httptest.NewRecorder()
	a.handleRoomsList(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got []roomView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	seen := map[string]bool{}
	for _, r := range got {
		seen[r.ID] = true
	}
	if !seen["pub"] || !seen["mine"] {
		t.Errorf("alice should see the public room and her own private room; got %v", seen)
	}
	if seen["hers"] {
		t.Error("alice must not see carol's private room")
	}
}

// Room ids are a flat global space on the server — app_id separates events, not
// room ids — so they are namespaced to make a collision between two examples
// impossible.
func TestVBRoom(t *testing.T) {
	a := &app{appID: "ptt"}
	if got, want := a.vbRoom("d05c2a0e89ae"), "ptt-d05c2a0e89ae"; got != want {
		t.Errorf("vbRoom() = %q, want %q", got, want)
	}
	other := &app{appID: "pbx"}
	if a.vbRoom("same") == other.vbRoom("same") {
		t.Error("different apps produced the same VoiceBlender room id")
	}
}
