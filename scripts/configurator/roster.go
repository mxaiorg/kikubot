package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// rosterAgent mirrors a single entry under `agents:` in configs/agents.yaml.
// It is the public-facing roster as opposed to the env-level agent config.
type rosterAgent struct {
	Name        string   `yaml:"name"`
	Email       string   `yaml:"email"`
	Role        string   `yaml:"role,omitempty"`
	Description string   `yaml:"description,omitempty"`
	Tools       []string `yaml:"tools,flow,omitempty"`
}

type roster struct {
	Agents []rosterAgent `yaml:"agents"`
}

func rosterPath(root string) string {
	return filepath.Join(root, "configs", "agents.yaml")
}

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

// findAgent returns the index of an agent matching email (case-insensitive)
// or -1.
func (r *roster) findAgent(email string) int {
	want := strings.ToLower(strings.TrimSpace(email))
	for i, a := range r.Agents {
		if strings.ToLower(strings.TrimSpace(a.Email)) == want {
			return i
		}
	}
	return -1
}

// upsert adds or updates an entry. The match key is email.
func (r *roster) upsert(a rosterAgent) {
	if i := r.findAgent(a.Email); i >= 0 {
		r.Agents[i] = a
		return
	}
	r.Agents = append(r.Agents, a)
}

func (r *roster) save(root string) error {
	dir := filepath.Dir(rosterPath(root))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
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
	return os.WriteFile(rosterPath(root), []byte(buf.String()), 0o644)
}

// strBuf adapts *strings.Builder to io.Writer (Builder already implements
// Write but yaml.NewEncoder wants io.Writer — Builder satisfies it).
// Keeping the indirection so we can swap in a buffer type that injects blank
// lines between agents later if needed.
type strBuf struct{ *strings.Builder }

func (b *strBuf) Write(p []byte) (int, error) { return b.Builder.Write(p) }

// rosterAgentFor returns the live roster entry for an agent (by email), or a
// zero-value entry if none exists yet.
func rosterAgentFor(root, email string) rosterAgent {
	r, err := loadRoster(root)
	if err != nil {
		return rosterAgent{}
	}
	if i := r.findAgent(email); i >= 0 {
		return r.Agents[i]
	}
	return rosterAgent{}
}
