package tools

import (
	"fmt"
	"kikubot/internal/config"
	"log"
)

// ── mxHERO mxMCP ──────────────────────

// mxHERO MCP is an MCP bridge for mxHERO Mail2Cloud Advanced.
// Mail2Cloud Advanced provides secure access to an organization's email record.
// Mail2Cloud Advanced is designed to allow AI tools to efficiently search across email
// accounts and across millions of emails spanning decades.
// More about Mail2Cloud Advanced: https://mxhero.com/ai-to-email-gateway/
//
// To configure, be sure to set the following environment variables:
// - MXMCP_API_KEY

func MxMCP() []ToolDefinition {
	var tools []ToolDefinition
	mcpTools, mcpErr := MCPBridge("mxMCP",
		"https://lab4-api.mxhero.com/mcp/connect",
		fmt.Sprintf("ApiKey %s", config.MxMcpApiKey),
	)
	if mcpErr != nil {
		log.Println("error initializing MCP bridge:", mcpErr)
	} else {
		tools = append(tools, mcpTools...)
	}

	return tools
}
