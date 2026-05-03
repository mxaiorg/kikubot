package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// AgentDef defines an agent's identity, role, and tool set.
// JSON tags are used when marshalling the coworker roster into the system prompt.
// The Tools field is excluded from JSON since coworkers don't need to see each other's scripts.
type AgentDef struct {
	Name        string   `yaml:"name" json:"name,omitempty"`
	Email       string   `yaml:"email" json:"email,omitempty"`
	Role        string   `yaml:"role" json:"role,omitempty"`
	Description string   `yaml:"description" json:"description,omitempty"`
	Tools       []string `yaml:"tools" json:"-"`
}

type AgentsConfig struct {
	Agents []AgentDef `yaml:"agents"`
}

// Load reads and parses an agents YAML services file.
// Returns (nil, nil) if the file does not exist (graceful fallback).
func Load(path string) (*AgentsConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading services %s: %w", path, err)
	}

	var cfg AgentsConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing services %s: %w", path, err)
	}
	return &cfg, nil
}

// FindAgent returns the agent definition matching the given email, or nil.
func (c *AgentsConfig) FindAgent(email string) *AgentDef {
	email = strings.ToLower(email)
	for i := range c.Agents {
		if strings.ToLower(c.Agents[i].Email) == email {
			return &c.Agents[i]
		}
	}
	return nil
}

// Coworkers returns all agents except the one matching selfEmail.
func (c *AgentsConfig) Coworkers(selfEmail string) []AgentDef {
	selfEmail = strings.ToLower(selfEmail)
	filtered := make([]AgentDef, 0, len(c.Agents))
	for _, a := range c.Agents {
		if strings.ToLower(a.Email) != selfEmail {
			filtered = append(filtered, a)
		}
	}
	return filtered
}
