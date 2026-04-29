package provider

import (
	"context"
	"encoding/json"

	"github.com/anthropics/anthropic-sdk-go"
)

// Provider abstracts the LLM API so that different backends (Anthropic,
// OpenRouter, etc.) can be swapped via configuration.
type Provider interface {
	// CreateMessage sends a chat completion request and returns a
	// provider-neutral response.
	CreateMessage(ctx context.Context, params MessageParams) (*MessageResponse, error)

	// BuildTools converts tool definitions into a provider-specific
	// representation. The returned value is opaque — pass it straight into
	// MessageParams.Tools.  serverToolNames lists scripts handled server-side
	// (e.g. web search) so the agent loop can skip local execution.
	BuildTools(defs []ToolDef, model string) (tools interface{}, serverToolNames []string)

	// ToHistoryParam converts an API response into an anthropic.MessageParam
	// suitable for appending to the conversation history.
	ToHistoryParam(resp *MessageResponse) anthropic.MessageParam

	// NewToolResult builds a tool-result content block in the internal
	// (Anthropic) format so it can be appended to history.
	NewToolResult(toolUseID, content string, isError bool) anthropic.ContentBlockParamUnion
}

// ToolDef is a slim projection of tools.ToolDefinition that the provider
// package needs. It avoids a circular import between provider ↔ scripts.
type ToolDef struct {
	Name        string
	Description string
	InputSchema json.RawMessage // JSON Schema for the tool input
}

// MessageParams carries everything needed to make a single LLM call.
//
// System is split into two pieces so that providers supporting prompt
// caching (Anthropic) can mark the stable portion as cacheable. Providers
// that don't support caching simply concatenate SystemStable + SystemVolatile.
type MessageParams struct {
	Model          string
	MaxTokens      int
	SystemStable   string // stable across calls — eligible for prompt caching
	SystemVolatile string // volatile per-call (e.g. per-email context)
	Messages       []anthropic.MessageParam
	Tools          interface{} // provider-specific, from BuildTools
}

// ContentBlock is a provider-neutral content block in an LLM response.
type ContentBlock struct {
	Type  string // "text", "tool_use", "server_tool_use", "web_search_tool_result", …
	Text  string // populated for "text" blocks
	ID    string // populated for "tool_use" blocks
	Name  string // populated for "tool_use" / "server_tool_use" blocks
	Input json.RawMessage

	// rawJSON stores the original JSON for blocks that need special
	// serialization when appended to history (e.g. server-side results).
	rawJSON string
}

// MessageResponse is the provider-neutral shape returned by CreateMessage.
type MessageResponse struct {
	Role       string // "assistant"
	Content    []ContentBlock
	StopReason string // "end_turn", "tool_use", "max_tokens", …

	// providerData is an opaque per-provider attachment. The Anthropic
	// provider stores the original typed response here so that
	// ToHistoryParam can use the SDK's proper per-variant ToParam()
	// conversions for server-side result blocks (web_search,
	// code_execution, …) instead of round-tripping through raw JSON,
	// which the streaming accumulator corrupts.
	providerData any
}
