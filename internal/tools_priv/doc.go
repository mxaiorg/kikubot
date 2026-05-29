// Package toolspriv holds company-specific ("private") tool implementations
// that should not ship in the public repo.
//
// These files are gated by presence, not by a build tag: they are not
// committed to the public repository, so a public checkout simply has no
// files here and the package contributes nothing. When the private files are
// present in this directory they are always compiled in — cmd/kikubot
// blank-imports this package unconditionally so each tool's init() runs.
//
// # Secrets convention
//
// Unlike public tools (which declare their env-var-backed credentials in
// internal/config/env_vars.go as exported package vars), private tools read
// their secrets directly with os.Getenv inside this package. The env vars
// still live in configs/secrets.env — which is gitignored and already loaded
// into every container — but no symbol referencing them appears in the public
// repo. That keeps the public codebase unaware that the tool, or its
// credentials, exist.
//
// To add a private tool:
//
//  1. Drop a new file here, e.g. `acme.go`:
//
//     package toolspriv
//
//     import (
//     "log"
//     "os"
//
//     "kikubot/internal/tools"
//     )
//
//     func acme() []tools.ToolDefinition {
//     key := os.Getenv("ACME_API_KEY")
//     if key == "" {
//     log.Println("[acme] ACME_API_KEY not set — Acme tools disabled")
//     return nil
//     }
//     // ...build ToolDefinitions using key...
//     }
//
//     func init() {
//     tools.Register("acme", acme, "Acme Corp integration — ...")
//     }
//
//  2. Add `ACME_API_KEY=...` to configs/secrets.env.
//
//  3. Reference the key ("acme") from the agent's `tools:` list in
//     configs/agents.yaml.
//
// The cmd/kikubot package blank-imports this package so the init() runs.
package toolspriv
