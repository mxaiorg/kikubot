# Agent Environment Variables

This directory contains the environment configuration files for the agents.

This directory facilitates the setting of environment variables when launching multiple agent containers at once via docker compose.

The environment files in this directory are provided as examples only. You should customize these files to suit your needs.

## Common Environment File

common.env sets environment variables shared with all agents.

## Per Agent Environment File Names

Per agent environment files allow setting of per agent environment values.

Files should be named with the email account part of the agent's email address. For example, if the agent's email is `agent1@example.com`, the file should be named `agent1.env`.

Typical per agent settings are:

- LLM_PROVIDER
  - Either `anthropic` or `openrouter`
- LLM_MODEL
  - The model to use for the LLM conforming to the LLM_PROVIDER
- LLM_OPENROUTER_BACKUP
  - Only used if LLM_PROVIDER is `openrouter`. 
  - Comma-separated list of backup LLM providers to use if the primary LLM provider is unavailable
- AGENT_TIMEOUT
  - If this agent has longer lasting tasks, increase the number of seconds to wait for the agent to complete.
- SYSTEM_PROMPT
  - This agent's system prompt. If not set, the default system prompt will be used. Include `{{coworkers}}` template variable to provide coworker awareness. Most important for coordinating agents.

### Access Control Environment Variables (ACL)

To control who can access the agent, use the following environment WHITELIST and BLACKLIST variables.

Only one of these variables should be set. WHITELIST takes precedence. If WHITELIST is set, BLACKLIST is ignored.

Lists can include complete email addresses or domains.

`e.g., "bob@partner.com,company.com,agents.com"`

Access control is tested against all senders of an email. If an email is forwarded between multiple agents, then be sure to include all the agents email addresses or the agent's domain in the whitelist in addition to allowed 'human' senders (email addresses or domains).

- WHITELIST
  - Only accept emails from these addresses or domains.
- BLACKLIST
  - If an email is from one of these addresses or domains, reject it.

> EXTREMELY IMPORTANT: Access Control will only be as good as your email domain security settings. Please ensure SPF, DKIM, DMARC, and other email security measures are properly configured to prevent spoofing and phishing attacks.