# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`kikubot` is an **email-driven multi-agent framework**. Each running container is one agent: it polls an IMAP mailbox every 30s, runs each new email through the Anthropic/OpenRouter agentic loop with a configurable tool set, and replies via SMTP. Agents collaborate by emailing each other (the `message_tool` core tool), so a deployment is typically several `kiku-*` containers (Kiku, Beta, Gamma, Delta) sharing a docker-mailserver instance.

## Build & run

`docker-compose.yml` contains one active service (`kiku-alpha`) plus commented templates for `beta/gamma/delta`. To bring up additional agents, uncomment the service and provide the matching per-agent env file.

## High-level architecture

### The agent loop (`cmd/kikubot/main.go`)

`process(ctx)` runs every 30 seconds and on startup:

1. **Fetch new emails** from IMAP (`services.GetNewEmails`).
2. For each email:
   - **Auto-reply guard** — if `Auto-Submitted` header is anything but unset/`no` (DSN, OOO, bounce), `handleAutoReply` runs out-of-band: it walks `References` (and the Sent folder, for our own bounced outbound) to find the immediate upstream caller, sends them a single notification, deletes any pending snooze for the thread, writes a terminal `[SYSTEM] Task aborted…` note into memory, and **never invokes the LLM**. Without this, bounce loops cause coordinators to retry indefinitely.
   - **ACL** (`agents.AccessControl`) — whitelist (strict, applied to immediate `From`+`X-Senders` only) or blacklist (lenient, walks the whole `References` thread). Whitelist takes precedence.
   - **Memory load** — per-thread JSON file keyed by `GetThreadRoot()` (= `References[0]` or `MessageId`). If no memory exists, reconstruct from prior emails in the thread via `MemoryFromReferences`.
   - **`agent.HandleMessage`** with `MaxTurns` budget and `AgentTimeout` deadline. **Always saves history afterward, even on error**, so partial tool results survive retries.
   - On `ErrMaxTurns`, send a one-shot notice to the sender and mark seen — re-running burns another budget and creates infinite delegation loops.
   - On other errors, leave unseen for the next poll.
3. **Snooze pump** — drain `services.NextSnoozed()` running each through `agent.HandleSnooze` (which strips `snooze_tool`/`unsnooze_tool` from the toolset for that turn so the model can't re-snooze itself).

### Agent loop details (`internal/agents/agent.go`)

- **System prompt is split**: `SystemStable` (base prompt + `StaticSystem` from tools, identical across emails → cacheable by the Anthropic provider) and `SystemVolatile` (per-email `tool.System(email)` output). Prefer `StaticSystem` on a `ToolDefinition` whenever the instructions don't depend on the email — this is what unlocks prompt caching.
- **Trusted email on context**: `services.WithSourceEmail(ctx, email)` stashes the inbound email on the context. Tools recover it via `services.SourceEmail(ctx)` to get authoritative `Message-Id` / `X-Senders` instead of trusting LLM-provided headers.
- **`trimHistory`** is anchor-aware. The "anchor" is the most recent user message authored by a non-agent address (compared against `config.AgentEmails`, populated from `agents.yaml`). Trimming candidates are restricted to safe cutpoints (user role, no `tool_result` blocks) at indices `>= anchor`. Losing the anchor causes coordinators to forget their task and re-delegate forever, so trimming will keep the anchor visible even if the resulting tail still exceeds `MaxHistoryChars`.
- **Server-side tool blocks** (`web_search_tool_result`, `code_execution_tool_result`, `bash_code_execution_tool_result`) come back already processed and are NOT fed to local `Execute`.

### Provider abstraction (`internal/provider/`)

`provider.Provider` interface with two impls — `anthropic.go` (default) and `openrouter.go` (OpenAI-compatible). Selection: `LLM_PROVIDER` env var; if unset and `LLM_MODEL` has a vendor prefix (`anthropic/…`), OpenRouter is auto-selected. `LLM_OPENROUTER_BACKUP` is a comma-separated fallback list, OpenRouter-only.

### Tools (`internal/tools/`)

- **`CoreTools()`** is always loaded: `set_task_status`, `message_tool` (peer-to-peer email), `mbox_search`.
- **`registry.go`** maps the YAML keys (`report`, `snooze`, `salesforce_mcp`, …) to tool factories. `agents.yaml` lists which keys each agent gets.
- **MCP bridges** (`mcp_helper.go`):
  - `LocalMCPBridge` — stdio subprocess (npx), one long-lived process per server. Used for `salesforce_mcp`, `buffer_mcp`, `box_cli`, `tavily_mcp`, `xero_mcp`. The Dockerfile pre-installs these globally so `npx` doesn't fetch at runtime.
  - HTTP MCP for `mxmcp`.
  - `parseMCPInput` tolerates empty input (`{}`) — OpenRouter occasionally truncates streamed tool-call args mid-stream.
- **`ToolDefinition`** can contribute `StaticSystem` (cacheable) or `System(email) → string` (volatile) to the prompt.
- **Tool result truncation** — `MaxToolResultChars` clamps each result, preserving UTF-8 boundaries and appending a marker. `0` (default) = no limit.

### Services (`internal/services/`)

- **`emailing.go`** — IMAP/SMTP via `emersion/go-imap` and `gomail`. `Email.UserMessage()` builds the Anthropic `MessageParam` from a JSON summary of headers + content, then appends image/PDF/text/Office attachments as native content blocks (size-capped: image 20MB, PDF 32MB, text 5MB; `.docx`/`.xlsx`/`.pptx` are unzipped and sent as plain text — the API doesn't natively accept those). `WithSourceEmail`/`SourceEmail` ride the context.
- **`memory.go`** — per-thread JSON file under `memory/` (dev) or `data/memory/` (container; persisted via the `./data/<agent>` volume in `docker-compose.yml`). Persisted `MessageParam`s are wrapped in `param.Override` so the SDK doesn't re-serialise them through its own (occasionally buggy) `ToParam` path. Messages with corrupt server-side result blocks (a known SDK streaming-accumulator bug) are dropped on read; trailing references to dropped tool_use blocks are stripped.
- **`snooze.go`** — single-process file-based scheduler (`snooze.json`); supports cron and one-shot. Timezone parses IANA names *or* fixed offsets (`-0500`).
- **`tika.go`** — extracts text from arbitrary docs via the Tika sidecar (`TIKA_URL`, default `http://localhost:9998`).
- **`docgen.go`** — mostly stubs (`ErrNotImplemented`); only `GenerateTxt` is functional.

### Configuration

- **`agents.yaml`** (`AGENTS_CONFIG` env > next to binary > next to source) declares the roster: `name`, `email`, `role`, `description`, `tools`. The `Coworkers()` list (everyone except self) is JSON-formatted into the system prompt at the `{{coworkers}}` template marker. Email keys are populated into `config.AgentEmails` for `findAnchor`.
- **Knowledge base** (`configs/knowledge/<agentKey>/*.md`, plus shared `configs/knowledge/common/`) is concatenated and appended to the system prompt at startup. `agentKey` = local-part of `AGENT_EMAIL` lowercased. Files are sorted by name, so use numeric prefixes (`01_…md`, `02_…md`) to control order. The Dockerfile copies `configs/knowledge/` to `/app/knowledge/`; `knowledgeBaseDir()` looks next to the executable first, then falls back to next to the source file in dev.
- **Per-agent env files** under `configs/env/<agentKey>.env` override `common.env`. `agents.yaml` and `*.env` example versions are committed; the live ones are `.gitignore`d (along with `box_config.json` and DMS certs/maildata).

## Key conventions

- **Don't feed auto-replies to the LLM.** Always check `email.AutoSubmitted` first and route through `handleAutoReply`. New error/bounce paths must follow the same out-of-band pattern.
- **Trust `services.SourceEmail(ctx)`, not LLM input** for `Message-Id`, `X-Senders`, or any ACL-relevant data.
- **`StaticSystem` over `System(email)`** unless the instructions genuinely depend on per-email state — only `StaticSystem` is in the cacheable prefix.
- **Never trim the anchor.** When changing `trimHistory`, preserve the invariant that the most recent human-authored user message stays in the trimmed tail.
- **`ErrMaxTurns` is non-retryable.** Don't re-queue on it; notify the sender and mark seen, otherwise coordinator agents loop forever.
- **MCP `parseMCPInput` tolerates `{}`** — leave the empty-input path alone unless you've confirmed the upstream provider behaviour has changed.
- **History persistence uses `param.Override`** to bypass SDK re-serialisation; if you change the memory format, keep this wrapper.

## Auxiliary services (`services/`)

Sidecar `docker-compose.yml`s for **Apache Tika** (file-to-text, port 9998) and **docker-mailserver / DMS** (IMAP/SMTP for the `agents.mxhero.com` domain, with postfix transport/sender-access config under `services/dms/config/`). DMS account management is documented in `services/dms/README.md` (`docker exec -it dms setup email add …`).
