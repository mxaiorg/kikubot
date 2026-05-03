package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Agent struct {
	Name        string   `yaml:"name"`
	Email       string   `yaml:"email"`
	Role        string   `yaml:"role"`
	Description string   `yaml:"description"`
	Tools       []string `yaml:"tools"`
}

type AgentsFile struct {
	Agents []Agent `yaml:"agents"`
}

// loadAgents reads and parses configs/agents.yaml. It returns the parsed file
// (possibly empty on error), and emits findings for missing/malformed input.
func loadAgents(root string, r *Report) (*AgentsFile, bool) {
	path := filepath.Join(root, "configs", "agents.yaml")
	sec := r.Section(fmt.Sprintf("agents.yaml (%s)", relPath(root, path)))

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			sec.Fail("file is missing")
		} else {
			sec.Fail("cannot read file: %v", err)
		}
		return nil, false
	}
	sec.Pass("file exists")

	var af AgentsFile
	if err := yaml.Unmarshal(data, &af); err != nil {
		sec.Fail("file is not well-formed YAML: %v", err)
		return nil, false
	}
	sec.Pass("file is well-formed YAML")

	if len(af.Agents) == 0 {
		sec.Fail("no agents defined")
		return &af, false
	}
	sec.Pass("declares %d agent(s)", len(af.Agents))

	ok := true
	seenEmails := map[string]int{}
	for i, a := range af.Agents {
		label := a.Name
		if label == "" {
			label = fmt.Sprintf("#%d", i+1)
		}
		var missing []string
		if strings.TrimSpace(a.Name) == "" {
			missing = append(missing, "name")
		}
		if strings.TrimSpace(a.Email) == "" {
			missing = append(missing, "email")
		}
		if strings.TrimSpace(a.Role) == "" {
			missing = append(missing, "role")
		}
		if strings.TrimSpace(a.Description) == "" {
			missing = append(missing, "description")
		}
		if a.Tools == nil {
			missing = append(missing, "tools")
		}
		if len(missing) > 0 {
			sec.Fail("agent %q missing required field(s): %s", label, strings.Join(missing, ", "))
			ok = false
			continue
		}
		if !strings.Contains(a.Email, "@") {
			sec.Fail("agent %q has invalid email %q", label, a.Email)
			ok = false
		}
		if prev, dup := seenEmails[strings.ToLower(a.Email)]; dup {
			sec.Fail("agent %q has duplicate email %q (also used by agent #%d)", label, a.Email, prev+1)
			ok = false
		} else {
			seenEmails[strings.ToLower(a.Email)] = i
		}
	}
	if ok {
		sec.Pass("all agents have name, email, role, description, tools")
	}
	return &af, true
}

// emailAccount returns the local part (before @) of an email, lowercased.
func emailAccount(email string) string {
	at := strings.Index(email, "@")
	if at < 0 {
		return strings.ToLower(strings.TrimSpace(email))
	}
	return strings.ToLower(strings.TrimSpace(email[:at]))
}
