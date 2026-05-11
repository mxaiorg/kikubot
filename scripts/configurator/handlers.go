package main

import (
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"strings"
)

type pageData struct {
	Active    string
	Root      string
	Flash     string
	FlashKind string // success|error
	Data      any
}

func (s *server) render(w http.ResponseWriter, r *http.Request, name string, p pageData) {
	p.Root = s.root
	// An inline Flash supplied by the handler (e.g. validation error from a
	// POST that re-renders) takes precedence over the one-shot cookie.
	if p.Flash == "" {
		p.Flash, p.FlashKind = readFlash(r, w)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpls[name].ExecuteTemplate(w, name, p); err != nil {
		log.Printf("render %s: %v", name, err)
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

// flash is a tiny one-shot cookie used to carry success/error notices across
// a redirect. It avoids needing a session store while still letting POST/redirect/GET work cleanly.
const flashCookie = "ac_flash"

func setFlash(w http.ResponseWriter, kind, msg string) {
	v := kind + "\x1f" + msg
	http.SetCookie(w, &http.Cookie{
		Name:  flashCookie,
		Value: v,
		Path:  "/",
	})
}

func readFlash(r *http.Request, w http.ResponseWriter) (msg, kind string) {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return "", ""
	}
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: "", Path: "/", MaxAge: -1})
	parts := strings.SplitN(c.Value, "\x1f", 2)
	if len(parts) != 2 {
		return "", ""
	}
	return parts[1], parts[0]
}

// ---- Home ----

func (s *server) handleHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	s.render(w, r, "home", pageData{Active: "home"})
}

// ---- Agent Defaults ----

func (s *server) handleDefaults(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		d := &commonDefaults{
			EmailServer:     r.FormValue("EMAIL_SERVER"),
			SMTPServer:      r.FormValue("SMTP_SERVER"),
			MaxHistoryChars: r.FormValue("MAX_HISTORY_CHARS"),
			MaxTokens:       r.FormValue("MAX_TOKENS"),
			AnthropicAPIKey: r.FormValue("ANTHROPIC_API_KEY"),
			OpenRouterKey:   r.FormValue("OPENROUTER_API_KEY"),
			SystemPrompt:    r.FormValue("SYSTEM_PROMPT"),
			AgentTimeout:    r.FormValue("AGENT_TIMEOUT"),
		}
		if err := saveCommonDefaults(s.root, d); err != nil {
			setFlash(w, "error", "Save failed: "+err.Error())
		} else {
			setFlash(w, "success", "Saved configs/env/common.env")
		}
		http.Redirect(w, r, "/agents/defaults", http.StatusSeeOther)
		return
	}
	d, err := loadCommonDefaults(s.root)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "defaults", pageData{Active: "defaults", Data: d})
}

// ---- Add / Edit Agent ----

func (s *server) handleAgentNew(w http.ResponseWriter, r *http.Request) {
	common, _ := loadCommonDefaults(s.root)
	a := &agentForm{
		LLMProvider:  "anthropic",
		SystemPrompt: "",
	}
	if common != nil {
		a.SystemPrompt = common.SystemPrompt
	}
	a.ExcludeCollaborators = false
	a.Registry, _ = loadToolRegistry(s.root)
	_, a.CoordinatorPromptDefault = commonPromptDefaults(s.root)
	a.HasAnthropicKey, a.HasOpenRouterKey = llmKeyAvailability(s.root, "")
	s.render(w, r, "agent_form", pageData{Active: "new", Data: a})
}

func (s *server) handleAgentEdit(w http.ResponseWriter, r *http.Request) {
	stem := r.URL.Query().Get("stem")
	if stem == "" {
		http.Redirect(w, r, "/agents/list", http.StatusSeeOther)
		return
	}
	a, err := loadAgentForm(s.root, stem)
	if err != nil {
		setFlash(w, "error", "Load failed: "+err.Error())
		http.Redirect(w, r, "/agents/list", http.StatusSeeOther)
		return
	}
	a.Registry, _ = loadToolRegistry(s.root)
	_, a.CoordinatorPromptDefault = commonPromptDefaults(s.root)
	a.HasAnthropicKey, a.HasOpenRouterKey = llmKeyAvailability(s.root, stem)
	s.render(w, r, "agent_form", pageData{Active: "list", Data: a})
}

func (s *server) handleAgentSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/agents/list", http.StatusSeeOther)
		return
	}
	_ = r.ParseForm()
	a := &agentForm{
		OriginalStem:         r.FormValue("original_stem"),
		Name:                 r.FormValue("name"),
		Email:                r.FormValue("email"),
		EmailPassword:        r.FormValue("email_password"),
		Whitelist:            r.FormValue("whitelist"),
		Blacklist:            r.FormValue("blacklist"),
		LLMProvider:          r.FormValue("llm_provider"),
		LLMModel:             r.FormValue("llm_model"),
		LLMOpenrouterBackup:  r.FormValue("llm_openrouter_backup"),
		SystemPrompt:         r.FormValue("system_prompt"),
		ExcludeCollaborators: r.FormValue("exclude_collaborators") == "1",
		Role:                 r.FormValue("role"),
		RoleDesc:             r.FormValue("role_description"),
		Tools:                splitCSV(r.FormValue("tools")),
	}
	if a.LLMProvider != "openrouter" {
		a.LLMOpenrouterBackup = ""
	}
	if _, err := a.save(s.root); err != nil {
		setFlash(w, "error", "Save failed: "+err.Error())
		// Re-render the form with submitted values so the user doesn't lose input.
		a.Registry, _ = loadToolRegistry(s.root)
		_, a.CoordinatorPromptDefault = commonPromptDefaults(s.root)
		a.HasAnthropicKey, a.HasOpenRouterKey = llmKeyAvailability(s.root, a.OriginalStem)
		s.render(w, r, "agent_form", pageData{Active: "new", Data: a, Flash: err.Error(), FlashKind: "error"})
		return
	}
	setFlash(w, "success", "Saved agent "+a.Name)
	http.Redirect(w, r, "/agents/list", http.StatusSeeOther)
}

// ---- List Agents ----

func (s *server) handleAgentsList(w http.ResponseWriter, r *http.Request) {
	rows, err := listAgents(s.root)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "agents_list", pageData{Active: "list", Data: rows})
}

// ---- Email Service ----

// emailServiceView is the render-time view that combines the postfix config
// with adjacent state (hostname from docker-compose, SSL cert presence).
type emailServiceView struct {
	*emailServiceConfig
	SSL sslCertStatus
}

func (s *server) handleEmailService(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		c := &emailServiceConfig{
			Enabled:         r.FormValue("enabled") == "1",
			Hostname:        strings.TrimSpace(r.FormValue("hostname")),
			AgentDomain:     strings.TrimSpace(r.FormValue("agent_domain")),
			LimitDelivery:   r.FormValue("limit_delivery") == "1",
			DeliveryDomains: splitCSV(r.FormValue("delivery_domains")),
			LimitAccept:     r.FormValue("limit_accept") == "1",
			AcceptDomains:   splitCSV(r.FormValue("accept_domains")),
		}
		if c.Enabled {
			if c.AgentDomain == "" {
				c.AgentDomain = hostnameDomain(c.Hostname)
			}
			if err := c.Save(s.root); err != nil {
				setFlash(w, "error", "Save failed: "+err.Error())
				http.Redirect(w, r, "/email-service", http.StatusSeeOther)
				return
			}
			if err := updateDmsCompose(s.root, c.Hostname, c.AgentDomain); err != nil {
				setFlash(w, "error", "Postfix saved, but docker-compose update failed: "+err.Error())
				http.Redirect(w, r, "/email-service", http.StatusSeeOther)
				return
			}
			setFlash(w, "success", "Saved postfix-transport.cf, postfix-sender-access.cf, and docker-compose.yml")
		} else {
			setFlash(w, "success", "Email service disabled (no files were modified)")
		}
		http.Redirect(w, r, "/email-service", http.StatusSeeOther)
		return
	}
	c := loadEmailServiceConfig(s.root)
	if c.Hostname == "" {
		c.Hostname = dmsHostname(s.root)
	}
	view := emailServiceView{
		emailServiceConfig: c,
		SSL:                loadSSLCertStatus(s.root),
	}
	s.render(w, r, "email_service", pageData{Active: "email", Data: view})
}

// templateFuncs is exposed for parsing.
var templateFuncs = template.FuncMap{
	"joinCSV":       joinCSV,
	"join":          strings.Join,
	"toolsDataAttr": toolsDataAttr,
	"infoIcon":      infoIcon,
}

// infoIcon renders a small clickable info marker that shows `text` in a popover
// on click. Used to attach contextual help to form-field labels.
//
// Rendered as a <span role="button"> rather than a <button> on purpose: when
// the icon lives inside a <label>, a real button would be the first labelable
// descendant and the label would forward all of its clicks to the icon —
// making the entire label row act as a click target for the popover. A span
// is not labelable, so the label's implicit association falls through to the
// input as intended.
func infoIcon(text string) template.HTML {
	esc := template.HTMLEscapeString(text)
	return template.HTML(`<span class="info-icon" data-info="` + esc + `" role="button" tabindex="0" aria-label="Help">ⓘ</span>`)
}

// toolsDataAttr serializes the tool registry to a JSON object for the chip
// UI's `data-tools` attribute. Output is HTML-attribute safe (the template
// engine still escapes it on render — we just want compact JSON).
func toolsDataAttr(infos []toolInfo) template.HTMLAttr {
	if len(infos) == 0 {
		return template.HTMLAttr("{}")
	}
	m := make(map[string]string, len(infos))
	for _, t := range infos {
		m[t.Key] = t.Description
	}
	b, err := json.Marshal(m)
	if err != nil {
		return template.HTMLAttr("{}")
	}
	return template.HTMLAttr(b)
}
