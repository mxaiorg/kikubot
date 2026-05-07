<p align="center">
  <!-- TODO: replace with the project logo -->
  <img src="assets/Kiku.png" alt="kikubot" width="240">
</p>

<h1 align="center">kikubot</h1>

<p align="center">
  An email-driven, multi-agent framework. Each agent is an inbox.
</p>

---

> **Disclaimer.** This repository is a public mirror of an internally developed project. It is provided as-is for community use and learning. We do not guarantee response times on issues or PRs, and development priorities are driven by internal needs. Contributions are welcome but may not be merged.

## Overview

Kikubot turns an email account into an autonomous agent. Each running container polls one IMAP mailbox, runs every new email through an LLM agentic loop with a configurable tool set, and replies via SMTP. Agents collaborate by emailing each other, so a typical deployment looks like several agents — a coordinator and a few specialists — sharing one mail server.

**Why email?** It's the universal asynchronous message bus: humans already use it, every system can send to it, threads carry their own history (`References:` / `In-Reply-To:`), and accounts give you free per-agent identity, ACLs, and durability.

**At a glance:**

- **Per-thread memory.** Each email thread is a long-running conversation; the agent's history is persisted as JSON keyed by the thread's root Message-Id.
- **Pluggable tools.** Built-in tools cover messaging, status reporting, snoozing, and mailbox search. Optional tools include Salesforce, WordPress, Buffer, Box, Helpjuice, Tavily web search, Apache Tika file-to-text, and arbitrary local/HTTP MCP servers.
- **Pluggable LLMs.** Anthropic API (default, with prompt caching) or OpenRouter (with backup-model fallback).
- **Knowledge base.** Per-agent and shared markdown files appended to the system prompt at startup.
- **Multi-agent coordination.** Agents talk to each other via the `message_tool` core tool; coordinator agents can delegate, fan out, and snooze pending work.
- **Recurring tasks.** Agents can schedule tasks to run at specific times or intervals.
- **Auto-reply / bounce safety.** DSNs and out-of-office replies bypass the LLM to prevent infinite delegation loops.

### One Agent to Thousands of Agents

You can spawn one or more agent containers with this repository on the same machine. Each container runs a single agent. You can also deploy this repository across multiple machines and spawn agents across your organization. The only requirement is that coordinator agents can reach each other via email.

Coordinator agents can be organized into teams, and each team can have multiple agents. Coordinator agents team members can in themselves be coordinators. Much like how organizations are structured into divisions, with each division representing multiple departments which in turn represent multiple teams – so can you structure your network of agents. Each coordinator only needs to know the subset of agents it works with directly. Theoretically, a Kikubot deployment can scale to hundreds of thousands of agents.

## References

This project is based on the research of mxHERO Labs. See our [blog post](https://medium.com/datadriveninvestor/the-ai-organization-source-code-included-f2359da8e35e) for more details. 

## Architecture

```
   ┌────────────┐       ┌──────────────────┐
   │   Users    │──┐    │    Coordinator   │ ◀──┐
   └────────────┘  │    │   (Kiku inbox)   │    │
                   ▼    └────────┬─────────┘    │
              ┌──────────┐       │              │   email
              │  IMAP /  │       ▼ delegate     │   threads
              │   SMTP   │  ┌─────────┐ ┌─────────┐ ┌─────────┐
              └────┬─────┘  │  Beta   │ │  Gamma  │ │  Delta  │
                   │        │ (CRM)   │ │(social) │ │  (web)  │
                   └────────┴─────────┘ └─────────┘ └─────────┘
                                  │            │           │
                              Salesforce    Buffer    WordPress
                              mxMCP         Tavily    Helpjuice
                                                      Box, Tika
```

Each agent container runs an identical Go binary, parameterised by environment variables and a shared `agents.yaml` roster.

## Prerequisites

- **Docker** (Compose v2).
- **An LLM API key** — `ANTHROPIC_API_KEY` and/or `OPENROUTER_API_KEY`.
- **An IMAP + SMTP server** with one mailbox per agent. You can use any provider; this repo includes a self-hostable docker-mailserver sidecar at `services/dms/` if you want one.
- **(Optional) tool credentials** for any integrations you enable (Salesforce, WordPress, Buffer, Helpjuice, Box, Xero, Tavily, mxMCP).

## Quick start

> ### Configuration Dashboard Tool
> A dashboard configuration tool can be found in the scripts directory. The aim is to provide a simple way to configure a Kikubot deployment. This is very much a work in progress and hasn't been tested extensively, but it is probably useful already.

### Manual Configuration

```bash
git clone https://github.com/mxaiorg/kikubot
cd kikubot

# 1. Configure the agent roster and per-agent env files (see "Configuration").
cp configs/agents-example.yaml     configs/agents.yaml
cp configs/env/examples/common.env configs/env/common.env
cp configs/env/examples/kiku.env   configs/env/kiku.env
# Edit configs/agents.yaml to match the agents you want to run, then edit the
# env files: set EMAIL_PASSWORD, ANTHROPIC_API_KEY (or OPENROUTER_API_KEY),
# WHITELIST, etc.

# 2. (Optional) Drop knowledge files into configs/knowledge/<agent>/*.md

# 3. Edit the docker-compose.yml to match your environment.
cp docker-compose.yml.example docker-compose.yml
#    - Set the service (agent) names to reflect your agents.yaml.
#    - Set the environment and volume mounts to match your agent specific env files
#      and data folder

# 4. Validate.
go run scripts/kikudoctor/*.go

# 5. Launch.
docker compose up -d --build
```

Send the agent an email from a whitelisted address and watch the reply land in your inbox.

To watch the conversation between agents recorded in the logs:

```bash
docker compose logs -f
```

## Configuration

### Agent roster — `configs/agents.yaml`

Start from configs/agents-example.yaml and edit to your deployment — configs/agents.yaml is what the running container reads.

Defines every agent on the network. Each running container looks itself up by `AGENT_EMAIL` and uses everyone else as the "coworkers" list injected into its system prompt. `agents.yaml` is how each agent becomes aware of the others. If you deploy Kikubots across multiple machines and you want agents to interact between machines, be sure to include those agents in the `agents.yaml` roster of each installation.

```yaml
agents:
  - name: Kiku
    email: kiku@agents.example.com
    role: "Coordinator"
    description: "Communicates with users. Coordinates other agents."
    tools: [report, snooze, tavily_mcp]

  - name: Beta
    email: beta@agents.example.com
    role: "CRM, Email Archivist"
    description: "Manages Salesforce and access to the company email record."
    tools: [mxmcp, salesforce_mcp]
```

Tool keys are defined in [`internal/tools/registry.go`](internal/tools/registry.go).

### Environment files — `configs/env/`

`common.env` is shared by every container; per-agent files (e.g. `kiku.env`) override individual variables. Examples are in `configs/env/examples/`.

Common settings:

| Variable | Purpose |
|---|---|
| `EMAIL_SERVER`, `SMTP_SERVER`, `SMTP_PORT` | IMAP / SMTP endpoints. |
| `ANTHROPIC_API_KEY` / `OPENROUTER_API_KEY` | LLM credentials. |
| `MAX_HISTORY_CHARS`, `MAX_TOKENS`, `MAX_TURNS` | Conversation budgets. |
| `AGENT_TIMEOUT` | Per-message deadline (seconds). |

Per-agent settings:

| Variable | Purpose |
|---|---|
| `AGENT_NAME`, `AGENT_EMAIL`, `EMAIL_PASSWORD` | This agent's identity and inbox. |
| `LLM_PROVIDER` | `anthropic` or `openrouter`. |
| `LLM_MODEL` | Model id (vendor-prefixed for OpenRouter, e.g. `anthropic/claude-sonnet-4.6`). |
| `LLM_OPENROUTER_BACKUP` | Comma-separated fallback models (OpenRouter only). |
| `SYSTEM_PROMPT` | Optional override; supports `{{coworkers}}` template. |
| `WHITELIST` / `BLACKLIST` | Comma-separated addresses or domains. **Whitelist takes precedence.** |

Whitelist mode is strict (every immediate sender must match). Blacklist mode is lenient (walks the full thread to catch hidden bad actors). See [`configs/env/README.md`](configs/env/README.md).

### Knowledge base — `configs/knowledge/`

Markdown files appended to each agent's system prompt at startup.

```
configs/knowledge/
├── common/         # loaded by every agent
│   ├── 01_company.md
│   └── 02_voice.md
└── kiku/           # loaded only by kiku@…  (matches the email local-part)
    └── 01_file_links.md
```

Files are sorted by name — use numeric prefixes (`01_`, `02_`) to control ordering. See [`configs/knowledge/readme.md`](configs/knowledge/readme.md).

### Tool credentials

Each integration adds its own variables to the agent's env file. The most common:

- **`salesforce_mcp`** — `SALESFORCE_CLIENT_ID`, `SALESFORCE_CLIENT_SECRET`, `SALESFORCE_INSTANCE_URL`
- **`wordpress`** — `WEBSITE_URL`, `WORDPRESS_USER`, `WORDPRESS_PASSWORD`
- **`buffer_mcp`** — `BUFFER_API_KEY`
- **`helpjuice`** — `HELPJUICE_API_KEY`, `HELPJUICE_ACCOUNT`
- **`xero_mcp`** — `XERO_CLIENT_ID`, `XERO_CLIENT_SECRET`
- **`tavily_mcp`** — `TAVILY_API_KEY`
- **`mxmcp`** — `MXMCP_API_KEY`
- **`box_cli`** — drop a Box JWT app config at `box_config.json` (the Dockerfile registers it during the image build)
- **`file_text`** — `TIKA_URL` (defaults to the bundled Tika sidecar)

## Tools

A **tool** is anything the agent can call mid-conversation. Each tool is a `ToolDefinition` with a name, JSON-schema input, an `Execute` function, and (optionally) text contributed to the system prompt.

### Built-in catalogue

**Core tools** — always loaded for every agent ([`internal/tools/core.go`](internal/tools/core.go)):

| Tool | Purpose |
|---|---|
| `message_tool` | Send email to a coworker — the primitive for multi-agent coordination. |
| `set_task_status` | Mark the current task waiting / complete / error so the memory file reflects state. |
| `mbox_search` | Search the agent's own IMAP mailbox by sender, subject, date, or full-text. |

**Optional tools** — opt-in per agent via the `tools:` list in `agents.yaml` ([`internal/tools/registry.go`](internal/tools/registry.go)):

| Key                    | What it does                                                                                          |
|------------------------|-------------------------------------------------------------------------------------------------------|
| `report`               | Send a structured reply to the user (used by coordinators).                                           |
| `snooze` / `unsnooze`  | Schedule or cancel a recurring/one-off replay of the current message — see **Recurring tasks** below. |
| `anthropic_web_search` | Anthropic's server-side web search tool. Only works with Anthropic LLMs.                              |
| `tavily_mcp`           | Tavily web search via MCP.                                                                            |
| `salesforce_mcp`       | Salesforce CRM via the `@tsmztech/mcp-server-salesforce` MCP server.                                  |
| `buffer_mcp`           | Schedule social posts via Buffer's MCP.                                                               |
| `xero_mcp`             | Xero accounting via MCP.                                                                              |
| `mxmcp`                | mxHERO email-search MCP.                                                                              |
| `wordpress`            | Read/write posts on a WordPress site.                                                                 |
| `helpjuice`            | Read/write FAQ articles in Helpjuice.                                                                 |
| `box_cli`              | File operations against Box via the Box CLI.                                                          |
| `download`             | Fetch a URL to disk.                                                                                  |
| `file_text`            | Convert any file to plain text via Apache Tika.                                                       |
| `bash`                 | Execute arbitrary bash locally — full network access.                                                 |
| `vimeo`                | Simplified read-only access to Vimeo library.                                                         |

### Writing your own tool

Every tool is a `ToolDefinition` value. The minimum:

```go
// internal/tools/weather.go
package tools

import (
    "context"
    "encoding/json"
    "fmt"
)

func WeatherTool() ToolDefinition {
    return ToolDefinition{
        Name:        "weather_lookup",
        Description: "Look up the current temperature for a city.",
        InputSchema: []byte(`{
            "type": "object",
            "properties": {
                "city": {"type": "string", "description": "City name"}
            },
            "required": ["city"]
        }`),
        // StaticSystem is appended to the cacheable prefix of the system
        // prompt — use it for instructions that don't depend on the email.
        StaticSystem: "Use weather_lookup when the user asks about the weather.",
        Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
            var args struct{ City string `json:"city"` }
            if err := json.Unmarshal(input, &args); err != nil {
                return "", err
            }
            // ... call your API ...
            return fmt.Sprintf("22°C in %s", args.City), nil
        },
    }
}
```

Then register it under a YAML key in [`internal/tools/registry.go`](internal/tools/registry.go):

```go
var registry = map[string]toolFactory{
    // ...
    "weather": wrap(WeatherTool),
}
```

…and add `weather` to the `tools:` list of any agent in `configs/agents.yaml`.

#### Environment variables for API keys
If an API key is passed in via environment variables, be sure to update `internal/config/env_vars.go` and include the exported variable (`config.YouEnvVar`) in your tool code. Then of course, add the env var to the `env/` files.

#### Injecting email context into the tool system prompt
For per-email context (e.g. injecting the current date, or summarising thread state), set the `System func(email services.Email) (string, error)` field instead of `StaticSystem` — its output goes into the *volatile* portion of the system prompt and is not cached.

### Helpers for common patterns

Most integrations don't need a hand-written `Execute`. The `tools` package provides three reusable bridges:

- **Shell commands** → [`BashTool()`](internal/tools/bash.go). Already registered as the `bash` key. Runs locally with full network access (unlike Anthropic's sandboxed `bash_code_execution`). Use this rather than rolling your own `os/exec` wrapper.

- **Local MCP servers (stdio)** → [`LocalMCPBridge(LocalMCPConfig)`](internal/tools/mcp_helper.go) launches an MCP server as a long-lived subprocess (e.g. an `npx`-installed package), discovers its tools, and exposes each one as a `ToolDefinition` named `<server>__<tool>`. Example from [`salesforce_mcp.go`](internal/tools/salesforce_mcp.go):

  ```go
  func SalesforceMCP() []ToolDefinition {
      cfg := LocalMCPConfig{
          ServerName: "salesforce",
          Command:    "npx",
          Args:       []string{"-y", "@tsmztech/mcp-server-salesforce"},
          Env: map[string]string{
              "SALESFORCE_CLIENT_ID":     os.Getenv("SALESFORCE_CLIENT_ID"),
              "SALESFORCE_CLIENT_SECRET": os.Getenv("SALESFORCE_CLIENT_SECRET"),
          },
      }
      tools, _ := LocalMCPBridge(cfg)
      return tools
  }
  ```

  If the MCP server ships as an npm package, also pre-install it in the Dockerfile so `npx` doesn't fetch it on first call.

- **Remote MCP servers (HTTP)** → [`MCPBridge(name, url, auth)`](internal/tools/mcp_helper.go) connects to a Streamable-HTTP MCP server and proxies its tools. Example from [`tavily_mcp.go`](internal/tools/tavily_mcp.go):

  ```go
  func TavilyMCP() []ToolDefinition {
      tools, _ := MCPBridge("tavily", "https://mcp.tavily.com/mcp", "Bearer "+os.Getenv("TAVILY_API_KEY"))
      return tools
  }
  ```

- **Hand-curated CLI wrappers** → [`CLIToolConfig`](internal/tools/cli_helper.go) is the same idea as `LocalMCPBridge` but for CLIs that don't speak MCP — you author the schemas yourself and the helper handles subprocess execution, JSON-flag injection, and root-path scoping.

### More about tools

Read more about tools in the [tools README](internal/tools/README.md)

## Recurring tasks

Agents can schedule themselves. The `snooze` / `unsnooze` tools (registered via the `snooze` and `unsnooze` keys in `agents.yaml`) let an agent park the current email and replay it on a cron schedule.

**How it works:**

1. The agent calls `snooze_tool` with the inbound `Message-Id`, a description, a `Once` flag, and a 5-field crontab expression (`minute hour dom month dow`).
2. The schedule is persisted to `data/snooze.json` (or `snooze.json` in dev) — one entry per snoozed message.
3. Every poll cycle (30s), the main loop drains all snoozes whose next-run time has passed. For each, it re-fetches the original email by Message-Id, prepends a system note ("This email is being replayed as a previously scheduled task — do NOT snooze again"), and runs `agent.HandleMessage`. The `snooze_tool` and `unsnooze_tool` are stripped from the toolset for that replay so the model can't re-schedule itself into a loop.
4. After successful execution: `Once: true` snoozes are deleted; recurring snoozes advance to the next cron-computed run time.
5. To cancel, the agent calls `unsnooze_tool` with the Message-Id. The system prompt also surfaces any active snoozes for the current thread so a follow-up "stop the daily report" maps to the right cancellation.

**Timezone handling.** The crontab is interpreted in the *user's* timezone, extracted from the original email's `Date:` header. So `0 7 * * *` means 7 AM in the sender's local time even if the server runs in UTC. Both IANA names (`America/New_York`) and fixed offsets (`-0500`) are supported.

**Example user prompts that trigger snoozing:**

- *"Send me the social-media metrics every Monday at 9am."* → `0 9 * * 1`, `Once: false`
- *"Remind me about the contract review tomorrow at 2pm."* → one-off with `Once: true`
- *"Stop the daily standup digest."* → triggers `unsnooze_tool` against the matching active snooze

The scheduler is single-process and file-backed — no external dependencies. If you run an agent across multiple replicas, only one should own the snooze file (mount it on a single volume or run a single instance per inbox).

## Auxiliary services

Both are optional sidecars with their own compose files.

### Mail server — `services/dms/`

A docker-mailserver instance for hosting agent inboxes on a domain you control (SPF/DKIM/DMARC strongly recommended). See [`services/dms/README.md`](services/dms/README.md) for account-management commands.

### Apache Tika — `services/tika/`

REST API for extracting text from PDFs, Office docs, HTML, and more. Used by the `file_text` tool. See [`services/tika/README.md`](services/tika/README.md).

## Running multiple agents

`docker-compose.yml` ships with one active service (`kiku-alpha`) and commented templates for `beta`, `gamma`, and `delta`. To bring up additional agents, uncomment the service block and create the matching `configs/env/<agent>.env`.

```bash
# After editing docker-compose.yml + adding env files:
docker compose up -d --build --remove-orphans

# After only env-variable changes:
docker compose up --force-recreate
```

## Development

Local development uses Go 1.26 and the `dev` build tag, which loads `./configs/env/common.env` and `./configs/env/kiku.env` via godotenv.

```bash
go run -tags=dev cmd/kikubot/main.go
```

Architectural notes for code changes live in [`CLAUDE.md`](CLAUDE.md).

## Naming

We named our framework, Kikubot, from the Japanese contraction for Kiku (機駆) - 機 (ki, "mechanism") + 駆 (ku, "to drive/propel"). A machine that moves.

## License

[MIT](LICENSE) — © [mxHERO](https://mxhero.com) Inc.
