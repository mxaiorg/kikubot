package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

// AnthropicProvider implements Provider using the Anthropic Messages API.
type AnthropicProvider struct {
	client anthropic.Client
}

// anthropicRetryBackoff is how long CreateMessage waits before retrying a
// retryable (overloaded / rate-limited) attempt: 1s, 2s, 4s… A variable so
// tests can retry without sleeping.
var anthropicRetryBackoff = func(attempt int) time.Duration {
	return time.Duration(1<<attempt) * time.Second
}

func NewAnthropicProvider() *AnthropicProvider {
	return &AnthropicProvider{
		client: anthropic.NewClient(), // reads ANTHROPIC_API_KEY from env
	}
}

func (p *AnthropicProvider) CreateMessage(ctx context.Context, params MessageParams) (*MessageResponse, error) {
	// Build the system prompt as up to two blocks: a stable (cacheable)
	// prefix and an optional volatile suffix. A cache_control breakpoint
	// on the stable block caches [scripts + stable system] for 5 minutes.
	var systemBlocks []anthropic.TextBlockParam
	if params.SystemStable != "" {
		systemBlocks = append(systemBlocks, anthropic.TextBlockParam{
			Text: params.SystemStable,
			CacheControl: anthropic.CacheControlEphemeralParam{
				Type: "ephemeral",
			},
		})
	}
	if params.SystemVolatile != "" {
		systemBlocks = append(systemBlocks, anthropic.TextBlockParam{
			Text: params.SystemVolatile,
		})
	}

	sdkParams := anthropic.MessageNewParams{
		Model:     anthropic.Model(params.Model),
		MaxTokens: int64(params.MaxTokens),
		System:    systemBlocks,
		Messages:  params.Messages,
	}

	if params.Tools != nil {
		sdkParams.Tools = params.Tools.([]anthropic.ToolUnionParam)
	}

	// Debug: dump the outgoing request so we can verify cache_control is
	// present and see what the cached prefix looks like.
	if os.Getenv("DEBUG_ANTHROPIC_REQUEST") == "1" {
		if raw, mErr := json.MarshalIndent(sdkParams, "", "  "); mErr == nil {
			log.Printf("  🔎 outgoing request:\n%s", string(raw))
		}
	}

	// Use streaming to avoid the "streaming is required for operations that
	// may take longer than 10 minutes" error from the API. The SDK's
	// Accumulate method collects streamed events into a complete Message.
	//
	// We also capture the ORIGINAL raw JSON of each content block at the
	// ContentBlockStart event. The SDK's Accumulate later clobbers
	// ContentBlockUnion.JSON.raw by re-marshaling the union struct with
	// stdlib json, which corrupts fields whose tags (`json:",inline"`) are
	// only understood by the SDK's apijson marshaler — producing garbage
	// like `"OfWebSearchResultBlockArray":null` for server-side result
	// blocks. Keeping the original raw per block lets us round-trip them
	// verbatim in ToHistoryParam.
	var resp *anthropic.Message
	var originalRaw []string
	var err error
	maxRetries := 3
	for attempt := range maxRetries {
		// Each attempt starts clean. The error used to carry over from a
		// failed attempt, so the next one skipped reading its stream and
		// "failed" with the stale error — no retry could ever succeed.
		err = nil
		stream := p.client.Messages.NewStreaming(ctx, sdkParams)
		acc := &streamAccumulator{}
		for stream.Next() {
			if accErr := acc.add(stream.Current()); accErr != nil {
				err = fmt.Errorf("stream accumulate error: %w", accErr)
				break
			}
		}
		if err == nil {
			err = stream.Err()
		}
		if err == nil {
			resp = acc.message()
			originalRaw = acc.originalRaw
			break
		}
		errStr := err.Error()
		retryable := strings.Contains(errStr, "529") || strings.Contains(errStr, "429") || strings.Contains(errStr, "overloaded")
		if !retryable || attempt == maxRetries-1 {
			break // non-retryable, or out of attempts: don't sleep before giving up
		}
		backoff := anthropicRetryBackoff(attempt)
		log.Printf("  ⏳ retryable API error (attempt %d/%d), retrying in %v: %v",
			attempt+1, maxRetries, backoff, err)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil, fmt.Errorf("context cancelled during retry backoff: %w", ctx.Err())
		}
	}
	if err != nil {
		return nil, fmt.Errorf("anthropic api call failed: %w", err)
	}

	u := resp.Usage
	log.Printf("  📊 tokens: in=%d out=%d cache_read=%d cache_write=%d",
		u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens)
	if os.Getenv("DEBUG_ANTHROPIC_REQUEST") == "1" {
		if raw, mErr := json.MarshalIndent(u, "", "  "); mErr == nil {
			log.Printf("  📊 full usage:\n%s", string(raw))
		}
	}

	out := anthropicResponseToMessage(resp)
	// Stash the typed SDK response so ToHistoryParam can use the SDK's
	// per-variant ToParam() conversions for server-side result blocks.
	out.providerData = resp
	// Replace each block's rawJSON with the pre-accumulate original, but only
	// for the server-side result blocks whose accumulated raw JSON is corrupt
	// (see originalRaw capture above). The original JSON round-trips cleanly
	// back to the API. We must NOT do this for blocks that stream their
	// payload via deltas — notably "thinking", whose ContentBlockStart
	// snapshot has an empty thinking body and an empty signature; overwriting
	// with that snapshot persisted an unusable thinking block that the API
	// rejects on re-submission. Those blocks keep their accumulated raw JSON
	// and are rebuilt from typed fields in ToHistoryParam.
	for i := range out.Content {
		if i < len(originalRaw) && originalRaw[i] != "" && needsOriginalRaw(out.Content[i].Type) {
			out.Content[i].rawJSON = originalRaw[i]
		}
	}
	return out, nil
}

func (p *AnthropicProvider) BuildTools(defs []ToolDef, model string) (interface{}, []string) {
	agentTools := make([]anthropic.ToolUnionParam, 0, len(defs))
	var serverTools []string

	for _, td := range defs {
		// Check if the tool is a built-in SDK tool (e.g. web search)
		if sdkTool := anthropicBuiltinTool(td.Name, model); sdkTool != nil {
			agentTools = append(agentTools, *sdkTool)
			serverTools = append(serverTools, td.Name)
			continue
		}

		// Minimum required fields for a regular LLM tool
		if td.Name == "" || td.InputSchema == nil {
			continue
		}

		var schema anthropic.ToolInputSchemaParam
		err := json.Unmarshal(td.InputSchema, &schema)
		if err != nil {
			log.Printf("error parsing tool input schema for %s: %s", td.Name, err)
			continue
		}

		agentTools = append(agentTools, anthropic.ToolUnionParam{
			OfTool: &anthropic.ToolParam{
				Name:        td.Name,
				Description: anthropic.String(td.Description),
				InputSchema: schema,
			},
		})
	}
	return agentTools, serverTools
}

// ToHistoryParam converts an API response into an anthropic.MessageParam.
//
// For server-side result blocks (code_execution_tool_result,
// web_search_tool_result, etc.) we prefer the SDK's typed ToParam()
// conversion from the original accumulated *anthropic.Message, because the
// raw JSON the streaming accumulator stores on ContentBlockUnion.JSON.raw is
// corrupted: it's produced by re-marshaling the union struct, which dumps
// every variant's zero-value fields (`citations:null`, `text:""`,
// `content:{"OfWebSearchResultBlockArray":null,…}`, `tool_use_id:""` …). The
// API rejects those extras ("Extra inputs are not permitted") or fails to
// match server_tool_use ↔ result pairs.
func (p *AnthropicProvider) ToHistoryParam(resp *MessageResponse) anthropic.MessageParam {
	var mp anthropic.MessageParam
	mp.Role = anthropic.MessageParamRole(resp.Role)
	mp.Content = make([]anthropic.ContentBlockParamUnion, len(resp.Content))

	for i, c := range resp.Content {
		switch c.Type {
		case "code_execution_tool_result", "bash_code_execution_tool_result",
			"text_editor_code_execution_tool_result",
			"web_search_tool_result", "web_fetch_tool_result":
			// Raw JSON here is the original pre-accumulate block JSON
			// (see CreateMessage); pass it through unchanged.
			mp.Content[i] = param.Override[anthropic.ContentBlockParamUnion](json.RawMessage(c.rawJSON))
		case "server_tool_use":
			// The SDK's streaming Accumulate() re-marshals ContentBlockUnion
			// into raw JSON with many empty fields that the API rejects on
			// input. Build a clean ServerToolUseBlockParam from just the
			// fields this block actually needs.
			mp.Content[i] = anthropic.ContentBlockParamUnion{
				OfServerToolUse: &anthropic.ServerToolUseBlockParam{
					ID:    c.ID,
					Name:  anthropic.ServerToolUseBlockParamName(c.Name),
					Input: c.Input,
				},
			}
		case "thinking":
			// Round-trip the reasoning text and its cryptographic signature
			// verbatim. The signature must be preserved exactly or the API
			// rejects the block on the next turn (extended thinking + tool use
			// requires the prior thinking blocks back with valid signatures).
			mp.Content[i] = anthropic.ContentBlockParamUnion{
				OfThinking: &anthropic.ThinkingBlockParam{
					Thinking:  c.Thinking,
					Signature: c.Signature,
				},
			}
		case "redacted_thinking":
			mp.Content[i] = anthropic.ContentBlockParamUnion{
				OfRedactedThinking: &anthropic.RedactedThinkingBlockParam{
					Data: c.Data,
				},
			}
		case "text":
			// Strip citations — web-search citations can contain empty URLs
			// that the API rejects on re-submission.
			mp.Content[i] = anthropic.ContentBlockParamUnion{
				OfText: &anthropic.TextBlockParam{Text: c.Text},
			}
		case "tool_use":
			mp.Content[i] = anthropic.ContentBlockParamUnion{
				OfToolUse: &anthropic.ToolUseBlockParam{
					ID:    c.ID,
					Name:  c.Name,
					Input: c.Input,
				},
			}
		default:
			// Fallback: use raw JSON if available
			if c.rawJSON != "" {
				mp.Content[i] = param.Override[anthropic.ContentBlockParamUnion](json.RawMessage(c.rawJSON))
			} else {
				mp.Content[i] = anthropic.ContentBlockParamUnion{
					OfText: &anthropic.TextBlockParam{Text: c.Text},
				}
			}
		}
	}
	return mp
}

func (p *AnthropicProvider) NewToolResult(toolUseID, content string, isError bool) anthropic.ContentBlockParamUnion {
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

// ── helpers ──────────────────────────────────────────────────────────────

// streamAccumulator folds streamed events into an anthropic.Message via the
// SDK's Accumulate, keeping two things Accumulate discards:
//
//   - each content block's raw JSON as sent in content_block_start
//     (originalRaw; see CreateMessage), and
//   - each tool_use block's input exactly as streamed.
//
// When the model truncates a tool_use input mid-stream (classically from
// inlining a large payload into tool args and hitting the output limit), the
// streamed input is incomplete JSON. Since anthropic-sdk-go v1.70 the stop
// events quietly replace such an input with `{}` so the block re-marshals
// (older versions failed Accumulate with a marshal error instead). A `{}` is
// valid JSON, so the agent loop's sanitizeTruncatedToolInputs could no longer
// tell the call was cut off: it executed the tool with empty arguments instead
// of telling the model its input was truncated and to resend the payload as an
// attachment. message() puts the truncated input back.
type streamAccumulator struct {
	msg         anthropic.Message
	originalRaw []string
	toolInput   map[int64][]byte // streamed input_json_delta bytes, by block index
}

func (s *streamAccumulator) add(event anthropic.MessageStreamEventUnion) error {
	// Snapshot the block's raw JSON as it arrives. Accumulate will later
	// overwrite JSON.raw in the stop event.
	if start, ok := event.AsAny().(anthropic.ContentBlockStartEvent); ok {
		s.originalRaw = append(s.originalRaw, start.ContentBlock.RawJSON())
	}
	if event.Type == "content_block_delta" && event.Delta.Type == "input_json_delta" && event.Delta.PartialJSON != "" {
		if s.toolInput == nil {
			s.toolInput = map[int64][]byte{}
		}
		s.toolInput[event.Index] = append(s.toolInput[event.Index], event.Delta.PartialJSON...)
	}
	return s.msg.Accumulate(event)
}

// message returns the accumulated message with every truncated tool_use input
// restored to the invalid bytes the model actually streamed. Only client
// tool_use blocks are restored: they are what sanitizeTruncatedToolInputs
// inspects (and replaces with `{}` before the block reaches history), whereas a
// server_tool_use input goes into history as-is and must stay valid JSON.
func (s *streamAccumulator) message() *anthropic.Message {
	for i := range s.msg.Content {
		block := &s.msg.Content[i]
		streamed := s.toolInput[int64(i)]
		if block.Type == "tool_use" && len(streamed) > 0 && !json.Valid(streamed) {
			block.Input = json.RawMessage(streamed)
		}
	}
	return &s.msg
}

// needsOriginalRaw reports whether a block type must keep its pre-accumulate
// raw JSON (captured at ContentBlockStart) instead of the accumulated raw,
// which the SDK streaming accumulator corrupts for server-side result blocks.
// Only those block types need it; everything else (text, thinking, tool_use,
// server_tool_use) is rebuilt from typed fields in ToHistoryParam.
func needsOriginalRaw(blockType string) bool {
	switch blockType {
	case "code_execution_tool_result", "bash_code_execution_tool_result",
		"text_editor_code_execution_tool_result",
		"web_search_tool_result", "web_fetch_tool_result":
		return true
	default:
		return false
	}
}

// anthropicBuiltinTool returns the SDK tool definition for a server-side tool.
func anthropicBuiltinTool(name, model string) *anthropic.ToolUnionParam {
	switch name {
	case "anthropic-web-search":
		if strings.Contains(model, "haiku") {
			return &anthropic.ToolUnionParam{
				OfWebSearchTool20250305: &anthropic.WebSearchTool20250305Param{},
			}
		} else if strings.Contains(model, "sonnet") || strings.Contains(model, "opus") {
			return &anthropic.ToolUnionParam{
				OfWebSearchTool20260209: &anthropic.WebSearchTool20260209Param{},
			}
		}
		log.Printf("anthropic-web-search: unsupported model %q", model)
		return nil
	}
	return nil
}

// anthropicResponseToMessage converts an SDK response to the neutral type.
func anthropicResponseToMessage(resp *anthropic.Message) *MessageResponse {
	mr := &MessageResponse{
		Role:       string(resp.Role),
		StopReason: string(resp.StopReason),
	}
	for _, block := range resp.Content {
		cb := ContentBlock{
			Type:      string(block.Type),
			Text:      block.Text,
			ID:        block.ID,
			Name:      block.Name,
			Thinking:  block.Thinking,
			Signature: block.Signature,
			Data:      block.Data,
			rawJSON:   block.RawJSON(),
		}
		if block.Input != nil {
			if raw, mErr := json.Marshal(block.Input); mErr == nil {
				cb.Input = raw
			} else {
				// Truncated tool input (model cut off mid-stream). Preserve the
				// raw bytes verbatim instead of dropping them to empty, so the
				// agent loop's sanitizeTruncatedToolInputs sees invalid JSON and
				// emits resend guidance rather than executing an empty call.
				cb.Input = json.RawMessage(block.Input)
			}
		}
		mr.Content = append(mr.Content, cb)
	}
	return mr
}
