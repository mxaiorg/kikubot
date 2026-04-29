package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
)

// ── Test Tools ──────────────────────────────────────────────────────────

func TestEchoTool() ToolDefinition {
	return ToolDefinition{
		Name:        "test_tool",
		Description: "A test tool that returns the input as a response",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"input": {
					"type": "string",
					"description": "The input to return"
				}
			},
			"required": ["input"]
		}`),
		Execute: func(_ context.Context, input json.RawMessage) (string, error) {
			var params struct {
				Input string `json:"input"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			return params.Input, nil
		},
	}
}

func TestProductTool() ToolDefinition {
	return ToolDefinition{
		Name:        "test_prod_tool",
		Description: "A test tool that returns the widget production numbers",
		InputSchema: []byte(`{"type":"object"}`),
		Execute: func(_ context.Context, _ json.RawMessage) (string, error) {
			production := rand.Intn(1501) + 500
			output := fmt.Sprintf("The widget production numbers are %d widgets", production)
			return output, nil
		},
	}
}
