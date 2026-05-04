package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ── Bash Tool ──────────────────────────────────────────────────────────

func BashTool() ToolDefinition {
	return ToolDefinition{
		Name:        "bash_exec",
		Description: "Execute a bash command and return stdout/stderr. Use for running code, installing packages, reading files, etc.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"command": {
					"type": "string",
					"description": "The bash command to execute"
				},
				"timeout_seconds": {
					"type": "integer",
					"description": "Max execution time in seconds (default 30)"
				}
			},
			"required": ["command"]
		}`),
		Execute: executeBash,
		StaticSystem: "- IMPORTANT: For executing shell commands, always use the `bash_exec` tool.\n" +
			"  - Never use `bash_code_execution` — that runs in a sandbox without internet access.\n" +
			"  - Your `bash_exec` tool runs locally with full network access.",
	}
}

// bashAvailability is computed once on first invocation. We don't probe at
// startup because not every agent has bash_exec wired up — paying a fork
// per process for an unused tool is wasteful. The lookup itself is cheap,
// but bash absence is permanent within a container's lifetime, so caching
// the result keeps the error path identical and obvious.
var (
	bashOnce       sync.Once
	bashUnavailErr string
)

func bashAvailable() (bool, string) {
	bashOnce.Do(func() {
		if _, err := exec.LookPath("bash"); err != nil {
			bashUnavailErr = "bash_exec: bash is not installed in this container. " +
				"This is an environment / image problem, not a usage error: ask an " +
				"operator to add `bash` (and any utilities your workflow needs, " +
				"e.g. curl, jq) to the runtime image. Do NOT retry this tool, do " +
				"NOT attempt to install bash yourself, and do NOT silently fall " +
				"back to alternative shells. Report the missing dependency to the " +
				"requester via report_tool / message_tool and stop."
		}
	})
	return bashUnavailErr == "", bashUnavailErr
}

func executeBash(ctx context.Context, input json.RawMessage) (string, error) {
	if ok, msg := bashAvailable(); !ok {
		return msg, nil // non-fatal to the agent — surfaced as guidance, not an Execute error
	}

	var params struct {
		Command        string `json:"command"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}

	timeout := 30
	if params.TimeoutSeconds > 0 {
		timeout = params.TimeoutSeconds
	}

	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "-c", params.Command)
	out, err := cmd.CombinedOutput()

	result := strings.TrimSpace(string(out))
	if err != nil {
		return fmt.Sprintf("exit error: %v\noutput:\n%s", err, result), nil // non-fatal to the agent
	}
	return result, nil
}
