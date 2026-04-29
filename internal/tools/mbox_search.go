package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"kikubot/internal/services"
	"time"
)

// ── Mailbox Search Tool ──────────────────────────────────────────────────────────
// Allows the agent to search through their mailbox.

func MboxSearchTool() ToolDefinition {
	return ToolDefinition{
		Name:        "mailbox_search",
		Description: "Search through your mailbox for emails matching the given criteria.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"from": {
					"type": "string",
					"description": "Filter by sender email address or name"
				},
				"to": {
					"type": "string",
					"description": "Filter by recipient email address or name"
				},
				"subject": {
					"type": "string",
					"description": "Filter by subject line"
				},
				"date_from": {
					"type": "string",
					"description": "Start date for search range (RFC3339 format, e.g. 2026-03-01T00:00:00Z)"
				},
				"date_to": {
					"type": "string",
					"description": "End date for search range (RFC3339 format, e.g. 2026-03-22T00:00:00Z)"
				},
				"unread": {
					"type": "boolean",
					"description": "If true, only return unread emails"
				},
				"starred": {
					"type": "boolean",
					"description": "If true, only return starred/flagged emails"
				},
				"has_attachments": {
					"type": "boolean",
					"description": "If true, only return emails with attachments"
				}
			}
		}`),
		Execute: searchMbox,
	}
}

// searchMbox searches the user's mailbox for emails matching the given criteria.
// Returns a JSON array of email messages.
func searchMbox(ctx context.Context, input json.RawMessage) (string, error) {
	var params struct {
		From           string `json:"from"`
		To             string `json:"to"`
		Subject        string `json:"subject"`
		DateFrom       string `json:"date_from"`
		DateTo         string `json:"date_to"`
		Unread         bool   `json:"unread"`
		Starred        bool   `json:"starred"`
		HasAttachments bool   `json:"has_attachments"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return "", fmt.Errorf("parsing input: %w", err)
	}

	search := services.MailSearch{
		From:           params.From,
		To:             params.To,
		Subject:        params.Subject,
		Unread:         params.Unread,
		Starred:        params.Starred,
		HasAttachments: params.HasAttachments,
	}

	if params.DateFrom != "" {
		t, err := time.Parse(time.RFC3339, params.DateFrom)
		if err != nil {
			return "", fmt.Errorf("parsing date_from: %w", err)
		}
		search.DateFrom = t
	}
	if params.DateTo != "" {
		t, err := time.Parse(time.RFC3339, params.DateTo)
		if err != nil {
			return "", fmt.Errorf("parsing date_to: %w", err)
		}
		search.DateTo = t
	}

	if ctx == nil {
		ctx = context.Background()
	}
	emails, err := services.MailBoxSearch(ctx, search)
	if err != nil {
		return "", fmt.Errorf("mailbox search: %w", err)
	}

	if len(emails) == 0 {
		return "No emails found matching the given criteria.", nil
	}

	result, err := json.Marshal(emails)
	if err != nil {
		return "", fmt.Errorf("marshaling results: %w", err)
	}

	return string(result), nil
}
