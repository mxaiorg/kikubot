# Configurator

This directory contains the configurator script for the project.

```bash
go run ./scripts/configurator                          # serves on 127.0.0.1:50042
go run ./scripts/configurator -port 50042 -addr 0.0.0.0  # bind externally
go run ./scripts/configurator -root /path/to/kikubot   # different deployment
```
