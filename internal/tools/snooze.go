package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"kikubot/internal/services"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron"
)

func SnoozeTools() []ToolDefinition {
	return []ToolDefinition{SnoozeTool(), UnSnoozeTool(), ListSnoozedTool()}
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

	// The summary below names the active tasks but not their schedules, so
	// point at the tool that can answer "when exactly does that run?".
	snoozeClause += " To read back your own schedule — every task's crontab expression, next run time and Message-Id — call 'list_snoozed_tool'."

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
				"If the user wants to cancel or stop it, call the 'unsnooze_tool' with the Message-Id. "+
				"For the tasks scheduled on other threads, call 'list_snoozed_tool'.",
			threadMatch.Description, threadMatch.MessageId, threadMatch.Crontab,
			nextRunString(*threadMatch))
	} else {
		// No thread match — include all active snoozes so the agent can
		// match a cross-thread cancellation request against them. Watchdog
		// entries are skipped: they are internal stuck-task timers, not tasks
		// a user scheduled, and offering them as cancellation candidates only
		// invites the agent to disarm its own safety net.
		allSnoozes, err := services.ReadSnoozeFile()
		if err != nil {
			log.Printf("error reading snooze file: %s", err)
		} else {
			var listed []services.Snooze
			for _, s := range allSnoozes {
				if !s.Watchdog {
					listed = append(listed, s)
				}
			}
			if len(listed) > 0 {
				snoozeClause += "\n\nThere are active scheduled tasks (but none belong to this thread).\n" +
					"If the user is asking to cancel or stop a task, identify the best match from the list below " +
					"using the following guidelines:\n" +
					"- If the match is clear, call 'unsnooze_tool' with its Message-Id.\n" +
					"- If ambiguous, reply to the user listing the possible matches and ask them to confirm which one to cancel.\n" +
					"- For each task's schedule and next run time, call 'list_snoozed_tool'.\n" +
					"ACTIVE SNOOZED TASKS:"
				for _, s := range listed {
					snoozeClause += fmt.Sprintf(
						"\n- %q (Message-Id: %s, subject: %q)", s.Description, s.MessageId, s.Subject)
				}
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

// ListSnoozedTool lets the agent read back its own schedule.
//
// snoozeSystem already names the active tasks in the prompt, but only their
// description/subject/Message-Id — not the crontab expression or the next run
// time — so an agent asked "when does that run?" or "what's the exact
// schedule?" had no way to answer. It also only lists other threads' tasks
// when the current thread has no snooze of its own, and it is dropped entirely
// from a replayed snooze turn (HandleSnooze strips the snooze tools, and with
// them their System text). A read-only query tool covers all three gaps.
func ListSnoozedTool() ToolDefinition {
	return ToolDefinition{
		Name: "list_snoozed_tool",
		Description: "List your scheduled (snoozed) tasks — each task's description, subject, crontab expression, " +
			"next run time and Message-Id. Use this whenever you need the exact schedule of a task, or the " +
			"Message-Id to pass to 'unsnooze_tool' to cancel one. Takes no arguments.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {}
		}`),
		Execute: listSnoozed,
	}
}

// listSnoozed returns the agent's scheduled tasks as JSON, soonest first.
//
// Input is ignored: the tool takes no arguments, and OpenRouter occasionally
// truncates streamed tool-call arguments mid-stream — a read-only listing has
// nothing to validate, so it should never fail over a malformed `{}`.
func listSnoozed(_ context.Context, _ json.RawMessage) (string, error) {
	snoozed, err := services.ReadSnoozeFile()
	if err != nil {
		return "", fmt.Errorf("reading scheduled tasks: %w", err)
	}
	return renderScheduledTasks(snoozed), nil
}

// scheduledTask is the slim, agent-facing projection of a services.Snooze.
// Field names spell out what each value is for, since the model reads them
// straight out of the tool result.
type scheduledTask struct {
	Description string `json:"description,omitempty"`
	Subject     string `json:"subject,omitempty"`
	Crontab     string `json:"crontab,omitempty"`
	Once        bool   `json:"once"`
	NextRun     string `json:"next_run,omitempty"`
	Timezone    string `json:"timezone,omitempty"`
	MessageId   string `json:"message-id,omitempty"`
	ThreadId    string `json:"thread-id,omitempty"`
}

// renderScheduledTasks projects the snooze file into the tool result.
//
// Watchdog entries are excluded from the list: they are internal stuck-task
// timers armed by set_task_status(waiting), not tasks anybody scheduled, and
// they clear themselves. They are still counted in a trailing note so the
// listing never silently under-reports what is pending.
func renderScheduledTasks(snoozed []services.Snooze) string {
	tasks := make([]services.Snooze, 0, len(snoozed))
	watchdogs := 0
	for _, s := range snoozed {
		if s.Watchdog {
			watchdogs++
			continue
		}
		tasks = append(tasks, s)
	}

	note := ""
	if watchdogs > 0 {
		note = fmt.Sprintf("\n\n(%d internal watchdog timer(s) are also pending. They are not scheduled tasks — "+
			"they fire only if a delegated task goes unanswered, and clear themselves once the thread moves on.)", watchdogs)
	}

	if len(tasks) == 0 {
		return "No scheduled tasks." + note
	}

	sort.Slice(tasks, func(i, j int) bool { return tasks[i].UnSnooze.Before(tasks[j].UnSnooze) })

	out := make([]scheduledTask, 0, len(tasks))
	for _, s := range tasks {
		out = append(out, scheduledTask{
			Description: s.Description,
			Subject:     s.Subject,
			Crontab:     s.Crontab,
			Once:        s.Once,
			NextRun:     nextRunString(s),
			Timezone:    s.Timezone,
			MessageId:   s.MessageId,
			ThreadId:    s.ThreadId,
		})
	}

	// Encoder with HTML escaping off: json.Marshal would render the angle
	// brackets of every Message-Id as \u003c/\u003e, which is correct JSON but
	// unreadable in a tool result the model has to copy ids out of.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		// Nothing in scheduledTask can fail to marshal; fall back to a plain
		// listing rather than failing the tool call outright.
		var b strings.Builder
		for _, t := range out {
			fmt.Fprintf(&b, "- %q (crontab: %s, next run: %s, Message-Id: %s)\n",
				t.Description, t.Crontab, t.NextRun, t.MessageId)
		}
		return b.String() + note
	}
	return strings.TrimRight(buf.String(), "\n") + note
}

// nextRunString renders a task's next run in the timezone it was scheduled
// against (the requesting user's), not the container's UTC — "07:00-05:00" is
// what the user asked for; "12:00Z" would read as the wrong hour to them.
func nextRunString(s services.Snooze) string {
	if s.UnSnooze.IsZero() {
		return ""
	}
	t := s.UnSnooze
	if s.Timezone != "" {
		if loc, err := services.LoadTimezone(s.Timezone); err == nil {
			t = t.In(loc)
		}
	}
	return t.Format(time.RFC3339)
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
