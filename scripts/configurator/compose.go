package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// emailServerHost reads EMAIL_SERVER from common.env (falling back to the
// example) and returns just the hostname portion (port stripped). Returns
// a placeholder if neither file defines it.
func emailServerHost(root string) string {
	f, err := loadCommonEnv(root)
	if err != nil || f == nil {
		return "mail.agents.example.com"
	}
	v, _ := f.Get("EMAIL_SERVER")
	v = strings.TrimSpace(v)
	if v == "" {
		return "mail.agents.example.com"
	}
	if i := strings.LastIndex(v, ":"); i > 0 {
		v = v[:i]
	}
	return v
}

// composePath is the live docker-compose.yml at the project root. The
// configurator regenerates this file from the list of agents in
// configs/env/<stem>.env whenever an agent is saved.
func composePath(root string) string {
	return filepath.Join(root, "docker-compose.yml")
}

// composeServiceName converts an agent's display name into a key that is
// valid as a docker-compose service identifier: lowercased, non-alphanumeric
// runs collapsed to a single '-', trimmed at the edges. Falls back to the
// file stem (local-part of email) when the result would be empty.
func composeServiceName(name, stem string) string {
	var b strings.Builder
	prevDash := true
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return stem
	}
	return out
}

// regenerateCompose writes docker-compose.yml at the project root with one
// service per agent currently configured in configs/env/. The format mirrors
// docker-compose-example.yml: build, two env_files (common + per-agent),
// RUNNING_IN_CONTAINER=true, restart policy, host.docker.internal extra host,
// and a ./data/<stem> volume mapped to /app/data.
func regenerateCompose(root string) error {
	agents, err := listAgents(root)
	if err != nil {
		return fmt.Errorf("listing agents: %w", err)
	}

	// Deterministic service ordering with collision handling so two agents
	// with names that sanitize to the same key still produce distinct keys.
	type svc struct {
		Key  string
		Stem string
	}
	// Email server host (without port) for the optional host-gateway entry.
	emailHost := emailServerHost(root)

	svcs := make([]svc, 0, len(agents))
	used := map[string]int{}
	for _, a := range agents {
		key := composeServiceName(a.Name, a.Stem)
		if n := used[key]; n > 0 {
			key = fmt.Sprintf("%s-%d", key, n+1)
		}
		used[composeServiceName(a.Name, a.Stem)]++
		svcs = append(svcs, svc{Key: key, Stem: a.Stem})
	}
	sort.Slice(svcs, func(i, j int) bool { return svcs[i].Key < svcs[j].Key })

	var b strings.Builder
	b.WriteString("services:\n")
	for i, s := range svcs {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "  %s:\n", s.Key)
		b.WriteString("    build: .\n")
		b.WriteString("    env_file:\n")
		b.WriteString("      - configs/env/common.env\n")
		fmt.Fprintf(&b, "      - configs/env/%s.env\n", s.Stem)
		b.WriteString("    environment:\n")
		b.WriteString("      - RUNNING_IN_CONTAINER=true\n")
		b.WriteString("    restart: unless-stopped\n")
		b.WriteString("    extra_hosts:\n")
		b.WriteString("      - \"host.docker.internal:host-gateway\"\n")
		fmt.Fprintf(&b, "#      - \"%s:host-gateway\" # optional for localhost email server\n", emailHost)
		b.WriteString("    volumes:\n")
		fmt.Fprintf(&b, "      - ./data/%s:/app/data\n", s.Stem)
	}

	return os.WriteFile(composePath(root), []byte(b.String()), 0o644)
}
