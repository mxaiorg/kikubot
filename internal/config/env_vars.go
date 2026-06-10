package config

import (
	"log"
	"net"
	"os"
	"strconv"
	"strings"
)

var (
	InContainer bool
	AgentName   = "Talos"
	AgentEmail  string
	// AgentEmails is the set of all configured agent emails (lowercase),
	// including self. Populated from agents.yaml at startup. Used by
	// trimHistory to distinguish human (anchor) messages from peer replies.
	AgentEmails         map[string]bool
	EmailPassword       string
	EmailServer         string // IMAP host:port
	EmailInsecureTLS    bool   // skip IMAP TLS verification (self-signed certs in dev)
	SmtpServer          string // SMTP host (port stripped from SMTP_SERVER)
	SmtpHelo            string // EHLO/HELO hostname for SMTP
	SmtpPort            int    // SMTP port (parsed from SMTP_SERVER, defaults to 587)
	AllowedRcpt         []string
	Whitelist           []string
	Blacklist           []string
	LlmModel            string
	LlmProvider         string   // "anthropic" (default) or "openrouter"
	LlmOpenRouterBackup []string // ordered fallback models for OpenRouter
	// LLMKeyMissing is true when the selected provider's API key env var is
	// empty. It lets the runtime degrade gracefully instead of crashing: the
	// provider factory returns a harmless stub (so startup doesn't log.Fatal)
	// and process() replies to inbound mail with a "running, but needs an API
	// key" notice instead of invoking the LLM. This is what powers the no-cost
	// demo (docker-compose-demo.yml) before the user pastes a key.
	LLMKeyMissing bool
	SysPrompt     string
	AgentTimeout  int // in seconds
	// WaitingWatchdogMinutes is the deadline, in minutes, after which a thread
	// still in the "waiting" state re-wakes its coordinator (see
	// services.ArmWaitingWatchdog). 0 disables the watchdog.
	WaitingWatchdogMinutes int
	MaxTurns               int // max agentic loop turns per inbound message (default 20)
	MaxTokens              int
	MaxHistoryChars        int // max total chars in serialized conversation history (0 = unlimited)
	MaxToolResultChars     int // max chars per individual tool result (0 = unlimited)
	MaxEmailRetries        int // max times an inbound email can be left unseen before we give up and bounce
	MaxMessageBodyChars    int // soft cap on inline email body length; bulk content must use attachments (0 = unlimited)
	InboxFolder            string
	SentFolder             string

	// WebSiteUrl WordPressConfig
	WebSiteUrl        string
	WordPressUser     string
	WordPressPassword string
	// BufferAPIKey part after "Bearer"
	BufferAPIKey string
	// XeroConfig
	XeroClientId     string
	XeroClientSecret string
	// HelpjuiceConfig
	HelpjuiceAPIKey  string
	HelpjuiceAccount string
	// Tika: File to Text
	TikaUrl string
	// mxMCP API Key
	MxMcpApiKey string
	// Tavily API Key
	TavilyApiKey string
	// Vimeo API Key
	VimeoApiKey string
	// Nuki API Token (smart lock — account user + smartlock auth management)
	NukiApiToken string
	// Supabase project URL (e.g. https://xyzcompany.supabase.co) and API key.
	SupabaseUrl    string
	SupabaseApiKey string
	// WeatherAPI.com API key (https://www.weatherapi.com).
	WeatherApiKey string
)

// Apply merges the yaml-loaded common+agent overrides into the package-level
// variables read elsewhere in the codebase. Secrets (API keys, mailbox
// passwords) are still read from environment variables — yaml carries only
// non-sensitive configuration.
//
// Identity is selected via AGENT_EMAIL: the matching entry in cfg.Agents
// supplies overrides on top of cfg.Common. Per-agent EMAIL_PASSWORD is
// looked up as <UPPER_STEM>_EMAIL_PASSWORD with a fallback to a plain
// EMAIL_PASSWORD env var (useful for single-agent setups).
//
// LoadEnv should be called once at startup, after dotenv.LoadEnvFile().
func Apply(cfg *AgentsConfig) {
	InContainer = os.Getenv("RUNNING_IN_CONTAINER") == "true"

	AgentEmail = strings.TrimSpace(os.Getenv("AGENT_EMAIL"))

	var agent *AgentDef
	common := CommonConfig{}
	if cfg != nil {
		common = cfg.Common
		agent = cfg.FindAgent(AgentEmail)
	}

	if agent != nil {
		AgentName = agent.Name
		// Agent file is authoritative for the email even if AGENT_EMAIL had
		// case/whitespace drift.
		AgentEmail = agent.Email
	} else {
		AgentName = "Talos"
	}

	EmailPassword = resolveEmailPassword(AgentEmail)

	// Mail server — yaml only; common provides default, agent overrides.
	EmailServer = pickStr(common.EmailServer, agentStr(agent, func(a *AgentDef) string { return a.EmailServer }))
	smtpHostPort := pickStr(common.SmtpServer, agentStr(agent, func(a *AgentDef) string { return a.SmtpServer }))
	SmtpPort = 587
	if smtpHostPort != "" {
		if host, portStr, splitErr := net.SplitHostPort(smtpHostPort); splitErr == nil {
			SmtpServer = host
			if p, parsePortErr := strconv.Atoi(portStr); parsePortErr == nil && p > 0 {
				SmtpPort = p
			}
		} else {
			SmtpServer = smtpHostPort
		}
	}
	if InContainer {
		if strings.HasPrefix(EmailServer, "localhost:") {
			EmailServer = "host.docker.internal:993"
		}
		if SmtpServer == "localhost" {
			SmtpServer = "host.docker.internal"
		}
	}

	// EmailInsecureTLS: agent (bool ptr) > common > env (only for SMTP_HELO compat).
	EmailInsecureTLS = common.EmailInsecureTLS
	if agent != nil && agent.EmailInsecureTLS != nil {
		EmailInsecureTLS = *agent.EmailInsecureTLS
	}

	SmtpHelo = GetEnvWithoutQuotesSafe("SMTP_HELO", "localhost.localdomain")

	// ALLOWED_RCPT remains env-only — it's a deployment-wide override used
	// in test/dev mode; never normally set in production.
	if v := os.Getenv("ALLOWED_RCPT"); v != "" {
		AllowedRcpt = splitCSV(v)
	} else {
		AllowedRcpt = nil
	}

	if agent != nil {
		Whitelist = trimAll(agent.Whitelist)
		Blacklist = trimAll(agent.Blacklist)
	} else {
		Whitelist = nil
		Blacklist = nil
	}

	if agent != nil && strings.TrimSpace(agent.LLMProvider) != "" {
		LlmProvider = strings.TrimSpace(agent.LLMProvider)
	} else {
		LlmProvider = ""
	}
	if agent != nil && strings.TrimSpace(agent.LLMModel) != "" {
		LlmModel = strings.TrimSpace(agent.LLMModel)
	} else {
		LlmModel = ""
	}
	if agent != nil {
		LlmOpenRouterBackup = trimAll(agent.LLMOpenRouterBackup)
	} else {
		LlmOpenRouterBackup = nil
	}

	// Env overrides for provider/model take precedence over agents.yaml. This
	// lets a deployment switch LLM backends without editing config — used by the
	// demo, where demo.sh picks the provider from whichever API key the user
	// pasted (so an OpenRouter user doesn't have to hand-edit agents.yaml).
	if v := strings.TrimSpace(os.Getenv("LLM_PROVIDER")); v != "" {
		LlmProvider = v
	}
	if v := strings.TrimSpace(os.Getenv("LLM_MODEL")); v != "" {
		LlmModel = v
	}

	// Detect a missing provider key. Mirror the provider factory's selection
	// (anthropic unless LlmProvider is explicitly "openrouter") and check the
	// matching env var. When empty, the runtime degrades to the demo notice
	// path rather than crashing on the first API call (or, for OpenRouter, at
	// construction time, which log.Fatals).
	if strings.EqualFold(LlmProvider, "openrouter") {
		LLMKeyMissing = strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY")) == ""
	} else {
		LLMKeyMissing = strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")) == ""
	}

	SysPrompt = pickStr(agentStr(agent, func(a *AgentDef) string { return a.SystemPrompt }), common.SystemPrompt)

	AgentTimeout = pickInt(intPtr(agent, func(a *AgentDef) *int { return a.AgentTimeout }), common.AgentTimeout, 300)
	// 0 = disabled (default). Opt-in per deployment via agents.yaml.
	WaitingWatchdogMinutes = pickInt(intPtr(agent, func(a *AgentDef) *int { return a.WaitingWatchdogMinutes }), common.WaitingWatchdogMinutes, 0)
	MaxTurns = pickInt(intPtr(agent, func(a *AgentDef) *int { return a.MaxTurns }), common.MaxTurns, 20)
	MaxHistoryChars = pickInt(intPtr(agent, func(a *AgentDef) *int { return a.MaxHistoryChars }), common.MaxHistoryChars, 50000)
	MaxEmailRetries = pickInt(intPtr(agent, func(a *AgentDef) *int { return a.MaxEmailRetries }), common.MaxEmailRetries, 3)
	MaxMessageBodyChars = pickInt(intPtr(agent, func(a *AgentDef) *int { return a.MaxMessageBodyChars }), common.MaxMessageBodyChars, 16000)
	MaxTokens = pickInt(intPtr(agent, func(a *AgentDef) *int { return a.MaxTokens }), common.MaxTokens, 8092)

	// MaxToolResultChars: 0 is a meaningful "no limit" value, so we can't fall
	// back to a positive default on 0. Use a pointer at every level.
	MaxToolResultChars = 80000 // default
	if common.MaxToolResultChars != nil {
		MaxToolResultChars = *common.MaxToolResultChars
	}
	if agent != nil && agent.MaxToolResultChars != nil {
		MaxToolResultChars = *agent.MaxToolResultChars
	}

	InboxFolder = GetEnvWithoutQuotesSafe("INBOX_FOLDER", "INBOX")
	SentFolder = GetEnvWithoutQuotesSafe("SENT_FOLDER", "Sent")

	// Tool credentials remain env-only.
	WebSiteUrl = os.Getenv("WEBSITE_URL")
	WordPressUser = os.Getenv("WORDPRESS_USER")
	WordPressPassword = os.Getenv("WORDPRESS_PASSWORD")
	BufferAPIKey = os.Getenv("BUFFER_API_KEY")
	XeroClientId = os.Getenv("XERO_CLIENT_ID")
	XeroClientSecret = os.Getenv("XERO_CLIENT_SECRET")
	HelpjuiceAPIKey = os.Getenv("HELPJUICE_API_KEY")
	HelpjuiceAccount = os.Getenv("HELPJUICE_ACCOUNT")
	TikaUrl = GetEnvWithoutQuotesSafe("TIKA_URL", "http://localhost:9998")
	MxMcpApiKey = os.Getenv("MXMCP_API_KEY")
	TavilyApiKey = os.Getenv("TAVILY_API_KEY")
	VimeoApiKey = os.Getenv("VIMEO_API_KEY")
	NukiApiToken = os.Getenv("NUKI_API_TOKEN")
	SupabaseUrl = os.Getenv("SUPABASE_URL")
	SupabaseApiKey = os.Getenv("SUPABASE_API_KEY")
	WeatherApiKey = os.Getenv("WEATHERAPI_KEY")

	if cfg != nil {
		AgentEmails = make(map[string]bool, len(cfg.Agents)+len(cfg.External))
		for _, a := range cfg.Agents {
			if a.Email != "" {
				AgentEmails[strings.ToLower(a.Email)] = true
			}
		}
		// External partners count as agents for both findAnchor (their
		// replies are peer messages, not the human anchor) and the
		// message_tool known-coworker check (so sends to them are allowed).
		for _, e := range cfg.External {
			if e.Email != "" {
				AgentEmails[strings.ToLower(e.Email)] = true
			}
		}
	} else {
		AgentEmails = nil
	}

	if AgentEmail == "" {
		log.Println("warning: AGENT_EMAIL is unset — agent identity cannot be resolved")
	} else if agent == nil {
		log.Printf("warning: no entry in agents.yaml for %s — running with defaults only", AgentEmail)
	}
}

// resolveEmailPassword reads the mailbox password from the environment.
// Lookup order:
//  1. <UPPER_STEM>_EMAIL_PASSWORD (e.g. KIKU_EMAIL_PASSWORD for kiku@…)
//  2. EMAIL_PASSWORD (plain — convenient for single-agent setups)
func resolveEmailPassword(email string) string {
	stem := strings.ToUpper(emailStem(email))
	if stem != "" {
		if v := os.Getenv(stem + "_EMAIL_PASSWORD"); v != "" {
			return v
		}
	}
	return os.Getenv("EMAIL_PASSWORD")
}

func emailStem(email string) string {
	email = strings.TrimSpace(email)
	if at := strings.Index(email, "@"); at > 0 {
		return strings.ToLower(email[:at])
	}
	return strings.ToLower(email)
}

func pickStr(opts ...string) string {
	for _, s := range opts {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func pickInt(override *int, common, def int) int {
	if override != nil && *override > 0 {
		return *override
	}
	if common > 0 {
		return common
	}
	return def
}

func agentStr(a *AgentDef, get func(*AgentDef) string) string {
	if a == nil {
		return ""
	}
	return get(a)
}

func intPtr(a *AgentDef, get func(*AgentDef) *int) *int {
	if a == nil {
		return nil
	}
	return get(a)
}

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

func trimAll(ss []string) []string {
	if len(ss) == 0 {
		return nil
	}
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func GetEnvWithoutQuotesSafe(key, fallback string) string {
	value := os.Getenv(key)
	val := strings.Trim(value, `"`)
	val = strings.TrimSpace(val)
	if val == "" {
		return fallback
	}
	return val
}
