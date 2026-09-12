# Configurator

The Configurator is a local web dashboard for setting up and managing a kikubot deployment without hand-editing config files.

It edits these files:

- **`configs/agents.yaml`** — the single source of truth for non-secret deployment config. The dashboard writes shared defaults to the `common:` block (IMAP/SMTP endpoints, history and token budgets, default system prompts) and per-agent identity / role / description / tools / overrides under `agents:`.
- **`configs/mcp_servers.yaml`** — the remote (Streamable HTTP) MCP server catalog. The **MCP Servers** page adds, edits and deletes rows (key, URL, auth mode, env-var names); the secret *values* those names refer to are written to `secrets.env` from the same form.
- **`configs/secrets.env`** — LLM API keys, per-agent mailbox passwords (`<UPPER_STEM>_EMAIL_PASSWORD`), and tool credentials.
- **`configs/knowledge/`** — the per-agent and shared markdown knowledge base (see below).
- **`services/dms/`** — the bundled docker-mailserver sidecar: the generated postfix maps under `config/`, `docker-compose.yml`, and `.email-service-disabled`, the marker recorded when **Use this service** is cleared on the Email Service page. Clearing the box leaves the generated files in place, so re-enabling restores the previous settings.

Whenever an agent is added or edited, the configurator also regenerates `docker-compose.yml` from the roster so the running set of containers stays aligned. Access control (whitelist or blacklist), tool selection, and the optional bundled docker-mailserver sidecar are all editable from the same UI. Tools backed by local-only source (see [private tools](../../internal/tools/README.md#private-tools-internaltools_priv)) are shown with a **private** badge in the tool picker. Tools backed by a remote MCP server (declared in `configs/mcp_servers.yaml`) are shown with an **MCP** badge — selecting one assigns the key to the agent, but the server entry and its credentials (and, for OAuth2, the seeded token file) must exist for the tool to work at runtime.

### MCP Servers

The **MCP Servers** page (under *Tools*) manages `configs/mcp_servers.yaml` — the declarative catalog of remote MCP servers described in the [main README](../../README.md#remote-mcp-servers--configsmcp_serversyaml). Each row is a tool key; assign it to an agent from the agent form's tool picker. The form adapts to the auth mode (`none`, `bearer`, `apikey`, `oauth2`), stores only env-var *names* in the YAML, and writes the secret *values* to `configs/secrets.env` (blank value = keep the current one). The list shows, per server, whether its referenced secrets are set, which agents use it, and — for OAuth2 servers — which of those agents still lack a seeded `data/<agent>/oauth/<key>.json` token file. Renaming a key rewrites every agent's `tools:` list to match; deleting a server strips the key from the agents that used it (the confirm dialog names them). Keys that collide with a built-in tool from `internal/tools/registry.go` are rejected.

### Hot reload of tools and MCP servers

Running agents pick up tool-assignment changes (an agent's `tools:` list) and MCP catalog edits **without a rebuild or restart**. The generated `docker-compose.yml` bind-mounts `./configs/agents.yaml` and `./configs/mcp_servers.yaml` read-only into every service; each agent re-checks both files' mtimes on its ~30s poll and rebuilds its tool set when either changed, and the configurator sends `SIGHUP` to the affected container(s) after a save (editing an existing agent signals that agent; saving or deleting an MCP server signals the agents that reference it) so the change lands immediately. Rebuilding reuses already-constructed tools whose definition didn't change, so unrelated local MCP subprocesses aren't respawned. Only the tool set is live; an agent's model, prompt, ACL and password are still read at startup and need `docker compose up -d` — the flash message after an agent save says so. Containers created from an older `docker-compose.yml` (without the two file mounts) keep the baked-in copies until recreated.

### Knowledge editor

The dashboard includes an editor for each agent's knowledge base — the markdown files under `configs/knowledge/common/` (shared by every agent) and `configs/knowledge/<agent>/` (loaded only by that agent). You can add, edit, rename, and delete files; renaming is how you re-order them (the runtime concatenates by filename, so numeric prefixes like `01_`, `02_` control order). Saving refuses to silently overwrite a different existing file, and the page warns before you navigate away with unsaved changes.

The generated `docker-compose.yml` bind-mounts `./configs/knowledge:/app/knowledge:ro` into every service, and agents hot-reload knowledge on change — so edits take effect **without rebuilding the image or restarting the container**. After a successful save or delete, the configurator sends `SIGHUP` to the affected container(s) (`common` edits signal every agent; an agent-scoped edit signals just that one) so the change propagates immediately; if the signal can't be delivered (e.g. `docker` isn't on `PATH`, or the containers aren't running), the agent's ~30s knowledge poll still picks it up. The flash message after each save reports which path was taken.

### Usage

```bash
go run ./scripts/configurator                          # serves on 127.0.0.1:50042
go run ./scripts/configurator -port 50042 -addr 0.0.0.0  # bind externally
go run ./scripts/configurator -root /path/to/kikubot   # different deployment
```
