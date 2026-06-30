package agents

import (
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
