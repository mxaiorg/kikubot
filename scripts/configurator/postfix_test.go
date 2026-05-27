package main

import "testing"

func TestEnsurePort(t *testing.T) {
	for _, tc := range []struct {
		in, defaultPort, want string
	}{
		{"", "993", ""},
		{"mail.example.com", "993", "mail.example.com:993"},
		{"mail.example.com:1993", "993", "mail.example.com:1993"},
		{"  mail.example.com  ", "993", "mail.example.com:993"},
		{"127.0.0.1", "587", "127.0.0.1:587"},
		{"127.0.0.1:25", "587", "127.0.0.1:25"},
		// Any colon ⇒ leave alone. Bracketed and bare IPv6 are not realistic
		// inputs for these fields; the heuristic intentionally trades
		// IPv6-no-port correctness for simplicity on the common path.
		{"[::1]:993", "993", "[::1]:993"},
	} {
		got := ensurePort(tc.in, tc.defaultPort)
		if got != tc.want {
			t.Errorf("ensurePort(%q, %q) = %q, want %q", tc.in, tc.defaultPort, got, tc.want)
		}
	}
}
