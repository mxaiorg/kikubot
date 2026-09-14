package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

// composeService is one generated docker-compose service: its service key
// (the YAML map key / `docker compose` target), the agent's email stem (used
// for the data volume and knowledge dir), and the agent email.
type composeService struct {
	Key   string
	Stem  string
	Email string
}

// composeServices derives the docker-compose service list from the current
// roster, applying the same keying (and duplicate-suffixing) as the generated
// docker-compose.yml. Shared by regenerateCompose and the knowledge-reload
// signaller so service names stay in lockstep.
func composeServices(root string) ([]composeService, error) {
	agents, err := listAgents(root)
	if err != nil {
		return nil, fmt.Errorf("listing agents: %w", err)
	}
	svcs := make([]composeService, 0, len(agents))
	used := map[string]int{}
	for _, a := range agents {
		key := composeServiceName(a.Name, a.Stem)
		if n := used[key]; n > 0 {
			key = fmt.Sprintf("%s-%d", key, n+1)
		}
		used[composeServiceName(a.Name, a.Stem)]++
		svcs = append(svcs, composeService{Key: key, Stem: a.Stem, Email: a.Email})
	}
	sort.Slice(svcs, func(i, j int) bool { return svcs[i].Key < svcs[j].Key })
	return svcs, nil
}

// dockerNetworkName matches Docker's rule for network names.
var dockerNetworkName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// composeNetworks returns common.docker_networks from agents.yaml: extra
// pre-existing Docker networks every agent joins (e.g. "web-agent", where the
// pwmcp-<site> Playwright MCP containers live). Blank and duplicate entries and
// "default" are dropped; an invalid name is an error rather than being emitted
// into the YAML.
func composeNetworks(root string) ([]string, error) {
	r, err := loadRoster(root)
	if err != nil {
		return nil, err
	}
	var out []string
	seen := map[string]bool{"default": true}
	for _, n := range r.Common.DockerNetworks {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] {
			continue
		}
		if !dockerNetworkName.MatchString(n) {
			return nil, fmt.Errorf("common.docker_networks: invalid network name %q", n)
		}
		seen[n] = true
		out = append(out, n)
	}
	return out, nil
}

// regenerateCompose writes docker-compose.yml with one service per agent
// currently in configs/agents.yaml. Each service shares the same
// configs/secrets.env env_file (so credentials are loaded once) and is
// distinguished by an AGENT_EMAIL environment variable — that selector lets
// the container pick the right entry from agents.yaml at startup.
func regenerateCompose(root string) error {
	svcs, err := composeServices(root)
	if err != nil {
		return err
	}
	// Keep a (possibly empty) catalog on disk. Compose files generated before
	// configs/ was mounted as a directory bind-mount the file itself, and a
	// missing host side would make Docker create a directory in its place.
	if err := ensureMCPServersFile(root); err != nil {
		return err
	}
	emailHost := emailServerHost(root)
	networks, err := composeNetworks(root)
	if err != nil {
		return err
	}

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
		// Point the agent at the configs/ directory mount below.
		b.WriteString("      - AGENTS_CONFIG=/app/configs/agents.yaml\n")
		b.WriteString("      - MCP_SERVERS_CONFIG=/app/configs/mcp_servers.yaml\n")
		b.WriteString("    restart: unless-stopped\n")
		b.WriteString("    extra_hosts:\n")
		b.WriteString("      - \"host.docker.internal:host-gateway\"\n")
		fmt.Fprintf(&b, "#      - \"%s:host-gateway\" # optional for localhost email server\n", emailHost)
		b.WriteString("    volumes:\n")
		fmt.Fprintf(&b, "      - ./data/%s:/app/data\n", s.Stem)
		// Live-mount the knowledge base so configurator edits are visible
		// without rebuilding the image; the agent hot-reloads on change
		// (poll + SIGHUP).
		b.WriteString("      - ./configs/knowledge:/app/knowledge:ro\n")
		// Live-mount the roster and the remote-MCP catalog too: the agent
		// hot-reloads its tool set when either file changes (poll + SIGHUP),
		// so assigning a tool or editing an MCP server needs no rebuild.
		// Other agents.yaml fields (model, prompt, ACL) are still read only
		// at startup. Mount the configs/ directory rather than the two files:
		// a single-file bind mount pins the inode it started with, so a host
		// file that gets replaced (write-temp-then-rename, restore, cp over
		// a deleted file) is never seen by the running container.
		b.WriteString("      - ./configs:/app/configs:ro\n")
		// Join the extra networks as well as the project default, so the agent
		// reaches sidecar MCP containers by name (http://pwmcp-<site>:<port>/mcp)
		// without their ports being published on the host.
		if len(networks) > 0 {
			b.WriteString("    networks:\n")
			b.WriteString("      - default\n")
			for _, n := range networks {
				fmt.Fprintf(&b, "      - %s\n", n)
			}
		}
	}
	if len(networks) > 0 {
		// External: compose attaches to the existing network and never creates
		// or removes it (web_agent's mcp.sh owns web-agent).
		b.WriteString("\nnetworks:\n")
		for _, n := range networks {
			fmt.Fprintf(&b, "  %s:\n    external: true\n", n)
		}
	}

	return fsWriteError(composePath(root), os.WriteFile(composePath(root), []byte(b.String()), 0o644))
}
