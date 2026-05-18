# Configurator

The Configurator is a local web dashboard for setting up and managing a kikubot deployment without hand-editing config files.

It edits two files:

- **`configs/agents.yaml`** — the single source of truth for non-secret deployment config. The dashboard writes shared defaults to the `common:` block (IMAP/SMTP endpoints, history and token budgets, default system prompts) and per-agent identity / role / description / tools / overrides under `agents:`.
- **`configs/secrets.env`** — LLM API keys, per-agent mailbox passwords (`<UPPER_STEM>_EMAIL_PASSWORD`), and tool credentials.

Whenever an agent is added or edited, the configurator also regenerates `docker-compose.yml` from the roster so the running set of containers stays aligned. Access control (whitelist or blacklist), tool selection, and the optional bundled docker-mailserver sidecar are all editable from the same UI.

### Usage

```bash
go run ./scripts/configurator                          # serves on 127.0.0.1:50042
go run ./scripts/configurator -port 50042 -addr 0.0.0.0  # bind externally
go run ./scripts/configurator -root /path/to/kikubot   # different deployment
```
