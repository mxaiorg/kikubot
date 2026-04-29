package tools

// ── Message Tool ──────────────────────────────────────────────────────────

func MessageTool() ToolDefinition {
	return ToolDefinition{
		Name:        "message_tool",
		Description: "Forward messages to coworkers or reply to messages from coworkers to complete task. Messages should be concise and to the point. ONLY use for sending to coworkers.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"To": {
					"type": "string",
					"description": "The coworker email address to send the sendEmail to"
				},
				"In-Reply-To" : {
					"type": "string",
					"description": "If a reply, the message ID of the email this is a reply to"
				},
				"X-Forwarded" : {
					"type": "string",
					"description": "If forwarding, the message ID of the email being forwarded"
				},
				"Subject": {
					"type": "string",
					"description": "If NOT replying or forwarding, the subject of the email"
				},
				"Message": {
					"type": "string",
					"description": "The message to send"
				},
				"attachments": {
					"type": "array",
					"description": "Optional file attachments to include with the email",
					"items": {
						"type": "object",
						"properties": {
							"name": {
								"type": "string",
								"description": "Filename with extension (e.g. report.csv, summary.pdf)"
							},
							"content": {
								"type": "string",
								"description": "File content: base64-encoded for binary files, plain text for text files"
							},
							"encoding": {
								"type": "string",
								"enum": ["base64", "text"],
								"description": "How the content field is encoded"
							}
						},
						"required": ["name", "content", "encoding"]
					}
				}
			},
			"required": ["To","Subject","Message"]
		}`),
		Execute: sendEmailInternal,
		StaticSystem: "- Communicate with coworkers using the message_tool\n" +
			"- Whenever sending messages with the message_tool, use the set_task_status tool to update the status of a task.\n" +
			"- If the body of your message would exceed roughly a few hundred lines (CSV, large JSON, long logs, multi-page tables), put the bulk in an `attachments` entry and keep `Message` to a short cover note. Stuffing large payloads into `Message` will exceed the model's output limit, truncate the tool call, and force a retry.",
	}
}
