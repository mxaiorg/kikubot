# Configurator

The Configurator is a local web dashboard for setting up and managing a kikubot deployment without hand-editing env files. It writes shared settings to configs/env/common.env (IMAP/SMTP endpoints, LLM credentials, history and token budgets, default system prompt) and per-agent overrides to configs/env/<agent>.env, while keeping agents.yaml in sync with the roster, roles, descriptions, and assigned tools. Access control (whitelist or blacklist, by domain or full address), tool-specific credentials, and knowledge-base content are all editable from the same UI, so bringing a new agent online is a matter of filling in a form rather than tracking down conventions across multiple files.

This directory contains the configurator script tool for the project.

- Agents can be created and edited
- Agent-specific email server settings can be configured

### Usage options

```bash
go run ./scripts/configurator                          # serves on 127.0.0.1:50042
go run ./scripts/configurator -port 50042 -addr 0.0.0.0  # bind externally
go run ./scripts/configurator -root /path/to/kikubot   # different deployment
```
