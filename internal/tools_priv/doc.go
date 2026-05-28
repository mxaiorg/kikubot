//go:build private

// Package toolspriv holds company-specific ("private") tool implementations
// that should not ship in the default build.
//
// Files in this package must carry the `//go:build private` constraint so the
// public binary compiles cleanly without them. Build the private variant with:
//
//	go build -tags=private ./cmd/kikubot
//	go run   -tags="dev private" ./cmd/kikubot
//
// # Secrets convention
//
// Unlike public tools (which declare their env-var-backed credentials in
// internal/config/env_vars.go as exported package vars), private tools read
// their secrets directly with os.Getenv inside this package. The env vars
// still live in configs/secrets.env — which is gitignored and already loaded
// into every container — but no symbol referencing them appears outside the
// `private` build. That keeps the public binary unaware that the tool, or its
// credentials, exist.
//
// To add a private tool:
//
//  1. Drop a new file here, e.g. `acme.go`, with the build tag at the top:
//
//     //go:build private
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
// The cmd/kikubot package has a matching build-tagged blank import that pulls
// this package into the binary so the init() runs.
package toolspriv
