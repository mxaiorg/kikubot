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
		StaticSystem: "- Coworkers cannot see anything you write as assistant text or reasoning. " +
			"The ONLY way to deliver words to a coworker — a delegation, a question, an " +
			"answer, an acknowledgement — is to call `message_tool`. If you intend a " +
			"coworker to read it, it MUST be in the `Message` field of a `message_tool` " +
			"call.\n" +
			"- Never end your turn having written a message to a coworker only as assistant " +
			"text. If you finished thinking and have something to send, you have not " +
			"actually sent it until `message_tool` is called. `set_task_status(waiting)` " +
			"without a preceding `message_tool` call is a deadlock: you will wait forever " +
			"for a reply to a message that was never delivered.\n" +
			"- Whenever you call `message_tool`, also call `set_task_status` to update the " +
			"status of the task (typically `waiting` if you expect a reply, `complete` if " +
			"the message was the final step).\n" +
			"- If the body of your message would exceed roughly a few hundred lines (CSV, large JSON, long logs, multi-page tables), put the bulk in an `attachments` entry and keep `Message` to a short cover note. Stuffing large payloads into `Message` will exceed the model's output limit, truncate the tool call, and force a retry.\n" +
			"- When delegating a task that requires a coworker to see an attachment from an inbound email, use `X-Forwarded` with that email's Message-Id rather than rebuilding the file in `attachments`. The system re-fetches the original message and re-attaches its files byte-for-byte; the `attachments` field cannot reproduce binary content you have not seen (you will at best send a placeholder). Reserve the `attachments` field for files you authored yourself in this turn (reports, exports, generated text).\n" +
			"- `X-Forwarded` lets the recipient SEE an attachment (as a vision/document block) — it does not give them or you the raw bytes to feed into another tool. If a task needs the bytes (multipart upload, media library POST, file hashing, etc.), the agent that needs them must call `save_attachment` on its own inbound message to materialise the file to disk.",
	}
}
