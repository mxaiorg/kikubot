package tools

import (
	"kikubot/internal/config"
	"log"
)

// ── Xero MCP Tool ──────────────────────

/*
https://github.com/xeroapi/xero-mcp-server

Create a 'custom connection' app in Xero at
https://developer.xero.com/app/manage
This will give you a Client ID and Client Secret.

Otherwise, you can create a WebApp and get a Bearer Token.
Not a great solution since it requires a user to log in. But good for testing.
*/

func XeroMCP() []ToolDefinition {
	log.Println("CLIENT_ID: ", config.XeroClientId)

	xeroConfig := LocalMCPConfig{
		ServerName: "xero",
		Command:    "npx",
		Args:       []string{"-y", "@xeroapi/xero-mcp-server@latest"},
		Env: map[string]string{
			"XERO_CLIENT_ID":     config.XeroClientId,
			"XERO_CLIENT_SECRET": config.XeroClientSecret,
			//"XERO_CLIENT_BEARER_TOKEN": xeroDevToken,
		},
	}

	tools, err := LocalMCPBridge(xeroConfig)
	if err != nil {
		log.Println("error initializing Xero MCP bridge:", err)
	} else {
		return tools
	}

	return tools
}
