# Private tools (`internal/tools_priv/`)

This package holds **company-specific ("private") tool implementations** that
should not ship in the public repository. They behave identically to the
built-in tools in [`internal/tools/`](../tools/) at runtime — list a key in an
agent's `tools:` array in `agents.yaml` and the agent gets the tool — but their
source (and the credentials they use) never lands in a public checkout.

The canonical, always-up-to-date reference is the package doc comment in
[`doc.go`](doc.go). This README expands on it with the *why* and the full
mechanics.

## How it works

- **Presence-based, not build-tagged.** `cmd/kikubot` blank-imports this package
  unconditionally ([`cmd/kikubot/main.go`](../../cmd/kikubot/main.go) →
  `_ "kikubot/internal/tools_priv"`). When private `.go` files are present here
  they are always compiled and their `init()` functions run; on a clean public
  checkout the directory has only `doc.go`/this README and the package
  contributes nothing. There is no build flag to remember.
- **Self-registration via `init()`.** Each tool file registers itself by calling
  `tools.Register(key, factory, description)` from an `init()`. `Register` adds
  the factory to the same registry the public `LookupTools` reads, so the new
  key is available before any agent is constructed.
- **Secrets stay private too.** Public tools declare their env-var-backed
  credentials as exported package vars in
  [`internal/config/env_vars.go`](../config/env_vars.go). Private tools instead
  read their secrets directly with `os.Getenv` *inside this package*. The
  variables still live in `configs/secrets.env` (gitignored, already loaded into
  every container), but no symbol referencing them appears in the public repo —
  so the public codebase stays unaware the tool, or its credentials, even exist.

## Adding a private tool

### 1. Drop a new file here

Create `internal/tools_priv/acme.go`:

```go
package toolspriv

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"kikubot/internal/tools"
)

// acme builds the Acme Corp tool set. Returns nil (tool disabled) when the
// credential is missing — this is the toolspriv convention so a container that
// hasn't been given the secret simply runs without the tool instead of failing.
func acme() []tools.ToolDefinition {
	key := os.Getenv("ACME_API_KEY")
	if key == "" {
		log.Println("[acme] ACME_API_KEY not set — Acme tools disabled")
		return nil
	}
	return []tools.ToolDefinition{
		{
			Name:        "acme_lookup",
			Description: "Look something up in Acme. Describe inputs and outputs clearly — the model only sees this text.",
			InputSchema: []byte(`{
				"type":"object",
				"properties":{
					"id":{"type":"string","description":"The Acme record id."}
				},
				"required":["id"]
			}`),
			Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
				var p struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(input, &p); err != nil {
					return "", fmt.Errorf("parsing input: %w", err)
				}
				// ...call Acme with key, return a string (usually JSON)...
				return "{}", nil
			},
			// Prefer StaticSystem for email-independent guidance — it lands in the
			// cacheable prompt prefix. Use System(email) only when the text must
			// vary per inbound email.
			StaticSystem: "- Use acme_lookup when the user references an Acme record id.\n",
		},
	}
}

func init() {
	tools.Register("acme", acme,
		"Acme Corp integration — looks up Acme records by id.")
}
```

### 2. Add the secret

Add the credential to `configs/secrets.env` (gitignored, loaded by every
container as an `env_file`):

```
ACME_API_KEY=...
```

### 3. Wire it to an agent

Reference the registry key (`acme`) from the agent's `tools:` list in
`configs/agents.yaml`:

```yaml
agents:
  - name: Kiku
    email: kiku@agents.mxhero.com
    tools:
      - report
      - acme        # ← your new private tool
```

That's all — the next container start picks up the tool. No change to the public
`registry.go` and no build flag.

## Conventions to follow

- **Return `nil` when the secret is missing.** Log a one-line `[toolname] … —
  <tool> disabled` and return `nil` from the factory rather than erroring. A
  deployment without that credential should run fine, just without the tool.
- **Read secrets with `os.Getenv` here — never add a symbol to
  `internal/config/env_vars.go`.** Keeping the env var name out of the public
  repo is the whole point of this package.
- **`tools.Register` is the only public seam.** Call it from `init()`. An empty
  description is allowed, but supply one (especially for MCP-style factories that
  build their `ToolDefinition`s dynamically) so the configurator dashboard has a
  tooltip to show.
- **`StaticSystem` over `System(email)`.** Only `StaticSystem` is in the
  cacheable prompt prefix; reach for the `System` function only when the guidance
  genuinely depends on the inbound email. See the
  [`ToolDefinition`](../tools/types.go) doc for the split.
- **A factory may return multiple `ToolDefinition`s.** Group related tools (e.g.
  lookup + book) under one registry key — see [`native.go`](native.go) for a
  full multi-tool example, and [`nuki_native.go`](nuki_native.go) for a private
  variant that shares an account with a public tool.
- **Validate input and re-check before mutating.** Unmarshal into a typed
  struct, validate required fields, and for write operations re-check
  preconditions immediately before the write (see `native_book_desk`'s
  pre-insert conflict check).

