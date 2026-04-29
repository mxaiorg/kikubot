package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"kikubot/internal/services"
)

func FileTextTool() ToolDefinition {
	return ToolDefinition{
		Name:        "file_text_tool",
		Description: "Extracts text from a file and returns it as a response",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"filename": {
					"type": "string",
					"description": "Optional name of the file (helps Tika detect format)"
				},
				"path": {
					"type": "string",
					"description": "The path to the file to extract text from"
				},
				"data": {
					"type": "string",
					"description": "Base64-encoded file contents"
				}
			},
			"oneOf": [
				{ "required": ["path"] },
				{ "required": ["data"] }
			]
		}`),
		Execute: textFromFile,
		StaticSystem: "- If asked to extract text from a file, use the file_text_tool and be sure to return " +
			"the text results back to the requester either in clearly formatted text block or as a txt file attachment",
	}
}

func textFromFile(_ context.Context, input json.RawMessage) (string, error) {
	var params struct {
		Filename string `json:"filename"`
		Path     string `json:"path"`
		Data     string `json:"data"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}

	var data []byte
	if params.Data != "" {
		decoded, err := base64.StdEncoding.DecodeString(params.Data)
		if err != nil {
			return "", fmt.Errorf("failed to decode base64 data: %w", err)
		}
		data = decoded
	}

	if len(data) > 0 {
		content, err := services.ExtractTextFromBytes(data, params.Filename)
		if err != nil {
			return "", err
		}
		return content, nil
	}

	if params.Path != "" {
		s, err := services.ExtractTextFromFile(params.Path)
		if err != nil {
			return "", err
		}
		return s, nil
	}

	return "", fmt.Errorf("no data provided")
}
