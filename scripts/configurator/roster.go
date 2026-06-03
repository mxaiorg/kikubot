package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"kikubot/internal/config"

	"gopkg.in/yaml.v3"
)

// roster is the in-memory model of configs/agents.yaml. The configurator
// reads, mutates, and writes through this type; the runtime parses the
// same YAML with kikubot/internal/config.Load.
type roster = config.AgentsConfig

// rosterPath returns the configs/agents.yaml path under root.
func rosterPath(root string) string {
	return filepath.Join(root, "configs", "agents.yaml")
}

// loadRoster reads configs/agents.yaml. Returns a zero-value roster (no
// error) when the file does not exist — first-run setups have nothing to
// read yet.
func loadRoster(root string) (*roster, error) {
	b, err := os.ReadFile(rosterPath(root))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &roster{}, nil
		}
		return nil, err
	}
	var r roster
	if err := yaml.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", rosterPath(root), err)
	}
	return &r, nil
}

// findAgentIndex returns the index of the agent matching email
// (case-insensitive), or -1.
func findAgentIndex(r *roster, email string) int {
	want := strings.ToLower(strings.TrimSpace(email))
	for i, a := range r.Agents {
		if strings.ToLower(strings.TrimSpace(a.Email)) == want {
			return i
		}
	}
	return -1
}

// upsertAgent adds or updates an entry. The match key is email.
func upsertAgent(r *roster, a config.AgentDef) {
	if i := findAgentIndex(r, a.Email); i >= 0 {
		r.Agents[i] = a
		return
	}
	r.Agents = append(r.Agents, a)
}

// removeAgent drops the entry matching email (case-insensitive). No-op when
// the email isn't present.
func removeAgent(r *roster, email string) {
	if i := findAgentIndex(r, email); i >= 0 {
		r.Agents = append(r.Agents[:i], r.Agents[i+1:]...)
	}
}

// saveRoster writes the roster back to configs/agents.yaml. The yaml encoder
// is configured for 2-space indent which mirrors the example file shipped
// with the repo.
func saveRoster(root string, r *roster) error {
	dir := filepath.Dir(rosterPath(root))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fsWriteError(dir, err)
	}
	var buf strings.Builder
	enc := yaml.NewEncoder(&strBuf{&buf})
	enc.SetIndent(2)
	if err := enc.Encode(r); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return fsWriteError(rosterPath(root), os.WriteFile(rosterPath(root), []byte(buf.String()), 0o644))
}

// strBuf adapts *strings.Builder to io.Writer for yaml.NewEncoder.
type strBuf struct{ *strings.Builder }

func (b *strBuf) Write(p []byte) (int, error) { return b.Builder.Write(p) }
