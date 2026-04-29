package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ── CLI Bridge ──────────────────────────────────────────────────────────
// Generic helper for wrapping any CLI tool into ToolDefinitions.
// Unlike the MCP bridge, scripts are hand-curated — each CLI integration
// defines its own schemas and maps inputs to subcommands.

// CLIToolConfig describes a CLI tool that will be invoked as a subprocess.
type CLIToolConfig struct {
	// Prefix is added to tool names (e.g. "box" → "box__search").
	Prefix string
	// Command is the executable to run (e.g. "npx").
	Command string
	// BaseArgs are prepended to every invocation (e.g. ["-y", "@box/cli"]).
	BaseArgs []string
	// Env is optional extra environment variables for the subprocess.
	Env map[string]string
	// Timeout in seconds (default 30).
	Timeout int
	// JSONFlag is appended by CLINavigator's run tool for structured output.
	// Default "--json". Set to "-" to disable.
	JSONFlag string
	// Roots scopes the CLI navigator to one or more subtrees.
	// Each entry is a command path, e.g. []string{"files"} or []string{"folders", "items"}.
	// Help is fetched for every root and concatenated. Run calls must start
	// with one of the roots (validated at execution time).
	// For a single root you can pass [][]string{{"files"}}.
	// For multiple roots you can pass [][]string{{"files"}, {"folders", "items"}}.
	Roots [][]string
}

// CLIExec runs a CLI subcommand and returns the output.
// The final command is: Command BaseArgs... subcommand...
// The caller is responsible for including flags like --json in subcommand.
func CLIExec(cfg CLIToolConfig, subcommand []string) (string, error) {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	args := make([]string, 0, len(cfg.BaseArgs)+len(subcommand))
	args = append(args, cfg.BaseArgs...)
	args = append(args, subcommand...)

	cmd := exec.CommandContext(ctx, cfg.Command, args...)

	// Inherit current env + extras
	cmd.Env = os.Environ()
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	out, err := cmd.CombinedOutput()
	result := strings.TrimSpace(string(out))
	if err != nil {
		return "", fmt.Errorf("cli exec %s: %w\noutput: %s", cfg.Prefix, err, result)
	}

	return result, nil
}

// ── CLI Navigator ───────────────────────────────────────────────────────
// Dynamic CLI integration: instead of hand-curating scripts, the LLM gets
// two scripts — one to explore --help and one to execute commands. Works
// with any CLI that supports nested --help output.
//
// Usage:
//
//	scripts := CLINavigator(CLIToolConfig{
//	    Prefix:   "box",
//	    Command:  "npx",
//	    BaseArgs: []string{"-y", "@box/cli"},
//	})

// CLINavigator returns two ToolDefinitions for any CLI:
//   - {prefix}__help  — run --help on any subcommand to discover available commands and flags
//   - {prefix}__run   — execute a fully-formed command with arguments
//
// At startup it runs --help once and embeds the top-level output in the
// help tool's description so the LLM knows the command tree immediately.
func CLINavigator(cfg CLIToolConfig) []ToolDefinition {
	// Capture help at init time for each root (or top-level if no roots)
	roots := cfg.Roots
	if len(roots) == 0 {
		roots = [][]string{{}} // single empty root = top-level
	}

	var helpSections []string
	for _, root := range roots {
		helpArgs := append(append([]string{}, root...), "--help")
		h, err := CLIExec(cfg, helpArgs)
		if err != nil {
			log.Printf("[%s] cli navigator: could not get help for %v: %v", cfg.Prefix, root, err)
			continue
		}
		if len(root) > 0 {
			helpSections = append(helpSections, fmt.Sprintf("── %s ──\n%s", strings.Join(root, " "), h))
		} else {
			helpSections = append(helpSections, h)
		}
	}
	if len(helpSections) == 0 {
		log.Printf("[%s] cli navigator: no help output for any root", cfg.Prefix)
		return nil
	}
	topHelp := strings.Join(helpSections, "\n\n")

	scope := rootsScope(cfg.Prefix, cfg.Roots)
	log.Printf("[%s] CLI navigator initialized (scope: %s)", cfg.Prefix, scope)

	// Default to --json unless the caller explicitly set JSONFlag
	jsonFlag := cfg.JSONFlag
	if jsonFlag == "" {
		jsonFlag = "--json"
	} else if jsonFlag == "-" {
		jsonFlag = "" // convention: set to "-" to disable
	}

	return []ToolDefinition{
		cliHelpTool(cfg, topHelp),
		cliRunTool(cfg, jsonFlag),
	}
}

// rootsScope returns a human-readable scope label.
func rootsScope(prefix string, roots [][]string) string {
	if len(roots) == 0 {
		return prefix
	}
	parts := make([]string, len(roots))
	for i, r := range roots {
		parts[i] = prefix + " " + strings.Join(r, " ")
	}
	return strings.Join(parts, ", ")
}

func cliHelpTool(cfg CLIToolConfig, topLevelHelp string) ToolDefinition {
	scope := rootsScope(cfg.Prefix, cfg.Roots)

	desc := fmt.Sprintf(
		"Get help/documentation for the %s CLI. Call with no subcommand for top-level help, "+
			"or with a subcommand to drill into its usage.\n\n"+
			"Available commands:\n%s", scope, topLevelHelp,
	)

	return ToolDefinition{
		Name:        cfg.Prefix + "__help",
		Description: desc,
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"subcommand": {
					"type": "string",
					"description": "The subcommand to get help for. Omit for top-level help."
				}
			}
		}`),
		Execute: func(_ context.Context, input json.RawMessage) (string, error) {
			var p struct {
				Subcommand string `json:"subcommand"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", fmt.Errorf("parsing input: %w", err)
			}

			if p.Subcommand == "" {
				return topLevelHelp, nil
			}

			// Subcommand is provided — prepend the matching root if any
			args := resolveRoot(cfg.Roots, p.Subcommand)
			args = append(args, "--help")

			return CLIExec(cfg, args)
		},
	}
}

func cliRunTool(cfg CLIToolConfig, jsonFlag string) ToolDefinition {
	scope := rootsScope(cfg.Prefix, cfg.Roots)

	desc := fmt.Sprintf(
		"Execute a %s command. Use %s__help first to discover available commands and flags. "+
			"Provide the argument string (e.g. \"get 12345 --fields name,size\").",
		scope, cfg.Prefix,
	)
	if jsonFlag != "" {
		desc += fmt.Sprintf(" The %s flag is appended automatically.", jsonFlag)
	}

	return ToolDefinition{
		Name:        cfg.Prefix + "__run",
		Description: desc,
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"args": {
					"type": "string",
					"description": "The command arguments as a single string"
				}
			},
			"required": ["args"]
		}`),
		Execute: func(_ context.Context, input json.RawMessage) (string, error) {
			var p struct {
				Args string `json:"args"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", fmt.Errorf("parsing input: %w", err)
			}

			// Prepend matching root (if roots are configured)
			args := resolveRoot(cfg.Roots, p.Args)
			if jsonFlag != "" {
				args = append(args, jsonFlag)
			}

			return CLIExec(cfg, args)
		},
	}
}

// resolveRoot finds the matching root for a user-supplied command string and
// prepends it. If the command already starts with a root's tokens, the root is
// not duplicated. If no roots are configured, the command is returned as-is.
func resolveRoot(roots [][]string, command string) []string {
	fields := strings.Fields(command)

	if len(roots) == 0 {
		return fields
	}

	// Check if command already starts with one of the roots
	for _, root := range roots {
		if len(root) == 0 {
			continue
		}
		if hasPrefix(fields, root) {
			return fields // root already present
		}
	}

	// Check if the first token matches the first token of any root — assume
	// the user meant that root (e.g. "files list" matches root ["files"])
	for _, root := range roots {
		if len(root) > 0 && len(fields) > 0 && fields[0] == root[0] {
			// Partial match on first token — prepend the full root minus
			// the overlapping prefix
			return append(root, fields[len(root):]...)
		}
	}

	// Single root: always prepend (backward-compatible behavior)
	if len(roots) == 1 && len(roots[0]) > 0 {
		return append(append([]string{}, roots[0]...), fields...)
	}

	// Multiple roots, no match — pass through as-is and let the CLI error
	return fields
}

// hasPrefix checks if tokens starts with all elements of prefix.
func hasPrefix(tokens, prefix []string) bool {
	if len(tokens) < len(prefix) {
		return false
	}
	for i, p := range prefix {
		if tokens[i] != p {
			return false
		}
	}
	return true
}
