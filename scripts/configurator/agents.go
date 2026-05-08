package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// envDir returns the abs path to configs/env under root.
func envDir(root string) string {
	return filepath.Join(root, "configs", "env")
}

// commonEnvPath returns the live common.env path (creating-on-save target).
func commonEnvPath(root string) string {
	return filepath.Join(envDir(root), "common.env")
}

// commonEnvFallback returns the example common.env to seed defaults from.
func commonEnvFallback(root string) string {
	return filepath.Join(envDir(root), "examples", "common.env")
}

// loadCommonEnv loads common.env or falls back to the example.
func loadCommonEnv(root string) (*envFile, error) {
	return loadEnvWithFallback(commonEnvPath(root), commonEnvFallback(root))
}

// agentEnvPath returns the per-agent env file path for `stem` (the local-part
// of the agent's email address, lowercased).
func agentEnvPath(root, stem string) string {
	return filepath.Join(envDir(root), stem+".env")
}

// agentSummary is a row in the agents list.
type agentSummary struct {
	Stem  string // file stem (local-part of email)
	Name  string // AGENT_NAME (or stem fallback)
	Email string
	Role  string // role from configs/agents.yaml (empty if absent)
}

// listAgents returns existing agent files, sorted alphabetically by name.
// `common.env` and any file whose first non-blank entry doesn't look like an
// agent file (no AGENT_EMAIL) are excluded. Each row's Role is filled from
// configs/agents.yaml (matched by email, case-insensitive).
func listAgents(root string) ([]agentSummary, error) {
	dir := envDir(root)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	rost, _ := loadRoster(root)
	roleByEmail := map[string]string{}
	if rost != nil {
		for _, a := range rost.Agents {
			roleByEmail[strings.ToLower(strings.TrimSpace(a.Email))] = a.Role
		}
	}
	var out []agentSummary
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".env") {
			continue
		}
		if e.Name() == "common.env" {
			continue
		}
		stem := strings.TrimSuffix(e.Name(), ".env")
		f, err := loadEnvFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		email, _ := f.Get("AGENT_EMAIL")
		if email == "" {
			continue
		}
		name, _ := f.Get("AGENT_NAME")
		if name == "" {
			name = stem
		}
		out = append(out, agentSummary{
			Stem:  stem,
			Name:  name,
			Email: email,
			Role:  roleByEmail[strings.ToLower(strings.TrimSpace(email))],
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// emailStem extracts the local-part of an email address and lowercases it.
func emailStem(email string) string {
	email = strings.TrimSpace(email)
	if at := strings.Index(email, "@"); at > 0 {
		return strings.ToLower(email[:at])
	}
	return strings.ToLower(email)
}

// agentForm captures the user-submitted state of the Add/Edit Agent form.
type agentForm struct {
	OriginalStem         string // empty for new agent
	Name                 string
	Email                string
	EmailPassword        string
	Whitelist            string // CSV
	Blacklist            string // CSV
	LLMProvider          string // anthropic|openrouter
	LLMModel             string
	LLMOpenrouterBackup  string
	SystemPrompt         string
	ExcludeCollaborators bool
	// Roster entry (configs/agents.yaml)
	Role     string
	RoleDesc string   // Description in agents.yaml — distinct from SYSTEM_PROMPT
	Tools    []string // registry keys, in user-chosen order

	// Render-only: full registry list (key + description) for the chip UI.
	Registry []toolInfo `json:"-"`
	// Render-only: COORDINATOR_SYS_PROMPT default sourced from common.env (or
	// its example fallback). Surfaced on the form so the "Paste Coordinator
	// prompt" button can drop it into the system-prompt textarea.
	CoordinatorPromptDefault string `json:"-"`
	// Render-only: whether the respective API keys are defined in either the
	// agent's env file or common.env. Drives the LLM-provider option-disable
	// logic in the form so the user can't pick a provider whose key is missing.
	HasAnthropicKey  bool `json:"-"`
	HasOpenRouterKey bool `json:"-"`
}

// llmKeyAvailability reports whether ANTHROPIC_API_KEY and OPENROUTER_API_KEY
// are defined (non-blank) in either the agent's env file or common.env. The
// agent file takes precedence when looking up values; if `agentStem` is empty
// (new agent), only common.env is consulted.
func llmKeyAvailability(root, agentStem string) (anthropic, openrouter bool) {
	common, _ := loadCommonEnv(root)
	var agent *envFile
	if agentStem != "" {
		agent, _ = loadEnvFile(agentEnvPath(root, agentStem))
	}
	has := func(key string) bool {
		if agent != nil {
			if v, ok := agent.Get(key); ok && strings.TrimSpace(v) != "" {
				return true
			}
		}
		if common != nil {
			if v, ok := common.Get(key); ok && strings.TrimSpace(v) != "" {
				return true
			}
		}
		return false
	}
	return has("ANTHROPIC_API_KEY"), has("OPENROUTER_API_KEY")
}

// validate runs basic input checks.
func (a *agentForm) validate() error {
	if strings.TrimSpace(a.Name) == "" {
		return fmt.Errorf("agent name is required")
	}
	if strings.TrimSpace(a.Email) == "" {
		return fmt.Errorf("agent email is required")
	}
	if !strings.Contains(a.Email, "@") {
		return fmt.Errorf("agent email must include @")
	}
	if strings.TrimSpace(a.LLMProvider) == "" {
		return fmt.Errorf("LLM provider is required")
	}
	if a.LLMProvider != "anthropic" && a.LLMProvider != "openrouter" {
		return fmt.Errorf("LLM provider must be anthropic or openrouter")
	}
	if strings.TrimSpace(a.LLMModel) == "" {
		return fmt.Errorf("LLM model is required")
	}
	if strings.TrimSpace(a.Whitelist) != "" && strings.TrimSpace(a.Blacklist) != "" {
		return fmt.Errorf("whitelist and blacklist are mutually exclusive")
	}
	if strings.TrimSpace(a.Role) == "" {
		return fmt.Errorf("roster role is required")
	}
	if strings.TrimSpace(a.RoleDesc) == "" {
		return fmt.Errorf("roster description is required")
	}
	return nil
}

// loadAgentForm loads existing per-agent env merged with defaults from common.
// Used to populate the edit form for an existing agent.
func loadAgentForm(root, stem string) (*agentForm, error) {
	path := agentEnvPath(root, stem)
	f, err := loadEnvFile(path)
	if err != nil {
		return nil, err
	}
	common, _ := loadCommonEnv(root)

	get := func(key string) string {
		if v, ok := f.Get(key); ok {
			return v
		}
		if common != nil {
			if v, ok := common.Get(key); ok {
				return v
			}
		}
		return ""
	}

	a := &agentForm{
		OriginalStem:        stem,
		Name:                strings.Trim(get("AGENT_NAME"), "\""),
		Email:               get("AGENT_EMAIL"),
		EmailPassword:       get("EMAIL_PASSWORD"),
		Whitelist:           get("WHITELIST"),
		Blacklist:           get("BLACKLIST"),
		LLMProvider:         get("LLM_PROVIDER"),
		LLMModel:            get("LLM_MODEL"),
		LLMOpenrouterBackup: get("LLM_OPENROUTER_BACKUP"),
		SystemPrompt:        get("SYSTEM_PROMPT"),
	}
	if a.LLMProvider == "" {
		a.LLMProvider = "anthropic"
	}
	a.ExcludeCollaborators = a.SystemPrompt != "" && !strings.Contains(a.SystemPrompt, "{{coworkers}}")

	// Pull Role / Description / Tools from configs/agents.yaml (matched by email).
	if r := rosterAgentFor(root, a.Email); r.Email != "" {
		a.Role = r.Role
		a.RoleDesc = r.Description
		a.Tools = append(a.Tools, r.Tools...)
	}
	return a, nil
}

// save writes the per-agent env file. If a value matches common.env, it is
// omitted from the agent file (per spec). The file stem is derived from the
// account part of the email address.
func (a *agentForm) save(root string) (newStem string, err error) {
	if err := a.validate(); err != nil {
		return "", err
	}
	stem := emailStem(a.Email)
	if stem == "" {
		return "", fmt.Errorf("could not derive file stem from email")
	}

	common, _ := loadCommonEnv(root)
	commonGet := func(key string) string {
		if common == nil {
			return ""
		}
		v, _ := common.Get(key)
		return v
	}

	// Build/update the system prompt per spec.
	prompt := a.SystemPrompt
	if a.ExcludeCollaborators {
		prompt = stripCoworkers(prompt)
	} else {
		prompt = ensureCoworkers(prompt)
	}

	// Load any pre-existing per-agent file so we preserve user comments.
	path := agentEnvPath(root, stem)
	f, err := loadEnvWithFallback(path, "")
	if err != nil {
		return "", err
	}

	setOrDrop := func(key, value string, isPasswordOrSecret bool) {
		val := strings.TrimSpace(value)
		if !isPasswordOrSecret {
			val = value // some keys (system prompt) intentionally have whitespace
		}
		if val == "" {
			f.Delete(key)
			return
		}
		if !isPasswordOrSecret && commonGet(key) == val {
			// Equal to common.env — don't store at the agent level.
			f.Delete(key)
			return
		}
		f.Set(key, val)
	}

	// Always write identity + auth at the agent level.
	f.Set("AGENT_NAME", strings.TrimSpace(a.Name))
	f.Set("AGENT_EMAIL", strings.TrimSpace(a.Email))
	if strings.TrimSpace(a.EmailPassword) != "" {
		f.Set("EMAIL_PASSWORD", a.EmailPassword)
	} else {
		f.Delete("EMAIL_PASSWORD")
	}

	// Access control: whitelist and blacklist are exclusive.
	wl := strings.TrimSpace(a.Whitelist)
	bl := strings.TrimSpace(a.Blacklist)
	if wl != "" {
		f.Set("WHITELIST", wl)
		f.Delete("BLACKLIST")
	} else if bl != "" {
		f.Set("BLACKLIST", bl)
		f.Delete("WHITELIST")
	} else {
		f.Delete("WHITELIST")
		f.Delete("BLACKLIST")
	}

	setOrDrop("LLM_PROVIDER", a.LLMProvider, false)
	setOrDrop("LLM_MODEL", a.LLMModel, false)
	if a.LLMProvider == "openrouter" {
		setOrDrop("LLM_OPENROUTER_BACKUP", a.LLMOpenrouterBackup, false)
	} else {
		f.Delete("LLM_OPENROUTER_BACKUP")
	}

	setOrDrop("SYSTEM_PROMPT", prompt, false)

	// Rename if email changed (different stem).
	if a.OriginalStem != "" && a.OriginalStem != stem {
		_ = os.Remove(agentEnvPath(root, a.OriginalStem))
	}
	if err := os.MkdirAll(envDir(root), 0o755); err != nil {
		return "", err
	}
	if err := f.Save(path); err != nil {
		return "", err
	}

	// Upsert the roster entry in configs/agents.yaml.
	if err := upsertRoster(root, a); err != nil {
		return stem, fmt.Errorf("env saved but roster update failed: %w", err)
	}
	return stem, nil
}

// upsertRoster updates configs/agents.yaml so the new/edited agent is
// reflected. If the agent's email changed, the old entry is removed first.
func upsertRoster(root string, a *agentForm) error {
	r, err := loadRoster(root)
	if err != nil {
		return err
	}
	// If renaming (email change), drop the old roster entry first.
	if a.OriginalStem != "" {
		oldEnv, err := loadEnvFile(agentEnvPath(root, a.OriginalStem))
		if err == nil {
			if oldEmail, _ := oldEnv.Get("AGENT_EMAIL"); oldEmail != "" && !strings.EqualFold(oldEmail, a.Email) {
				if i := r.findAgent(oldEmail); i >= 0 {
					r.Agents = append(r.Agents[:i], r.Agents[i+1:]...)
				}
			}
		}
		// We're editing — fall through to upsert by current email, even if
		// the old env file has been removed already.
	}
	tools := append([]string(nil), a.Tools...)
	r.upsert(rosterAgent{
		Name:        strings.TrimSpace(a.Name),
		Email:       strings.TrimSpace(a.Email),
		Role:        strings.TrimSpace(a.Role),
		Description: strings.TrimSpace(a.RoleDesc),
		Tools:       tools,
	})
	return r.save(root)
}

// stripCoworkers removes a `{{coworkers}}` token (and any leading newline)
// from a system prompt.
func stripCoworkers(s string) string {
	s = strings.ReplaceAll(s, "\n{{coworkers}}", "")
	s = strings.ReplaceAll(s, "{{coworkers}}", "")
	return strings.TrimRight(s, " \t\n")
}

// ensureCoworkers appends `\n{{coworkers}}` if not already present.
func ensureCoworkers(s string) string {
	if s == "" {
		return s
	}
	if strings.Contains(s, "{{coworkers}}") {
		return s
	}
	if strings.HasSuffix(s, "\n") {
		return s + "{{coworkers}}"
	}
	return s + "\n{{coworkers}}"
}

// commonDefaults represents the editable subset of common.env shown on the
// Agent Defaults page. Fields are formatted as plain strings to fit a form.
type commonDefaults struct {
	EmailServer     string
	SMTPServer      string
	SMTPPort        string
	MaxHistoryChars string
	MaxTokens       string
	AnthropicAPIKey string
	OpenRouterKey   string
	SystemPrompt    string
	AgentTimeout    string
}

func (d *commonDefaults) populateFromEnv(f *envFile) {
	get := func(k string) string { v, _ := f.Get(k); return v }
	d.EmailServer = get("EMAIL_SERVER")
	d.SMTPServer = get("SMTP_SERVER")
	d.SMTPPort = get("SMTP_PORT")
	d.MaxHistoryChars = get("MAX_HISTORY_CHARS")
	d.MaxTokens = get("MAX_TOKENS")
	d.AnthropicAPIKey = get("ANTHROPIC_API_KEY")
	d.OpenRouterKey = get("OPENROUTER_API_KEY")
	d.SystemPrompt = get("SYSTEM_PROMPT")
	d.AgentTimeout = get("AGENT_TIMEOUT")
}

// applyDefaults writes form values into the env file, uncommenting commented
// vars when a value is supplied.
func (d *commonDefaults) applyTo(f *envFile) {
	setOrLeave := func(key, val string) {
		val = strings.TrimSpace(val)
		if val == "" {
			return // leave existing entry as-is
		}
		f.Set(key, val)
	}
	setOrLeave("EMAIL_SERVER", d.EmailServer)
	setOrLeave("SMTP_SERVER", d.SMTPServer)
	setOrLeave("SMTP_PORT", d.SMTPPort)
	setOrLeave("MAX_HISTORY_CHARS", d.MaxHistoryChars)
	setOrLeave("MAX_TOKENS", d.MaxTokens)
	setOrLeave("ANTHROPIC_API_KEY", d.AnthropicAPIKey)
	setOrLeave("OPENROUTER_API_KEY", d.OpenRouterKey)
	setOrLeave("AGENT_TIMEOUT", d.AgentTimeout)
	// SYSTEM_PROMPT may legitimately contain whitespace; preserve as-is when set.
	if strings.TrimSpace(d.SystemPrompt) != "" {
		f.Set("SYSTEM_PROMPT", d.SystemPrompt)
	}
}

// saveCommon persists common.env merging defaults overlay.
func saveCommonDefaults(root string, d *commonDefaults) error {
	f, err := loadCommonEnv(root)
	if err != nil {
		return err
	}
	d.applyTo(f)
	if err := os.MkdirAll(envDir(root), 0o755); err != nil {
		return err
	}
	return f.Save(commonEnvPath(root))
}

// defaultSystemPrompt is the spec-supplied seed when no common.env exists.
const defaultSystemPrompt = `You are a helpful agent that serves two groups — users and coworkers. Your role is to resolve tasks submitted by users, drawing on your coworkers below as needed.
{{coworkers}}`

// commonPromptDefaults returns (SYSTEM_PROMPT, COORDINATOR_SYS_PROMPT) read
// from configs/env/common.env with fallback to configs/env/examples/common.env.
// SYSTEM_PROMPT falls back to the in-code defaultSystemPrompt so callers
// (e.g. loadCommonDefaults) get a usable seed even before common.env exists.
func commonPromptDefaults(root string) (system, coordinator string) {
	f, err := loadCommonEnv(root)
	if err != nil {
		return defaultSystemPrompt, ""
	}
	system, _ = f.Get("SYSTEM_PROMPT")
	if system == "" {
		system = defaultSystemPrompt
	}
	coordinator, _ = f.Get("COORDINATOR_SYS_PROMPT")
	return
}

// loadCommonDefaults builds a commonDefaults from common.env (or the example
// fallback). Missing values get spec-provided defaults.
func loadCommonDefaults(root string) (*commonDefaults, error) {
	f, err := loadCommonEnv(root)
	if err != nil {
		return nil, err
	}
	d := &commonDefaults{}
	d.populateFromEnv(f)
	if d.SMTPPort == "" {
		d.SMTPPort = "587"
	}
	if d.MaxHistoryChars == "" {
		d.MaxHistoryChars = "200000"
	}
	if d.MaxTokens == "" {
		d.MaxTokens = "6000"
	}
	if d.AgentTimeout == "" {
		d.AgentTimeout = "300"
	}
	if d.SystemPrompt == "" {
		d.SystemPrompt = defaultSystemPrompt
	}
	return d, nil
}
