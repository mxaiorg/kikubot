# agents.yaml

This file defines agents and their associated scripts for the Kikubot platform. It is used to inform an agent of the scripts they use and of other agents they can interact with.

## Tools
Tools are defined in internal/scripts/registry.go

Tools are only read by the corresponding agent when its container is started.

Agents from other Kikubot machines should be added here if they are to be
used by the agents on this machine. Their 'tools' field is optional and will be ignored regardless.
