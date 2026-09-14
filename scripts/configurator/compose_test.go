package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func writeTestRoster(t *testing.T, commonExtra string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	roster := "common:\n  email_server: mail.example.com:993\n" + commonExtra +
		"agents:\n  - name: Kiku\n    email: alpha@example.com\n"
	if err := os.WriteFile(rosterPath(root), []byte(roster), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func readCompose(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(composePath(root))
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := yaml.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("generated compose is not valid YAML: %v\n%s", err, b)
	}
	return string(b)
}

func TestRegenerateComposeMountsConfigsDir(t *testing.T) {
	root := writeTestRoster(t, "")
	if err := regenerateCompose(root); err != nil {
		t.Fatal(err)
	}
	out := readCompose(t, root)
	for _, want := range []string{
		"      - AGENTS_CONFIG=/app/configs/agents.yaml\n",
		"      - MCP_SERVERS_CONFIG=/app/configs/mcp_servers.yaml\n",
		"      - ./configs:/app/configs:ro\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "/app/agents.yaml") || strings.Contains(out, "networks:") {
		t.Errorf("unexpected single-file mount or networks block:\n%s", out)
	}
}

func TestRegenerateComposeDockerNetworks(t *testing.T) {
	root := writeTestRoster(t, "  docker_networks: [web-agent, default, ' web-agent ', '']\n")
	if err := regenerateCompose(root); err != nil {
		t.Fatal(err)
	}
	out := readCompose(t, root)
	for _, want := range []string{
		"    networks:\n      - default\n      - web-agent\n",
		"\nnetworks:\n  web-agent:\n    external: true\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "web-agent"); n != 2 {
		t.Errorf("web-agent appears %d times, want 2 (service + top-level):\n%s", n, out)
	}
}

func TestRegenerateComposeRejectsBadNetworkName(t *testing.T) {
	root := writeTestRoster(t, "  docker_networks: ['bad name: x']\n")
	if err := regenerateCompose(root); err == nil {
		t.Fatal("expected an error for an invalid network name")
	}
}
