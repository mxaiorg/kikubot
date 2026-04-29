package tools

// ── Anthropic Web Search Tool ──────────────────────

// AnthropicWebSearch is a tool built into the Anthropic SDK.
// This can only be used by Agents that use Anthropic Models:
// Haiku or Sonnet/Opus.
// * For other LLMs use another web search provider like Tavily
//   See tavily_mcp.go for an example.

// AnthropicWebSearch returns a ToolDefinition marker for the server-side
// web search tool. It has no InputSchema or Execute — buildSDKTools
// recognises it by name and injects the correct ToolUnionParam directly.
func AnthropicWebSearch() ToolDefinition {
	return ToolDefinition{
		Name:         "anthropic-web-search",
		Description:  "Search the web using Anthropic's web search API",
		StaticSystem: "- You can search the web with the anthropic-web-search tool\n",
	}
}
