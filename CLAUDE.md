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

`provider.Provider` interface with two impls — `anthropic.go` (default) and `openrouter.go` (OpenAI-compatible). Selection comes from `llm_provider` in agents.yaml (common or per-agent override); if unset and `llm_model` has a vendor prefix (`anthropic/…`), OpenRouter is auto-selected. `llm_openrouter_backup` is a YAML list of fallback model ids, OpenRouter-only.

### Tools (`internal/tools/`)

- **`CoreTools()`** is always loaded: `set_task_status`, `message_tool` (peer-to-peer email), `mbox_search`.
- **`registry.go`** maps the YAML keys (`report`, `snooze`, `salesforce_mcp`, …) to tool factories. `agents.yaml` lists which keys each agent gets.
- **MCP bridges** (`mcp_helper.go`):
  - `LocalMCPBridge` — stdio subprocess (npx), one long-lived process per server. Used for `salesforce_mcp`, `box_cli`, `xero_mcp`. The Dockerfile pre-installs these globally so `npx` doesn't fetch at runtime.
  - `MCPBridge` (static `Authorization` header) and `MCPBridgeOAuth` (OAuth2) — remote Streamable HTTP MCP. Both open a **fresh dial per call** (the transport's session/SSE context goes stale between calls). Don't call these directly for new servers — use the declarative `mcp_servers:` table instead (see below).
  - **Declarative remote MCP servers (`mcp_servers.go`).** `configs/mcp_servers.yaml` (`MCP_SERVERS_CONFIG` env > next to binary > next to source; its own file, separate from `agents.yaml`) holds a `mcp_servers:` list of `config.MCPServer` — the no-code seam for remote MCPs. `config.LoadMCPServers` parses it (missing file = no remote MCPs, not fatal) and `RegisterMCPServers` (called from `main` before `initAgent`) registers one registry factory per row, keyed by `key`. Agents opt in by listing the `key` in their `tools:` in `agents.yaml`. Auth modes: `none`; `bearer` (static `Authorization: <scheme> <token>` from `token_env`; scheme defaults to `Bearer`); `apikey` (same static-header machinery — defaults to `Authorization: ApiKey <token>`, as mxMCP wants; set `header:` e.g. `X-Api-Key` for servers that take the key in a separate header with a raw token, or `scheme:` to override the prefix); and `oauth2`. Static modes share one arbitrary-header bridge (`MCPBridgeHeaders`). **Every remote HTTP MCP lives in this table** — `mxmcp`, `buffer_mcp`, `tavily_mcp`, `box_mcp`; there are no bespoke `MxMCP`/`BufferMCP`/`TavilyMCP`/`BoxMCP` factories anymore. The `registry.go` literal now holds only local/stdio bridges and non-MCP tools. Adding a server is a YAML row (+ a seed file for oauth2); never new Go.
  - **OAuth2 refresh is entirely mcp-go's.** `transport.WithHTTPOAuth` wires an `OAuthHandler` that checks expiry, exchanges the refresh token (discovering the token endpoint from the server's OAuth metadata, or `metadata_url` if set), and persists the rotated token via a `transport.TokenStore`. The **only** local code is `FileTokenStore` (`oauth_store.go`) — a file-backed `TokenStore` at `oauth/<key>.json` (dev) / `data/oauth/<key>.json` (container, persisted volume; flipped by `InitOAuthDir`). Hand-seed the access+refresh pair once (a serialised `transport.Token`); kikubot rotates+rewrites it on every refresh, so the single-use refresh token survives restarts. Set `expires_at` in the past to force a refresh on the first call. The dev seed dir `oauth/` is gitignored.
  - `parseMCPInput` tolerates empty input (`{}`) — OpenRouter occasionally truncates streamed tool-call args mid-stream.
- **`ToolDefinition`** can contribute `StaticSystem` (cacheable) or `System(email) → string` (volatile) to the prompt.
- **Tool result truncation** — `MaxToolResultChars` clamps each result, preserving UTF-8 boundaries and appending a marker. `0` (default) = no limit.

### Services (`internal/services/`)

- **`emailing.go`** — IMAP/SMTP via `emersion/go-imap` and `gomail`. `Email.UserMessage()` builds the Anthropic `MessageParam` from a JSON summary of headers + content, then appends image/PDF/text/Office attachments as native content blocks (size-capped: image 20MB, PDF 32MB, text 5MB; `.docx`/`.xlsx`/`.pptx` are unzipped and sent as plain text — the API doesn't natively accept those). `WithSourceEmail`/`SourceEmail` ride the context.
- **`memory.go`** — per-thread JSON file under `memory/` (dev) or `data/memory/` (container; persisted via the `./data/<agent>` volume in `docker-compose.yml`). Persisted `MessageParam`s are wrapped in `param.Override` so the SDK doesn't re-serialise them through its own (occasionally buggy) `ToParam` path. Messages with corrupt server-side result blocks (a known SDK streaming-accumulator bug) are dropped on read; trailing references to dropped tool_use blocks are stripped.
- **`snooze.go`** — single-process file-based scheduler (`snooze.json`); supports cron and one-shot. Timezone parses IANA names *or* fixed offsets (`-0500`). Also hosts the **stuck-task watchdog** (`ArmWaitingWatchdog`): entries with `Watchdog:true` are armed automatically by `set_task_status(waiting)` (gated on `waiting_watchdog_minutes > 0` and a real delivery this turn). When `waiting_watchdog_minutes` elapse the poll loop's `runWatchdog` re-checks the thread — if it's no longer `waiting` it stands down; otherwise it nudges the coordinator (replays the inbound with full history + an instruction to follow up or fall back to answering the requester), capped at `waitingWatchdogMaxFires`, after which it marks the thread `error` and bounces a notice to the immediate upstream so the chain unwinds. Watchdog entries own their lifecycle and bypass `AdvanceOrDeleteSnooze`; a real (non-watchdog) snooze on the same thread takes precedence and is never clobbered.
- **`tika.go`** — extracts text from arbitrary docs via the Tika sidecar (`TIKA_URL`, default `http://localhost:9998`).
- **`docgen.go`** — mostly stubs (`ErrNotImplemented`); only `GenerateTxt` is functional.

### Configuration

- **`configs/agents.yaml`** (`AGENTS_CONFIG` env > next to binary > next to source) is the single source of truth for non-secret deployment config. It has these sections: `common:` (defaults inherited by every agent — mail server, prompts, budgets), `agents:` (roster — `name`, `email`, `role`, `description`, `tools`, plus optional per-agent overrides of any `common:` field), and optional `external:` (partner agents on other machines/domains; see below). Remote MCP servers are declared in a **separate file**, `configs/mcp_servers.yaml` (see the Tools section); agents reference them by `key` in their `tools:`. The `Peers()` list (in-roster coworkers + external partners, everyone except self, with overrides stripped) is JSON-formatted into the system prompt at the `{{coworkers}}` template marker. Email keys are populated into `config.AgentEmails` for `findAnchor`.
- **`external:` roster (cross-machine / cross-domain peers).** Each entry is an `ExternalAgent{name, email, role, description}` — identity-only, since we never run these agents (tools/budgets/LLM/ACL fields don't apply). Listing a peer here (1) renders it into `{{coworkers}}` tagged `"scope":"external"` so the model can delegate to it, and (2) registers its address in `config.AgentEmails`, which both makes `findAnchor` treat its replies as peer messages and relaxes `message_tool`'s same-domain send gate for that exact address. The gate stays an **allowlist** — `sendEmailInternal` permits a recipient that is same-domain *or* in `AgentEmails`; arbitrary outside addresses are still blocked (anti-exfiltration). The roster is **outbound-only**: a partner emailing *in* is still gated by each agent's `whitelist:`, so add the partner's address/domain there too for two-way collaboration. `warnUncoveredExternals` logs a startup warning for any `external:` peer not covered by this agent's whitelist (whitelist mode only).
- **`configs/secrets.env`** carries every secret — LLM provider keys, tool credentials, and per-agent `<UPPER_STEM>_EMAIL_PASSWORD` (e.g. `KIKU_EMAIL_PASSWORD` for `kiku@…`). Every container loads it as `env_file`. The container picks its identity via the per-service `AGENT_EMAIL` env var and resolves its mailbox password by uppercasing the local-part.
- **Knowledge base** (`configs/knowledge/<agentKey>/*.md`, plus shared `configs/knowledge/common/`) is concatenated and appended to the system prompt. `agentKey` = local-part of `AGENT_EMAIL` lowercased. Files are sorted by name, so use numeric prefixes (`01_…md`, `02_…md`) to control order. The Dockerfile copies `configs/knowledge/` to `/app/knowledge/` (a baked default); `knowledgeBaseDir()` looks next to the executable first, then falls back to next to the source file in dev. **Hot reload:** `docker-compose.yml` bind-mounts `./configs/knowledge:/app/knowledge:ro` so edits (e.g. via the configurator) are visible without rebuilding the image, and `process()` calls `reloadKnowledgeIfChanged()` each poll — an mtime check on the knowledge dirs that re-appends the block via `agent.SetSystem()` (the knowledge-free `baseSystem` is cached at init). For near-instant propagation, `SIGHUP` (`docker compose kill -s HUP <service>`) forces an immediate reload via `forceReloadKnowledge()` instead of waiting up to 30s for the next poll; both paths are serialized by `knowledgeReloadMu`. The reload only swaps the prompt string, not the agent or its MCP subprocesses, and only invalidates the cacheable prefix when files actually change. In-flight threads keep their saved history; the new prompt applies to the next message.
- **Example files** (`configs/agents-example.yaml`, `configs/mcp_servers-example.yaml`, `configs/secrets-example.env`, `docker-compose-example.yml`) are committed; the live counterparts (`agents.yaml`, `mcp_servers.yaml`, `secrets.env`) are gitignored along with `box_config.json` and DMS certs/maildata.

## Key conventions

- **Don't feed auto-replies to the LLM.** Always check `email.AutoSubmitted` first and route through `handleAutoReply`. New error/bounce paths must follow the same out-of-band pattern.
- **Trust `services.SourceEmail(ctx)`, not LLM input** for `Message-Id`, `X-Senders`, or any ACL-relevant data.
- **`StaticSystem` over `System(email)`** unless the instructions genuinely depend on per-email state — only `StaticSystem` is in the cacheable prefix.
- **Never trim the anchor.** When changing `trimHistory`, preserve the invariant that the most recent human-authored user message stays in the trimmed tail.
- **`ErrMaxTurns` is non-retryable.** Don't re-queue on it; notify the sender and mark seen, otherwise coordinator agents loop forever.
- **A `waiting` thread must have a way to resume.** Setting status `waiting` arms the watchdog (when enabled) so a never-answered delegate can't black-hole a task. If you add new code that parks a thread waiting on an external reply, make sure either an inbound reply or the watchdog can re-wake it — never leave `waiting` as a terminal state with no timer.
- **Attach files by `path`, not inlined base64.** When a tool needs to send a file it fetched/saved (`download_file`, `save_attachment`, `bash_exec`), pass the local path to `message_tool`'s `attachments[].path`. Inlining large base64 into tool args truncates mid-stream — the failure that silently dropped the JFE flag attachment.
- **MCP `parseMCPInput` tolerates `{}`** — leave the empty-input path alone unless you've confirmed the upstream provider behaviour has changed.
- **History persistence uses `param.Override`** to bypass SDK re-serialisation; if you change the memory format, keep this wrapper.

## Auxiliary services (`services/`)

Sidecar `docker-compose.yml`s for **Apache Tika** (file-to-text, port 9998) and **docker-mailserver / DMS** (IMAP/SMTP for the `agents.mxhero.com` domain, with postfix transport/sender-access config under `services/dms/config/`). DMS account management is documented in `services/dms/README.md` (`docker exec -it dms setup email add …`).
