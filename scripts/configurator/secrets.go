package main

import (
	"os"
	"path/filepath"
	"strings"
)

// secretsPath returns the absolute path to configs/secrets.env.
func secretsPath(root string) string {
	return filepath.Join(root, "configs", "secrets.env")
}

// secretsExamplePath returns the example secrets file shipped with the repo,
// used as a fallback template when the live file does not yet exist.
func secretsExamplePath(root string) string {
	return filepath.Join(root, "configs", "secrets-example.env")
}

// loadSecrets reads configs/secrets.env. Falls back to the example template
// when the live file is missing — that way the configurator can present the
// canonical key list to first-run users without forcing them to copy files
// by hand. Never returns nil.
func loadSecrets(root string) *envFile {
	if _, err := os.Stat(secretsPath(root)); err == nil {
		if f, err := loadEnvFile(secretsPath(root)); err == nil {
			return f
		}
	}
	if _, err := os.Stat(secretsExamplePath(root)); err == nil {
		if f, err := loadEnvFile(secretsExamplePath(root)); err == nil {
			return f
		}
	}
	return &envFile{}
}

// saveSecrets writes the secrets file to configs/secrets.env.
func saveSecrets(root string, f *envFile) error {
	if err := os.MkdirAll(filepath.Dir(secretsPath(root)), 0o755); err != nil {
		return err
	}
	return f.Save(secretsPath(root))
}

// emailPasswordKey returns the env-var name used for an agent's mailbox
// password. The container resolves the same key by uppercasing the local-part
// of AGENT_EMAIL (see config.resolveEmailPassword).
func emailPasswordKey(email string) string {
	return strings.ToUpper(emailStem(email)) + "_EMAIL_PASSWORD"
}

// setAgentPassword updates the mailbox password env-var for an agent in
// configs/secrets.env, creating the entry when absent.
func setAgentPassword(root, email, password string) error {
	if strings.TrimSpace(email) == "" {
		return nil
	}
	f := loadSecrets(root)
	key := emailPasswordKey(email)
	if strings.TrimSpace(password) == "" {
		f.Delete(key)
	} else {
		f.Set(key, password)
	}
	return saveSecrets(root, f)
}

// removeAgentPassword drops the per-agent password entry from secrets.env.
func removeAgentPassword(root, email string) error {
	if strings.TrimSpace(email) == "" {
		return nil
	}
	f := loadSecrets(root)
	f.Delete(emailPasswordKey(email))
	return saveSecrets(root, f)
}

// getAgentPassword returns the stored mailbox password for an agent, or "".
func getAgentPassword(root, email string) string {
	if strings.TrimSpace(email) == "" {
		return ""
	}
	f := loadSecrets(root)
	v, _ := f.Get(emailPasswordKey(email))
	return v
}

// hasLLMKeys reports whether ANTHROPIC_API_KEY / OPENROUTER_API_KEY are
// populated in secrets.env. Drives the provider-option enablement on the
// agent form.
func hasLLMKeys(root string) (anthropic, openrouter bool) {
	f := loadSecrets(root)
	has := func(key string) bool {
		v, _ := f.Get(key)
		return strings.TrimSpace(v) != ""
	}
	return has("ANTHROPIC_API_KEY"), has("OPENROUTER_API_KEY")
}
