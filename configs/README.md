# agents.yaml

This file defines agents and their associated scripts for the Kikubot platform. It is used to inform an agent of the scripts they use and of other agents they can interact with.

## Common section

`common` sets configurations shared with all agents.

## Per Agent sections

Per agent configurations allow setting per agent and override common configurations.

Sections should be named with the email account part of the agent's email address. For example, if the agent's email is `agent1@example.com`, the file should be named `agent1`.

Typical per agent settings are:

- llm_provider
    - Either `anthropic` or `openrouter`
- llm_model
    - The model to use for the LLM conforming to the LLM_PROVIDER
- llm_openrouter_backup
    - Only used if LLM_PROVIDER is `openrouter`.
    - Comma-separated list of backup LLM providers to use if the primary LLM provider is unavailable
- agent_timeout
    - If this agent has longer lasting tasks, increase the number of seconds to wait for the agent to complete.
- system_prompt
    - This agent's system prompt. If not set, the default system prompt will be used. Include `{{coworkers}}` template variable to provide coworker awareness. Most important for coordinating agents.

### Access Control Configurations (ACL)

To control who can access the agent, use the configuration `whitelist` and `blacklist` variables.

Only one of these configurations should be set. `whitelist` takes precedence. If `whitelist` is set, `blacklist` is ignored.

Lists can include complete email addresses or domains.

`e.g., "bob@partner.com,company.com,agents.com"`

Access control is tested against all senders of an email. If an email is forwarded between multiple agents, then be sure to include all the agents email addresses or the agent's domain in the whitelist in addition to allowed 'human' senders (email addresses or domains).

- `whitelist`
    - Only accept emails from these addresses or domains.
- `blacklist`
    - If an email is from one of these addresses or domains, reject it.

> ‼️EXTREMELY IMPORTANT: Access Control will only be as good as your email domain security settings. Please ensure SPF, DKIM, DMARC, and other email security measures are properly configured to prevent spoofing and phishing attacks.

## Tools
Tools are defined in internal/scripts/registry.go

Tools are only read by the corresponding agent when its container is started.

Agents from other Kikubot machines should be added here if they are to be
used by the agents on this machine. Their 'tools' field is optional and will be ignored regardless.
