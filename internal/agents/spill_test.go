package agents

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestSanitizeForFilename(t *testing.T) {
	cases := map[string]string{
		"salesforce__salesforce_query_records": "salesforce-salesforce-query-records",
		"Kiku":                                 "kiku",
		"box_cli":                              "box-cli",
		"  ":                                   "result",
		"////":                                 "result",
		"a..b":                                 "a-b",
		"Beta-Agent_01":                        "beta-agent-01",
	}
	for in, want := range cases {
		if got := sanitizeForFilename(in); got != want {
			t.Errorf("sanitizeForFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSpillToolResultRoundTrip(t *testing.T) {
	full := strings.Repeat("Record 1: BOX - SOME OPPORTUNITY (JAPAN)\n", 2000)
	path, ok := spillToolResult("beta", "salesforce__salesforce_query_records", full)
	if !ok {
		t.Fatal("spillToolResult returned ok=false")
	}
	t.Cleanup(func() { os.Remove(path) })

	if !strings.Contains(path, "kikubot-beta-salesforce-salesforce-query-records-") {
		t.Errorf("spill path %q does not carry agent/tool stem", path)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading spill file: %s", err)
	}
	if string(got) != full {
		t.Errorf("spilled file content differs from input (len got=%d want=%d)", len(got), len(full))
	}
}

// The path note is the actionable part of a spilled result; it must survive
// even when the body is clamped right up to MaxToolResultChars. This guards the
// ordering in HandleMessage (truncate body first, append note last).
func TestSpillNoteSurvivesTruncation(t *testing.T) {
	const maxChars = 80000
	original := 79990 // body that, with the note appended pre-truncation, would overflow
	body := truncateToolResult(strings.Repeat("x", original), maxChars)
	final := body + toolResultSpillNote(original, "/tmp/kikubot-beta-tool-123.txt")

	if !strings.Contains(final, "/tmp/kikubot-beta-tool-123.txt") {
		t.Error("spill path was clipped out of the final result")
	}
	if !strings.Contains(final, "attachments[].path") {
		t.Error("spill note lost its path-attach guidance")
	}
}

// The anti-flail guard keys tool calls by name + canonical input. Calls that
// differ only in JSON formatting or object-key order must collide (so a repeat
// is recognized), while different tool names, arguments, or values must not.
func TestToolCallKey(t *testing.T) {
	same := [][2]string{
		// key order differs
		{`{"folder_id":"123","limit":100}`, `{"limit":100,"folder_id":"123"}`},
		// whitespace differs
		{`{"folder_id":"123"}`, `{ "folder_id" : "123" }`},
		// empty/whitespace-only inputs both canonicalize to ""
		{``, `   `},
	}
	for _, c := range same {
		if a, b := toolCallKey("box__folder_list", json.RawMessage(c[0])), toolCallKey("box__folder_list", json.RawMessage(c[1])); a != b {
			t.Errorf("expected equal keys for %q and %q, got %q vs %q", c[0], c[1], a, b)
		}
	}

	differ := [][2]struct{ name, input string }{
		// different argument value
		{{"box__folder_list", `{"folder_id":"123"}`}, {"box__folder_list", `{"folder_id":"456"}`}},
		// different tool, same input
		{{"box__folder_list", `{"folder_id":"123"}`}, {"box__search", `{"folder_id":"123"}`}},
		// extra argument present
		{{"box__folder_list", `{"folder_id":"123"}`}, {"box__folder_list", `{"folder_id":"123","limit":5}`}},
	}
	for _, c := range differ {
		if a, b := toolCallKey(c[0].name, json.RawMessage(c[0].input)), toolCallKey(c[1].name, json.RawMessage(c[1].input)); a == b {
			t.Errorf("expected distinct keys for %+v and %+v, both = %q", c[0], c[1], a)
		}
	}
}

func TestRepeatedFailureNote(t *testing.T) {
	note := repeatedFailureNote("box__folder_list", "cli exec box: exit status 2\nNonexistent flag: --limit")
	if !strings.Contains(note, "box__folder_list") {
		t.Error("note should name the blocked tool")
	}
	if !strings.Contains(note, "--limit") {
		t.Error("note should surface the prior error so the model can adjust")
	}

	long := strings.Repeat("x", 2000)
	got := repeatedFailureNote("t", long)
	if !strings.Contains(got, "…") {
		t.Error("note should mark the prior error as trimmed with an ellipsis")
	}
	// Trimmed error (≤600) + fixed template, well under the 2000-char input.
	if len(got) > 1100 {
		t.Errorf("note should trim a very long prior error, got %d chars", len(got))
	}
}
