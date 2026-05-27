package main

import "testing"

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
