package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// ── Box CLI Tools ───────────────────────────────────────────────────────
// Wraps the Box CLI (@box/cli) via the generic CLI bridge.
// Auth is configured by running "box configure:environments:add /path/to/services.json"
// before first use (e.g. in Docker entrypoint).

// This is a more directed version of the Box CLI.
// For a more general CLI integration see cli_helper.go and CLINavigator.

// Limit to most important fields
var boxFields = "--fields=type,id,name,description,created_by,shared_link"

func boxConfig() CLIToolConfig {
	return CLIToolConfig{
		Prefix:   "box",
		Command:  "npx",
		BaseArgs: []string{"-y", "@box/cli"},
		Timeout:  30,
	}
}

// BoxCLI returns Box tool definitions.
func BoxCLI() []ToolDefinition {
	cfg := boxConfig()

	// Verify the CLI is reachable at startup
	if _, err := CLIExec(cfg, []string{"--version"}); err != nil {
		log.Println("box cli not available:", err)
		return nil
	}
	log.Println("[box] CLI bridge initialized")

	return []ToolDefinition{
		boxSearchTool(cfg),
		boxFileGetTool(cfg),
		boxFolderListTool(cfg),
		boxFileDownloadTool(cfg),
	}
}

// ── box__search ─────────────────────────────────────────────────────────

func boxSearchTool(cfg CLIToolConfig) ToolDefinition {
	return ToolDefinition{
		Name:        "box__search",
		Description: "Full-text search across all Box content (files, folders, web links). Returns matching items with metadata.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"query": {
					"type": "string",
					"description": "The search query string"
				},
				"file_extensions": {
					"type": "string",
					"description": "Comma-separated file extensions to filter by (e.g. pdf,docx)"
				},
				"type": {
					"type": "string",
					"enum": ["file", "folder", "web_link"],
					"description": "Limit results to a specific item type"
				},
				"ancestor_folder_id": {
					"type": "string",
					"description": "Limit search to items within this folder ID"
				},
				"limit": {
					"type": "integer",
					"description": "Max results to return (default 20)"
				}
			},
			"required": ["query"]
		}`),
		Execute: func(_ context.Context, input json.RawMessage) (string, error) {
			var p struct {
				Query            string `json:"query"`
				FileExtensions   string `json:"file_extensions"`
				Type             string `json:"type"`
				AncestorFolderID string `json:"ancestor_folder_id"`
				Limit            int    `json:"limit"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", fmt.Errorf("parsing input: %w", err)
			}

			args := []string{"search", p.Query, "--json", boxFields}
			if p.FileExtensions != "" {
				args = append(args, "--file-extensions", p.FileExtensions)
			}
			if p.Type != "" {
				args = append(args, "--type", p.Type)
			}
			if p.AncestorFolderID != "" {
				args = append(args, "--ancestor-folder-ids", p.AncestorFolderID)
			}
			if p.Limit > 0 {
				args = append(args, "--limit", fmt.Sprintf("%d", p.Limit))
			}

			return CLIExec(cfg, args)
		},
	}
}

// ── box__file_get ───────────────────────────────────────────────────────

func boxFileGetTool(cfg CLIToolConfig) ToolDefinition {
	return ToolDefinition{
		Name:        "box__file_get",
		Description: "Get metadata for a Box file by its ID. Returns name, size, owner, dates, shared link info, and more.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"file_id": {
					"type": "string",
					"description": "The Box file ID"
				}
			},
			"required": ["file_id"]
		}`),
		Execute: func(_ context.Context, input json.RawMessage) (string, error) {
			var p struct {
				FileID string `json:"file_id"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", fmt.Errorf("parsing input: %w", err)
			}

			return CLIExec(cfg, []string{"files:get", p.FileID, "--json", boxFields})
		},
		StaticSystem: "- When sharing a file from Box, always prefer Shared and Direct Download links\n" +
			"- Use box__file_get to download files from Box only if you need them for immediate use, " +
			"analysis, or requested as an attachment\n" +
			"- Downloading a file does not make it available to the user. " +
			"The file needs to be sent back to the user either as a link or as an email attachment\n",
	}
}

// ── box__folder_list ────────────────────────────────────────────────────

func boxFolderListTool(cfg CLIToolConfig) ToolDefinition {
	return ToolDefinition{
		Name:        "box__folder_list",
		Description: "List items (files and subfolders) in a Box folder. Use folder ID '0' for the root folder.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"folder_id": {
					"type": "string",
					"description": "The Box folder ID (use '0' for root)"
				},
				"limit": {
					"type": "integer",
					"description": "Max items to return (default 20)"
				}
			},
			"required": ["folder_id"]
		}`),
		Execute: func(_ context.Context, input json.RawMessage) (string, error) {
			var p struct {
				FolderID string `json:"folder_id"`
				Limit    int    `json:"limit"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", fmt.Errorf("parsing input: %w", err)
			}

			args := []string{"folders:items", p.FolderID, "--json", boxFields}
			if p.Limit > 0 {
				args = append(args, "--limit", fmt.Sprintf("%d", p.Limit))
			}

			return CLIExec(cfg, args)
		},
	}
}

// ── box__file_download ──────────────────────────────────────────────────

func boxFileDownloadTool(cfg CLIToolConfig) ToolDefinition {
	return ToolDefinition{
		Name:        "box__file_download",
		Description: "Download a file from Box. Returns the filename and base64-encoded file contents so you can attach it to an email.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"file_id": {
					"type": "string",
					"description": "The Box file ID to download"
				}
			},
			"required": ["file_id"]
		}`),
		Execute: func(_ context.Context, input json.RawMessage) (string, error) {
			var p struct {
				FileID string `json:"file_id"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", fmt.Errorf("parsing input: %w", err)
			}

			// Download to a temp directory so we can read the file back
			tmpDir, err := os.MkdirTemp("", "box-download-*")
			if err != nil {
				return "", fmt.Errorf("creating temp dir: %w", err)
			}
			defer os.RemoveAll(tmpDir)

			_, err = CLIExec(cfg, []string{"files:download", p.FileID, "--destination", tmpDir})
			if err != nil {
				return "", fmt.Errorf("downloading file: %w", err)
			}

			// The Box CLI saves the file with its original name in the destination dir
			entries, err := os.ReadDir(tmpDir)
			if err != nil || len(entries) == 0 {
				return "", fmt.Errorf("no file found after download")
			}

			filePath := filepath.Join(tmpDir, entries[0].Name())
			data, err := os.ReadFile(filePath)
			if err != nil {
				return "", fmt.Errorf("reading downloaded file: %w", err)
			}

			// Return structured result with filename and base64 content
			result := struct {
				Filename string `json:"filename"`
				Content  string `json:"content"`
				Encoding string `json:"encoding"`
				Size     int    `json:"size"`
			}{
				Filename: entries[0].Name(),
				Content:  base64.StdEncoding.EncodeToString(data),
				Encoding: "base64",
				Size:     len(data),
			}

			out, _ := json.Marshal(result)
			return string(out), nil
		},
	}
}
