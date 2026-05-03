package tools

import (
	"kikubot/internal/config"
	"log"
)

// ── Tavily MCP ──────────────────────
// Web search from Tavily

func TavilyMCP() []ToolDefinition {
	var tools []ToolDefinition
	mcpTools, mcpErr := MCPBridge("tavilyMCP",
		"https://mcp.tavily.com/mcp",
		"Bearer "+config.TavilyApiKey)
	if mcpErr != nil {
		log.Println("error initializing MCP bridge:", mcpErr)
	} else {
		tools = append(tools, mcpTools...)
	}

	return tools
}
