package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"kikubot/internal/services"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func DownloadTool() ToolDefinition {
	return ToolDefinition{
		Name: "download_file",
		Description: "Download a file from a URL. By default extracts and returns the text content " +
			"(PDF, DOCX, etc.). Set extract_text to false to just save the file and return the local path.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"url": {
					"type": "string",
					"description": "The URL to download the file from"
				},
				"filename": {
					"type": "string",
					"description": "Optional filename to save as. If omitted, derived from the URL."
				},
				"extract_text": {
					"type": "boolean",
					"description": "If true (default), extract and return text content from the file. If false, just save and return the path."
				}
			},
			"required": ["url"]
		}`),
		Execute: executeDownload,
		StaticSystem: "- Use the download_file tool to download files from URLs and extract their text content.\n" +
			"- This runs locally with full internet access.\n" +
			"- By default it downloads AND extracts text (PDF, DOCX, etc.) in one step.\n" +
			"- Set extract_text to false if you only need to save the file.\n",
	}
}

func executeDownload(ctx context.Context, input json.RawMessage) (string, error) {
	var params struct {
		URL         string `json:"url"`
		Filename    string `json:"filename"`
		ExtractText *bool  `json:"extract_text"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	if params.URL == "" {
		return "", fmt.Errorf("url is required")
	}

	// Default: extract text
	extractText := true
	if params.ExtractText != nil {
		extractText = *params.ExtractText
	}

	if ctx == nil {
		ctx = context.Background()
	}
	client := &http.Client{Timeout: 60 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, params.URL, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	// Determine filename
	filename := params.Filename
	if filename == "" {
		filename = filepath.Base(params.URL)
		if filename == "" || filename == "." || filename == "/" {
			filename = "downloaded_file"
		}
	}

	tmpDir := os.TempDir()
	destPath := filepath.Join(tmpDir, filename)

	out, err := os.Create(destPath)
	if err != nil {
		return "", fmt.Errorf("failed to create file: %w", err)
	}
	defer out.Close()

	written, err := io.Copy(out, resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to write file: %w", err)
	}

	if !extractText {
		return fmt.Sprintf("Downloaded %d bytes to %s", written, destPath), nil
	}

	// Extract text via Tika
	text, err := services.ExtractTextFromFile(destPath)
	if err != nil {
		// Fall back to returning the path if extraction fails
		return fmt.Sprintf("Downloaded %d bytes to %s (text extraction failed: %v)", written, destPath, err), nil
	}

	return text, nil
}
