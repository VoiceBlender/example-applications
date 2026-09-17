package main

import (
	"reflect"
	"testing"
)

// A register trunk dials at its registrar even with a proxy configured: the
// proxy is a next hop the server attaches to the INVITE, not the destination
// domain. Getting this wrong would put the proxy's host in the To URI.
func TestTrunkDialHostIgnoresOutboundProxy(t *testing.T) {
	tr := Trunk{
		Type:          trunkRegister,
		RegistrarURI:  "sip:sip.provider.com",
		OutboundProxy: "sip:edge.provider.com:5284",
	}
	if got, want := tr.dialHost(), "sip.provider.com"; got != want {
		t.Errorf("dialHost() = %q, want the registrar %q", got, want)
	}
}

// Inbound calls arrive from the proxy once one is configured, so source-IP
// matching has to accept it as well as the registrar.
func TestTrunkPeerHosts(t *testing.T) {
	cases := []struct {
		name  string
		trunk Trunk
		want  []string
	}{
		{
			name:  "register with proxy",
			trunk: Trunk{Type: trunkRegister, RegistrarURI: "sip:198.51.100.7", OutboundProxy: "sip:203.0.113.5:5284"},
			want:  []string{"198.51.100.7", "203.0.113.5"},
		},
		{
			name:  "register without proxy",
			trunk: Trunk{Type: trunkRegister, RegistrarURI: "sip:198.51.100.7"},
			want:  []string{"198.51.100.7"},
		},
		{
			name:  "ip trunk",
			trunk: Trunk{Type: trunkIP, PeerURI: "sip:203.0.113.9"},
			want:  []string{"203.0.113.9"},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.trunk.peerHosts(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("peerHosts() = %v, want %v", got, tt.want)
			}
		})
	}
}
