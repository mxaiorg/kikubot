package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// validateEnvFiles checks configs/env: common.env presence and that any
// per-agent <stem>.env file matches an agent's email account.
func validateEnvFiles(root string, af *AgentsFile, r *Report) {
	envDir := filepath.Join(root, "configs", "env")
	sec := r.Section(fmt.Sprintf("env files (%s)", relPath(root, envDir)))

	commonPath := filepath.Join(envDir, "common.env")
	if _, err := os.Stat(commonPath); err != nil {
		if os.IsNotExist(err) {
			sec.Fail("common.env is missing")
		} else {
			sec.Fail("cannot stat common.env: %v", err)
		}
	} else {
		sec.Pass("common.env exists")
	}

	entries, err := os.ReadDir(envDir)
	if err != nil {
		sec.Fail("cannot read env directory: %v", err)
		return
	}

	accounts := map[string]string{}
	if af != nil {
		for _, a := range af.Agents {
			acct := emailAccount(a.Email)
			if acct == "" {
				continue
			}
			accounts[acct] = a.Email
		}
	}

	bad := 0
	matched := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".env") {
			continue
		}
		stem := strings.ToLower(strings.TrimSuffix(name, ".env"))
		if stem == "common" {
			continue
		}
		if _, ok := accounts[stem]; !ok {
			sec.Fail("%s does not match any agent email account in agents.yaml", name)
			bad++
		} else {
			matched++
		}
	}
	if bad == 0 && matched > 0 {
		sec.Pass("%d agent-specific env file(s) match agent email accounts", matched)
	} else if bad == 0 && matched == 0 {
		sec.Pass("no agent-specific env files present (allowed)")
	}
}
