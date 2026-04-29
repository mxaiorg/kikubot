package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// parseMCPInput decodes tool input into a map, tolerating the empty-input
// case that OpenRouter/streaming providers sometimes emit when a tool call's
// arguments get truncated mid-stream or the model signals "no args." An
// empty payload becomes an empty map so the MCP server can run its own
// schema validation and return an actionable error back to the LLM, instead
// of us failing with a cryptic "unexpected end of JSON input." A malformed
// (non-empty) payload produces a descriptive error that explains the
// likely cause so the model can retry correctly.
func parseMCPInput(input json.RawMessage, toolName string) (map[string]any, error) {
	trimmed := bytes.TrimSpace(input)
	if len(trimmed) == 0 {
		log.Printf("[mcp] %s called with empty input — treating as {} (likely streaming truncation or no-arg call)", toolName)
		return map[string]any{}, nil
	}
	var args map[string]any
	if err := json.Unmarshal(trimmed, &args); err != nil {
		return nil, fmt.Errorf(
			"tool %q received malformed JSON arguments (%s). The model likely emitted an incomplete tool call — retry with all required arguments as a complete JSON object",
			toolName, err,
		)
	}
	return args, nil
}

// ── Local (stdio) MCP ──────────────────────

// LocalMCPConfig describes a local MCP server launched as a subprocess.
type LocalMCPConfig struct {
	// ServerName is the prefix added to tool names (e.g. "salesforce").
	ServerName string
	// Command is the executable to run (e.g. "npx").
	Command string
	// Args are the command-line arguments (e.g. ["-y", "@tsmztech/mcp-server-salesforce"]).
	Args []string
	// Env is optional extra environment variables for the subprocess.
	Env map[string]string
}

// LocalMCPBridge spawns a local MCP server via stdio, discovers its scripts,
// and returns them as ToolDefinitions. Unlike the HTTP bridge, the stdio
// transport keeps a single long-lived subprocess — each tool call reuses it.
func LocalMCPBridge(cfg LocalMCPConfig) ([]ToolDefinition, error) {
	// Build env slice: inherit current env + extras
	var envList []string
	for k, v := range cfg.Env {
		envList = append(envList, k+"="+v)
	}

	c, err := mcpclient.NewStdioMCPClient(cfg.Command, envList, cfg.Args...)
	if err != nil {
		return nil, fmt.Errorf("stdio mcp start %s: %w", cfg.ServerName, err)
	}

	if _, err = c.Initialize(context.Background(), mcp.InitializeRequest{}); err != nil {
		c.Close()
		return nil, fmt.Errorf("stdio mcp init %s: %w", cfg.ServerName, err)
	}

	toolList, err := c.ListTools(context.Background(), mcp.ListToolsRequest{})
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("stdio mcp list scripts %s: %w", cfg.ServerName, err)
	}

	log.Printf("[%s] discovered %d scripts via stdio", cfg.ServerName, len(toolList.Tools))

	var defs []ToolDefinition
	for _, t := range toolList.Tools {
		tool := t
		schema, _ := json.Marshal(tool.InputSchema)

		defs = append(defs, ToolDefinition{
			Name:        cfg.ServerName + "__" + tool.Name,
			Description: tool.Description,
			InputSchema: schema,
			Execute: func(_ context.Context, input json.RawMessage) (string, error) {
				args, err := parseMCPInput(input, tool.Name)
				if err != nil {
					return "", err
				}

				result, err := c.CallTool(context.Background(), mcp.CallToolRequest{
					Params: mcp.CallToolParams{
						Name:      tool.Name,
						Arguments: args,
					},
				})
				if err != nil {
					return "", fmt.Errorf("call tool %s: %w", tool.Name, err)
				}

				var parts []string
				for _, content := range result.Content {
					if textContent, ok := content.(mcp.TextContent); ok {
						parts = append(parts, textContent.Text)
					}
				}
				return strings.Join(parts, "\n"), nil
			},
		})
	}

	return defs, nil
}

// ── MCP Tool (connect to an external MCP server) ──────────────────────
//
// This uses mark3labs/mcp-go to connect to any MCP server and proxy
// its scripts into the agent cluster.
//
// Usage:
//   cluster.RegisterTool(MCPTool("weather-server", "http://localhost:8080/mcp", "Bearer <token>"))
//
// The MCP client discovers scripts from the server, then exposes each one
// as a ToolDefinition that the cluster can route to individual agents.

// mcpDial creates a new MCP Streamable HTTP client, initializes the session,
// and returns the ready-to-use client. Caller must close it when done.
func mcpDial(serverURL, auth string) (*mcpclient.Client, error) {
	var opts []transport.StreamableHTTPCOption
	if auth != "" {
		opts = append(opts, transport.WithHTTPHeaders(map[string]string{
			"Authorization": auth,
		}))
	}
	c, err := mcpclient.NewStreamableHttpClient(serverURL, opts...)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if _, err = c.Initialize(context.Background(), mcp.InitializeRequest{}); err != nil {
		c.Close()
		return nil, fmt.Errorf("init: %w", err)
	}
	return c, nil
}

// MCPBridge connects to an MCP server, discovers its scripts, and returns them
// as ToolDefinitions. Each tool call opens a fresh session to avoid stale
// connection / canceled-context errors from the Streamable HTTP transport.
func MCPBridge(serverName, serverURL, auth string) ([]ToolDefinition, error) {
	// One-shot connection just to discover available scripts
	c, err := mcpDial(serverURL, auth)
	if err != nil {
		log.Printf("error connecting to MCP server %s: %s", serverURL, err)
		return nil, fmt.Errorf("mcp connect %s: %w", serverName, err)
	}

	toolList, err2 := c.ListTools(context.Background(), mcp.ListToolsRequest{})
	c.Close() // discovery done, close immediately
	if err2 != nil {
		return nil, fmt.Errorf("mcp list scripts %s: %w", serverName, err2)
	}

	var defs []ToolDefinition
	for _, t := range toolList.Tools {
		tool := t // capture loop var
		schema, _ := json.Marshal(tool.InputSchema)

		defs = append(defs, ToolDefinition{
			Name:        serverName + "__" + tool.Name,
			Description: tool.Description,
			InputSchema: schema,
			Execute: func(_ context.Context, input json.RawMessage) (string, error) {
				args, argsErr := parseMCPInput(input, tool.Name)
				if argsErr != nil {
					return "", argsErr
				}

				// Fresh connection per call — the Streamable HTTP transport's
				// internal session/SSE context can go stale between calls.
				cli, dialErr := mcpDial(serverURL, auth)
				if dialErr != nil {
					log.Printf("error dialing MCP for tool %s: %s", tool.Name, dialErr)
					return "", dialErr
				}
				defer cli.Close()

				result, err4 := cli.CallTool(context.Background(), mcp.CallToolRequest{
					Params: mcp.CallToolParams{
						Name:      tool.Name,
						Arguments: args,
					},
				})
				if err4 != nil {
					log.Printf("error calling tool %s: %s", tool.Name, err4)
					return "", err4
				}

				var parts []string
				for _, content := range result.Content {
					if textContent, ok := content.(mcp.TextContent); ok {
						parts = append(parts, textContent.Text)
					}
				}
				return strings.Join(parts, "\n"), nil
			},
		})
	}

	return defs, nil
}
