package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"kikubot/internal/services"
	"log"
	"strings"
	"time"

	"github.com/robfig/cron"
)

func SnoozeTools() []ToolDefinition {
	return []ToolDefinition{SnoozeTool(), UnSnoozeTool()}
}

func SnoozeTool() ToolDefinition {
	return ToolDefinition{
		Name:        "snooze_tool",
		Description: "Snooze message for a specified amount of time",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"Message-Id" : {
					"type": "string",
					"description": "The Message-Id of the email to snooze"
				},
				"Description" : {
					"type": "string",
					"description": "A short description of the snoozed task"
				},
				"Once": {
					"type": "boolean",
					"description": "If the task is a one-time request, set to true"
				},
				"Crontab": {
					"type": "string",
					"description": "The crontab expression to snooze until. E.g. '0 0 * * *' for every day at midnight"
				}
			},
			"required": ["Message-Id", "Description", "Once", "Crontab"]
		}`),
		Execute: snoozeEmail,
		System:  snoozeSystem,
	}
}

func snoozeSystem(email services.Email) (string, error) {
	snoozeClause := fmt.Sprintf("If the user's request indicates this is a task that is to be scheduled, call the 'snooze_tool' to set the next execution time. The message time is: %s. Crontab times should be set exactly as the user states them (e.g. '7am' → hour 7). The system will automatically adjust for the user's timezone.", email.Date.Format(time.RFC3339))

	// Check if this thread already has an active snooze
	threadRoot := email.GetThreadRoot()
	var threadMatch *services.Snooze
	if threadRoot != "" {
		existing, err := services.FindSnoozeByThread(threadRoot)
		if err != nil {
			log.Printf("error checking existing snooze: %s", err)
		}
		threadMatch = existing
	}

	if threadMatch != nil {
		// Direct thread match — agent can cancel immediately if asked
		snoozeClause += fmt.Sprintf(
			"\n\nThis thread has an active scheduled task:\n\n%q\n\nMessage-Id: %s, schedule: %s, next run: %s\n\n"+
				"If the user wants to cancel or stop it, call the 'unsnooze_tool' with the Message-Id.",
			threadMatch.Description, threadMatch.MessageId, threadMatch.Crontab,
			threadMatch.UnSnooze.Format(time.RFC3339))
	} else {
		// No thread match — include all active snoozes so the agent can
		// match a cross-thread cancellation request against them.
		allSnoozes, err := services.ReadSnoozeFile()
		if err != nil {
			log.Printf("error reading snooze file: %s", err)
		} else if len(allSnoozes) > 0 {
			snoozeClause += "\n\nThere are active scheduled tasks (but none belong to this thread).\n" +
				"If the user is asking to cancel or stop a task, identify the best match from the list below " +
				"using the following guidelines:\n" +
				"- If the match is clear, call 'unsnooze_tool' with its Message-Id.\n" +
				"- If ambiguous, reply to the user listing the possible matches and ask them to confirm which one to cancel.\n" +
				"ACTIVE SNOOZED TASKS:"
			for _, s := range allSnoozes {
				snoozeClause += fmt.Sprintf(
					"\n- %q (Message-Id: %s, subject: %q)", s.Description, s.MessageId, s.Subject)
			}
		}
	}

	snoozeClause += "\n\n"

	return snoozeClause, nil
}

func UnSnoozeTool() ToolDefinition {
	return ToolDefinition{
		Name:        "unsnooze_tool",
		Description: "Cancel a scheduled/snoozed task for a message",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"Message-Id" : {
					"type": "string",
					"description": "The Message-Id of the snoozed email to cancel"
				}
			},
			"required": ["Message-Id"]
		}`),
		Execute: unsnoozeEmail,
	}
}

func unsnoozeEmail(_ context.Context, input json.RawMessage) (string, error) {
	var params struct {
		MessageId string `json:"Message-Id"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	if params.MessageId == "" {
		return "", fmt.Errorf("Message-Id is required")
	}
	messageId := services.EnsureAngleBrackets(params.MessageId)

	s := &services.Snooze{MessageId: messageId}
	if err := s.DeleteSnooze(); err != nil {
		return "", fmt.Errorf("error cancelling snooze: %w", err)
	}

	return "Scheduled task cancelled", nil
}

func snoozeEmail(ctx context.Context, input json.RawMessage) (string, error) {
	var params struct {
		MessageId   string `json:"Message-Id"`
		Description string `json:"Description"`
		Once        bool   `json:"Once"`
		Crontab     string `json:"Crontab"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	if params.MessageId == "" {
		return "", fmt.Errorf("Message-Id is required")
	}
	messageId := services.EnsureAngleBrackets(params.MessageId)

	if ctx == nil {
		ctx = context.Background()
	}

	// Get Subject
	emails, emailErr := services.GetEmails(ctx, []string{messageId})
	if emailErr != nil {
		log.Printf("error getting emails: %s", emailErr)
		return "", fmt.Errorf("error getting emails: %w", emailErr)
	}
	if len(emails) == 0 {
		log.Printf("no email found")
		return "", fmt.Errorf("no email found")
	}

	email := emails[0]

	subject := email.Subject
	description := params.Description
	once := params.Once
	date := email.Date

	// Parse Crontab
	crontab := strings.TrimSpace(params.Crontab)
	specParser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	sched, err := specParser.Parse(crontab)
	if err != nil {
		return "", fmt.Errorf("couldn't parse crontab expression (%s): %w", crontab, err)
	}

	// Extract the user's timezone from the email date header so that
	// crontab expressions are always evaluated in the user's local time,
	// even when the server runs in a different timezone (e.g. UTC).
	userTZ := services.TimezoneFromTime(date)

	snooze := services.Snooze{
		MessageId:   messageId,
		Subject:     subject,
		Description: description,
		Once:        once,
		Crontab:     crontab,
		UnSnooze:    sched.Next(date),
		Timezone:    userTZ,
	}

	log.Printf("Snooze Subject: %s, Crontab: %s\n", snooze.Subject, snooze.Crontab)

	snoozeErr := snooze.SaveSnooze(ctx)
	if snoozeErr != nil {
		log.Printf("error saving snooze: %s", snoozeErr)
		return "", fmt.Errorf("error saving snooze: %w", snoozeErr)
	}

	return "Message snoozed", nil
}
