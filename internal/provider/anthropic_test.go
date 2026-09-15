package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// accumulate feeds raw stream events through a streamAccumulator the way
// CreateMessage does, failing the test on any accumulate error.
func accumulate(t *testing.T, events []string) *streamAccumulator {
	t.Helper()
	acc := &streamAccumulator{}
	for _, raw := range events {
		var ev anthropic.MessageStreamEventUnion
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			t.Fatalf("unmarshal event: %v", err)
		}
		if err := acc.add(ev); err != nil {
			t.Fatalf("accumulate %s: %v", ev.Type, err)
		}
	}
	return acc
}

const messageStart = `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}`

// When the model truncates a tool_use input mid-stream (e.g. inlining a large
// payload into tool args and hitting the output limit), the incomplete input
// must reach the agent loop as the invalid JSON it is, so
// sanitizeTruncatedToolInputs can flag it and tell the model to resend. The SDK
// (v1.70+) blanks such an input to a valid `{}` at the stop events, which would
// hide the truncation and execute the tool with no arguments.
func TestStreamAccumulatorPreservesTruncatedToolInput(t *testing.T) {
	const partial = `{"To":"user@x","message":"hello`
	acc := accumulate(t, []string{
		messageStart,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"report_tool","input":{}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"To\":\"user@x\","}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"message\":\"hello"}}`,
		// Truncated by the output limit: no content_block_stop for the open
		// block, then a max_tokens message_delta + message_stop.
		`{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":50}}`,
		`{"type":"message_stop"}`,
	})

	msg := acc.message()
	if len(msg.Content) != 1 {
		t.Fatalf("expected 1 accumulated content block, got %d", len(msg.Content))
	}
	if msg.Content[0].Name != "report_tool" {
		t.Errorf("tool name lost: got %q", msg.Content[0].Name)
	}
	if msg.StopReason != "max_tokens" {
		t.Errorf("stop reason lost: got %q", msg.StopReason)
	}
	if got := string(msg.Content[0].Input); got != partial {
		t.Errorf("tool input = %s, want the truncated stream %s", got, partial)
	}
	if len(acc.originalRaw) != 1 || !strings.Contains(acc.originalRaw[0], `"report_tool"`) {
		t.Errorf("originalRaw = %q, want the content_block_start JSON", acc.originalRaw)
	}

	// The invalid input must survive into the neutral response — not be
	// dropped to empty or replaced by valid JSON.
	resp := anthropicResponseToMessage(msg)
	if len(resp.Content) != 1 {
		t.Fatalf("expected 1 neutral content block, got %d", len(resp.Content))
	}
	if got := string(resp.Content[0].Input); got != partial {
		t.Errorf("neutral input = %s, want the truncated stream %s", got, partial)
	}
	if json.Valid(resp.Content[0].Input) {
		t.Errorf("expected the preserved input to be invalid JSON (truncated), got valid: %s", resp.Content[0].Input)
	}
}

// A tool input that streams completely is left exactly as the SDK assembled it.
func TestStreamAccumulatorCompleteToolInputUnchanged(t *testing.T) {
	acc := accumulate(t, []string{
		messageStart,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Sending."}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"report_tool","input":{}}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"To\":\"user@x\","}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"message\":\"hello\"}"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":20}}`,
		`{"type":"message_stop"}`,
	})

	msg := acc.message()
	if len(msg.Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(msg.Content))
	}
	if msg.Content[0].Text != "Sending." {
		t.Errorf("text = %q, want %q", msg.Content[0].Text, "Sending.")
	}
	const want = `{"To":"user@x","message":"hello"}`
	if got := string(msg.Content[1].Input); got != want {
		t.Errorf("tool input = %s, want %s", got, want)
	}
}

// A tool call with no arguments streams no input deltas; its `{}` input stays.
func TestStreamAccumulatorNoArgToolInputUnchanged(t *testing.T) {
	acc := accumulate(t, []string{
		messageStart,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"list_snoozed_tool","input":{}}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`,
		`{"type":"message_stop"}`,
	})

	if got := string(acc.message().Content[0].Input); got != `{}` {
		t.Errorf("tool input = %s, want {}", got)
	}
}

// A truncated server_tool_use input is not restored: it goes into history
// verbatim (ToHistoryParam), where invalid JSON would break the next request.
func TestStreamAccumulatorLeavesServerToolUseInputValid(t *testing.T) {
	acc := accumulate(t, []string{
		messageStart,
		`{"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"devo"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":50}}`,
		`{"type":"message_stop"}`,
	})

	input := acc.message().Content[0].Input
	if !json.Valid(input) {
		t.Errorf("server_tool_use input = %s, want valid JSON", input)
	}
}
