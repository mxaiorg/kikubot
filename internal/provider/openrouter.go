package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"kikubot/internal/config"
	"log"
	"os"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/revrost/go-openrouter"
)

// OpenRouterProvider implements Provider using the OpenRouter API via
// the go-openrouter SDK.
type OpenRouterProvider struct {
	client *openrouter.Client
}

func NewOpenRouterProvider() *OpenRouterProvider {
	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		log.Fatal("OPENROUTER_API_KEY environment variable is required for OpenRouter provider")
	}
	return &OpenRouterProvider{
		client: openrouter.NewClient(apiKey),
	}
}

func (p *OpenRouterProvider) CreateMessage(ctx context.Context, params MessageParams) (*MessageResponse, error) {
	// OpenRouter doesn't support Anthropic-style cache_control uniformly
	// across providers, so just concatenate the stable and volatile system
	// sections into a single system message.
	system := params.SystemStable
	if params.SystemVolatile != "" {
		if system != "" {
			system += "\n\n"
		}
		system += params.SystemVolatile
	}

	// Convert Anthropic message history to OpenRouter messages
	msgs := p.convertMessages(system, params.Messages)

	req := openrouter.ChatCompletionRequest{
		Model:     params.Model,
		Messages:  msgs,
		MaxTokens: params.MaxTokens,
	}

	// Add fallback models from LLM_OPENROUTER_BACKUP (comma-separated).
	if config.LlmOpenRouterBackup != "" {
		var models []string
		for _, m := range strings.Split(config.LlmOpenRouterBackup, ",") {
			m = strings.TrimSpace(m)
			if m != "" {
				models = append(models, m)
			}
		}
		if len(models) > 0 {
			req.Models = append([]string{params.Model}, models...)
		}
	}
	//log.Printf("MODELS: %s", strings.Join(req.Models, ","))

	if params.Tools != nil {
		req.Tools = params.Tools.([]openrouter.Tool)
	}

	var resp openrouter.ChatCompletionResponse
	var err error
	maxRetries := 3
	for attempt := range maxRetries {
		resp, err = p.client.CreateChatCompletion(ctx, req)
		if err == nil {
			break
		}
		errStr := err.Error()
		if strings.Contains(errStr, "529") || strings.Contains(errStr, "429") || strings.Contains(errStr, "overloaded") || strings.Contains(errStr, "rate") {
			backoff := time.Duration(1<<attempt) * time.Second
			log.Printf("  ⏳ retryable OpenRouter API error (attempt %d/%d), retrying in %v: %v",
				attempt+1, maxRetries, backoff, err)
			select {
			case <-time.After(backoff):
				continue
			case <-ctx.Done():
				return nil, fmt.Errorf("context cancelled during retry backoff: %w", ctx.Err())
			}
		}
		break
	}
	if err != nil {
		return nil, fmt.Errorf("openrouter api call failed: %w", err)
	}

	return p.toMessageResponse(resp), nil
}

func (p *OpenRouterProvider) BuildTools(defs []ToolDef, model string) (interface{}, []string) {
	tools := make([]openrouter.Tool, 0, len(defs))
	var serverTools []string

	for _, td := range defs {
		// Skip Anthropic-only server-side scripts
		if td.Name == "anthropic-web-search" {
			log.Printf("⚠️  skipping anthropic-web-search tool (not available on OpenRouter; use plugins for web search)")
			continue
		}

		if td.Name == "" || td.InputSchema == nil {
			continue
		}

		// The FunctionDefinition.Parameters field accepts any JSON-marshalable
		// value. json.RawMessage works directly.
		tools = append(tools, openrouter.Tool{
			Type: openrouter.ToolTypeFunction,
			Function: &openrouter.FunctionDefinition{
				Name:        td.Name,
				Description: td.Description,
				Parameters:  td.InputSchema,
			},
		})
	}
	return tools, serverTools
}

// ToHistoryParam converts an OpenRouter response into an Anthropic
// MessageParam for history persistence. Tool calls are mapped back to the
// Anthropic tool_use block format.
func (p *OpenRouterProvider) ToHistoryParam(resp *MessageResponse) anthropic.MessageParam {
	var mp anthropic.MessageParam
	mp.Role = anthropic.MessageParamRoleAssistant

	for _, c := range resp.Content {
		switch c.Type {
		case "text":
			mp.Content = append(mp.Content, anthropic.ContentBlockParamUnion{
				OfText: &anthropic.TextBlockParam{Text: c.Text},
			})
		case "tool_use":
			mp.Content = append(mp.Content, anthropic.ContentBlockParamUnion{
				OfToolUse: &anthropic.ToolUseBlockParam{
					ID:    c.ID,
					Name:  c.Name,
					Input: json.RawMessage(c.Input),
				},
			})
		default:
			if c.rawJSON != "" {
				mp.Content = append(mp.Content, param.Override[anthropic.ContentBlockParamUnion](json.RawMessage(c.rawJSON)))
			}
		}
	}

	// Ensure there's at least one content block
	if len(mp.Content) == 0 {
		mp.Content = []anthropic.ContentBlockParamUnion{
			{OfText: &anthropic.TextBlockParam{Text: ""}},
		}
	}

	return mp
}

func (p *OpenRouterProvider) NewToolResult(toolUseID, content string, isError bool) anthropic.ContentBlockParamUnion {
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

// ── conversion helpers ──────────────────────────────────────────────────

// convertMessages translates Anthropic MessageParam history into
// OpenRouter ChatCompletionMessage slices.
func (p *OpenRouterProvider) convertMessages(system string, history []anthropic.MessageParam) []openrouter.ChatCompletionMessage {
	var msgs []openrouter.ChatCompletionMessage

	// System prompt as the first message
	if system != "" {
		msgs = append(msgs, openrouter.SystemMessage(system))
	}

	for _, m := range history {
		role := string(m.Role)

		// param.Override-wrapped messages (loaded from memory) have empty
		// struct fields — the data lives only in the JSON override. We must
		// parse the raw JSON to convert them to OpenRouter format.
		if role == "" || len(m.Content) == 0 {
			converted := rawMessageToOpenRouter(m)
			msgs = append(msgs, converted...)
			continue
		}

		switch role {
		case "user":
			msgs = append(msgs, p.convertUserMessages(m)...)
		case "assistant":
			msgs = append(msgs, p.convertAssistantMessage(m))
		default:
			// Fallback: extract text content
			msgs = append(msgs, openrouter.ChatCompletionMessage{
				Role:    role,
				Content: openrouter.Content{Text: extractText(m)},
			})
		}
	}

	return msgs
}

// convertUserMessages handles user messages, including tool results and
// multimodal content blocks. Returns a slice because Anthropic bundles
// multiple tool results into one user message, but OpenRouter requires
// each tool result as a separate "tool" role message.
func (p *OpenRouterProvider) convertUserMessages(m anthropic.MessageParam) []openrouter.ChatCompletionMessage {
	var toolResults []anthropic.ContentBlockParamUnion
	var otherBlocks []anthropic.ContentBlockParamUnion
	for _, block := range m.Content {
		if block.OfToolResult != nil {
			toolResults = append(toolResults, block)
		} else {
			otherBlocks = append(otherBlocks, block)
		}
	}

	// Pure tool results → one OpenRouter tool message per result
	if len(toolResults) > 0 && len(otherBlocks) == 0 {
		var msgs []openrouter.ChatCompletionMessage
		for _, tr := range toolResults {
			text := extractToolResultText(tr.OfToolResult)
			msgs = append(msgs, openrouter.ToolMessage(tr.OfToolResult.ToolUseID, text))
		}
		return msgs
	}

	// Build multimodal parts from non-tool-result blocks
	var parts []openrouter.ChatMessagePart
	for _, block := range m.Content {
		if block.OfToolResult != nil {
			continue
		}
		if block.OfText != nil {
			parts = append(parts, openrouter.ChatMessagePart{
				Type: openrouter.ChatMessagePartTypeText,
				Text: block.OfText.Text,
			})
		} else if block.OfImage != nil {
			src := block.OfImage.Source
			if src.OfBase64 != nil {
				dataURL := fmt.Sprintf("data:%s;base64,%s", src.OfBase64.MediaType, src.OfBase64.Data)
				parts = append(parts, openrouter.ChatMessagePart{
					Type:     openrouter.ChatMessagePartTypeImageURL,
					ImageURL: &openrouter.ChatMessageImageURL{URL: dataURL},
				})
			}
		} else if block.OfDocument != nil {
			doc := block.OfDocument
			if doc.Source.OfBase64 != nil {
				parts = append(parts, openrouter.ChatMessagePart{
					Type: openrouter.ChatMessagePartTypeFile,
					File: &openrouter.FileContent{
						Filename: "document.pdf",
						FileData: fmt.Sprintf("data:application/pdf;base64,%s", doc.Source.OfBase64.Data),
					},
				})
			} else if doc.Source.OfText != nil {
				title := ""
				if doc.Title.Value != "" {
					title = fmt.Sprintf("[%s]\n", doc.Title.Value)
				}
				parts = append(parts, openrouter.ChatMessagePart{
					Type: openrouter.ChatMessagePartTypeText,
					Text: title + doc.Source.OfText.Data,
				})
			}
		}
	}

	var msg openrouter.ChatCompletionMessage
	if len(parts) == 1 && parts[0].Type == openrouter.ChatMessagePartTypeText {
		msg = openrouter.UserMessage(parts[0].Text)
	} else if len(parts) > 0 {
		msg = openrouter.ChatCompletionMessage{
			Role:    openrouter.ChatMessageRoleUser,
			Content: openrouter.Content{Multi: parts},
		}
	} else {
		msg = openrouter.UserMessage(extractText(m))
	}

	// If there were mixed tool results + text, append tool messages after
	msgs := []openrouter.ChatCompletionMessage{msg}
	for _, tr := range toolResults {
		text := extractToolResultText(tr.OfToolResult)
		msgs = append(msgs, openrouter.ToolMessage(tr.OfToolResult.ToolUseID, text))
	}
	return msgs
}

// convertAssistantMessage maps an assistant MessageParam (which may
// contain tool_use blocks) to an OpenRouter ChatCompletionMessage.
func (p *OpenRouterProvider) convertAssistantMessage(m anthropic.MessageParam) openrouter.ChatCompletionMessage {
	msg := openrouter.ChatCompletionMessage{
		Role: openrouter.ChatMessageRoleAssistant,
	}

	var textParts []string
	var toolCalls []openrouter.ToolCall

	for _, block := range m.Content {
		if block.OfText != nil {
			textParts = append(textParts, block.OfText.Text)
		} else if block.OfToolUse != nil {
			args, _ := json.Marshal(block.OfToolUse.Input)
			toolCalls = append(toolCalls, openrouter.ToolCall{
				ID:   block.OfToolUse.ID,
				Type: openrouter.ToolTypeFunction,
				Function: openrouter.FunctionCall{
					Name:      block.OfToolUse.Name,
					Arguments: string(args),
				},
			})
		}
	}

	msg.Content = openrouter.Content{Text: strings.Join(textParts, "\n")}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}

	return msg
}

// toMessageResponse converts an OpenRouter response to the neutral type.
func (p *OpenRouterProvider) toMessageResponse(resp openrouter.ChatCompletionResponse) *MessageResponse {
	mr := &MessageResponse{
		Role: "assistant",
	}

	if len(resp.Choices) == 0 {
		return mr
	}

	choice := resp.Choices[0]

	// Map finish reason
	switch choice.FinishReason {
	case openrouter.FinishReasonStop:
		mr.StopReason = "end_turn"
	case openrouter.FinishReasonToolCalls:
		mr.StopReason = "tool_use"
	case openrouter.FinishReasonLength:
		mr.StopReason = "max_tokens"
	default:
		mr.StopReason = string(choice.FinishReason)
	}

	// Extract text content
	if choice.Message.Content.Text != "" {
		mr.Content = append(mr.Content, ContentBlock{
			Type: "text",
			Text: choice.Message.Content.Text,
		})
	}

	// Extract tool calls
	for _, tc := range choice.Message.ToolCalls {
		mr.Content = append(mr.Content, ContentBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: json.RawMessage(tc.Function.Arguments),
		})
	}

	return mr
}

// ── utility functions ────────────────────────────────────────────────────

// parseRoleFromRaw extracts the "role" field from a param.Override-wrapped
// MessageParam by marshalling it to JSON and peeking at the role key.
func parseRoleFromRaw(m anthropic.MessageParam) string {
	raw, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	var peek struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(raw, &peek); err != nil {
		return ""
	}
	return peek.Role
}

// rawMessageToOpenRouter converts a param.Override-wrapped MessageParam
// (loaded from memory) into OpenRouter messages by parsing the raw JSON.
// It returns one or more messages because user messages with multiple tool
// results must be split into individual tool messages for OpenRouter.
func rawMessageToOpenRouter(m anthropic.MessageParam) []openrouter.ChatCompletionMessage {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil
	}

	var parsed struct {
		Role    string            `json:"role"`
		Content []json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil
	}

	var msgs []openrouter.ChatCompletionMessage
	var textParts []string
	var toolCalls []openrouter.ToolCall

	for _, blockRaw := range parsed.Content {
		var block struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
			IsError   bool            `json:"is_error"`
		}
		if err := json.Unmarshal(blockRaw, &block); err != nil {
			continue
		}

		switch block.Type {
		case "text":
			textParts = append(textParts, block.Text)
		case "tool_use":
			args, _ := json.Marshal(block.Input)
			toolCalls = append(toolCalls, openrouter.ToolCall{
				ID:   block.ID,
				Type: openrouter.ToolTypeFunction,
				Function: openrouter.FunctionCall{
					Name:      block.Name,
					Arguments: string(args),
				},
			})
		case "tool_result":
			text := extractToolResultTextFromRaw(block.Content)
			msgs = append(msgs, openrouter.ToolMessage(block.ToolUseID, text))
		}
	}

	switch parsed.Role {
	case "assistant":
		msg := openrouter.ChatCompletionMessage{
			Role:    openrouter.ChatMessageRoleAssistant,
			Content: openrouter.Content{Text: strings.Join(textParts, "\n")},
		}
		if len(toolCalls) > 0 {
			msg.ToolCalls = toolCalls
		}
		return append([]openrouter.ChatCompletionMessage{msg}, msgs...)
	case "user":
		if len(msgs) > 0 {
			// Tool result messages already built
			return msgs
		}
		return []openrouter.ChatCompletionMessage{
			openrouter.UserMessage(strings.Join(textParts, "\n")),
		}
	default:
		return []openrouter.ChatCompletionMessage{
			{Role: parsed.Role, Content: openrouter.Content{Text: strings.Join(textParts, "\n")}},
		}
	}
}

// extractToolResultTextFromRaw extracts text from a tool_result content field.
func extractToolResultTextFromRaw(content json.RawMessage) string {
	// Try as string first
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s
	}
	// Try as array of content blocks
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &blocks); err == nil {
		for _, b := range blocks {
			if b.Type == "text" {
				return b.Text
			}
		}
	}
	return string(content)
}

// extractText pulls the concatenated text from an Anthropic MessageParam.
// It handles both structured (OfText) and raw JSON (param.Override)
// message formats.
func extractText(m anthropic.MessageParam) string {
	var texts []string
	for _, block := range m.Content {
		if block.OfText != nil {
			texts = append(texts, block.OfText.Text)
		}
	}
	if len(texts) > 0 {
		return strings.Join(texts, "\n")
	}

	// Fallback: try to extract from raw JSON (for param.Override messages)
	raw, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	var parsed struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ""
	}

	// Try as array of content blocks
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(parsed.Content, &blocks); err == nil {
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				texts = append(texts, b.Text)
			}
		}
	}
	return strings.Join(texts, "\n")
}

// extractToolResultText gets the text content from a tool result block.
func extractToolResultText(tr *anthropic.ToolResultBlockParam) string {
	for _, c := range tr.Content {
		if c.OfText != nil {
			return c.OfText.Text
		}
	}
	return ""
}
