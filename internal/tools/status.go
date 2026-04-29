package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"kikubot/internal/services"
	"log"
)

// ── Set Status Tool ──────────────────────────────────────────────────────────

func SetTaskStatusTool() ToolDefinition {
	return ToolDefinition{
		Name:        "set_task_status",
		Description: "Set the status of the agent to 'waiting', 'complete', or 'error'.  If fulfillment of a task requested by the user is waiting on other tasks or email responses, set the status to 'waiting'. If a final response has been returned to the manager, set the status to 'complete'.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"status": {
					"type": "string",
					"enum": ["waiting", "complete", "error"],
					"description": "The status to set the task to"
				},
				"message-id": {
					"type": "string",
					"description": "The Message-Id to associate with the status update"
				}
			}
		}`),
		Execute: setTaskStatus,
	}
}

func setTaskStatus(ctx context.Context, input json.RawMessage) (string, error) {
	var params struct {
		Status    string `json:"status"`
		MessageId string `json:"message-id"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return "", fmt.Errorf("failed to parse setTaskStatus input: %w", err)
	}

	if ctx == nil {
		ctx = context.Background()
	}
	messageId := services.EnsureAngleBrackets(params.MessageId)
	err := services.SetMemoryStatus(ctx, services.MemoryStatus(params.Status), messageId)
	if err != nil {
		log.Printf("error setting memory status: %s", err)
		return "", err
	}

	return "Status Updated", nil
}
