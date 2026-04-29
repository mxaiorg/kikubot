package tools

import (
	"log"
	"os"
)

// See salesforce_mcp.md for more setup information.

func SalesforceMCP() []ToolDefinition {
	config := LocalMCPConfig{
		ServerName: "salesforce",
		Command:    "npx",
		Args:       []string{"-y", "@tsmztech/mcp-server-salesforce"},
		Env: map[string]string{
			"SALESFORCE_CONNECTION_TYPE": "OAuth_2.0_Client_Credentials",
			"SALESFORCE_CLIENT_ID":       os.Getenv("SALESFORCE_CLIENT_ID"),
			"SALESFORCE_CLIENT_SECRET":   os.Getenv("SALESFORCE_CLIENT_SECRET"),
			"SALESFORCE_INSTANCE_URL":    os.Getenv("SALESFORCE_INSTANCE_URL"),
		},
	}

	tools, err := LocalMCPBridge(config)
	if err != nil {
		log.Println("error initializing Salesforce MCP bridge:", err)
	} else {
		return tools
	}

	return tools
}
