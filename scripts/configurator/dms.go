package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// dmsComposePath returns the live docker-compose.yml path under services/dms/.
func dmsComposePath(root string) string {
	return filepath.Join(root, "services", "dms", "docker-compose.yml")
}

// dmsComposeExamplePath is the example sidecar shipped with the repo.
func dmsComposeExamplePath(root string) string {
	return filepath.Join(root, "services", "dms", "docker-compose-example.yml")
}

// dmsCertsDir is where fullchain.pem / privkey.pem live.
func dmsCertsDir(root string) string {
	return filepath.Join(root, "services", "dms", "certs")
}

// dmsHostname extracts the `hostname:` value from the live docker-compose.yml.
// Falls back to the example file. Returns "" if neither exists or no value
// is set.
func dmsHostname(root string) string {
	for _, p := range []string{dmsComposePath(root), dmsComposeExamplePath(root)} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if v := extractYAMLScalar(string(b), "hostname"); v != "" {
			return v
		}
	}
	return ""
}

var (
	hostnameRE   = regexp.MustCompile(`(?m)^(\s*)hostname:\s*(.*)$`)
	domainnameRE = regexp.MustCompile(`(?m)^(\s*)domainname:\s*(.*)$`)
)

// extractYAMLScalar finds the first `<key>:` line in s and returns its
// trimmed scalar value (no comment-stripping; assumes simple values).
func extractYAMLScalar(s, key string) string {
	re := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(key) + `:\s*(.+?)\s*$`)
	m := re.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	v := strings.TrimSpace(m[1])
	v = strings.Trim(v, `"'`)
	return v
}

// updateDmsCompose sets hostname and domainname in the live compose file. If
// the live file is missing, it copies the example first. If even the example
// is missing, returns an error.
func updateDmsCompose(root, hostname, domainname string) error {
	live := dmsComposePath(root)
	if _, err := os.Stat(live); errors.Is(err, fs.ErrNotExist) {
		example := dmsComposeExamplePath(root)
		b, err := os.ReadFile(example)
		if err != nil {
			return fmt.Errorf("neither %s nor %s exists: %w", live, example, err)
		}
		if err := os.WriteFile(live, b, 0o644); err != nil {
			return fmt.Errorf("seeding %s from example: %w", live, err)
		}
	}
	b, err := os.ReadFile(live)
	if err != nil {
		return err
	}
	src := string(b)
	if hostname != "" {
		src = hostnameRE.ReplaceAllString(src, "${1}hostname: "+hostname)
	}
	if domainname != "" {
		src = domainnameRE.ReplaceAllString(src, "${1}domainname: "+domainname)
	}
	return os.WriteFile(live, []byte(src), 0o644)
}

// sslCertStatus reports whether the two cert files exist in services/dms/certs/.
type sslCertStatus struct {
	FullchainExists bool
	PrivkeyExists   bool
}

func (s sslCertStatus) BothPresent() bool {
	return s.FullchainExists && s.PrivkeyExists
}

func loadSSLCertStatus(root string) sslCertStatus {
	dir := dmsCertsDir(root)
	return sslCertStatus{
		FullchainExists: fileNonEmpty(filepath.Join(dir, "fullchain.pem")),
		PrivkeyExists:   fileNonEmpty(filepath.Join(dir, "privkey.pem")),
	}
}

func fileNonEmpty(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Size() > 0
}

// generateSelfSignedCert writes a 10-year self-signed RSA-4096 certificate to
// services/dms/certs/{fullchain.pem,privkey.pem}. The certificate's CN is the
// hostname; SANs include the hostname and (if different) the agent domain.
// Implemented in-process via crypto/x509 to avoid an openssl dependency —
// works identically on Linux, macOS, and Windows.
func generateSelfSignedCert(root, hostname, domain string) error {
	hostname = strings.TrimSpace(hostname)
	if hostname == "" {
		return fmt.Errorf("hostname is required to generate a certificate")
	}
	domain = strings.TrimSpace(domain)

	key, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return fmt.Errorf("generating private key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("generating serial: %w", err)
	}

	var dnsNames []string
	var ipAddrs []net.IP
	addSAN := func(s string) {
		if s == "" {
			return
		}
		if ip := net.ParseIP(s); ip != nil {
			ipAddrs = append(ipAddrs, ip)
			return
		}
		for _, existing := range dnsNames {
			if existing == s {
				return
			}
		}
		dnsNames = append(dnsNames, s)
	}
	addSAN(hostname)
	addSAN(domain)

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: hostname},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           ipAddrs,
		SignatureAlgorithm:    x509.SHA256WithRSA,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("creating certificate: %w", err)
	}

	dir := dmsCertsDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	if err := writePEM(filepath.Join(dir, "fullchain.pem"), 0o644, &pem.Block{
		Type: "CERTIFICATE", Bytes: der,
	}); err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshalling private key: %w", err)
	}
	if err := writePEM(filepath.Join(dir, "privkey.pem"), 0o600, &pem.Block{
		Type: "PRIVATE KEY", Bytes: keyDER,
	}); err != nil {
		return err
	}
	return nil
}

func writePEM(path string, mode os.FileMode, block *pem.Block) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	if err := pem.Encode(f, block); err != nil {
		f.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return f.Close()
}
