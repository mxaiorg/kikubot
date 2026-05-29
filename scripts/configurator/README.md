# Configurator

The Configurator is a local web dashboard for setting up and managing a kikubot deployment without hand-editing config files.

It edits these files:

- **`configs/agents.yaml`** — the single source of truth for non-secret deployment config. The dashboard writes shared defaults to the `common:` block (IMAP/SMTP endpoints, history and token budgets, default system prompts) and per-agent identity / role / description / tools / overrides under `agents:`.
- **`configs/secrets.env`** — LLM API keys, per-agent mailbox passwords (`<UPPER_STEM>_EMAIL_PASSWORD`), and tool credentials.
- **`configs/knowledge/`** — the per-agent and shared markdown knowledge base (see below).

Whenever an agent is added or edited, the configurator also regenerates `docker-compose.yml` from the roster so the running set of containers stays aligned. Access control (whitelist or blacklist), tool selection, and the optional bundled docker-mailserver sidecar are all editable from the same UI. Tools backed by local-only source (see [private tools](../../internal/tools/README.md#private-tools-internaltools_priv)) are shown with a **private** badge in the tool picker.

### Knowledge editor

The dashboard includes an editor for each agent's knowledge base — the markdown files under `configs/knowledge/common/` (shared by every agent) and `configs/knowledge/<agent>/` (loaded only by that agent). You can add, edit, rename, and delete files; renaming is how you re-order them (the runtime concatenates by filename, so numeric prefixes like `01_`, `02_` control order). Saving refuses to silently overwrite a different existing file, and the page warns before you navigate away with unsaved changes.

The generated `docker-compose.yml` bind-mounts `./configs/knowledge:/app/knowledge:ro` into every service, and agents hot-reload knowledge on change — so edits take effect **without rebuilding the image or restarting the container**. After a successful save or delete, the configurator sends `SIGHUP` to the affected container(s) (`common` edits signal every agent; an agent-scoped edit signals just that one) so the change propagates immediately; if the signal can't be delivered (e.g. `docker` isn't on `PATH`, or the containers aren't running), the agent's ~30s knowledge poll still picks it up. The flash message after each save reports which path was taken.

### Usage

```bash
go run ./scripts/configurator                          # serves on 127.0.0.1:50042
go run ./scripts/configurator -port 50042 -addr 0.0.0.0  # bind externally
go run ./scripts/configurator -root /path/to/kikubot   # different deployment
```
