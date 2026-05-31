package tools

// ── Report Tool ──────────────────────────────────────────────────────────

// ReportTool sends an email to the user. This should only be
// used for agents that can communicate directly with users.
func ReportTool() ToolDefinition {
	return ToolDefinition{
		Name:        "report_tool",
		Description: "Report back to the user with the results of your task or to request additional information needed to fulfill the task",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"To": {
					"type": "string",
					"description": "The email addresses (comma separated) to send the email to"
				},
				"Cc": {
					"type": "string",
					"description": "The email addresses (comma separated) to copy the email to"
				},
				"In-Reply-To" : {
					"type": "string",
					"description": "The Message-Id of the email this is a reply to"
				},
				"message": {
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
			"required": ["To","In-Reply-To","message"]
		}`),
		Execute: sendReportEmail,
		StaticSystem: "- Users cannot see anything you write as assistant text or reasoning. " +
			"The ONLY way to deliver any words to them — a full report, a clarifying " +
			"question, an acknowledgement, or a refusal — is to call `report_tool`. If " +
			"you intend the user to read it, it MUST be in the `message` field of a " +
			"`report_tool` call.\n" +
			"- Never end your turn having written a reply only as assistant text. If you " +
			"finished thinking and have something to say, you have not actually said it " +
			"until `report_tool` is called. `set_task_status` alone is internal state — " +
			"it does not communicate with the user.\n" +
			"- Whenever you call `report_tool`, also call `set_task_status` to update the " +
			"status of the task (typically `waiting` if you asked a question, `complete` " +
			"if you delivered a final answer).\n" +
			"- If the body of your reply would exceed roughly a few hundred lines (CSV, large JSON, long logs, multi-page tables), put the bulk in an `attachments` entry and keep `message` to a short cover note. Stuffing large payloads into `message` will exceed the model's output limit, truncate the tool call, and force a retry.",
	}
}
