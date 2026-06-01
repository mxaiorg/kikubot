package main

import (
	"fmt"
	"sort"
	"strings"

	"kikubot/internal/config"
)

// externalSummary is a row in the external-partners list.
type externalSummary struct {
	Name        string
	Email       string
	Role        string
	Description string
	// Uncovered lists the names of in-roster agents running in whitelist mode
	// that do NOT admit this partner's address/domain. Those agents can
	// delegate to the partner (the external roster relaxes the outbound send
	// gate) but will bounce the partner's replies at ACL — a silent
	// dead-end. Empty means every whitelist-mode agent already covers it.
	Uncovered []string
}

// listExternalAgents returns the `external:` roster from configs/agents.yaml,
// sorted by name, each annotated with any whitelist coverage gaps.
func listExternalAgents(root string) ([]externalSummary, error) {
	r, err := loadRoster(root)
	if err != nil {
		return nil, err
	}
	out := make([]externalSummary, 0, len(r.External))
	for _, e := range r.External {
		if strings.TrimSpace(e.Email) == "" {
			continue
		}
		out = append(out, externalSummary{
			Name:        strings.TrimSpace(e.Name),
			Email:       e.Email,
			Role:        e.Role,
			Description: e.Description,
			Uncovered:   externalWhitelistGaps(r, e.Email),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// externalForm captures the user-submitted state of the Add/Edit External
// Partner form. External partners run on other machines/domains and are never
// executed here, so the form carries identity + description only — no tools,
// budgets, LLM, ACL or password fields apply.
type externalForm struct {
	OriginalEmail string // empty for a new partner
	Name          string
	Email         string
	Role          string
	Description   string
}

// validate runs basic input checks.
func (e *externalForm) validate() error {
	if strings.TrimSpace(e.Name) == "" {
		return fmt.Errorf("partner name is required")
	}
	if strings.TrimSpace(e.Email) == "" {
		return fmt.Errorf("partner email is required")
	}
	if !strings.Contains(e.Email, "@") {
		return fmt.Errorf("partner email must include @")
	}
	if strings.TrimSpace(e.Role) == "" {
		return fmt.Errorf("role is required")
	}
	if strings.TrimSpace(e.Description) == "" {
		return fmt.Errorf("description is required")
	}
	return nil
}

// loadExternalForm fills the form from configs/agents.yaml.
func loadExternalForm(root, email string) (*externalForm, error) {
	r, err := loadRoster(root)
	if err != nil {
		return nil, err
	}
	idx := findExternalIndex(r, email)
	if idx < 0 {
		return nil, fmt.Errorf("external partner %q not found in roster", email)
	}
	e := r.External[idx]
	return &externalForm{
		OriginalEmail: e.Email,
		Name:          e.Name,
		Email:         e.Email,
		Role:          e.Role,
		Description:   e.Description,
	}, nil
}

// save persists the partner to the `external:` block of configs/agents.yaml.
// Returns the partner's email on success.
func (e *externalForm) save(root string) (string, error) {
	if err := e.validate(); err != nil {
		return "", err
	}
	r, err := loadRoster(root)
	if err != nil {
		return "", err
	}
	def := config.ExternalAgent{
		Name:        strings.TrimSpace(e.Name),
		Email:       strings.TrimSpace(e.Email),
		Role:        strings.TrimSpace(e.Role),
		Description: strings.TrimSpace(e.Description),
	}
	// Drop the old entry if the email changed.
	if e.OriginalEmail != "" && !strings.EqualFold(e.OriginalEmail, def.Email) {
		removeExternal(r, e.OriginalEmail)
	}
	upsertExternal(r, def)
	if err := saveRoster(root, r); err != nil {
		return "", err
	}
	return def.Email, nil
}

// findExternalIndex returns the index of the external partner matching email
// (case-insensitive), or -1.
func findExternalIndex(r *roster, email string) int {
	want := strings.ToLower(strings.TrimSpace(email))
	for i, e := range r.External {
		if strings.ToLower(strings.TrimSpace(e.Email)) == want {
			return i
		}
	}
	return -1
}

// upsertExternal adds or updates an entry. The match key is email.
func upsertExternal(r *roster, e config.ExternalAgent) {
	if i := findExternalIndex(r, e.Email); i >= 0 {
		r.External[i] = e
		return
	}
	r.External = append(r.External, e)
}

// removeExternal drops the entry matching email (case-insensitive). No-op when
// the email isn't present.
func removeExternal(r *roster, email string) {
	if i := findExternalIndex(r, email); i >= 0 {
		r.External = append(r.External[:i], r.External[i+1:]...)
	}
}

// externalWhitelistGaps returns the names of in-roster agents running in
// whitelist mode that do not admit extEmail. Agents without a whitelist
// (blacklist or open mode) accept the partner by default and never appear.
func externalWhitelistGaps(r *roster, extEmail string) []string {
	var gaps []string
	for _, a := range r.Agents {
		if len(a.Whitelist) == 0 {
			continue // blacklist or open mode — admits the partner by default
		}
		if !matchesWhitelistEntry(extEmail, a.Whitelist) {
			name := strings.TrimSpace(a.Name)
			if name == "" {
				name = a.Email
			}
			gaps = append(gaps, name)
		}
	}
	return gaps
}

// matchesWhitelistEntry mirrors the runtime ACL match (internal/agents/acl.go):
// entries containing '@' match the full address; entries without '@' match the
// sender's domain. All comparisons are case-insensitive.
func matchesWhitelistEntry(email string, list []string) bool {
	email = strings.ToLower(strings.TrimSpace(email))
	domain := ""
	if at := strings.LastIndex(email, "@"); at >= 0 {
		domain = email[at+1:]
	}
	for _, w := range list {
		w = strings.ToLower(strings.TrimSpace(w))
		if w == "" {
			continue
		}
		if strings.Contains(w, "@") {
			if w == email {
				return true
			}
		} else if domain != "" && w == domain {
			return true
		}
	}
	return false
}
