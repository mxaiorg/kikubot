package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"kikubot/internal/services"
	"os"
	"path/filepath"
	"strings"
)

// SaveAttachmentTool materialises an inbound attachment to a local file so
// downstream tools (bash_exec/curl, wordpress_tool, etc.) can stream the
// bytes to an external API. Inline images and PDFs are delivered to the
// model as Anthropic vision/document blocks — the model can describe them
// but cannot read or copy their bytes into another tool call. Without this
// tool the only path from "attached image" to "POST /wp-json/wp/v2/media"
// is via IMAP fetch with raw credentials, which routinely burns a whole
// turn budget.
func SaveAttachmentTool() ToolDefinition {
	return ToolDefinition{
		Name: "save_attachment",
		Description: "Save an attachment from the current inbound email to a local file path. " +
			"Use this when you need to feed a binary attachment (image, PDF, archive) into another " +
			"tool that takes a file path or multipart upload (curl -F file=@PATH, wordpress media " +
			"upload, etc.). Returns the local path; pipe it straight into bash_exec.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"name": {
					"type": "string",
					"description": "Filename of the attachment to save, as it appears in the email's attachments list (case-insensitive match)."
				}
			},
			"required": ["name"]
		}`),
		Execute: executeSaveAttachment,
		StaticSystem: "- Inline images, PDFs, and Office docs from inbound emails are passed to you as native vision/document blocks. You can SEE them, but you cannot read their raw bytes inside a tool call — they are not text. Asking bash/python/node to access \"the image above\" will not work.\n" +
			"- To upload an inbound attachment to an external service (WordPress media, Buffer image upload, multipart curl POSTs, etc.), call save_attachment first with the attachment's filename. It writes the bytes to a local path and returns that path. Then pass the path to bash_exec (e.g. `curl -F file=@/tmp/foo.png …`).\n" +
			"- save_attachment only operates on the CURRENT inbound email's attachments. If a coworker forwarded a file to you, they used X-Forwarded so the bytes are re-attached on your inbound — call save_attachment with the filename shown in the email's attachments list.",
	}
}

func executeSaveAttachment(ctx context.Context, input json.RawMessage) (string, error) {
	var params struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	requested := strings.TrimSpace(params.Name)
	if requested == "" {
		return "", fmt.Errorf("name is required")
	}

	src := services.SourceEmail(ctx)
	if src == nil {
		return "", fmt.Errorf("no inbound email on context — save_attachment is only callable while processing an email")
	}

	if len(src.Attachments) == 0 {
		return "", fmt.Errorf("the current inbound email has no attachments")
	}

	// Case-insensitive filename match. Multiple attachments may share a name
	// (rare but legal); take the first hit and let the caller disambiguate
	// upstream if it matters.
	var match *services.Attachment
	lcReq := strings.ToLower(requested)
	for i := range src.Attachments {
		if strings.ToLower(src.Attachments[i].Name) == lcReq {
			match = &src.Attachments[i]
			break
		}
	}
	if match == nil {
		available := make([]string, 0, len(src.Attachments))
		for _, a := range src.Attachments {
			available = append(available, a.Name)
		}
		return "", fmt.Errorf("no attachment named %q on the inbound email; available: %s",
			requested, strings.Join(available, ", "))
	}
	if len(match.Data) == 0 {
		return "", fmt.Errorf("attachment %q has zero bytes", match.Name)
	}

	// Sanitise the on-disk name: strip any path components so an LLM-supplied
	// or maliciously-crafted filename cannot escape the temp dir.
	safeName := filepath.Base(match.Name)
	if safeName == "" || safeName == "." || safeName == "/" {
		safeName = "attachment.bin"
	}
	destPath := filepath.Join(os.TempDir(), safeName)

	if err := os.WriteFile(destPath, match.Data, 0o600); err != nil {
		return "", fmt.Errorf("write attachment: %w", err)
	}

	return fmt.Sprintf("Saved %d bytes to %s", len(match.Data), destPath), nil
}
