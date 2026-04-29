# Salesforce MCP Setup

A Salesforce local MCP using:

https://github.com/tsmztech/mcp-server-salesforce

## Setup

### 1. Install Salesforce local MCP

In the Dockerfile add or uncomment:

`RUN npm install --global @tsmztech/mcp-server-salesforce`

### 2. Create a Salesforce Connected App

In Salesforce: **Setup (near avatar) → App Manager → New External Client App**

- Enable OAuth settings
- Callback URL: `http://localhost` (or any valid URL for the initial flow)
- Scopes: `api`, `refresh_token`, `offline_access`
- Client Credentials flow in Salesforce requires a specific Connected App configuration — 
  - it must have the "Client Credentials Flow" enabled and 
  - be assigned to a specific user via "Run As" (in Policies > OAuth Policies).
- Save → note the **Client ID** and **Client Secret**

Get your Salesforce **instance URL** from Setup → Company Settings → My Domain → Current My Domain URL or `sf org display --target-org $org --json`

### Set the following environment variables:

* SALESFORCE_CLIENT_ID
* SALESFORCE_CLIENT_SECRET
* SALESFORCE_INSTANCE_URL