package main

import "testing"

func TestMaskCaller(t *testing.T) {
	tests := []struct {
		name string
		from string
		mode string
		want string
	}{
		{"last4 e164 sip", "sip:+447700900123@example.com", maskModeLast4, "sip:+********0123@example.com"},
		{"last4 e164 bare", "+447700900123", maskModeLast4, "+********0123"},
		{"last4 sips uppercase scheme", "SIP:+15551234567@host", maskModeLast4, "SIP:+*******4567@host"},
		{"last4 short user", "sip:12@host", maskModeLast4, "sip:**@host"},
		{"hidden sip", "sip:+447700900123@example.com", maskModeHidden, "sip:caller@example.com"},
		{"hidden bare", "+447700900123", maskModeHidden, "caller"},
		{"full pass-through", "sip:+447700900123@example.com", maskModeFull, "sip:+447700900123@example.com"},
		{"empty", "", maskModeLast4, ""},
		{"display name with sip uri", `"Alice" <sip:+15551234567@host>`, maskModeLast4, `"Alice" <sip:+*******4567@host>`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := maskCaller(tc.from, tc.mode)
			if got != tc.want {
				t.Errorf("maskCaller(%q, %q) = %q, want %q", tc.from, tc.mode, got, tc.want)
			}
		})
	}
}

func TestResolveMaskMode(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", maskModeLast4},
		{"last4", maskModeLast4},
		{"  Last4  ", maskModeLast4},
		{"hidden", maskModeHidden},
		{"HIDDEN", maskModeHidden},
		{"full", maskModeFull},
		{"FULL", maskModeFull},
		{"junk", maskModeLast4},
	}
	for _, tc := range tests {
		if got := resolveMaskMode(tc.in); got != tc.want {
			t.Errorf("resolveMaskMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
