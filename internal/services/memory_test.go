package services

import (
	"encoding/json"
	"testing"
)

// blockTypes returns the "type" of each content block in a sanitized message,
// in order — enough to assert which blocks survived.
func blockTypes(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var msg struct {
		Content []struct {
			Type string `json:"type"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("unmarshal sanitized message: %v", err)
	}
	out := make([]string, len(msg.Content))
	for i, b := range msg.Content {
		out[i] = b.Type
	}
	return out
}

func TestStripUnusableThinking(t *testing.T) {
	t.Run("drops empty-body thinking but keeps tool_use", func(t *testing.T) {
		// The wedge from Kiku's thread: a thinking block with a real signature
		// but empty body, alongside a tool_use. The thinking block must go; the
		// tool_use must stay.
		in := json.RawMessage(`{"role":"assistant","content":[
			{"type":"thinking","thinking":"","signature":"EpYCabc123"},
			{"type":"tool_use","id":"toolu_1","name":"message_tool","input":{"To":"x"}}
		]}`)
		got := blockTypes(t, stripUnusableThinking(in))
		if len(got) != 1 || got[0] != "tool_use" {
			t.Errorf("expected [tool_use], got %v", got)
		}
	})

	t.Run("drops empty-signature thinking", func(t *testing.T) {
		in := json.RawMessage(`{"role":"assistant","content":[
			{"type":"thinking","thinking":"reasoning here","signature":""},
			{"type":"text","text":"hello"}
		]}`)
		got := blockTypes(t, stripUnusableThinking(in))
		if len(got) != 1 || got[0] != "text" {
			t.Errorf("expected [text], got %v", got)
		}
	})

	t.Run("keeps a valid thinking block", func(t *testing.T) {
		in := json.RawMessage(`{"role":"assistant","content":[
			{"type":"thinking","thinking":"real reasoning","signature":"EpYCsig"},
			{"type":"tool_use","id":"toolu_1","name":"t","input":{}}
		]}`)
		got := blockTypes(t, stripUnusableThinking(in))
		if len(got) != 2 || got[0] != "thinking" || got[1] != "tool_use" {
			t.Errorf("expected [thinking tool_use], got %v", got)
		}
	})

	t.Run("drops redacted_thinking with empty data", func(t *testing.T) {
		in := json.RawMessage(`{"role":"assistant","content":[
			{"type":"redacted_thinking","data":""},
			{"type":"text","text":"hi"}
		]}`)
		got := blockTypes(t, stripUnusableThinking(in))
		if len(got) != 1 || got[0] != "text" {
			t.Errorf("expected [text], got %v", got)
		}
	})

	t.Run("never empties a thinking-only message", func(t *testing.T) {
		// Dropping would leave zero content blocks (invalid), so the message is
		// left intact — better a tolerated bad block than an empty message.
		in := json.RawMessage(`{"role":"assistant","content":[
			{"type":"thinking","thinking":"","signature":"EpYCabc"}
		]}`)
		got := blockTypes(t, stripUnusableThinking(in))
		if len(got) != 1 || got[0] != "thinking" {
			t.Errorf("expected the message left intact [thinking], got %v", got)
		}
	})

	t.Run("leaves messages without thinking untouched", func(t *testing.T) {
		in := json.RawMessage(`{"role":"user","content":[{"type":"text","text":"hi"}]}`)
		if got := string(stripUnusableThinking(in)); got != string(in) {
			t.Errorf("expected byte-identical passthrough, got %s", got)
		}
	})
}
