package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// emailServiceConfig captures the user-facing settings for the bundled
// docker-mailserver. It maps onto two postfix files: a transport map and a
// sender-access map.
type emailServiceConfig struct {
	Enabled         bool
	Hostname        string
	AgentDomain     string
	LimitDelivery   bool
	DeliveryDomains []string // domains agents may send to (besides the agent domain)
	LimitAccept     bool
	AcceptDomains   []string // domains/emails agents may receive from (besides the agent domain)
}

// dmsConfigDir returns the abs path to services/dms/config under root.
func dmsConfigDir(root string) string {
	return filepath.Join(root, "services", "dms", "config")
}

// loadEmailServiceConfig best-effort reads the current postfix-transport.cf,
// postfix-sender-access.cf, and postfix-main.cf to populate a config. If none
// exist, returns a zero-value struct (Enabled=false).
//
// postfix-main.cf is the authoritative source for LimitAccept because that's
// the file that actually controls whether Postfix rejects unmatched senders
// (via smtpd_sender_restrictions). The sender-access list is consulted only
// for the AcceptDomains values themselves. For legacy installs that predate
// the main.cf override, we fall back to detecting a `.  OK` sentinel in
// sender-access.
func loadEmailServiceConfig(root string) *emailServiceConfig {
	c := &emailServiceConfig{}
	dir := dmsConfigDir(root)
	tPath := filepath.Join(dir, "postfix-transport.cf")
	sPath := filepath.Join(dir, "postfix-sender-access.cf")
	mPath := filepath.Join(dir, "postfix-main.cf")

	if data, err := os.ReadFile(tPath); err == nil {
		c.Enabled = true
		c.parseTransport(string(data))
	}
	// Read main.cf first so parseSenderAccess can know whether LimitAccept is
	// authoritatively set. If main.cf is absent (legacy install), fall back
	// to the `.  OK` sentinel heuristic in parseSenderAccess.
	mainDecided := false
	if data, err := os.ReadFile(mPath); err == nil {
		c.Enabled = true
		mainDecided = c.parseMain(string(data))
	}
	if data, err := os.ReadFile(sPath); err == nil {
		c.Enabled = true
		c.parseSenderAccess(string(data), mainDecided)
	}
	return c
}

// parseMain extracts LimitAccept from a postfix-main.cf override. Returns
// true if the file contained an `smtpd_sender_restrictions` directive
// (authoritative); false means caller should fall back to other heuristics.
func (c *emailServiceConfig) parseMain(s string) bool {
	decided := false
	for _, raw := range strings.Split(s, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// `key = value` — split on the first `=`.
		eq := strings.Index(line, "=")
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if key != "smtpd_sender_restrictions" {
			continue
		}
		decided = true
		// If the directive references check_sender_access AND ends with a
		// reject action, unmatched senders are blocked — that's the
		// restricted mode.
		c.LimitAccept = strings.Contains(val, "check_sender_access") &&
			strings.Contains(val, "reject")
	}
	return decided
}

func (c *emailServiceConfig) parseTransport(s string) {
	c.LimitDelivery = false
	c.DeliveryDomains = nil
	hasCatchAll := false
	starSmtp := false
	for _, raw := range strings.Split(s, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		key := fields[0]
		dest := fields[1]
		switch {
		case strings.HasPrefix(dest, "lmtp:"):
			c.AgentDomain = key
		case key == "*" && strings.HasPrefix(dest, "error:"):
			hasCatchAll = true
		case key == "*" && strings.HasPrefix(dest, "smtp:"):
			starSmtp = true
		case strings.HasPrefix(dest, "smtp:"):
			c.DeliveryDomains = append(c.DeliveryDomains, key)
		}
	}
	c.LimitDelivery = hasCatchAll && !starSmtp
	if !c.LimitDelivery {
		c.DeliveryDomains = nil
	}
}

// parseSenderAccess populates AcceptDomains and AgentDomain from the
// sender-access file. If mainDecided is true, LimitAccept has already been
// set authoritatively from postfix-main.cf and won't be touched here.
// Otherwise we fall back to the legacy `.  OK` sentinel: presence means the
// file was generated in unrestricted mode.
//
// Note: `.  OK` is NOT a functional Postfix wildcard — Postfix access maps
// don't treat `.` as a catch-all. It existed only as a marker that the prior
// configurator used to decide it shouldn't have emitted a domain list. The
// actual permit/reject behaviour is controlled by smtpd_sender_restrictions
// in postfix-main.cf.
func (c *emailServiceConfig) parseSenderAccess(s string, mainDecided bool) {
	c.AcceptDomains = nil
	hasDot := false
	for _, raw := range strings.Split(s, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		key := fields[0]
		val := fields[1]
		if val != "OK" {
			continue
		}
		if key == "." {
			hasDot = true
			continue
		}
		if c.AgentDomain == "" {
			c.AgentDomain = key
			continue
		}
		if key == c.AgentDomain {
			continue
		}
		c.AcceptDomains = append(c.AcceptDomains, key)
	}
	if !mainDecided {
		c.LimitAccept = !hasDot
	}
	if !c.LimitAccept {
		c.AcceptDomains = nil
	}
}

// SaveEmailService rewrites the two postfix files based on c. The
// `*-example.cf` siblings are used as scaffolding only when generating
// fresh — we generate canonical contents directly, which is simpler than
// patching the example file.
func (c *emailServiceConfig) Save(root string) error {
	if c.AgentDomain == "" {
		return fmt.Errorf("agent email domain is required")
	}
	dir := dmsConfigDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	files := []struct {
		name, content string
	}{
		{"postfix-transport.cf", c.renderTransport()},
		{"postfix-sender-access.cf", c.renderSenderAccess()},
		{"postfix-main.cf", c.renderMainCf()},
	}
	for _, f := range files {
		p := filepath.Join(dir, f.name)
		if err := os.WriteFile(p, []byte(f.content), 0o644); err != nil {
			return fsWriteError(p, err)
		}
	}
	return nil
}

func (c *emailServiceConfig) renderTransport() string {
	var b strings.Builder
	b.WriteString("# RESTRICT SENDING TO EXTERNAL DOMAINS\n")
	b.WriteString("# Generated by Agent Configurator. Edit via the dashboard or by hand.\n\n")
	b.WriteString("# Own domain - deliver locally\n")
	fmt.Fprintf(&b, "%s    lmtp:unix:/var/run/dovecot/lmtp\n\n", c.AgentDomain)

	if c.LimitDelivery {
		b.WriteString("# Allowed external domains\n")
		for _, d := range c.DeliveryDomains {
			d = strings.TrimSpace(d)
			if d == "" || strings.EqualFold(d, c.AgentDomain) {
				// Agent's own domain is already routed via lmtp above —
				// repeating it as an smtp destination would be a no-op.
				continue
			}
			fmt.Fprintf(&b, "%s     smtp:\n", d)
		}
		b.WriteString("\n# Reject everything else\n")
		b.WriteString("*              error:5.7.1 Sending to this domain is not permitted\n")
	} else {
		b.WriteString("# Allowed external domains\n")
		b.WriteString("*\tsmtp:\n")
	}
	return b.String()
}

func (c *emailServiceConfig) renderSenderAccess() string {
	var b strings.Builder
	b.WriteString("# RESTRICT RECEIVING\n")
	b.WriteString("# Generated by Agent Configurator. Edit via the dashboard or by hand.\n\n")
	b.WriteString("# Agent domain - always allowed\n")
	fmt.Fprintf(&b, "%s    OK\n", c.AgentDomain)

	if c.LimitAccept {
		b.WriteString("\n# Allowed external sender domains\n")
		for _, d := range c.AcceptDomains {
			d = strings.TrimSpace(d)
			if d == "" || strings.EqualFold(d, c.AgentDomain) {
				// Agent's own domain is already accepted above.
				continue
			}
			fmt.Fprintf(&b, "%s          OK\n", d)
		}
	} else {
		// In unrestricted mode the sender-access list is unused: postfix-main.cf
		// omits `check_sender_access` entirely. We still keep the agent domain
		// entry above so the file remains a valid texthash if anything else
		// references it, and so legacy loaders that pre-date postfix-main.cf
		// can still recover the agent domain.
		b.WriteString("\n# Unrestricted mode: smtpd_sender_restrictions is disabled in\n")
		b.WriteString("# postfix-main.cf, so this list is not consulted for accept/reject.\n")
	}
	return b.String()
}

// renderMainCf builds the postfix-main.cf override that docker-mailserver
// applies on top of its baseline postfix configuration. Two directives matter:
//
//   - transport_maps: always points at our postfix-transport.cf. The actual
//     restrict-delivery vs. allow-all-outbound decision is baked into that
//     file (LimitDelivery toggles the catch-all entry).
//   - smtpd_sender_restrictions: this is the lever for LimitAccept. When the
//     user has chosen to accept mail from anyone (LimitAccept=false), we set
//     it to `permit` so unmatched senders are not rejected. When the user has
//     listed allowed sender domains (LimitAccept=true), we restore the
//     `check_sender_access … , reject` chain so anything outside the list is
//     refused at SMTP time.
//
// Writing `permit` explicitly (rather than omitting the directive) ensures
// that toggling from restricted → unrestricted actually clears the prior
// rule on the next DMS restart; docker-mailserver regenerates main.cf from
// defaults plus this override at startup, so any directive present here
// wins.
func (c *emailServiceConfig) renderMainCf() string {
	var b strings.Builder
	b.WriteString("# Postfix main.cf overrides\n")
	b.WriteString("# Generated by Agent Configurator. Edit via the dashboard or by hand.\n")
	b.WriteString("# docker-mailserver applies each `key = value` line via `postconf -e` at startup.\n\n")

	b.WriteString("# Route mail through the transport map for selective delivery control.\n")
	b.WriteString("transport_maps = texthash:/tmp/docker-mailserver/postfix-transport.cf\n\n")

	if c.LimitAccept {
		b.WriteString("# Restrict RECEIVING to senders listed in postfix-sender-access.cf.\n")
		b.WriteString("smtpd_sender_restrictions = check_sender_access texthash:/tmp/docker-mailserver/postfix-sender-access.cf, reject\n")
	} else {
		b.WriteString("# Accept mail from any sender (no whitelist/blacklist configured).\n")
		b.WriteString("smtpd_sender_restrictions = permit\n")
	}
	return b.String()
}

// splitCSV splits on commas, trims whitespace, drops empties.
func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// joinCSV is the inverse for display in form fields.
func joinCSV(ss []string) string {
	return strings.Join(ss, ", ")
}

// ensurePort returns addr unchanged if it already contains a port (any colon
// counts — the realistic inputs are `host` and `host:port`); otherwise
// appends ":<defaultPort>".
//
// Defends against operators saving EMAIL_SERVER/SMTP_SERVER as a bare
// hostname — the IMAP dialer requires host:port and emits "missing port in
// address" otherwise.
func ensurePort(addr, defaultPort string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return addr
	}
	if strings.Contains(addr, ":") {
		return addr
	}
	return addr + ":" + defaultPort
}

// hostnameDomain returns the parent domain of a hostname (everything past
// the first dot), or "" for bare hostnames.
func hostnameDomain(hostname string) string {
	hostname = strings.TrimSpace(hostname)
	if hostname == "" {
		return ""
	}
	if i := strings.Index(hostname, "."); i > 0 && i < len(hostname)-1 {
		return hostname[i+1:]
	}
	return ""
}
