# CONFIGURATION.md — LLM-Guided Kikubot Setup

This document tells **you, the LLM agent**, how to walk a user through configuring a Kikubot deployment as a multi-step chat. Read this whole document once, then drive the conversation step-by-step. Do not dump the full plan on the user up front — work through one step at a time, asking only the questions that step requires, and confirming or validating before moving on.

## How to use this document

- You are the configuration assistant. The user is a human operator setting up Kikubot on their machine.
- Treat the **working directory** as the Kikubot repository root (the directory containing `docker-compose-example.yml`, `configs/`, `services/`, `scripts/`).
- After every change, briefly summarise what you wrote and where, so the user can audit.
- Never invent values. If you don't know something, ask. If a value is sensitive (API keys, passwords), do not echo it back or store it anywhere except the file the user expects.
- All shell commands you suggest should be run from the repository root unless explicitly noted.
- If the user has already partially configured the deployment, **read the existing files first** and offer the existing values as defaults the user can keep or change.
- At the end of every section, ask the user to confirm before moving on.

---

## Step 1 — Requirements

Goal: confirm the user has Docker Compose (or a compatible runtime).

1. Run a quick check:
   ```bash
   docker compose version
   ```
   If that succeeds, you're done with this step. If it fails, try:
   ```bash
   docker-compose version
   podman compose version
   ```
2. If none of those exist, **stop** and tell the user that Kikubot requires Docker Compose v2 (or a compatible drop-in like `podman compose`) and point them at https://docs.docker.com/compose/install/. Do not proceed until they confirm an install.
3. If a compatible runtime is present, tell the user which one was detected and ask them to confirm it's the one they want to use.

---

## Step 2 — Environment files

Kikubot reads two layers of environment files:
- `configs/env/common.env` — shared by every agent container.
- `configs/env/<agent>.env` — one file per agent, overriding `common.env`. The file stem must be the lowercased local-part of the agent's email (e.g. `kiku@agents.example.com` → `configs/env/kiku.env`).

Examples live in `configs/env/examples/`. The configurator tool already encodes the canonical set of fields — mirror those exact field names.

### 2a. Seed `common.env`

1. If `configs/env/common.env` does not exist, copy from the example:
   ```bash
   cp configs/env/examples/common.env configs/env/common.env
   ```
2. Then collect values for the **common** fields below. For each field, show the current value (from `common.env` if it exists, otherwise from `configs/env/examples/common.env`) as a default, and let the user accept or override.

   | Field | Prompt to user | Notes |
   |---|---|---|
   | `EMAIL_SERVER` | "IMAP server (`host:port`) for the agents' inboxes." | e.g. `mail.agents.example.com:993`. If the user opts to run the bundled email server (Step 4) and wants it on `localhost`, use the chosen hostname here even though it resolves to localhost via `extra_hosts`. |
   | `SMTP_SERVER` | "SMTP server (`host:port`) for outgoing mail." | Defaults to port 587 if no port is given. |
   | `EMAIL_INSECURE_TLS` | "Accept self-signed TLS certs from the mail server?" | `true` or `false`. Required `true` if Step 4 uses self-signed certs. |
   | `MAX_HISTORY_CHARS` | "Max characters of per-thread history retained per turn." | Default `200000`. |
   | `MAX_TOKENS` | "Max LLM output tokens per response." | Default `6000`. |
   | `AGENT_TIMEOUT` | "Per-message processing deadline, in seconds." | Default `300`. |
   | `SYSTEM_PROMPT` | "Default system prompt (multiline, may include `{{coworkers}}`)." | Show the example default. The user can keep it. |
   | `COORDINATOR_SYS_PROMPT` | "Coordinator system prompt template (recommended for coordinator agents to inherit/override)." | Show the example default. |

3. **LLM provider API keys** — `ANTHROPIC_API_KEY` and `OPENROUTER_API_KEY`.
   - **Do not collect these values from the user in chat.** For security, tell the user:
     > "One of `ANTHROPIC_API_KEY` or `OPENROUTER_API_KEY` must be filled out manually in `configs/env/common.env`. I will not ask you to paste it here — please open that file and set the key for the provider(s) you intend to use."
   - Confirm the user has done this before moving on. You may check by reading the file and confirming the placeholder (`<API-KEY-HERE>`) has been replaced. Do **not** display the value back to the user.
4. **Tool credentials** in `common.env` (Salesforce, Buffer, Xero, Helpjuice, Tika, mxMCP, Vimeo, WordPress, etc.) are also placeholders. Mention them briefly — the user only needs to fill in the ones for tools they will actually assign to an agent in Step 3. Do not collect these values in chat either.

### 2b. Per-agent env files

A Kikubot deployment needs **at least one agent**. Ask the user how many agents they want to define and, for each, collect the per-agent fields. Mirror the configurator's Add Agent form.

For each new agent, build a `configs/env/<stem>.env` file. The stem is the lowercased local-part of the agent's email.

For each field below, **if `common.env` defines a value, present that as the default** so the user can accept or alter. Only write a field to the per-agent file when its value differs from the `common.env` value, or when it is identity/auth (`AGENT_NAME`, `AGENT_EMAIL`, `EMAIL_PASSWORD` are always written at the agent level).

| Field | Prompt to user | Notes |
|---|---|---|
| `AGENT_NAME` | "Display name of the agent (e.g. `Kiku`)." | Required. |
| `AGENT_EMAIL` | "Agent's full email address." | Required. The local-part lowercased becomes the file stem. |
| `EMAIL_PASSWORD` | "Password for this mailbox." | Required. Treat as a secret — do not echo. |
| `WHITELIST` *(mutually exclusive with BLACKLIST)* | "Comma-separated allowed senders (domains or addresses). Whitelist is strict and takes precedence." | Optional. |
| `BLACKLIST` *(mutually exclusive with WHITELIST)* | "Comma-separated blocked senders." | Optional. |
| `LLM_PROVIDER` | "Provider: `anthropic` or `openrouter`." | Default `anthropic`. You **must not** offer a provider whose API key is unset in `common.env` (and the agent file). |
| `LLM_MODEL` | "Model id (provider-specific tag, e.g. `claude-sonnet-4-6` for anthropic, or `anthropic/claude-sonnet-4.6` for openrouter)." | Required. |
| `LLM_OPENROUTER_BACKUP` | "Comma-separated fallback model list." | Only ask if provider is `openrouter`. |
| `SYSTEM_PROMPT` | "Override the default system prompt for this agent (leave blank to inherit `common.env`). May include `{{coworkers}}`." | If the agent is a coordinator, offer to seed it from `COORDINATOR_SYS_PROMPT` in `common.env`. |
| `MAX_HISTORY_CHARS` / `MAX_TURNS` | "Override conversation budgets for this agent?" | Optional. Coordinators usually need a higher `MAX_TURNS` (e.g. `40`) than the default 20 because each delegation burns turns. |

**Validation checklist for each agent:**
- Whitelist and blacklist must not both be set.
- The chosen `LLM_PROVIDER` must have its API key defined in `common.env` (or in the agent's own env file).
- `AGENT_EMAIL` must contain `@`.

Write the file and confirm with the user before moving on. Repeat for additional agents.

---

## Step 3 — Agent roster (`configs/agents.yaml`)

Output target: `configs/agents.yaml`. Every agent file in `configs/env/` must have a corresponding entry here. This is how agents become aware of one another (the `{{coworkers}}` template is filled from this roster at runtime).

The YAML shape is:
```yaml
agents:
  - name: Kiku
    email: kiku@agents.example.com
    role: "Coordinator"
    description: "Communicates with users. Coordinates other agents."
    tools: [report, snooze, message_tool, mbox_search]
```

For each agent defined in Step 2, collect:

1. **`role`** — short tag-like label (e.g. `"Coordinator"`, `"Social Media"`, `"CRM"`). **Required.**
2. **`description`** — one-sentence summary of what the agent does. This text is injected into peer agents' system prompts. **Required.**
3. **`tools`** — list of tool keys (see below).

### 3a. Discovering available tools

Run the tools_list script to enumerate every tool the running binary supports:

```bash
go run ./scripts/tools_list
```

This prints a JSON array. Each entry has:
- `key` — the value the user puts in `agents.yaml` under `tools:` (empty `key` means the tool is a core tool, always loaded — do **not** add core tools to the YAML).
- `name` — the tool name the LLM sees at runtime.
- `description` — what the tool does.
- `core: true` — present only on core tools.

Parse the output and present the **non-core** tools (those with a non-empty `key`) to the user as a menu. Group them by key — some keys (like MCP bridges) expand to multiple `name`s.

### 3b. Tool selection guidance

- **Coordinator agents should include `snooze`** so they can schedule recurring or one-off tasks. Recommend this whenever the user's stated role for an agent is a coordinator.
- **`report`** is recommended for coordinator agents so they reply to users in a structured form.
- Some tools require credentials. Before letting the user assign a tool, confirm the relevant API key/credential is set in `common.env` (or the agent file):
  - `salesforce_mcp` → `SALESFORCE_CLIENT_ID`, `SALESFORCE_CLIENT_SECRET`, `SALESFORCE_INSTANCE_URL`
  - `buffer_mcp` → `BUFFER_API_KEY`
  - `xero_mcp` → `XERO_CLIENT_ID`, `XERO_CLIENT_SECRET`
  - `tavily_mcp` → `TAVILY_API_KEY`
  - `helpjuice` → `HELPJUICE_API_KEY`, `HELPJUICE_ACCOUNT`
  - `wordpress` → `WEBSITE_URL`, `WORDPRESS_USER`, `WORDPRESS_PASSWORD`
  - `mxmcp` → `MXMCP_API_KEY`
  - `box_cli` → a Box JWT app config dropped at `box_config.json`
  - `file_text` → `TIKA_URL` (defaults to the bundled Tika sidecar)
  - `anthropic_web_search` → only works when `LLM_PROVIDER=anthropic`. Warn the user if they try to assign it to an openrouter agent.
- Tool keys are ordered as listed — keep the user's chosen order.

Write the agent's entry into `configs/agents.yaml` (upsert by email). Confirm with the user.

---

## Step 4 — Email service

Kikubot agents communicate by email, so the user must have an IMAP+SMTP server available. Two paths:

### Path A — User-provided mail server

If the user already has a mail server (Gmail, Office 365, FastMail, their own postfix, etc.):
- Confirm the values they entered in Step 2a (`EMAIL_SERVER`, `SMTP_SERVER`) are correct.
- Remind them that each agent needs a real mailbox on that server with the password entered in Step 2b.
- Nothing else to do for this step — skip to Step 5.

### Path B — Bundled docker-mailserver sidecar (optional)

Ask the user: **"Do you want to use the project's bundled email server (docker-mailserver at `services/dms/`)?"**

If **no**, skip to Step 5.

If **yes**, proceed:

1. **Localhost question.** Ask: "Do you want the mail server to be reachable as `localhost` from the agent containers?" If yes, the user must have the chosen hostname (from `EMAIL_SERVER` / `SMTP_SERVER`) mapped to localhost from inside each Kikubot container. This is done in Step 6 by uncommenting an `extra_hosts:` line in `docker-compose.yml`.
2. **Compose file.** Create `services/dms/docker-compose.yml` by copying the example:
   ```bash
   cp services/dms/docker-compose-example.yml services/dms/docker-compose.yml
   ```
3. **Edit `services/dms/docker-compose.yml`** to set the user's chosen values:
   - `hostname:` → the mail server's public hostname (e.g. `mail.agents.example.com`).
   - `domainname:` → the email domain (the parent domain of `hostname`, e.g. `agents.example.com`). This must match the `@<domain>` of the agent emails defined in Step 2b.
4. **Postfix policy files** (optional but recommended). The configurator generates `services/dms/config/postfix-transport.cf` and `services/dms/config/postfix-sender-access.cf` to restrict which external domains agents may send to and which external senders agents may receive from. If the user wants these limits, ask for:
   - "Limit delivery to specific external domains?" If yes, collect a comma-separated list. The agent's own domain is always allowed.
   - "Limit which external domains may send mail to the agents?" If yes, collect a comma-separated list. The agent's own domain is always allowed.
   - Write these files in the format used by the configurator's `postfix.go` (the inline comments in those files describe the format; the simplest acceptable contents are documented in `services/dms/config/`).
5. **TLS certificates.** The mail server needs `fullchain.pem` and `privkey.pem` at `services/dms/certs/`.
   - For production, the user supplies real certs for `hostname` (e.g. via Let's Encrypt or their CA).
   - **For development/testing, offer to generate self-signed certs.** Ask the user: "Do you want me to generate a self-signed certificate for `<hostname>` / `<domainname>` (development only)?" If they say yes, run the openssl recipe from `services/dms/README.md`, substituting the user's configured `hostname` (from step 3) for the CN and both `hostname` and `domainname` for the subjectAltName:
     ```bash
     mkdir -p services/dms/certs
     openssl req -x509 -newkey rsa:4096 -sha256 -days 3650 -nodes \
       -keyout services/dms/certs/privkey.pem \
       -out   services/dms/certs/fullchain.pem \
       -subj  "/CN=<hostname>" \
       -addext "subjectAltName=DNS:<hostname>,DNS:<domainname>"
     ```
     After generating, **set `EMAIL_INSECURE_TLS=true` in `configs/env/common.env`** so the agent containers accept the self-signed chain. Confirm the change with the user.
   - If the user declines (they will supply their own certs), remind them to drop `fullchain.pem` and `privkey.pem` into `services/dms/certs/` before bringing the mail server up.
6. **Bring the mail server up:**
   ```bash
   cd services/dms && docker compose up -d
   ```
   Confirm it is running:
   ```bash
   docker compose ps
   ```
   Both columns should show the container healthy.
7. **Add mailboxes.** For each agent defined in Step 2b, create a matching account on the mail server. Use the password set in that agent's env file:
   ```bash
   docker exec -it dms setup email add <agent>@<domain> "<password>"
   ```
   Verify the full list:
   ```bash
   docker exec -it dms setup email list
   ```
   The output must include every `AGENT_EMAIL` from Step 2b. Full account-management commands (delete, update, recreate) are documented in `services/dms/README.md`.

---

## Step 5 — Inform about the configurator tool

Once the basic configuration is in place, tell the user:

> "If you'd like to inspect or further modify the configuration in a web dashboard, you can run `go run ./scripts/configurator` (serves on `127.0.0.1:50042` by default). It edits the same files we just created — `configs/env/*.env`, `configs/agents.yaml`, and the DMS sidecar — so you can switch between hand-editing and the dashboard at any time."

This is informational only; do not insist they use it.

---

## Step 6 — Kikubot `docker-compose.yml`

The configurator regenerates `docker-compose.yml` automatically when invoked, but for a hand-run flow:

1. Create the live compose file from the example:
   ```bash
   cp docker-compose-example.yml docker-compose.yml
   ```
2. Edit `docker-compose.yml` so the set of services matches the agents defined in Step 2b:
   - Rename `kiku-alpha` to a key derived from the agent's name (lowercase, non-alphanumeric runs collapsed to `-`).
   - Set `env_file:` to `configs/env/common.env` plus `configs/env/<stem>.env`.
   - Set the volume mapping to `./data/<stem>:/app/data`.
   - For each additional agent defined in Step 2b, uncomment (or add) a matching service block.
3. **If the email server hostname points to `localhost`** (Step 4, Path B with the localhost option), uncomment the `extra_hosts:` line for the email server in each service block:
   ```yaml
   extra_hosts:
     - "host.docker.internal:host-gateway"
     - "mail.agents.example.com:host-gateway"   # ← uncomment this, replace hostname
   ```
   The hostname here must exactly match the host portion of `EMAIL_SERVER` / `SMTP_SERVER` from `common.env`.

Show the resulting file to the user and confirm.

---

## Step 7 — Validate configuration

Before launching, run the doctor:

```bash
go run scripts/kikudoctor/*.go
```

- If the report is clean, tell the user they are ready to launch with `docker compose up -d --build`.
- If the report flags issues, walk the user through each finding one at a time. Common categories:
  - Missing or malformed env vars in `common.env` or an agent file.
  - Agents in `configs/env/` not present in `configs/agents.yaml` (or vice versa).
  - Tool keys assigned to an agent whose required credentials are missing.
  - Compose-file mismatches against the agent roster.

After fixing each issue, re-run `kikudoctor` and continue until the report is clean. Only then declare the configuration complete.

---

## Conversation pacing

- One topic per turn. Do not ask three questions when the next decision depends only on one.
- Read existing files before asking — if a value is already on disk, present it as a default and ask "keep, or change?"
- When you write a file, show a diff (or the full final contents if it's small) and ask for confirmation.
- Never paste secrets back in chat. Refer to them as "the value you put in `common.env`" instead.
- After Step 7 passes, the deployment is configured. Hand off to the user with the launch command, the log-tailing command (`docker compose logs -f`), and a reminder that they can re-run `scripts/configurator` or `scripts/kikudoctor` at any time.
