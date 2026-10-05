package main

import (
	"encoding/json"
	"strings"
	"testing"

	voiceblender "github.com/VoiceBlender/voiceblender-go"
)

// An extension's filter chain is configured as JSON alongside its codecs, so it
// must round-trip through the stored form unchanged — including parameters.
func TestExtensionFiltersRoundTrip(t *testing.T) {
	raw := `{
		"id": "e1",
		"number": "1001",
		"name": "Warehouse handset",
		"username": "acme-1001",
		"codecs": ["PCMU"],
		"filters": [
			{"type": "bandpass", "params": {"low_hz": 300, "high_hz": 3400}},
			{"type": "denoise"}
		]
	}`
	var e Extension
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		t.Fatalf("decode extension: %v", err)
	}
	if len(e.Filters) != 2 {
		t.Fatalf("filters = %+v, want 2 entries", e.Filters)
	}
	if e.Filters[0].Type != "bandpass" || e.Filters[0].Params["low_hz"] != 300 {
		t.Errorf("first filter = %+v, want bandpass with low_hz=300", e.Filters[0])
	}
	if e.Filters[1].Type != "denoise" {
		t.Errorf("second filter = %+v, want denoise", e.Filters[1])
	}

	out, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("encode extension: %v", err)
	}
	var back Extension
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("re-decode extension: %v", err)
	}
	if len(back.Filters) != 2 || back.Filters[0].Params["high_hz"] != 3400 {
		t.Errorf("filters did not survive a round trip: %+v", back.Filters)
	}
	t.Logf("round-tripped %d filters: %+v", len(back.Filters), back.Filters)

	// An extension with no chain must stay absent rather than serialising as
	// an empty array, which the API reads as an explicit opt-out.
	plain, err := json.Marshal(Extension{ID: "e2", Number: "1002"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(plain); strings.Contains(got, `"filters"`) {
		t.Errorf("an unset chain must be omitted, got %s", got)
	}
	t.Log("unset chain omitted from JSON, so the server default still applies")
}

// The chain configured on an extension is what reaches the leg VoiceBlender
// originates towards that extension's phone.
func TestExtensionFiltersReachOriginatedLeg(t *testing.T) {
	ext := Extension{
		Number:  "1001",
		Filters: []voiceblender.FilterSpec{{Type: "denoise"}},
	}

	// The single-target path carries the chain on callMeta.
	meta := callMeta{tenantID: "acme", from: "1002", to: ext.Number, kind: "internal"}
	meta.codecs = ext.Codecs
	meta.filters = ext.Filters
	req := voiceblender.CreateLegRequest{
		Type: "sip", To: "sip:acme-1001@pbx", Codecs: meta.codecs, Filters: meta.filters,
	}
	if len(req.Filters) != 1 || req.Filters[0].Type != "denoise" {
		t.Errorf("originated leg filters = %+v, want [denoise]", req.Filters)
	}

	// The fork path carries it per target.
	target := forkTarget{number: ext.Number, aor: "sip:acme-1001@pbx", filters: ext.Filters}
	forked := voiceblender.CreateLegRequest{
		Type: "sip", To: target.aor, Codecs: target.codecs, Filters: target.filters,
	}
	if len(forked.Filters) != 1 || forked.Filters[0].Type != "denoise" {
		t.Errorf("forked leg filters = %+v, want [denoise]", forked.Filters)
	}
	t.Log("extension chain reaches both the single-target and forked originate paths")
}

// Filtering is ingress-only, so a call is fully covered only when both parties'
// chains are applied: the originated leg carries the callee's chain, and the
// caller's own inbound leg carries the caller's. Covering only one direction
// means an extension configured for denoise gets nothing when it places a call.
func TestBothCallDirectionsCarryFilters(t *testing.T) {
	caller := Extension{Number: "1001", Filters: []voiceblender.FilterSpec{{Type: "denoise"}}}
	callee := Extension{Number: "1002", Filters: []voiceblender.FilterSpec{
		{Type: "bandpass", Params: map[string]float64{"low_hz": 300}},
	}}

	meta := callMeta{
		tenantID: "acme", from: caller.Number, to: callee.Number, kind: "internal",
		callerFilters: caller.Filters,
	}
	meta.filters = callee.Filters

	// The leg we originate towards the callee's phone carries the callee's chain.
	originated := voiceblender.CreateLegRequest{Type: "sip", To: "sip:acme-1002@pbx", Filters: meta.filters}
	if len(originated.Filters) != 1 || originated.Filters[0].Type != "bandpass" {
		t.Errorf("originated leg = %+v, want the callee's bandpass", originated.Filters)
	}

	// The caller's own leg carries the caller's chain when we answer it.
	b := &bridge{aLeg: "a-leg", callerFilters: meta.callerFilters}
	answer := voiceblender.AnswerLegPayload{ID: b.aLeg, Filters: b.callerFilters}
	if len(answer.Filters) != 1 || answer.Filters[0].Type != "denoise" {
		t.Errorf("caller leg = %+v, want the caller's denoise", answer.Filters)
	}

	// Same on the forked (multi-device) path.
	g := &forkGroup{aLeg: "a-leg", callerFilters: meta.callerFilters}
	forkAnswer := voiceblender.AnswerLegPayload{ID: g.aLeg, Filters: g.callerFilters}
	if len(forkAnswer.Filters) != 1 || forkAnswer.Filters[0].Type != "denoise" {
		t.Errorf("forked caller leg = %+v, want the caller's denoise", forkAnswer.Filters)
	}
	t.Logf("callee leg: %+v   caller leg: %+v", originated.Filters, answer.Filters)
}

// The PBX keeps no allowlist of filter names: it forwards whatever the console
// sends and lets VoiceBlender validate. That is what lets a filter added
// server-side reach the console without a PBX release.
func TestUnknownFilterNamesArePassedThrough(t *testing.T) {
	for _, name := range []string{"denoise", "denoise_gtcrn", "some-future-filter"} {
		var req struct {
			Filters []voiceblender.FilterSpec `json:"filters"`
		}
		body := `{"filters":[{"type":"` + name + `"},{"type":"bandpass","params":{"low_hz":300}}]}`
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
		if len(req.Filters) != 2 || req.Filters[0].Type != name {
			t.Errorf("%s did not survive decoding: %+v", name, req.Filters)
		}
		out, err := json.Marshal(req.Filters)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), name) {
			t.Errorf("%s did not survive re-encoding: %s", name, out)
		}
	}
	t.Log("filter names the PBX has never heard of reach VoiceBlender unchanged")
}
