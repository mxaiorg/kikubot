package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// emailServerHost reads common.email_server from agents.yaml and returns
// just the hostname portion (port stripped). Returns a placeholder when
// the roster is empty or missing.
func emailServerHost(root string) string {
	r, err := loadRoster(root)
	if err != nil || r == nil || strings.TrimSpace(r.Common.EmailServer) == "" {
		return "mail.agents.example.com"
	}
	v := strings.TrimSpace(r.Common.EmailServer)
	if i := strings.LastIndex(v, ":"); i > 0 {
		v = v[:i]
	}
	return v
}

// composePath is the live docker-compose.yml at the project root. The
// configurator regenerates this file from the roster whenever an agent is
// saved.
func composePath(root string) string {
	return filepath.Join(root, "docker-compose.yml")
}

// composeServiceName converts an agent's display name into a valid
// docker-compose service identifier: lowercased, non-alphanumeric runs
// collapsed to '-', trimmed at the edges. Falls back to the file stem
// (local-part of email) when the result is empty.
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

// regenerateCompose writes docker-compose.yml with one service per agent
// currently in configs/agents.yaml. Each service shares the same
// configs/secrets.env env_file (so credentials are loaded once) and is
// distinguished by an AGENT_EMAIL environment variable — that selector lets
// the container pick the right entry from agents.yaml at startup.
func regenerateCompose(root string) error {
	agents, err := listAgents(root)
	if err != nil {
		return fmt.Errorf("listing agents: %w", err)
	}

	type svc struct {
		Key   string
		Stem  string
		Email string
	}
	emailHost := emailServerHost(root)
	svcs := make([]svc, 0, len(agents))
	used := map[string]int{}
	for _, a := range agents {
		key := composeServiceName(a.Name, a.Stem)
		if n := used[key]; n > 0 {
			key = fmt.Sprintf("%s-%d", key, n+1)
		}
		used[composeServiceName(a.Name, a.Stem)]++
		svcs = append(svcs, svc{Key: key, Stem: a.Stem, Email: a.Email})
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
		b.WriteString("      - configs/secrets.env\n")
		b.WriteString("    environment:\n")
		b.WriteString("      - RUNNING_IN_CONTAINER=true\n")
		fmt.Fprintf(&b, "      - AGENT_EMAIL=%s\n", s.Email)
		b.WriteString("    restart: unless-stopped\n")
		b.WriteString("    extra_hosts:\n")
		b.WriteString("      - \"host.docker.internal:host-gateway\"\n")
		fmt.Fprintf(&b, "#      - \"%s:host-gateway\" # optional for localhost email server\n", emailHost)
		b.WriteString("    volumes:\n")
		fmt.Fprintf(&b, "      - ./data/%s:/app/data\n", s.Stem)
	}

	return os.WriteFile(composePath(root), []byte(b.String()), 0o644)
}
