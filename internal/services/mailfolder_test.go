package services

import (
	"testing"

	"kikubot/internal/config"
)

// TestResolveMailFolder covers the indirection between the logical folder
// names exposed to the LLM and the deployment-configured IMAP folder names.
// The value arrives from tool input, so anything unrecognised must be
// rejected rather than handed to IMAP's SELECT.
func TestResolveMailFolder(t *testing.T) {
	origInbox, origSent := config.InboxFolder, config.SentFolder
	defer func() { config.InboxFolder, config.SentFolder = origInbox, origSent }()
	// Deliberately non-default names: the mapping must come from config,
	// not from the logical name being reused as the IMAP folder.
	config.InboxFolder = "INBOX"
	config.SentFolder = "INBOX.Sent"

	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"empty defaults to inbox", "", "INBOX", false},
		{"inbox", "inbox", "INBOX", false},
		{"sent", "sent", "INBOX.Sent", false},
		{"case insensitive", "SeNt", "INBOX.Sent", false},
		{"whitespace tolerated", "  sent  ", "INBOX.Sent", false},
		{"unknown rejected", "Drafts", "", true},
		{"raw imap name rejected", "INBOX.Sent", "", true},
		{"injection attempt rejected", `INBOX" "*`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveMailFolder(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveMailFolder(%q) = %q, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveMailFolder(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("resolveMailFolder(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
