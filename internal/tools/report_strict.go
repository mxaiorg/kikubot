package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"kikubot/internal/config"
	"kikubot/internal/services"
	"log"
)

// ── Report Strict Tool ───────────────────────────────────────────────────

// ReportStrictTool is a hardened variant of ReportTool that guarantees the
// outbound email is delivered to exactly one recipient: the original
// requester at the root of the thread. The LLM cannot influence the
// recipient list — To and Cc are not accepted in the schema and are
// overwritten before send. Intended for public/test deployments where the
// agent must not be usable as a relay to email arbitrary third parties.
func ReportStrictTool() ToolDefinition {
	return ToolDefinition{
		Name: "report_strict_tool",
		Description: "Report back to the original requester. The recipient is fixed " +
			"by the system to the human who started the thread — you cannot choose " +
			"or override it. Use this to deliver results or ask the requester for " +
			"more information.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
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
			"required": ["In-Reply-To","message"]
		}`),
		Execute: sendReportStrictEmail,
		StaticSystem: "- The requester cannot see anything you write as assistant text or " +
			"reasoning. The ONLY way to deliver any words to them — a full report, a " +
			"clarifying question, an acknowledgement, or a refusal — is to call " +
			"`report_strict_tool`. If you intend the requester to read it, it MUST be " +
			"in the `message` field of a `report_strict_tool` call.\n" +
			"- Never end your turn having written a reply only as assistant text. If you " +
			"finished thinking and have something to say, you have not actually said it " +
			"until `report_strict_tool` is called. `set_task_status` alone is internal " +
			"state — it does not communicate with the requester.\n" +
			"- The recipient is determined by the system (the human at the root of the " +
			"thread); you do not specify To or Cc.\n" +
			"- Whenever you call `report_strict_tool`, also call `set_task_status` to " +
			"update the status of the task (typically `waiting` if you asked a question, " +
			"`complete` if you delivered a final answer).\n" +
			"- If the body of your reply would exceed roughly a few hundred lines (CSV, large JSON, long logs, multi-page tables), put the bulk in an `attachments` entry and keep `message` to a short cover note. Stuffing large payloads into `message` will exceed the model's output limit, truncate the tool call, and force a retry.",
	}
}

// sendReportStrictEmail resolves the original requester from the trusted
// inbound (services.SourceEmail) and walks back to the thread root to find
// the human who initiated the conversation. The recipient is then forced
// into the tool input — any To/Cc the LLM may have tried to smuggle in is
// discarded — before delegating to sendEmail.
func sendReportStrictEmail(ctx context.Context, input json.RawMessage) (string, error) {
	locked, err := lockReportRecipient(ctx, input)
	if err != nil {
		return "", err
	}
	var params sendMailMsg
	if err := json.Unmarshal(locked, &params); err != nil {
		return "", fmt.Errorf("invalid locked report input: %w", err)
	}
	result, err := sendEmail(ctx, locked)
	if err != nil {
		return "", err
	}
	fireReportSent(ctx, SentReport{
		To:        params.To,
		Cc:        params.Cc,
		InReplyTo: params.InReplyTo,
		Subject:   params.Subject,
		Message:   params.Message,
	})
	return result, nil
}

// lockReportRecipient parses the tool input, resolves the authorised
// recipient (the original requester), and returns a re-marshalled payload
// with To set to that single address and Cc cleared.
func lockReportRecipient(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	var params sendMailMsg
	if err := json.Unmarshal(input, &params); err != nil {
		return nil, fmt.Errorf("invalid input: %w", err)
	}

	requester, err := resolveOriginalRequester(ctx)
	if err != nil {
		return nil, err
	}

	if params.To != "" && !addressMatches(params.To, requester) {
		log.Printf("report_strict_tool: discarding LLM-supplied To %q; locking to original requester %q",
			params.To, requester)
	}
	if params.Cc != "" {
		log.Printf("report_strict_tool: discarding LLM-supplied Cc %q", params.Cc)
	}

	params.To = requester
	params.Cc = ""

	out, marshalErr := json.Marshal(params)
	if marshalErr != nil {
		return nil, fmt.Errorf("failed to re-marshal locked input: %w", marshalErr)
	}
	return out, nil
}

// resolveOriginalRequester returns the bare email address of the human who
// started the current thread. It prefers the thread root's From, falling
// back to the trusted inbound's own From when the root can't be fetched or
// is the inbound itself. Returns an error when no non-agent human can be
// resolved — strict mode refuses to send if there is no clear requester.
func resolveOriginalRequester(ctx context.Context) (string, error) {
	src := services.SourceEmail(ctx)
	if src == nil {
		return "", fmt.Errorf("report_strict_tool: no triggering email on context — cannot determine original requester")
	}

	rootId := services.EnsureAngleBrackets(src.GetThreadRoot())
	srcId := services.EnsureAngleBrackets(src.MessageId)

	if rootId != "" && rootId != srcId {
		if emails, err := services.GetEmails(ctx, []string{rootId}); err == nil && len(emails) > 0 {
			if addr := bareAddressFromEmail(emails[0].From); addr != "" && !config.AgentEmails[addr] {
				return addr, nil
			}
		}
	}

	// Root unavailable, or root is srcEmail, or root sender is an agent
	// (e.g. delegation chain). Fall back to the trusted inbound's own From
	// when it's a human.
	if addr := bareAddressFromEmail(src.From); addr != "" && !config.AgentEmails[addr] {
		return addr, nil
	}

	return "", fmt.Errorf("report_strict_tool: cannot resolve a human requester from thread root or triggering email")
}

// addressMatches reports whether two email-address strings refer to the
// same mailbox after parsing and lower-casing. Used only for logging the
// LLM's attempted override; never gates correctness.
func addressMatches(a, b string) bool {
	pa := bareAddressFromEmail(a)
	pb := bareAddressFromEmail(b)
	if pa == "" || pb == "" {
		return false
	}
	return pa == pb
}
