// Package config loads kikubot's deployment configuration.
//
// A deployment is described by a single YAML file (configs/agents.yaml by
// default) with two sections:
//
//	common: <CommonConfig>   # defaults applied to every agent
//	agents: [<AgentDef>...]  # roster plus per-agent overrides
//
// Secrets (API keys, mailbox passwords) live exclusively in environment
// variables, typically loaded from configs/secrets.env. The runtime picks
// its identity from the AGENT_EMAIL env var, looks the agent up in agents.yaml,
// and merges common + agent into the package-level variables that the rest of
// the codebase reads.
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// AgentDef is one entry under `agents:`. All fields except Name/Email/Role/
// Description/Tools are optional overrides of CommonConfig.
//
// JSON tags exist because Coworkers() is serialised into the system prompt
// at the {{coworkers}} template marker — the LLM only sees the identity-and-
// description fields, never overrides or secrets.
type AgentDef struct {
	Name        string   `yaml:"name" json:"name,omitempty"`
	Email       string   `yaml:"email" json:"email,omitempty"`
	Role        string   `yaml:"role" json:"role,omitempty"`
	Description string   `yaml:"description" json:"description,omitempty"`
	Tools       []string `yaml:"tools,flow,omitempty" json:"-"`

	// Per-agent overrides. A nil/zero value means "inherit from common".
	LLMProvider         string   `yaml:"llm_provider,omitempty" json:"-"`
	LLMModel            string   `yaml:"llm_model,omitempty" json:"-"`
	LLMOpenRouterBackup []string `yaml:"llm_openrouter_backup,flow,omitempty" json:"-"`
	SystemPrompt        string   `yaml:"system_prompt,omitempty" json:"-"`
	Whitelist           []string `yaml:"whitelist,flow,omitempty" json:"-"`
	Blacklist           []string `yaml:"blacklist,flow,omitempty" json:"-"`
	MaxHistoryChars     *int     `yaml:"max_history_chars,omitempty" json:"-"`
	MaxTokens           *int     `yaml:"max_tokens,omitempty" json:"-"`
	MaxTurns            *int     `yaml:"max_turns,omitempty" json:"-"`
	MaxToolResultChars  *int     `yaml:"max_tool_result_chars,omitempty" json:"-"`
	MaxEmailRetries     *int     `yaml:"max_email_retries,omitempty" json:"-"`
	MaxMessageBodyChars *int     `yaml:"max_message_body_chars,omitempty" json:"-"`
	AgentTimeout        *int     `yaml:"agent_timeout,omitempty" json:"-"`

	// Mail server overrides — rarely needed, but useful when one agent
	// uses a different mailbox host than the rest of the roster.
	EmailServer      string `yaml:"email_server,omitempty" json:"-"`
	SmtpServer       string `yaml:"smtp_server,omitempty" json:"-"`
	EmailInsecureTLS *bool  `yaml:"email_insecure_tls,omitempty" json:"-"`
}

// CommonConfig holds defaults shared by every agent. Any field can be
// overridden per-agent in AgentDef.
type CommonConfig struct {
	EmailServer             string `yaml:"email_server,omitempty"`
	SmtpServer              string `yaml:"smtp_server,omitempty"`
	EmailInsecureTLS        bool   `yaml:"email_insecure_tls,omitempty"`
	MaxHistoryChars         int    `yaml:"max_history_chars,omitempty"`
	MaxTokens               int    `yaml:"max_tokens,omitempty"`
	MaxTurns                int    `yaml:"max_turns,omitempty"`
	MaxToolResultChars      *int   `yaml:"max_tool_result_chars,omitempty"`
	MaxEmailRetries         int    `yaml:"max_email_retries,omitempty"`
	MaxMessageBodyChars     int    `yaml:"max_message_body_chars,omitempty"`
	AgentTimeout            int    `yaml:"agent_timeout,omitempty"`
	SystemPrompt            string `yaml:"system_prompt,omitempty"`
	CoordinatorSystemPrompt string `yaml:"coordinator_system_prompt,omitempty"`
}

// AgentsConfig is the deserialised contents of configs/agents.yaml.
type AgentsConfig struct {
	Common CommonConfig `yaml:"common"`
	Agents []AgentDef   `yaml:"agents"`
}

// Load reads and parses an agents YAML file. Returns (nil, nil) when the
// file does not exist (graceful fallback for first-run/test contexts).
func Load(path string) (*AgentsConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var cfg AgentsConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
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

// Coworkers returns all agents except the one matching selfEmail. The
// returned slice is JSON-marshalled into the {{coworkers}} block of the
// system prompt, so it must not include secrets or overrides.
func (c *AgentsConfig) Coworkers(selfEmail string) []AgentDef {
	selfEmail = strings.ToLower(selfEmail)
	filtered := make([]AgentDef, 0, len(c.Agents))
	for _, a := range c.Agents {
		if strings.ToLower(a.Email) == selfEmail {
			continue
		}
		// Strip overrides — coworkers only see identity + role + description.
		filtered = append(filtered, AgentDef{
			Name:        a.Name,
			Email:       a.Email,
			Role:        a.Role,
			Description: a.Description,
		})
	}
	return filtered
}
