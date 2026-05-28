package main

import (
	"crypto/rand"
	"crypto/sha512"
	"crypto/subtle"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// services/dms/config/postfix-accounts.cf is the file docker-mailserver reads
// at startup for mailbox identities. Each line is
//
//	<email>|{SHA512-CRYPT}$6$<salt>$<hash>
//
// Normally maintained by running `setup email add` inside the dms container.
// We generate it directly from the roster and configs/secrets.env so the
// operator never has to exec into the container.

const (
	cryptAlphabet      = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	cryptScheme        = "{SHA512-CRYPT}"
	cryptDefaultRounds = 5000
	cryptMinRounds     = 1000
	cryptMaxRounds     = 999_999_999
	cryptSaltLen       = 16
)

// dmsAccountsPath returns the absolute path to postfix-accounts.cf.
func dmsAccountsPath(root string) string {
	return filepath.Join(dmsConfigDir(root), "postfix-accounts.cf")
}

// regenerateDmsAccounts rewrites postfix-accounts.cf from the roster and
// configs/secrets.env. For each agent it keeps the existing hash when the
// stored hash still verifies against the current password (avoids salt churn
// across saves), and generates a fresh SHA-512 crypt hash otherwise. Entries
// already present in postfix-accounts.cf for non-roster emails are preserved
// verbatim — they represent mailboxes provisioned manually (e.g. via
// `setup email add`) that the operator owns outside the roster.
//
// Skipped when the bundled email service isn't in use (services/dms/config/
// missing, or the postfix transport/sender-access files aren't present).
func regenerateDmsAccounts(root string) error {
	dir := dmsConfigDir(root)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if !loadEmailServiceConfig(root).Enabled {
		return nil
	}
	r, err := loadRoster(root)
	if err != nil {
		return err
	}
	existing := parseDmsAccounts(dmsAccountsPath(root))

	type acct struct{ email, hash string }
	accts := make([]acct, 0, len(r.Agents))
	for _, a := range r.Agents {
		email := strings.TrimSpace(a.Email)
		if email == "" {
			continue
		}
		pw := getAgentPassword(root, email)
		if pw == "" {
			continue
		}
		key := strings.ToLower(email)
		if h, ok := existing[key]; ok && verifySha512Crypt(pw, h) {
			accts = append(accts, acct{email, h})
			continue
		}
		salt, err := newCryptSalt()
		if err != nil {
			return fmt.Errorf("generating salt: %w", err)
		}
		accts = append(accts, acct{email, sha512Crypt(pw, salt)})
	}
	known := make(map[string]bool, len(accts))
	for _, a := range accts {
		known[strings.ToLower(a.email)] = true
	}
	for email, h := range existing {
		if !known[email] {
			accts = append(accts, acct{email, h})
		}
	}
	sort.Slice(accts, func(i, j int) bool {
		return strings.ToLower(accts[i].email) < strings.ToLower(accts[j].email)
	})

	var sb strings.Builder
	for _, a := range accts {
		sb.WriteString(a.email)
		sb.WriteByte('|')
		sb.WriteString(a.hash)
		sb.WriteByte('\n')
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(dmsAccountsPath(root), []byte(sb.String()), 0o600)
}

// parseDmsAccounts returns a lowercased-email → hash map. Missing file ⇒
// empty map (no error).
func parseDmsAccounts(path string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.Index(line, "|")
		if i < 0 {
			continue
		}
		email := strings.TrimSpace(line[:i])
		hash := strings.TrimSpace(line[i+1:])
		if email != "" && hash != "" {
			out[strings.ToLower(email)] = hash
		}
	}
	return out
}

// newCryptSalt returns 16 random characters from the crypt(3) alphabet.
func newCryptSalt() (string, error) {
	b := make([]byte, cryptSaltLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, cryptSaltLen)
	for i, x := range b {
		out[i] = cryptAlphabet[int(x)&0x3f]
	}
	return string(out), nil
}

// sha512Crypt returns "{SHA512-CRYPT}$6$<salt>$<hash>" for the given password
// and salt, using the default 5000 rounds — the same output `doveadm pw -s
// SHA512-CRYPT` and `mkpasswd -m sha-512-crypt` produce.
func sha512Crypt(password, salt string) string {
	return sha512CryptRounds(password, salt, cryptDefaultRounds, false)
}

// sha512CryptRounds is the Drepper SHA-512 crypt(3) algorithm. See
// https://www.akkadia.org/drepper/SHA-crypt.txt — comments below cite the
// numbered steps from that spec.
func sha512CryptRounds(password, salt string, rounds int, roundsExplicit bool) string {
	if rounds < cryptMinRounds {
		rounds = cryptMinRounds
	}
	if rounds > cryptMaxRounds {
		rounds = cryptMaxRounds
	}
	if len(salt) > cryptSaltLen {
		salt = salt[:cryptSaltLen]
	}
	pw := []byte(password)
	st := []byte(salt)

	// Steps 4–8: digest B = SHA512(pw || st || pw)
	bh := sha512.New()
	bh.Write(pw)
	bh.Write(st)
	bh.Write(pw)
	b := bh.Sum(nil)

	// Steps 1–3, 9–12: digest A
	ah := sha512.New()
	ah.Write(pw)
	ah.Write(st)
	for n := len(pw); n > 0; {
		if n >= sha512.Size {
			ah.Write(b)
			n -= sha512.Size
		} else {
			ah.Write(b[:n])
			n = 0
		}
	}
	for i := len(pw); i > 0; i >>= 1 {
		if i&1 == 1 {
			ah.Write(b)
		} else {
			ah.Write(pw)
		}
	}
	a := ah.Sum(nil)

	// Steps 13–16: byte sequence P
	dph := sha512.New()
	for i := 0; i < len(pw); i++ {
		dph.Write(pw)
	}
	dp := dph.Sum(nil)
	p := make([]byte, 0, len(pw))
	for n := len(pw); n > 0; {
		if n >= sha512.Size {
			p = append(p, dp...)
			n -= sha512.Size
		} else {
			p = append(p, dp[:n]...)
			n = 0
		}
	}

	// Steps 17–20: byte sequence S
	dsh := sha512.New()
	for i := 0; i < 16+int(a[0]); i++ {
		dsh.Write(st)
	}
	ds := dsh.Sum(nil)
	s := make([]byte, 0, len(st))
	for n := len(st); n > 0; {
		if n >= sha512.Size {
			s = append(s, ds...)
			n -= sha512.Size
		} else {
			s = append(s, ds[:n]...)
			n = 0
		}
	}

	// Step 21: rounds of mixing
	c := a
	for i := 0; i < rounds; i++ {
		ch := sha512.New()
		if i&1 == 1 {
			ch.Write(p)
		} else {
			ch.Write(c)
		}
		if i%3 != 0 {
			ch.Write(s)
		}
		if i%7 != 0 {
			ch.Write(p)
		}
		if i&1 == 1 {
			ch.Write(c)
		} else {
			ch.Write(p)
		}
		c = ch.Sum(nil)
	}

	// Step 22: encode under the crypt(3) base64 permutation.
	var head strings.Builder
	head.WriteString(cryptScheme)
	head.WriteString("$6$")
	if roundsExplicit {
		fmt.Fprintf(&head, "rounds=%d$", rounds)
	}
	head.WriteString(salt)
	head.WriteByte('$')
	head.WriteString(encodeCrypt(c))
	return head.String()
}

// sha512CryptPerm is the byte-permutation table from the spec — 21 groups of
// three indices that get packed into 24-bit values and base64-emitted in
// little-endian order, plus a trailing two-char emit for byte 63.
var sha512CryptPerm = [21][3]int{
	{0, 21, 42}, {22, 43, 1}, {44, 2, 23}, {3, 24, 45}, {25, 46, 4},
	{47, 5, 26}, {6, 27, 48}, {28, 49, 7}, {50, 8, 29}, {9, 30, 51},
	{31, 52, 10}, {53, 11, 32}, {12, 33, 54}, {34, 55, 13}, {56, 14, 35},
	{15, 36, 57}, {37, 58, 16}, {59, 17, 38}, {18, 39, 60}, {40, 61, 19},
	{62, 20, 41},
}

func encodeCrypt(c []byte) string {
	var sb strings.Builder
	sb.Grow(86)
	emit := func(b2, b1, b0 byte, n int) {
		w := uint(b2)<<16 | uint(b1)<<8 | uint(b0)
		for j := 0; j < n; j++ {
			sb.WriteByte(cryptAlphabet[w&0x3f])
			w >>= 6
		}
	}
	for _, p := range sha512CryptPerm {
		emit(c[p[0]], c[p[1]], c[p[2]], 4)
	}
	emit(0, 0, c[63], 2)
	return sb.String()
}

// verifySha512Crypt reports whether password produces hashLine. hashLine may
// be either "{SHA512-CRYPT}$6$..." or "$6$..." form.
func verifySha512Crypt(password, hashLine string) bool {
	norm := strings.TrimPrefix(hashLine, cryptScheme)
	if !strings.HasPrefix(norm, "$6$") {
		return false
	}
	body := norm[3:]
	rounds := cryptDefaultRounds
	roundsExplicit := false
	if strings.HasPrefix(body, "rounds=") {
		end := strings.Index(body, "$")
		if end < 0 {
			return false
		}
		n, err := strconv.Atoi(body[len("rounds="):end])
		if err != nil || n < 1 {
			return false
		}
		rounds = n
		roundsExplicit = true
		body = body[end+1:]
	}
	sep := strings.Index(body, "$")
	if sep < 0 {
		return false
	}
	salt := body[:sep]
	got := sha512CryptRounds(password, salt, rounds, roundsExplicit)
	want := cryptScheme + norm
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
