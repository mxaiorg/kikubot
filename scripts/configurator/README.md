# Configurator

This directory contains the configurator script tool for the project.

- Agents can be created and edited
- Agent-specific email server settings can be configured

### Usage options

```bash
go run ./scripts/configurator                          # serves on 127.0.0.1:50042
go run ./scripts/configurator -port 50042 -addr 0.0.0.0  # bind externally
go run ./scripts/configurator -root /path/to/kikubot   # different deployment
```
