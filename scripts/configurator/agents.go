package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"kikubot/internal/config"

	"gopkg.in/yaml.v3"
)

// agentSummary is a row in the agents list.
type agentSummary struct {
	Stem  string // file/url stem (local-part of email, lowercased)
	Name  string
	Email string
	Role  string
}

// listAgents returns the roster from configs/agents.yaml, sorted by name.
func listAgents(root string) ([]agentSummary, error) {
	r, err := loadRoster(root)
	if err != nil {
		return nil, err
	}
	out := make([]agentSummary, 0, len(r.Agents))
	for _, a := range r.Agents {
		if strings.TrimSpace(a.Email) == "" {
			continue
		}
		out = append(out, agentSummary{
			Stem:  emailStem(a.Email),
			Name:  strings.TrimSpace(a.Name),
			Email: a.Email,
			Role:  a.Role,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// emailStem returns the lowercased local-part of an email, used as both the
// URL/form id for an agent and the prefix of <STEM>_EMAIL_PASSWORD in
// secrets.env.
func emailStem(email string) string {
	email = strings.TrimSpace(email)
	if at := strings.Index(email, "@"); at > 0 {
		return strings.ToLower(email[:at])
	}
	return strings.ToLower(email)
}

// agentForm captures the user-submitted state of the Add/Edit Agent form.
type agentForm struct {
	OriginalEmail string // empty for new agent
	Name          string
	Email         string
	EmailPassword string
	Whitelist     string // CSV
	Blacklist     string // CSV

	// LLM
	LLMProvider         string
	LLMModel            string
	LLMOpenrouterBackup string

	SystemPrompt         string
	ExcludeCollaborators bool

	// Roster entry
	Role     string
	RoleDesc string
	Tools    []string

	// Render-only.
	Registry                 []toolInfo `json:"-"`
	CoordinatorPromptDefault string     `json:"-"`
	HasAnthropicKey          bool       `json:"-"`
	HasOpenRouterKey         bool       `json:"-"`
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

// loadAgentForm fills the form from configs/agents.yaml + secrets.env.
func loadAgentForm(root, email string) (*agentForm, error) {
	r, err := loadRoster(root)
	if err != nil {
		return nil, err
	}
	idx := findAgentIndex(r, email)
	if idx < 0 {
		return nil, fmt.Errorf("agent %q not found in roster", email)
	}
	a := r.Agents[idx]

	f := &agentForm{
		OriginalEmail:       a.Email,
		Name:                a.Name,
		Email:               a.Email,
		EmailPassword:       getAgentPassword(root, a.Email),
		Whitelist:           strings.Join(a.Whitelist, ", "),
		Blacklist:           strings.Join(a.Blacklist, ", "),
		LLMProvider:         a.LLMProvider,
		LLMModel:            a.LLMModel,
		LLMOpenrouterBackup: strings.Join(a.LLMOpenRouterBackup, ", "),
		SystemPrompt:        a.SystemPrompt,
		Role:                a.Role,
		RoleDesc:            a.Description,
		Tools:               append([]string(nil), a.Tools...),
	}
	if f.LLMProvider == "" {
		f.LLMProvider = "anthropic"
	}
	if f.SystemPrompt == "" {
		// Inherit common's system prompt onto the form so the operator can
		// edit it as-an-override; if they don't change it, save() will skip
		// writing it back (no point in storing identical text).
		f.SystemPrompt = r.Common.SystemPrompt
	}
	f.ExcludeCollaborators = f.SystemPrompt != "" && !strings.Contains(f.SystemPrompt, "{{coworkers}}")
	return f, nil
}

// newAgentForm seeds a blank form with the common defaults.
func newAgentForm(root string) *agentForm {
	r, _ := loadRoster(root)
	f := &agentForm{LLMProvider: "anthropic"}
	if r != nil {
		f.SystemPrompt = r.Common.SystemPrompt
	}
	return f
}

// save persists the agent to configs/agents.yaml (overrides only — values
// equal to common are omitted) and writes the mailbox password to
// configs/secrets.env. Returns the lowercase email stem on success.
func (a *agentForm) save(root string) (newEmail string, err error) {
	if err := a.validate(); err != nil {
		return "", err
	}
	r, err := loadRoster(root)
	if err != nil {
		return "", err
	}

	prompt := a.SystemPrompt
	if a.ExcludeCollaborators {
		prompt = stripCoworkers(prompt)
	} else {
		prompt = ensureCoworkers(prompt)
	}

	// Start from the existing entry (if any) so per-agent overrides that
	// don't surface on the form (max_turns, max_history_chars, etc.) are
	// preserved across an edit.
	var def config.AgentDef
	if a.OriginalEmail != "" {
		if i := findAgentIndex(r, a.OriginalEmail); i >= 0 {
			def = r.Agents[i]
		}
	}
	def.Name = strings.TrimSpace(a.Name)
	def.Email = strings.TrimSpace(a.Email)
	def.Role = strings.TrimSpace(a.Role)
	def.Description = strings.TrimSpace(a.RoleDesc)
	def.Tools = append([]string(nil), a.Tools...)

	// Form-controlled fields. A blank value clears the override (the runtime
	// then inherits the common: default).
	def.LLMProvider = strings.TrimSpace(a.LLMProvider)
	def.LLMModel = strings.TrimSpace(a.LLMModel)
	if a.LLMProvider == "openrouter" {
		def.LLMOpenRouterBackup = splitCSV(a.LLMOpenrouterBackup)
	} else {
		def.LLMOpenRouterBackup = nil
	}
	if v := strings.TrimSpace(prompt); v != "" && v != strings.TrimSpace(r.Common.SystemPrompt) {
		def.SystemPrompt = prompt
	} else {
		def.SystemPrompt = ""
	}
	def.Whitelist = splitCSV(a.Whitelist)
	def.Blacklist = splitCSV(a.Blacklist)
	if def.Whitelist != nil {
		def.Blacklist = nil // form already enforces this, double-checked here
	}

	// Drop the old entry if the email changed.
	if a.OriginalEmail != "" && !strings.EqualFold(a.OriginalEmail, def.Email) {
		removeAgent(r, a.OriginalEmail)
		if err := removeAgentPassword(root, a.OriginalEmail); err != nil {
			return "", fmt.Errorf("removing old password: %w", err)
		}
	}
	upsertAgent(r, def)
	if err := saveRoster(root, r); err != nil {
		return "", err
	}
	if err := setAgentPassword(root, def.Email, a.EmailPassword); err != nil {
		return "", fmt.Errorf("writing password: %w", err)
	}
	if err := regenerateCompose(root); err != nil {
		return def.Email, fmt.Errorf("roster saved but docker-compose.yml update failed: %w", err)
	}
	if err := regenerateDmsAccounts(root); err != nil {
		return def.Email, fmt.Errorf("roster saved but DMS accounts file update failed: %w", err)
	}
	return def.Email, nil
}

// stripCoworkers removes a {{coworkers}} token from a system prompt.
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

// commonDefaults represents the editable subset of common: in agents.yaml.
type commonDefaults struct {
	EmailServer      string
	SMTPServer       string
	EmailInsecureTLS bool
	MaxHistoryChars  string
	MaxTokens        string
	AgentTimeout     string
	SystemPrompt     string

	// Secrets — surfaced on the same form for ergonomic first-run setup,
	// but stored in configs/secrets.env, not agents.yaml.
	AnthropicAPIKey string
	OpenRouterKey   string
}

// loadCommonDefaults builds a commonDefaults from agents.yaml + secrets.env.
func loadCommonDefaults(root string) (*commonDefaults, error) {
	r, err := loadRoster(root)
	if err != nil {
		return nil, err
	}
	s := loadSecrets(root)

	d := &commonDefaults{
		EmailServer:      r.Common.EmailServer,
		SMTPServer:       r.Common.SmtpServer,
		EmailInsecureTLS: r.Common.EmailInsecureTLS,
		SystemPrompt:     r.Common.SystemPrompt,
	}
	if r.Common.MaxHistoryChars > 0 {
		d.MaxHistoryChars = strconv.Itoa(r.Common.MaxHistoryChars)
	} else {
		d.MaxHistoryChars = "200000"
	}
	if r.Common.MaxTokens > 0 {
		d.MaxTokens = strconv.Itoa(r.Common.MaxTokens)
	} else {
		d.MaxTokens = "6000"
	}
	if r.Common.AgentTimeout > 0 {
		d.AgentTimeout = strconv.Itoa(r.Common.AgentTimeout)
	} else {
		d.AgentTimeout = "300"
	}
	if strings.TrimSpace(d.SystemPrompt) == "" {
		d.SystemPrompt = defaultSystemPrompt
	}
	d.AnthropicAPIKey, _ = s.Get("ANTHROPIC_API_KEY")
	d.OpenRouterKey, _ = s.Get("OPENROUTER_API_KEY")
	return d, nil
}

// saveCommonDefaults persists the form: knob values into agents.yaml's
// common: block, API keys into secrets.env.
func saveCommonDefaults(root string, d *commonDefaults) error {
	r, err := loadRoster(root)
	if err != nil {
		return err
	}
	if v := strings.TrimSpace(d.EmailServer); v != "" {
		r.Common.EmailServer = v
	}
	if v := strings.TrimSpace(d.SMTPServer); v != "" {
		r.Common.SmtpServer = v
	}
	r.Common.EmailInsecureTLS = d.EmailInsecureTLS
	if v := strings.TrimSpace(d.MaxHistoryChars); v != "" {
		if n, parseErr := strconv.Atoi(v); parseErr == nil && n > 0 {
			r.Common.MaxHistoryChars = n
		}
	}
	if v := strings.TrimSpace(d.MaxTokens); v != "" {
		if n, parseErr := strconv.Atoi(v); parseErr == nil && n > 0 {
			r.Common.MaxTokens = n
		}
	}
	if v := strings.TrimSpace(d.AgentTimeout); v != "" {
		if n, parseErr := strconv.Atoi(v); parseErr == nil && n > 0 {
			r.Common.AgentTimeout = n
		}
	}
	if strings.TrimSpace(d.SystemPrompt) != "" {
		r.Common.SystemPrompt = d.SystemPrompt
	}
	if err := saveRoster(root, r); err != nil {
		return err
	}

	s := loadSecrets(root)
	if strings.TrimSpace(d.AnthropicAPIKey) != "" {
		s.Set("ANTHROPIC_API_KEY", d.AnthropicAPIKey)
	}
	if strings.TrimSpace(d.OpenRouterKey) != "" {
		s.Set("OPENROUTER_API_KEY", d.OpenRouterKey)
	}
	return saveSecrets(root, s)
}

// commonPromptDefaults returns (SystemPrompt, CoordinatorSystemPrompt) from
// agents.yaml's common: block, falling back to the in-code default for the
// base system prompt and to configs/agents-example.yaml's common block for
// the coordinator prompt when agents.yaml doesn't define one.
func commonPromptDefaults(root string) (system, coordinator string) {
	r, err := loadRoster(root)
	if err == nil && r != nil {
		system = r.Common.SystemPrompt
		coordinator = r.Common.CoordinatorSystemPrompt
	}
	if system == "" {
		system = defaultSystemPrompt
	}
	if coordinator == "" {
		coordinator = exampleCoordinatorPrompt(root)
	}
	return
}

// exampleCoordinatorPrompt loads coordinator_system_prompt from
// configs/agents-example.yaml's common: block. Returns "" if the file is
// missing or unparseable — the example file is the source of the seed prompt
// surfaced by the "Paste Coordinator prompt" button.
func exampleCoordinatorPrompt(root string) string {
	b, err := os.ReadFile(filepath.Join(root, "configs", "agents-example.yaml"))
	if err != nil {
		return ""
	}
	var r roster
	if err := yaml.Unmarshal(b, &r); err != nil {
		return ""
	}
	return r.Common.CoordinatorSystemPrompt
}

// defaultSystemPrompt is the seed used when agents.yaml hasn't been written yet.
const defaultSystemPrompt = `You are a helpful agent that serves two groups — users and coworkers. Your role is to resolve tasks submitted by users, drawing on your coworkers below as needed.
{{coworkers}}`
