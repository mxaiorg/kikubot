package provider

import (
	"context"

	"github.com/anthropics/anthropic-sdk-go"
)

// stubProvider is a no-op Provider used when no LLM API key is configured.
//
// Its purpose is purely to let the process start and stay up in that state:
// the real providers either need a key to do anything useful (Anthropic) or
// log.Fatal at construction without one (OpenRouter). When config.LLMKeyMissing
// is set, NewProvider returns this instead, and the poll loop short-circuits
// inbound mail with a templated "I'm running but need a key" reply
// (see cmd/kikubot/main.go) — so CreateMessage here is never reached on the
// normal path. The canned response is a defensive fallback only.
type stubProvider struct{}

func newStubProvider() *stubProvider { return &stubProvider{} }

const stubMessage = "Kikubot is running, but no LLM API key is configured, so it cannot generate a reply."

func (p *stubProvider) CreateMessage(ctx context.Context, params MessageParams) (*MessageResponse, error) {
	return &MessageResponse{
		Role:       "assistant",
		Content:    []ContentBlock{{Type: "text", Text: stubMessage}},
		StopReason: "end_turn",
	}, nil
}

func (p *stubProvider) BuildTools(defs []ToolDef, model string) (interface{}, []string) {
	return nil, nil
}

func (p *stubProvider) ToHistoryParam(resp *MessageResponse) anthropic.MessageParam {
	return anthropic.NewAssistantMessage(anthropic.NewTextBlock(stubMessage))
}

func (p *stubProvider) NewToolResult(toolUseID, content string, isError bool) anthropic.ContentBlockParamUnion {
	return anthropic.ContentBlockParamUnion{
		OfToolResult: &anthropic.ToolResultBlockParam{
			ToolUseID: toolUseID,
			Content: []anthropic.ToolResultBlockParamContentUnion{
				{OfText: &anthropic.TextBlockParam{Text: content}},
			},
			IsError: anthropic.Bool(isError),
		},
	}
}
