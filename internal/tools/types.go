package tools

import (
	"context"
	"encoding/json"
	"kikubot/internal/services"
)

// ToolDefinition wraps everything needed to expose a tool to the SDK
// and execute it locally when Claude requests it.
//
// A tool can contribute to the agent's system prompt via one of two fields:
//
//   - StaticSystem: plain text that never varies per email. Goes into the
//     cacheable (stable) portion of the system prompt.
//   - System: a function that may produce different text depending on the
//     email or other mutable state. Goes into the volatile portion and is
//     not cached.
//
// Prefer StaticSystem whenever the instructions don't depend on the email.
type ToolDefinition struct {
	Name         string
	Description  string
	InputSchema  json.RawMessage                                                  // JSON Schema for the tool input
	Execute      func(ctx context.Context, input json.RawMessage) (string, error) // Local execution
	StaticSystem string                                                           // cacheable, email-independent
	System       func(email services.Email) (string, error)                       // volatile, may use email
}

func WithoutTool(tools []ToolDefinition, name string) []ToolDefinition {
	result := make([]ToolDefinition, 0, len(tools)-1)
	for _, t := range tools {
		if t.Name != name {
			result = append(result, t)
		}
	}
	return result
}
