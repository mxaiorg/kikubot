package provider

import (
	"encoding/json"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// When the model truncates a tool_use input mid-stream (e.g. inlining a large
// payload into tool args and hitting the output limit), the SDK's Accumulate
// fails at message_stop re-marshalling the incomplete json.RawMessage. This
// test pins two facts the CreateMessage recovery relies on:
//  1. the error is one isAccumulateMarshalError recognizes, and
//  2. the accumulated content blocks survive on the Message despite the error,
//     with the invalid tool input preserved (so the agent loop can detect it).
func TestAccumulateTruncatedToolInputRecovery(t *testing.T) {
	events := []string{
		`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"report_tool","input":{}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"To\":\"user@x\",\"message\":\"hello"}}`,
		// Truncated by the output limit: no content_block_stop for the open
		// block, then a max_tokens message_delta + message_stop.
		`{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":50}}`,
		`{"type":"message_stop"}`,
	}

	acc := &anthropic.Message{}
	var lastErr error
	for _, raw := range events {
		var ev anthropic.MessageStreamEventUnion
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			t.Fatalf("unmarshal event: %v", err)
		}
		if err := acc.Accumulate(ev); err != nil {
			lastErr = err
		}
	}

	if lastErr == nil {
		t.Fatal("expected Accumulate to error on the truncated tool input")
	}
	if !isAccumulateMarshalError(lastErr) {
		t.Fatalf("isAccumulateMarshalError did not match the real SDK error: %v", lastErr)
	}

	// Recovery premise: the content block is intact on acc despite the error.
	if len(acc.Content) != 1 {
		t.Fatalf("expected 1 accumulated content block, got %d", len(acc.Content))
	}
	if acc.Content[0].Name != "report_tool" {
		t.Errorf("tool name lost: got %q", acc.Content[0].Name)
	}
	if acc.StopReason != "max_tokens" {
		t.Errorf("stop reason lost: got %q", acc.StopReason)
	}

	// The invalid (truncated) input must survive into the neutral response so
	// the agent loop's truncation guard can flag it — not be dropped to empty.
	resp := anthropicResponseToMessage(acc)
	if len(resp.Content) != 1 {
		t.Fatalf("expected 1 neutral content block, got %d", len(resp.Content))
	}
	if json.Valid(resp.Content[0].Input) {
		t.Errorf("expected the preserved input to be invalid JSON (truncated), got valid: %s", resp.Content[0].Input)
	}
	if len(resp.Content[0].Input) == 0 {
		t.Error("truncated input was dropped to empty; agent loop can no longer detect truncation")
	}
}

func TestIsAccumulateMarshalError(t *testing.T) {
	cases := map[string]bool{
		"error converting accumulated message to JSON: json: error calling MarshalJSON for type json.RawMessage: unexpected end of JSON input": true,
		"error converting content block to JSON: oops":      true,
		"stream accumulate error: received event of type x": false,
		"some unrelated error":                              false,
	}
	for msg, want := range cases {
		if got := isAccumulateMarshalError(errString(msg)); got != want {
			t.Errorf("isAccumulateMarshalError(%q) = %v, want %v", msg, got, want)
		}
	}
	if isAccumulateMarshalError(nil) {
		t.Error("nil error should not match")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
