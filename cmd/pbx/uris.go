package main

import "strings"

// sipUser extracts the user-part of a SIP URI or AOR. It accepts values like
// "sip:1001@pbx.local", "1001@pbx.local", "<sip:1001@host>;tag=..", or a bare
// "1001", and returns "1001".
func sipUser(uri string) string {
	s := strings.TrimSpace(uri)
	if s == "" {
		return ""
	}
	// Strip angle brackets and any trailing parameters.
	if i := strings.Index(s, "<"); i >= 0 {
		s = s[i+1:]
		if j := strings.Index(s, ">"); j >= 0 {
			s = s[:j]
		}
	}
	s = strings.TrimPrefix(s, "sip:")
	s = strings.TrimPrefix(s, "sips:")
	if i := strings.Index(s, "@"); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexAny(s, ";?"); i >= 0 {
		s = s[:i]
	}
	return s
}

// sipHost extracts the host-part of a SIP URI, dropping any scheme, user-part,
// port, and parameters. "sip:alice@sip.provider.com:5060" → "sip.provider.com".
func sipHost(uri string) string {
	s := strings.TrimSpace(uri)
	if i := strings.Index(s, "<"); i >= 0 {
		s = s[i+1:]
		if j := strings.Index(s, ">"); j >= 0 {
			s = s[:j]
		}
	}
	s = strings.TrimPrefix(s, "sip:")
	s = strings.TrimPrefix(s, "sips:")
	if i := strings.Index(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.IndexAny(s, ";?"); i >= 0 {
		s = s[:i]
	}
	// Strip :port (but leave IPv6 in brackets alone for the demo).
	if i := strings.LastIndex(s, ":"); i >= 0 && !strings.Contains(s, "]") {
		s = s[:i]
	}
	return s
}

// hostIP strips the port from a "host:port" source address, returning just the
// host/IP. "203.0.113.5:5060" → "203.0.113.5".
func hostIP(sourceAddr string) string {
	s := strings.TrimSpace(sourceAddr)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "[") { // [ipv6]:port
		if i := strings.Index(s, "]"); i >= 0 {
			return s[1:i]
		}
	}
	if i := strings.LastIndex(s, ":"); i >= 0 {
		return s[:i]
	}
	return s
}
