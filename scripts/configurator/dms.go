package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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
