package tools

import "log"

// ── Tavily MCP ──────────────────────
// Web search from Tavily

func TavilyMCP() []ToolDefinition {
	var tools []ToolDefinition
	mcpTools, mcpErr := MCPBridge("tavilyMCP",
		"https://mcp.tavily.com/mcp",
		"Bearer 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")
	if mcpErr != nil {
		log.Println("error initializing MCP bridge:", mcpErr)
	} else {
		tools = append(tools, mcpTools...)
	}

	return tools
}
