# Kikubot Tools

Kikubot tools provide various capabilities that are distributed across your agents. By distributing tools across multiple agents, agents can better perform their tasks.

## Creating Tools

There are a couple helper functions to make it easier to create tools.

- cli_helper.go - Helper functions for using command line tools
  - box_cli.go is an example of a tool that uses the cli_helper.go
- mcp_helper.go - Helper functions for adding MCPs
  - provides helper functions for local and remote MCPs
    - mxmcp.go is an example of a remote MCP usage
    - salesforce_mcp.go is an example of a local MCP usage

Agent tools can supplement the agent's system prompt to provide additional instructions. System prompts can be static or dynamic. Dynamic system prompts take the email being processed as an input. This allows you to inject per email specific instructions into the system prompt. See `types.go` for more information.

LLMs execute tools via the tool's Execute function. This function takes a context which carries the email being processed. The reason this is done is to provide a check on LLM input that may be incorrect. 

## Tips for Creating Tools

With AI development tools, like Claude Code, you can create a tool that wraps almost any REST API in a matter of minutes. The Vimeo tool was created with the following prompt in Claude Code:

> Create a tool for listing and searching Vimeo videos from an authenticated Vimeo account (using an API key passed in via environment). Use Vimeo's API https://developer.vimeo.com/api/reference to implement the API. Keep functionality to read-only operations so that the tool can help the LLM to search for videos and provide video links and other information. You might use the existing helpjuice.go tool as a reference.

A few minutes later, the tool was created. Thanks, Claude!

⚠️ Once you have a tool, be sure to:
1. register it in registry.go and then add it to your agent definitions in your `agents.yaml` file. 
2. Also, remember to pass in your API key as an environment variable.

🤔 You might be wondering why we created a Vimeo tool instead of using an existing Vimeo MCP tool. The reason is that the Vimeo MCP tools implements much more capability than we need. As a result, it consumes a lot more tokens than required for our case.

## Private tools (`internal/tools_priv/`)

Public tools (the ones in *this* directory) are registered in `registry.go` and live in the published repository. **Private tools** are for the things you don't want to publish: company-specific integrations, proprietary logic, or tools whose very existence (and credentials) should stay out of the public codebase. They go in [`internal/tools_priv/`](../tools_priv/) instead, and once present they behave identically to built-in tools — add the key to an agent's `tools:` list in `agents.yaml` and you're done.

### How it works

- **Presence-based, not build-tagged.** `cmd/kikubot` blank-imports `internal/tools_priv` unconditionally. If files are present there, they're compiled and their `init()` functions run; if the directory is empty (a clean public checkout), the package contributes nothing. There's no build flag to remember.
- **Self-registration.** Instead of editing the `registry.go` map literal (which lives in the public package), a private tool registers itself at startup with `tools.Register(key, factory, description)` from an `init()`. The `description` is what the configurator dashboard shows in its tooltip — supply one, especially for MCP/CLI factories that build their `ToolDefinition`s dynamically.
- **Secrets stay private too.** Public tools declare their env-var-backed credentials as exported vars in `internal/config/env_vars.go`. Private tools instead read secrets directly with `os.Getenv` inside `internal/tools_priv`. The variables still live in `configs/secrets.env` (gitignored, already loaded into every container), but no symbol referencing them appears in the public repo — so the public codebase stays unaware the tool, or its credentials, exist.
- **Graceful when unconfigured.** A private tool factory should return `nil` (and log a line) when its required env var is unset, so an agent that lists the key but lacks the secret simply gets no tool rather than a crash.

### Why this matters: update without merge conflicts

Because the published repository tracks this directory as effectively empty, your private `.go` files sit *outside* the upstream-tracked code. You can keep pulling project updates (`git pull`) and never hit a merge conflict over your own tools or the shared `registry.go`. Your private code is additive — it plugs in through `tools.Register` rather than by editing files the upstream project also changes.

### Minimal example

```go
// internal/tools_priv/acme.go
package toolspriv

import (
    "log"
    "os"

    "kikubot/internal/tools"
)

func acme() []tools.ToolDefinition {
    key := os.Getenv("ACME_API_KEY")
    if key == "" {
        log.Println("[acme] ACME_API_KEY not set — Acme tools disabled")
        return nil
    }
    // ...build ToolDefinitions using key...
    return nil
}

func init() {
    tools.Register("acme", acme, "Acme Corp integration — ...")
}
```

Then add `ACME_API_KEY=...` to `configs/secrets.env` and reference the key (`acme`) from an agent's `tools:` list in `configs/agents.yaml`. The configurator dashboard flags private tools with a small **private** badge so it's clear which tools depend on local-only source. The package doc comment in [`internal/tools_priv/doc.go`](../tools_priv/doc.go) is the canonical reference.
