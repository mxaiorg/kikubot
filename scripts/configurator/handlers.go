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

// resolveAgentEmail looks up an agent by either the lowercased local-part of
// the email (legacy "stem" query param) or the full email address. Both forms
// are accepted to keep existing bookmarks working.
func resolveAgentEmail(root, stemOrEmail string) string {
	stemOrEmail = strings.TrimSpace(stemOrEmail)
	if stemOrEmail == "" {
		return ""
	}
	if strings.Contains(stemOrEmail, "@") {
		return stemOrEmail
	}
	want := strings.ToLower(stemOrEmail)
	r, err := loadRoster(root)
	if err != nil {
		return ""
	}
	for _, a := range r.Agents {
		if emailStem(a.Email) == want {
			return a.Email
		}
	}
	return ""
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
			EmailServer:      r.FormValue("EMAIL_SERVER"),
			SMTPServer:       r.FormValue("SMTP_SERVER"),
			EmailInsecureTLS: r.FormValue("EMAIL_INSECURE_TLS") == "1",
			MaxHistoryChars:  r.FormValue("MAX_HISTORY_CHARS"),
			MaxTokens:        r.FormValue("MAX_TOKENS"),
			AgentTimeout:     r.FormValue("AGENT_TIMEOUT"),
			SystemPrompt:     r.FormValue("SYSTEM_PROMPT"),
			AnthropicAPIKey:  r.FormValue("ANTHROPIC_API_KEY"),
			OpenRouterKey:    r.FormValue("OPENROUTER_API_KEY"),
		}
		if err := saveCommonDefaults(s.root, d); err != nil {
			setFlash(w, "error", "Save failed: "+err.Error())
		} else {
			setFlash(w, "success", "Saved configs/agents.yaml (common) and configs/secrets.env")
		}
		http.Redirect(w, r, "/agents/defaults", http.StatusSeeOther)
		return
	}
	d, err := loadCommonDefaults(s.root)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	view := defaultsView{commonDefaults: d, Knowledge: buildKnowledgeView(s.root, knowledgeScopeCommon)}
	s.render(w, r, "defaults", pageData{Active: "defaults", Data: view})
}

// defaultsView combines the editable common: knobs with the common knowledge
// editor so the Agent Defaults page can render both.
type defaultsView struct {
	*commonDefaults
	Knowledge knowledgeView
}

// ---- Add / Edit Agent ----

func (s *server) handleAgentNew(w http.ResponseWriter, r *http.Request) {
	a := newAgentForm(s.root)
	a.Registry, _ = loadToolRegistry(s.root)
	_, a.CoordinatorPromptDefault = commonPromptDefaults(s.root)
	a.HasAnthropicKey, a.HasOpenRouterKey = hasLLMKeys(s.root)
	s.render(w, r, "agent_form", pageData{Active: "new", Data: a})
}

func (s *server) handleAgentEdit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	email := resolveAgentEmail(s.root, q.Get("email"))
	if email == "" {
		email = resolveAgentEmail(s.root, q.Get("stem"))
	}
	if email == "" {
		http.Redirect(w, r, "/agents/list", http.StatusSeeOther)
		return
	}
	a, err := loadAgentForm(s.root, email)
	if err != nil {
		setFlash(w, "error", "Load failed: "+err.Error())
		http.Redirect(w, r, "/agents/list", http.StatusSeeOther)
		return
	}
	a.Registry, _ = loadToolRegistry(s.root)
	_, a.CoordinatorPromptDefault = commonPromptDefaults(s.root)
	a.HasAnthropicKey, a.HasOpenRouterKey = hasLLMKeys(s.root)
	a.Knowledge = buildKnowledgeView(s.root, email)
	s.render(w, r, "agent_form", pageData{Active: "list", Data: a})
}

func (s *server) handleAgentSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/agents/list", http.StatusSeeOther)
		return
	}
	_ = r.ParseForm()
	original := r.FormValue("original_email")
	if original == "" {
		// Accept legacy 'original_stem' field — older templates still post it.
		original = resolveAgentEmail(s.root, r.FormValue("original_stem"))
	}
	a := &agentForm{
		OriginalEmail:        original,
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
		a.Registry, _ = loadToolRegistry(s.root)
		_, a.CoordinatorPromptDefault = commonPromptDefaults(s.root)
		a.HasAnthropicKey, a.HasOpenRouterKey = hasLLMKeys(s.root)
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

// ---- External Partners ----

func (s *server) handleExternalList(w http.ResponseWriter, r *http.Request) {
	rows, err := listExternalAgents(s.root)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "external_list", pageData{Active: "external", Data: rows})
}

func (s *server) handleExternalNew(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "external_form", pageData{Active: "external-new", Data: &externalForm{}})
}

func (s *server) handleExternalEdit(w http.ResponseWriter, r *http.Request) {
	email := resolveExternalEmail(s.root, r.URL.Query().Get("email"))
	if email == "" {
		http.Redirect(w, r, "/agents/external", http.StatusSeeOther)
		return
	}
	e, err := loadExternalForm(s.root, email)
	if err != nil {
		setFlash(w, "error", "Load failed: "+err.Error())
		http.Redirect(w, r, "/agents/external", http.StatusSeeOther)
		return
	}
	s.render(w, r, "external_form", pageData{Active: "external", Data: e})
}

func (s *server) handleExternalSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/agents/external", http.StatusSeeOther)
		return
	}
	_ = r.ParseForm()
	e := &externalForm{
		OriginalEmail: r.FormValue("original_email"),
		Name:          r.FormValue("name"),
		Email:         r.FormValue("email"),
		Role:          r.FormValue("role"),
		Description:   r.FormValue("description"),
	}
	if _, err := e.save(s.root); err != nil {
		s.render(w, r, "external_form", pageData{Active: "external", Data: e, Flash: err.Error(), FlashKind: "error"})
		return
	}
	setFlash(w, "success", "Saved external partner "+e.Name)
	http.Redirect(w, r, "/agents/external", http.StatusSeeOther)
}

func (s *server) handleExternalDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/agents/external", http.StatusSeeOther)
		return
	}
	_ = r.ParseForm()
	email := strings.TrimSpace(r.FormValue("email"))
	rr, err := loadRoster(s.root)
	if err != nil {
		setFlash(w, "error", "Delete failed: "+err.Error())
		http.Redirect(w, r, "/agents/external", http.StatusSeeOther)
		return
	}
	if findExternalIndex(rr, email) < 0 {
		setFlash(w, "error", "No external partner with email "+email)
		http.Redirect(w, r, "/agents/external", http.StatusSeeOther)
		return
	}
	removeExternal(rr, email)
	if err := saveRoster(s.root, rr); err != nil {
		setFlash(w, "error", "Delete failed: "+err.Error())
		http.Redirect(w, r, "/agents/external", http.StatusSeeOther)
		return
	}
	setFlash(w, "success", "Deleted external partner "+email)
	http.Redirect(w, r, "/agents/external", http.StatusSeeOther)
}

// resolveExternalEmail accepts a full email (the only id used for external
// partners) and returns it if present in the `external:` roster, else "".
func resolveExternalEmail(root, email string) string {
	email = strings.TrimSpace(email)
	if email == "" {
		return ""
	}
	r, err := loadRoster(root)
	if err != nil {
		return ""
	}
	if findExternalIndex(r, email) >= 0 {
		return email
	}
	return ""
}

// ---- Email Service ----

// emailServiceView is the render-time view that combines the postfix config
// with adjacent state (hostname from docker-compose, SSL cert presence).
//
// Dirty is set when the rendered form values differ from what's on disk —
// used after the cert-generation side-action re-renders with unsaved edits
// so the template can mark the form pre-dirty (Save button enabled).
type emailServiceView struct {
	*emailServiceConfig
	SSL   sslCertStatus
	Dirty bool
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
			if err := regenerateDmsAccounts(s.root); err != nil {
				setFlash(w, "error", "Postfix saved, but mailbox accounts file update failed: "+err.Error())
				http.Redirect(w, r, "/email-service", http.StatusSeeOther)
				return
			}
			if err := seedCommonMailServersFromDMS(s.root, c.Hostname); err != nil {
				setFlash(w, "error", "Postfix saved, but updating common defaults failed: "+err.Error())
				http.Redirect(w, r, "/email-service", http.StatusSeeOther)
				return
			}
			setFlash(w, "success", "Saved postfix-transport.cf, postfix-sender-access.cf, postfix-main.cf, docker-compose.yml, and postfix-accounts.cf")
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

// handleEmailServiceCert generates a self-signed cert from the submitted
// hostname/agent_domain values and re-renders the page. Uses the submitted
// form values (not the on-disk config) so unsaved edits to the hostname/domain
// are honoured and preserved in the re-rendered form.
func (s *server) handleEmailServiceCert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/email-service", http.StatusSeeOther)
		return
	}
	_ = r.ParseForm()
	hostname := strings.TrimSpace(r.FormValue("hostname"))
	domain := strings.TrimSpace(r.FormValue("agent_domain"))
	if domain == "" {
		domain = hostnameDomain(hostname)
	}

	c := &emailServiceConfig{
		Enabled:         r.FormValue("enabled") == "1",
		Hostname:        hostname,
		AgentDomain:     domain,
		LimitDelivery:   r.FormValue("limit_delivery") == "1",
		DeliveryDomains: splitCSV(r.FormValue("delivery_domains")),
		LimitAccept:     r.FormValue("limit_accept") == "1",
		AcceptDomains:   splitCSV(r.FormValue("accept_domains")),
	}

	var flashKind, flashMsg string
	if err := generateSelfSignedCert(s.root, hostname, domain); err != nil {
		flashKind = "error"
		flashMsg = "Certificate generation failed: " + err.Error()
	} else if err := setCommonInsecureTLS(s.root, true); err != nil {
		flashKind = "error"
		flashMsg = "Certificate generated, but enabling email_insecure_tls in agents.yaml failed: " + err.Error()
	} else {
		flashKind = "success"
		flashMsg = "Generated self-signed certificate in services/dms/certs/ and set common.email_insecure_tls=true in agents.yaml."
	}

	view := emailServiceView{
		emailServiceConfig: c,
		SSL:                loadSSLCertStatus(s.root),
		Dirty:              !emailServiceConfigEqual(c, loadEmailServiceConfig(s.root)),
	}
	s.render(w, r, "email_service", pageData{Active: "email", Data: view, Flash: flashMsg, FlashKind: flashKind})
}

// emailServiceConfigEqual reports whether two configs are field-for-field
// equal, treating nil and empty slices as equivalent.
func emailServiceConfigEqual(a, b *emailServiceConfig) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Enabled != b.Enabled ||
		a.Hostname != b.Hostname ||
		a.AgentDomain != b.AgentDomain ||
		a.LimitDelivery != b.LimitDelivery ||
		a.LimitAccept != b.LimitAccept {
		return false
	}
	return stringSliceEqual(a.DeliveryDomains, b.DeliveryDomains) &&
		stringSliceEqual(a.AcceptDomains, b.AcceptDomains)
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---- Knowledge ----

// renderKnowledge writes the standalone knowledge editor partial — the HTMX
// swap target for save/delete responses.
func (s *server) renderKnowledge(w http.ResponseWriter, v knowledgeView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpls["knowledge_editor"].ExecuteTemplate(w, "knowledge_editor", v); err != nil {
		log.Printf("render knowledge: %v", err)
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

// handleKnowledge renders the editor for a scope (GET ?scope=common|<email>).
func (s *server) handleKnowledge(w http.ResponseWriter, r *http.Request) {
	s.renderKnowledge(w, buildKnowledgeView(s.root, r.URL.Query().Get("scope")))
}

// handleKnowledgeSave creates/updates/renames a knowledge file, then re-renders
// the editor. On error the attempted edit is preserved in the re-rendered form
// so the operator doesn't lose what they typed.
func (s *server) handleKnowledgeSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.renderKnowledge(w, buildKnowledgeView(s.root, r.URL.Query().Get("scope")))
		return
	}
	_ = r.ParseForm()
	scope := r.FormValue("scope")
	oldName := r.FormValue("oldname")
	name := r.FormValue("name")
	content := r.FormValue("content")

	err := saveKnowledgeFile(s.root, scope, oldName, name, content)
	v := buildKnowledgeView(s.root, scope)
	if err != nil {
		// Re-inject the attempted edit so it survives the re-render.
		if strings.TrimSpace(oldName) == "" {
			v.Draft = knowledgeFile{Name: name, Content: content}
		} else {
			for i := range v.Files {
				if v.Files[i].Name == oldName {
					v.Files[i].Name = name
					v.Files[i].Content = content
					break
				}
			}
		}
		v.Flash, v.FlashKind = "Save failed: "+err.Error(), "error"
	} else {
		saved := name
		if n, nErr := validKnowledgeName(name); nErr == nil {
			saved = n
		}
		v.Flash, v.FlashKind = "Saved "+v.Dir+"/"+saved+knowledgeReloadNote(s.root, scope), "success"
	}
	s.renderKnowledge(w, v)
}

// handleKnowledgeDelete removes a knowledge file, then re-renders the editor.
func (s *server) handleKnowledgeDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.renderKnowledge(w, buildKnowledgeView(s.root, r.URL.Query().Get("scope")))
		return
	}
	_ = r.ParseForm()
	scope := r.FormValue("scope")
	name := r.FormValue("name")
	err := deleteKnowledgeFile(s.root, scope, name)
	v := buildKnowledgeView(s.root, scope)
	if err != nil {
		v.Flash, v.FlashKind = "Delete failed: "+err.Error(), "error"
	} else {
		v.Flash, v.FlashKind = "Deleted "+name+knowledgeReloadNote(s.root, scope), "success"
	}
	s.renderKnowledge(w, v)
}

// templateFuncs is exposed for parsing.
var templateFuncs = template.FuncMap{
	"joinCSV":          joinCSV,
	"join":             strings.Join,
	"toolsDataAttr":    toolsDataAttr,
	"privateToolsAttr": privateToolsAttr,
	"mcpToolsAttr":     mcpToolsAttr,
	"infoIcon":         infoIcon,
	"add":              func(a, b int) int { return a + b },
}

// privateToolsAttr returns a comma-separated list of the registry keys that
// are private (registered from internal/tools_priv). The chip UI uses it to
// badge those tools as only-available-in-a-private-build.
func privateToolsAttr(infos []toolInfo) string {
	var keys []string
	for _, t := range infos {
		if t.Private {
			keys = append(keys, t.Key)
		}
	}
	return strings.Join(keys, ",")
}

// mcpToolsAttr returns a comma-separated list of the registry keys that are
// backed by a remote MCP server (declared in configs/mcp_servers.yaml). The
// chip UI uses it to badge those tools so the operator knows the key needs a
// catalog entry plus credentials, not just selection.
func mcpToolsAttr(infos []toolInfo) string {
	var keys []string
	for _, t := range infos {
		if t.MCP {
			keys = append(keys, t.Key)
		}
	}
	return strings.Join(keys, ",")
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
// UI's `data-tools` attribute.
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
