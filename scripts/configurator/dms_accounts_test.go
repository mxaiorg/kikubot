package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Reference output cross-checked against `openssl passwd -6 -salt saltstring`,
// which matches the GNU libc / Dovecot SHA-512 crypt implementation.
func TestSha512Crypt_ReferenceVector(t *testing.T) {
	got := sha512Crypt("Hello world!", "saltstring")
	want := "{SHA512-CRYPT}$6$saltstring$svn8UoSVapNtMuq1ukKS4tPQd8iKwSMHWjl/O817G3uBnIFNjnQJuesI68u4OTLiBFdcbYEdFCoEOfaS35inz1"
	if got != want {
		t.Errorf("sha512Crypt mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestSha512Crypt_VerifyRoundTrip(t *testing.T) {
	salt, err := newCryptSalt()
	if err != nil {
		t.Fatal(err)
	}
	hash := sha512Crypt("hunter2", salt)
	if !verifySha512Crypt("hunter2", hash) {
		t.Errorf("verify failed for matching password")
	}
	if verifySha512Crypt("hunter3", hash) {
		t.Errorf("verify succeeded for non-matching password")
	}
}

// Hash produced by `openssl passwd -6` (same algorithm Dovecot's SHA512-CRYPT
// scheme uses). Verifying an externally-generated hash confirms our verify
// path doesn't just round-trip its own output.
func TestSha512Crypt_VerifyExternalHash(t *testing.T) {
	const stored = "$6$abcd1234ABCD5678$2pjxAHzvaR5fOog8gsuTI4e2tEM407dtLI1maaNsNURFrpdMbD4X.zKmkC4uEMIWVdGJsZiSlzgjNfhDtedce1"
	if !verifySha512Crypt("correcthorsebatterystaple", stored) {
		t.Errorf("verify failed against openssl-produced hash")
	}
	if verifySha512Crypt("wrong", stored) {
		t.Errorf("verify accepted wrong password")
	}
	// Scheme-prefixed form should also be accepted (this is how we store it).
	if !verifySha512Crypt("correcthorsebatterystaple", cryptScheme+stored) {
		t.Errorf("verify failed against scheme-prefixed hash")
	}
}

// Accounts added manually inside the dms container (e.g. via `setup email
// add`) must survive a configurator save. Only roster-managed entries get
// rewritten; everything else in postfix-accounts.cf is preserved verbatim.
func TestRegenerateDmsAccounts_PreservesManualEntries(t *testing.T) {
	root := t.TempDir()

	// Mark the bundled email service as enabled by dropping a transport file.
	dmsDir := filepath.Join(root, "services", "dms", "config")
	if err := os.MkdirAll(dmsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dmsDir, "postfix-transport.cf"), []byte("# stub\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Roster with one agent.
	if err := os.MkdirAll(filepath.Join(root, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	rosterYAML := "common: {}\nagents:\n  - name: Kiku\n    email: kiku@agents.example.com\n"
	if err := os.WriteFile(filepath.Join(root, "configs", "agents.yaml"), []byte(rosterYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setAgentPassword(root, "kiku@agents.example.com", "rosterpass"); err != nil {
		t.Fatal(err)
	}

	// Pre-existing postfix-accounts.cf: one roster mailbox, one manual mailbox.
	manualHash := sha512Crypt("manualpass", "manualsalt000000")
	rosterHash := sha512Crypt("rosterpass", "rostersalt000000")
	pre := "kiku@agents.example.com|" + rosterHash + "\n" +
		"shared@agents.example.com|" + manualHash + "\n"
	if err := os.WriteFile(dmsAccountsPath(root), []byte(pre), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := regenerateDmsAccounts(root); err != nil {
		t.Fatalf("regenerateDmsAccounts: %v", err)
	}

	got := parseDmsAccounts(dmsAccountsPath(root))
	if h, ok := got["kiku@agents.example.com"]; !ok {
		t.Errorf("roster account missing after regenerate")
	} else if h != rosterHash {
		t.Errorf("roster account hash was rotated unnecessarily")
	}
	if h, ok := got["shared@agents.example.com"]; !ok {
		t.Errorf("manually-added account was erased by regenerate")
	} else if h != manualHash {
		t.Errorf("manual account hash changed: got %q want %q", h, manualHash)
	}
}
