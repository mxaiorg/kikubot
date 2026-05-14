package config

import (
	"log"
	"net"
	"os"
	"regexp"
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
	EmailPassword       = ""
	EmailServer         = "" // IMAP host:port
	EmailInsecureTLS    bool // skip IMAP TLS verification (self-signed certs in dev)
	SmtpServer          = "" // SMTP host (port stripped from SMTP_SERVER)
	SmtpHelo            = "" // EHLO/HELO hostname for SMTP
	SmtpPort            int  // SMTP port (parsed from SMTP_SERVER, defaults to 587)
	AllowedRcpt         []string
	Whitelist           []string
	Blacklist           []string
	LlmModel            string
	LlmProvider         string // "anthropic" (default) or "openrouter"
	LlmOpenRouterBackup string // comma-separated fallback models for OpenRouter
	SysPrompt           string
	AgentTimeout        int // in seconds
	MaxTurns            int // max agentic loop turns per inbound message (default 20)
	MaxTokens           int
	MaxHistoryChars     int // max total chars in serialized conversation history (0 = unlimited)
	MaxToolResultChars  int // max chars per individual tool result (0 = unlimited)
	MaxEmailRetries     int // max times an inbound email can be left unseen before we give up and bounce
	MaxMessageBodyChars int // soft cap on inline email body length; bulk content must use attachments (0 = unlimited)
	InboxFolder         string
	SentFolder          string

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
	HelpjuiceAccount string // subdomain, e.g. "myaccount" for myaccount.helpjuice.com
	// Tika: File to Text
	TikaUrl string
	// mxMCP API Key
	MxMcpApiKey string
	// Tavily API Key
	TavilyApiKey string
	// Vimeo API Key
	VimeoApiKey string
)

var splitRegexp = regexp.MustCompile(`\s*,\s*`)

func LoadEnv() {
	InContainer = os.Getenv("RUNNING_IN_CONTAINER") == "true"
	AgentName = os.Getenv("AGENT_NAME")
	AgentEmail = os.Getenv("AGENT_EMAIL")
	EmailPassword = os.Getenv("EMAIL_PASSWORD")
	EmailServer = os.Getenv("EMAIL_SERVER")
	switch strings.ToLower(os.Getenv("EMAIL_INSECURE_TLS")) {
	case "1", "true", "yes":
		EmailInsecureTLS = true
	}
	// SMTP_SERVER may be either "host" or "host:port" (consistent with EMAIL_SERVER).
	// When the port is omitted we default to 587.
	smtpRaw := os.Getenv("SMTP_SERVER")
	SmtpPort = 587
	if smtpRaw != "" {
		if host, portStr, splitErr := net.SplitHostPort(smtpRaw); splitErr == nil {
			SmtpServer = host
			if p, parsePortErr := strconv.Atoi(portStr); parsePortErr == nil && p > 0 {
				SmtpPort = p
			}
		} else {
			SmtpServer = smtpRaw
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
	SmtpHelo = os.Getenv("SMTP_HELO")
	if SmtpHelo == "" {
		SmtpHelo = "localhost.localdomain"
	}
	allowedRcptEnv := os.Getenv("ALLOWED_RCPT")
	if allowedRcptEnv != "" {
		parts := splitRegexp.Split(allowedRcptEnv, -1)
		AllowedRcpt = append(AllowedRcpt, parts...)
	}
	whitelistEnv := os.Getenv("WHITELIST")
	if whitelistEnv != "" {
		parts := splitRegexp.Split(whitelistEnv, -1)
		Whitelist = append(Whitelist, parts...)
	}
	blacklistEnv := os.Getenv("BLACKLIST")
	if blacklistEnv != "" {
		parts := splitRegexp.Split(blacklistEnv, -1)
		Blacklist = append(Blacklist, parts...)
	}
	LlmModel = os.Getenv("LLM_MODEL")
	LlmProvider = os.Getenv("LLM_PROVIDER") // "anthropic" (default) or "openrouter"
	LlmOpenRouterBackup = os.Getenv("LLM_OPENROUTER_BACKUP")
	SysPrompt = os.Getenv("SYSTEM_PROMPT")
	agentTimeout, parseErr := strconv.Atoi(os.Getenv("AGENT_TIMEOUT"))
	if parseErr != nil {
		log.Println("AGENT_TIMEOUT is not a number, using default value")
		AgentTimeout = 300 // 5 minutes
	} else {
		AgentTimeout = agentTimeout
	}
	maxTurns, maxTurnsErr := strconv.Atoi(os.Getenv("MAX_TURNS"))
	if maxTurnsErr != nil || maxTurns <= 0 {
		MaxTurns = 20
	} else {
		MaxTurns = maxTurns
	}
	maxHistChars, parseHistErr := strconv.Atoi(os.Getenv("MAX_HISTORY_CHARS"))
	if parseHistErr != nil || maxHistChars <= 0 {
		MaxHistoryChars = 50000 // ~12.5K tokens — leaves room for system prompt + scripts + output
	} else {
		MaxHistoryChars = maxHistChars
	}
	// Default cap on a single tool result. A few tools (mailbox_search, mxmcp,
	// MCP bridges) can return arbitrarily large payloads; without a cap, one
	// runaway result balloons history past trim budget and the agent loses
	// anchor context on subsequent turns. 80 KB ≈ 20 K tokens — plenty for any
	// reasonable tool output, small enough that a few stacked results still
	// leave room. Set MAX_TOOL_RESULT_CHARS=0 explicitly to disable.
	rawToolRes, hasToolRes := os.LookupEnv("MAX_TOOL_RESULT_CHARS")
	if !hasToolRes {
		MaxToolResultChars = 80000
	} else if maxToolRes, parseToolErr := strconv.Atoi(rawToolRes); parseToolErr == nil && maxToolRes >= 0 {
		MaxToolResultChars = maxToolRes
	} else {
		MaxToolResultChars = 80000
	}
	maxRetries, maxRetriesErr := strconv.Atoi(os.Getenv("MAX_EMAIL_RETRIES"))
	if maxRetriesErr != nil || maxRetries <= 0 {
		MaxEmailRetries = 3
	} else {
		MaxEmailRetries = maxRetries
	}
	maxBody, maxBodyErr := strconv.Atoi(os.Getenv("MAX_MESSAGE_BODY_CHARS"))
	if maxBodyErr != nil || maxBody < 0 {
		MaxMessageBodyChars = 16000 // ~4K tokens — long enough for a real email, short enough to never truncate the tool call
	} else {
		MaxMessageBodyChars = maxBody
	}
	maxTokens, maxTokensErr := strconv.Atoi(os.Getenv("MAX_TOKENS"))
	if maxTokensErr != nil {
		log.Println("MAX_TOKENS is not a number, using default value")
		maxTokens = 8092
	} else {
		MaxTokens = maxTokens
	}
	InboxFolder = GetEnvWithoutQuotesSafe("INBOX_FOLDER", "INBOX")
	SentFolder = GetEnvWithoutQuotesSafe("SENT_FOLDER", "Sent")
	// WordPressConfig
	WebSiteUrl = os.Getenv("WEBSITE_URL")
	WordPressUser = os.Getenv("WORDPRESS_USER")
	WordPressPassword = os.Getenv("WORDPRESS_PASSWORD")
	// BufferConfig
	BufferAPIKey = os.Getenv("BUFFER_API_KEY")
	// XeroConfig
	XeroClientId = os.Getenv("XERO_CLIENT_ID")
	XeroClientSecret = os.Getenv("XERO_CLIENT_SECRET")
	// HelpjuiceConfig
	HelpjuiceAPIKey = os.Getenv("HELPJUICE_API_KEY")
	HelpjuiceAccount = os.Getenv("HELPJUICE_ACCOUNT")
	// Tika: File to Text
	TikaUrl = GetEnvWithoutQuotesSafe("TIKA_URL", "http://localhost:9998")
	// mxMCP API Key
	MxMcpApiKey = os.Getenv("MXMCP_API_KEY")
	// Tavily API Key
	TavilyApiKey = os.Getenv("TAVILY_API_KEY")
	// Vimeo API Key
	VimeoApiKey = os.Getenv("VIMEO_API_KEY")
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
