package tools

import (
	"fmt"
	"kikubot/internal/config"
	"log"
)

func BufferMCP() []ToolDefinition {
	var tools []ToolDefinition

	authValue := fmt.Sprintf("Bearer %s", config.BufferAPIKey)

	mcpTools, mcpErr := MCPBridge("Buffer",
		"https://mcp.buffer.com/mcp",
		authValue)
	if mcpErr != nil {
		log.Println("error initializing MCP bridge:", mcpErr)
	} else {
		tools = append(tools, mcpTools...)
	}

	return tools
}
