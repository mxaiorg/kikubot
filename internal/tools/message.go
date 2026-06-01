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
					"description": "Optional file attachments to include with the email. Prefer 'path' for any file already on disk (downloaded or saved); use 'content' only for small files you author inline in this turn.",
					"items": {
						"type": "object",
						"properties": {
							"name": {
								"type": "string",
								"description": "Filename with extension (e.g. report.csv, summary.pdf). If omitted when using 'path', the path's basename is used."
							},
							"path": {
								"type": "string",
								"description": "Local file path of an existing file to attach (e.g. the /tmp/... path returned by download_file or save_attachment). Use this for any binary or large file — the bytes are read from disk, so nothing is truncated. When set, 'content' and 'encoding' are ignored."
							},
							"content": {
								"type": "string",
								"description": "Inline file content, used only when 'path' is not provided: base64-encoded for binary files, plain text for text files. Do NOT paste large base64 blobs here — they get truncated; use 'path' instead."
							},
							"encoding": {
								"type": "string",
								"enum": ["base64", "text"],
								"description": "How the 'content' field is encoded (ignored when 'path' is set)"
							}
						},
						"required": ["name"]
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
			"- To attach a file that already exists on disk — anything you fetched with `download_file` (use `extract_text:false` to keep the raw file) or materialised with `save_attachment`, or wrote via `bash_exec` — pass its local path as the attachment's `path` field. The bytes are read from disk, so binary files (images, PDFs, archives) attach intact. NEVER base64-encode a downloaded file and paste it into `content`: large blobs get truncated mid-stream and produce invalid base64. Reserve `content` for small files you author inline this turn (short reports, exports, plain text).\n" +
			"- When delegating a task that requires a coworker to see an attachment from an inbound email, use `X-Forwarded` with that email's Message-Id rather than rebuilding the file in `attachments`. The system re-fetches the original message and re-attaches its files byte-for-byte.\n" +
			"- `X-Forwarded` lets the recipient SEE an attachment (as a vision/document block) — it does not give them or you the raw bytes to feed into another tool. If a task needs the bytes (multipart upload, media library POST, file hashing, etc.), the agent that needs them must call `save_attachment` on its own inbound message to materialise the file to disk.",
	}
}
