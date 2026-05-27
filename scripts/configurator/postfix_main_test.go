package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRenderMainCfUnrestricted: when the operator has not configured a
// sender whitelist, the generated main.cf must explicitly disable
// smtpd_sender_restrictions so external mail (e.g. Gmail bounces, replies
// from arbitrary correspondents) is not refused at SMTP time.
func TestRenderMainCfUnrestricted(t *testing.T) {
	c := &emailServiceConfig{
		AgentDomain: "agents.example.com",
		LimitAccept: false,
	}
	got := c.renderMainCf()
	if !strings.Contains(got, "smtpd_sender_restrictions = permit") {
		t.Errorf("expected `smtpd_sender_restrictions = permit` in unrestricted main.cf, got:\n%s", got)
	}
	if strings.Contains(got, "check_sender_access") {
		t.Errorf("unrestricted main.cf must not call check_sender_access, got:\n%s", got)
	}
	if !strings.Contains(got, "transport_maps = texthash:/tmp/docker-mailserver/postfix-transport.cf") {
		t.Errorf("expected transport_maps directive in main.cf, got:\n%s", got)
	}
}

// TestRenderMainCfRestricted: when the operator has chosen to limit
// senders, the main.cf must restore the check_sender_access … , reject
// chain so unlisted senders are bounced.
func TestRenderMainCfRestricted(t *testing.T) {
	c := &emailServiceConfig{
		AgentDomain:   "agents.example.com",
		LimitAccept:   true,
		AcceptDomains: []string{"example.com"},
	}
	got := c.renderMainCf()
	if !strings.Contains(got, "check_sender_access texthash:/tmp/docker-mailserver/postfix-sender-access.cf") {
		t.Errorf("expected check_sender_access in restricted main.cf, got:\n%s", got)
	}
	if !strings.Contains(got, ", reject") {
		t.Errorf("expected `, reject` terminator in restricted main.cf, got:\n%s", got)
	}
}

// TestParseMain reads the same strings the renderer produces and confirms
// LimitAccept round-trips. Also covers the legacy fallback: if main.cf is
// absent / silent on smtpd_sender_restrictions, parseMain returns false so
// callers know to consult the sender-access sentinel.
func TestParseMain(t *testing.T) {
	for _, tc := range []struct {
		name        string
		body        string
		wantDecided bool
		wantLimit   bool
	}{
		{
			name:        "restricted",
			body:        "smtpd_sender_restrictions = check_sender_access texthash:/tmp/docker-mailserver/postfix-sender-access.cf, reject\n",
			wantDecided: true,
			wantLimit:   true,
		},
		{
			name:        "permissive",
			body:        "smtpd_sender_restrictions = permit\n",
			wantDecided: true,
			wantLimit:   false,
		},
		{
			name:        "silent",
			body:        "# nothing relevant here\nmynetworks = 127.0.0.0/8\n",
			wantDecided: false,
			wantLimit:   false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &emailServiceConfig{}
			decided := c.parseMain(tc.body)
			if decided != tc.wantDecided {
				t.Errorf("decided = %v, want %v", decided, tc.wantDecided)
			}
			if c.LimitAccept != tc.wantLimit {
				t.Errorf("LimitAccept = %v, want %v", c.LimitAccept, tc.wantLimit)
			}
		})
	}
}

// TestSaveAndLoadRoundtrip: writing a config with LimitAccept=false and
// reading it back must yield LimitAccept=false. This is the regression case
// for "user disabled the whitelist but DMS still rejects" — it requires the
// authoritative signal to live in main.cf, not in a sentinel line that
// Postfix ignores.
func TestSaveAndLoadRoundtrip(t *testing.T) {
	for _, limit := range []bool{false, true} {
		t.Run("limit_"+boolStr(limit), func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(dmsConfigDir(root), 0o755); err != nil {
				t.Fatal(err)
			}
			orig := &emailServiceConfig{
				Enabled:       true,
				AgentDomain:   "agents.example.com",
				LimitAccept:   limit,
				AcceptDomains: []string{"example.com"},
			}
			if err := orig.Save(root); err != nil {
				t.Fatalf("Save: %v", err)
			}

			// main.cf must exist now and reflect the choice.
			mainPath := filepath.Join(dmsConfigDir(root), "postfix-main.cf")
			data, err := os.ReadFile(mainPath)
			if err != nil {
				t.Fatalf("reading generated main.cf: %v", err)
			}
			body := string(data)
			if limit {
				if !strings.Contains(body, "reject") {
					t.Errorf("restricted main.cf missing reject:\n%s", body)
				}
			} else {
				if strings.Contains(body, "reject") {
					t.Errorf("unrestricted main.cf should not contain reject:\n%s", body)
				}
			}

			loaded := loadEmailServiceConfig(root)
			if loaded.LimitAccept != limit {
				t.Errorf("round-trip LimitAccept = %v, want %v", loaded.LimitAccept, limit)
			}
			if loaded.AgentDomain != "agents.example.com" {
				t.Errorf("round-trip AgentDomain = %q, want %q", loaded.AgentDomain, "agents.example.com")
			}
		})
	}
}

// TestLegacySenderAccessSentinel: a pre-existing install that has a
// sender-access file with `.  OK` but no postfix-main.cf must still be read
// as unrestricted, so upgrading the configurator doesn't silently flip
// behaviour on the next page load.
func TestLegacySenderAccessSentinel(t *testing.T) {
	root := t.TempDir()
	dir := dmsConfigDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Mimic the legacy on-disk format: sender-access with `. OK`, no main.cf.
	senderBody := "agents.example.com    OK\n.          OK\n"
	if err := os.WriteFile(filepath.Join(dir, "postfix-sender-access.cf"), []byte(senderBody), 0o644); err != nil {
		t.Fatal(err)
	}
	transportBody := "agents.example.com    lmtp:unix:/var/run/dovecot/lmtp\n*\tsmtp:\n"
	if err := os.WriteFile(filepath.Join(dir, "postfix-transport.cf"), []byte(transportBody), 0o644); err != nil {
		t.Fatal(err)
	}

	loaded := loadEmailServiceConfig(root)
	if loaded.LimitAccept {
		t.Errorf("legacy install with `. OK` sentinel should be detected as unrestricted, got LimitAccept=true")
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
