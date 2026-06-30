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
// JSON tags exist because Peers() is serialised into the system prompt
// at the {{coworkers}} template marker — the LLM only sees the identity-and-
// description fields, never overrides or secrets.
type AgentDef struct {
	Name          string   `yaml:"name" json:"name,omitempty"`
	Email         string   `yaml:"email" json:"email,omitempty"`
	Role          string   `yaml:"role" json:"role,omitempty"`
	Description   string   `yaml:"description" json:"description,omitempty"`
	Tools         []string `yaml:"tools,flow,omitempty" json:"-"`
	DisabledTools []string `yaml:"disabled_tools,flow,omitempty" json:"-"`

	// Per-agent overrides. A nil/zero value means "inherit from common".
	LLMProvider            string   `yaml:"llm_provider,omitempty" json:"-"`
	LLMModel               string   `yaml:"llm_model,omitempty" json:"-"`
	LLMOpenRouterBackup    []string `yaml:"llm_openrouter_backup,flow,omitempty" json:"-"`
	SystemPrompt           string   `yaml:"system_prompt,omitempty" json:"-"`
	Whitelist              []string `yaml:"whitelist,flow,omitempty" json:"-"`
	Blacklist              []string `yaml:"blacklist,flow,omitempty" json:"-"`
	MaxHistoryChars        *int     `yaml:"max_history_chars,omitempty" json:"-"`
	MaxTokens              *int     `yaml:"max_tokens,omitempty" json:"-"`
	MaxTurns               *int     `yaml:"max_turns,omitempty" json:"-"`
	MaxToolResultChars     *int     `yaml:"max_tool_result_chars,omitempty" json:"-"`
	ToolResultSpillChars   *int     `yaml:"tool_result_spill_chars,omitempty" json:"-"`
	MaxEmailRetries        *int     `yaml:"max_email_retries,omitempty" json:"-"`
	MaxMessageBodyChars    *int     `yaml:"max_message_body_chars,omitempty" json:"-"`
	AgentTimeout           *int     `yaml:"agent_timeout,omitempty" json:"-"`
	WaitingWatchdogMinutes *int     `yaml:"waiting_watchdog_minutes,omitempty" json:"-"`

	// Mail server overrides — rarely needed, but useful when one agent
	// uses a different mailbox host than the rest of the roster.
	EmailServer      string `yaml:"email_server,omitempty" json:"-"`
	SmtpServer       string `yaml:"smtp_server,omitempty" json:"-"`
	EmailInsecureTLS *bool  `yaml:"email_insecure_tls,omitempty" json:"-"`
}

// ExternalAgent is one entry under `external:` — a peer that runs on another
// machine and/or another mail domain, outside this deployment's control. We
// never run these agents, so they carry only identity-and-description fields:
// tools, budgets, LLM config and ACL overrides do not apply.
//
// The roster grants this deployment's agents the ability to *reach* an
// external peer via message_tool (it relaxes the same-domain send gate and
// registers the address in AgentEmails). It does NOT grant the external peer
// inbound access: a partner emailing in is still gated by each agent's
// whitelist, which must list the address or its domain explicitly.
type ExternalAgent struct {
	Name        string `yaml:"name" json:"name,omitempty"`
	Email       string `yaml:"email" json:"email,omitempty"`
	Role        string `yaml:"role" json:"role,omitempty"`
	Description string `yaml:"description" json:"description,omitempty"`
}

// PromptPeer is the shape rendered into the {{coworkers}} block of the system
// prompt. In-roster coworkers leave Scope empty; external partners set
// Scope="external" so the model knows the peer is off-box (possibly slower,
// capabilities unknown, no shared memory).
type PromptPeer struct {
	Name        string `json:"name,omitempty"`
	Email       string `json:"email,omitempty"`
	Role        string `json:"role,omitempty"`
	Description string `json:"description,omitempty"`
	Scope       string `json:"scope,omitempty"`
}

// CommonConfig holds defaults shared by every agent. Any field can be
// overridden per-agent in AgentDef.
type CommonConfig struct {
	EmailServer             string   `yaml:"email_server,omitempty"`
	SmtpServer              string   `yaml:"smtp_server,omitempty"`
	EmailInsecureTLS        bool     `yaml:"email_insecure_tls,omitempty"`
	MaxHistoryChars         int      `yaml:"max_history_chars,omitempty"`
	MaxTokens               int      `yaml:"max_tokens,omitempty"`
	MaxTurns                int      `yaml:"max_turns,omitempty"`
	MaxToolResultChars      *int     `yaml:"max_tool_result_chars,omitempty"`
	ToolResultSpillChars    *int     `yaml:"tool_result_spill_chars,omitempty"`
	MaxEmailRetries         int      `yaml:"max_email_retries,omitempty"`
	MaxMessageBodyChars     int      `yaml:"max_message_body_chars,omitempty"`
	AgentTimeout            int      `yaml:"agent_timeout,omitempty"`
	WaitingWatchdogMinutes  int      `yaml:"waiting_watchdog_minutes,omitempty"`
	SystemPrompt            string   `yaml:"system_prompt,omitempty"`
	CoordinatorSystemPrompt string   `yaml:"coordinator_system_prompt,omitempty"`
	DisabledTools           []string `yaml:"disabled_tools,flow,omitempty"`
}

// MCPServer is one entry under `mcp_servers:` — a remote (Streamable HTTP) MCP
// server exposed to agents as a tool key. The table is the declarative seam for
// adding remote MCPs: a new server is a YAML row (plus, for oauth2, a hand-seeded
// token file), never new Go. RegisterMCPServers turns each entry into a registry
// factory keyed by Key, so an agent gets the server by listing Key in its
// `tools:`.
//
// Auth modes (all static modes read the secret from TokenEnv at startup and
// send it in a fixed request header; Header defaults to "Authorization"):
//   - "none"   — no auth header.
//   - "bearer" — header value "<Scheme> <token>"; Scheme defaults to "Bearer".
//   - "apikey" — same static-header mechanism, but Scheme defaults to "ApiKey"
//     when the header is Authorization (e.g. mxMCP wants "Authorization: ApiKey
//     <token>"), and defaults to empty (raw token) when a custom Header is set
//     (e.g. "X-Api-Key: <token>"). Set Scheme explicitly to override either.
//   - "oauth2" — OAuth2 authorization-code tokens with automatic refresh handled
//     by the mcp-go OAuth handler. ClientIDEnv/ClientSecretEnv supply the app
//     credentials; the access+refresh pair lives in a hand-seeded
//     data/oauth/<Key>.json (a serialised mcp-go transport.Token) which kikubot
//     rotates and rewrites on every refresh. MetadataURL optionally pins the
//     OAuth server metadata endpoint when discovery from URL doesn't work.
type MCPServer struct {
	Key             string `yaml:"key"`
	URL             string `yaml:"url"`
	Auth            string `yaml:"auth,omitempty"` // none | bearer | apikey | oauth2 (default none)
	Header          string `yaml:"header,omitempty"`
	Scheme          string `yaml:"scheme,omitempty"`
	TokenEnv        string `yaml:"token_env,omitempty"`
	ClientIDEnv     string `yaml:"client_id_env,omitempty"`
	ClientSecretEnv string `yaml:"client_secret_env,omitempty"`
	MetadataURL     string `yaml:"metadata_url,omitempty"`
	Description     string `yaml:"description,omitempty"`
}

// AgentsConfig is the deserialised contents of configs/agents.yaml.
type AgentsConfig struct {
	Common   CommonConfig    `yaml:"common"`
	Agents   []AgentDef      `yaml:"agents"`
	External []ExternalAgent `yaml:"external,omitempty"`
}

// MCPServersConfig is the deserialised contents of configs/mcp_servers.yaml —
// the declarative table of remote MCP servers, kept in its own file so the
// agent roster (agents.yaml) and the remote-integration catalog evolve
// independently.
type MCPServersConfig struct {
	MCPServers []MCPServer `yaml:"mcp_servers"`
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

// LoadMCPServers reads and parses an mcp_servers YAML file. Returns (nil, nil)
// when the file does not exist — a deployment may simply declare no remote MCP
// servers, which is not an error.
func LoadMCPServers(path string) ([]MCPServer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var cfg MCPServersConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return cfg.MCPServers, nil
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

// Peers returns every peer this agent may collaborate with — in-roster
// coworkers (everyone under `agents:` except self) followed by external
// partners (everyone under `external:`). The result is JSON-marshalled into
// the {{coworkers}} block of the system prompt, so it carries only identity +
// role + description; overrides and secrets are never included. External
// peers are tagged Scope="external" so the model can distinguish them.
func (c *AgentsConfig) Peers(selfEmail string) []PromptPeer {
	selfEmail = strings.ToLower(selfEmail)
	peers := make([]PromptPeer, 0, len(c.Agents)+len(c.External))
	for _, a := range c.Agents {
		if strings.ToLower(a.Email) == selfEmail {
			continue
		}
		// Strip overrides — coworkers only see identity + role + description.
		peers = append(peers, PromptPeer{
			Name:        a.Name,
			Email:       a.Email,
			Role:        a.Role,
			Description: a.Description,
		})
	}
	for _, e := range c.External {
		if strings.ToLower(e.Email) == selfEmail {
			continue
		}
		peers = append(peers, PromptPeer{
			Name:        e.Name,
			Email:       e.Email,
			Role:        e.Role,
			Description: e.Description,
			Scope:       "external",
		})
	}
	return peers
}
