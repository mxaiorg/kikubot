package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// validateSecrets verifies that configs/secrets.env exists and provides at
// least one of ANTHROPIC_API_KEY / OPENROUTER_API_KEY, plus a mailbox
// password for every locally-deployed agent.
//
// Missing per-tool credentials are intentionally not validated here — the
// operator may legitimately leave them blank for tools they don't use. Only
// the LLM keys and mailbox passwords are required to boot.
func validateSecrets(root string, af *AgentsFile, localAccounts map[string]bool, r *Report) {
	path := filepath.Join(root, "configs", "secrets.env")
	sec := r.Section(fmt.Sprintf("secrets.env (%s)", relPath(root, path)))

	values, err := readEnvFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			sec.Fail("file is missing — copy configs/secrets-example.env and fill in")
		} else {
			sec.Fail("cannot read file: %v", err)
		}
		return
	}
	sec.Pass("file exists")

	hasAnthropic := strings.TrimSpace(values["ANTHROPIC_API_KEY"]) != ""
	hasOpenRouter := strings.TrimSpace(values["OPENROUTER_API_KEY"]) != ""
	switch {
	case hasAnthropic && hasOpenRouter:
		sec.Pass("ANTHROPIC_API_KEY and OPENROUTER_API_KEY are set")
	case hasAnthropic:
		sec.Pass("ANTHROPIC_API_KEY is set")
	case hasOpenRouter:
		sec.Pass("OPENROUTER_API_KEY is set")
	default:
		sec.Fail("neither ANTHROPIC_API_KEY nor OPENROUTER_API_KEY is set — at least one is required")
	}

	if af == nil {
		return
	}

	// Mailbox password presence per locally-deployed agent.
	missing := 0
	checked := 0
	for _, a := range af.Agents {
		acct := emailAccount(a.Email)
		if acct == "" {
			continue
		}
		if len(localAccounts) > 0 && !localAccounts[acct] {
			continue // not deployed on this machine
		}
		checked++
		key := strings.ToUpper(acct) + "_EMAIL_PASSWORD"
		if strings.TrimSpace(values[key]) == "" {
			sec.Fail("%s is empty (required for agent %s)", key, a.Email)
			missing++
		}
	}
	if checked > 0 && missing == 0 {
		sec.Pass("mailbox password set for all %d locally-deployed agent(s)", checked)
	}
}

// readEnvFile is a minimal .env parser: KEY=VALUE per line, # comments and
// surrounding quotes stripped from values. Sufficient to spot-check secrets
// presence — we do not interpret backslash escapes or multi-line values.
func readEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, `"'`)
		out[key] = val
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
