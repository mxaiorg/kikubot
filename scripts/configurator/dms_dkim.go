package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// dmsDKIMSigningConfPath is the rspamd dkim_signing override that DMS's
// `setup config dkim` writes (host side of the /tmp/docker-mailserver mount).
func dmsDKIMSigningConfPath(root string) string {
	return filepath.Join(root, "services", "dms", "config", "rspamd", "override.d", "dkim_signing.conf")
}

// dmsDKIMKeyDir holds the generated key material (rspamd signer layout).
func dmsDKIMKeyDir(root string) string {
	return filepath.Join(root, "services", "dms", "config", "rspamd", "dkim")
}

var (
	// Any `use_esld = <value>;` line, keeping its indentation and trailing
	// comment. Matched line-wise so the rewrite never touches other keys.
	useESLDLineRE  = regexp.MustCompile(`(?m)^(\s*use_esld\s*=\s*)[^;#\n]*;?([^\n]*)$`)
	useESLDFalseRE = regexp.MustCompile(`(?m)^\s*use_esld\s*=\s*false\s*;`)
)

// fixDKIMUseESLD forces `use_esld = false` in the rspamd dkim_signing
// override, if that file exists. Returns true when the file was changed.
//
// Why: DMS's `setup config dkim` writes `use_esld = true`, which makes rspamd
// reduce the From: domain to its effective second-level domain before looking
// it up in the `domain {}` map. Agents here live on a dedicated subdomain
// (agents.acme.com), so the lookup key becomes acme.com, misses the map, and
// rspamd silently skips signing — no error, just LOCAL_OUTBOUND without
// DKIM_SIGNED. Patching it here means saving the Email Service page after
// generating a key is enough; the operator still has to restart the
// container for override.d changes to load.
//
// A missing file is not an error (no key generated yet). A file that already
// says false is left untouched so we don't churn its mtime.
func fixDKIMUseESLD(root string) (bool, error) {
	path := dmsDKIMSigningConfPath(root)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	src := string(b)
	if useESLDFalseRE.MatchString(src) {
		return false, nil
	}
	var out string
	if useESLDLineRE.MatchString(src) {
		// Rewrite the existing line in place. UCL turns a duplicated key into
		// an array rather than last-wins, so appending a second use_esld would
		// leave rspamd with [true, false] — never add, always replace.
		out = useESLDLineRE.ReplaceAllString(src, "${1}false;${2}")
	} else {
		if !strings.HasSuffix(src, "\n") {
			src += "\n"
		}
		out = src + "\n# Added by the kikubot configurator: sign for subdomains (do not reduce From: to the eSLD).\nuse_esld = false;\n"
	}
	info, err := os.Stat(path)
	mode := os.FileMode(0o644)
	if err == nil {
		mode = info.Mode().Perm()
	}
	return true, fsWriteError(path, os.WriteFile(path, []byte(out), mode))
}

// dkimStatus is what the Email Service page shows under Access Security so an
// operator can see, without shelling into the container, whether the DKIM
// pieces that commonly go wrong are in place.
type dkimStatus struct {
	// Domain the status was evaluated for (the agent email domain).
	Domain string
	// KeyExists: a private key for Domain exists under config/rspamd/dkim/.
	KeyExists bool
	// KeyFiles: basenames of the matching private keys (usually one).
	KeyFiles []string
	// ConfExists: override.d/dkim_signing.conf is present.
	ConfExists bool
	// DomainInConf: Domain has a `domain { <Domain> { … } }` block.
	DomainInConf bool
	// UseESLDFalse: the conf explicitly sets use_esld = false (required for a
	// subdomain). False when the setting is true or absent.
	UseESLDFalse bool
}

// Ready reports that everything the configurator can check is in order.
func (s dkimStatus) Ready() bool {
	return s.KeyExists && s.ConfExists && s.DomainInConf && s.UseESLDFalse
}

// Started reports that key generation has been attempted at all; the page
// shows the checklist only in that case and a "how to start" hint otherwise.
func (s dkimStatus) Started() bool {
	return s.KeyExists || s.ConfExists
}

// loadDKIMStatus inspects the on-disk DMS config for domain. Never errors:
// anything unreadable simply reads as "not present".
func loadDKIMStatus(root, domain string) dkimStatus {
	st := dkimStatus{Domain: strings.ToLower(strings.TrimSpace(domain))}
	if st.Domain == "" {
		return st
	}
	if entries, err := os.ReadDir(dmsDKIMKeyDir(root)); err == nil {
		suffix := "-" + st.Domain + ".private.txt"
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if strings.HasSuffix(strings.ToLower(e.Name()), suffix) {
				st.KeyExists = true
				st.KeyFiles = append(st.KeyFiles, e.Name())
			}
		}
	}
	b, err := os.ReadFile(dmsDKIMSigningConfPath(root))
	if err != nil {
		return st
	}
	st.ConfExists = true
	src := string(b)
	domainBlockRE := regexp.MustCompile(`(?mi)^\s*` + regexp.QuoteMeta(st.Domain) + `\s*\{`)
	st.DomainInConf = domainBlockRE.MatchString(src)
	st.UseESLDFalse = useESLDFalseRE.MatchString(src)
	return st
}
