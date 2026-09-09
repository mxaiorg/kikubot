package tools

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"kikubot/internal/services"
)

// TestListSnoozedToolRegistered: the query tool has to ship with the other two
// or agents with the "snooze" key still can't read their own schedule.
func TestListSnoozedToolRegistered(t *testing.T) {
	var found bool
	for _, tool := range SnoozeTools() {
		if tool.Name == "list_snoozed_tool" {
			found = true
			if tool.Execute == nil {
				t.Error("list_snoozed_tool has no Execute")
			}
		}
	}
	if !found {
		t.Errorf("SnoozeTools() does not include list_snoozed_tool")
	}
}

func TestRenderScheduledTasks_Empty(t *testing.T) {
	if got := renderScheduledTasks(nil); got != "No scheduled tasks." {
		t.Errorf("renderScheduledTasks(nil) = %q, want %q", got, "No scheduled tasks.")
	}
}

// TestRenderScheduledTasks_WatchdogsOnly: watchdog entries are internal
// stuck-task timers, not scheduled tasks — listing them as cancellable work
// would invite the agent to disarm its own safety net. They are still counted
// so the listing doesn't under-report what's pending.
func TestRenderScheduledTasks_WatchdogsOnly(t *testing.T) {
	got := renderScheduledTasks([]services.Snooze{
		{MessageId: "<a@x>", Watchdog: true, Description: "watchdog: still awaiting a reply"},
		{MessageId: "<b@x>", Watchdog: true},
	})
	if !strings.HasPrefix(got, "No scheduled tasks.") {
		t.Errorf("watchdog-only listing = %q, want it to report no scheduled tasks", got)
	}
	if !strings.Contains(got, "2 internal watchdog timer(s)") {
		t.Errorf("watchdog-only listing = %q, want a count of the pending watchdogs", got)
	}
	if strings.Contains(got, "<a@x>") {
		t.Errorf("watchdog entry leaked into the listing: %q", got)
	}
}

// TestRenderScheduledTasks_JSON: the listing is what the agent reads back when
// asked "what's the exact schedule?" — it must carry the crontab and a next
// run rendered in the timezone the task was scheduled against (the user's),
// not the container's UTC, and be ordered soonest-first.
func TestRenderScheduledTasks_JSON(t *testing.T) {
	later := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	sooner := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

	got := renderScheduledTasks([]services.Snooze{
		{
			MessageId: "<later@x>", ThreadId: "<root-later@x>", Subject: "Weekly report",
			Description: "send the weekly report", Crontab: "0 15 * * 1", UnSnooze: later,
		},
		{MessageId: "<wd@x>", Watchdog: true},
		{
			MessageId: "<sooner@x>", ThreadId: "<root-sooner@x>", Subject: "Standup",
			Description: "post the standup reminder", Crontab: "0 7 * * *", Once: false,
			UnSnooze: sooner, Timezone: "-0500",
		},
	})

	body, note, _ := strings.Cut(got, "\n\n(")
	if !strings.Contains(note, "1 internal watchdog timer(s)") {
		t.Errorf("listing = %q, want a trailing note counting the 1 watchdog", got)
	}

	var tasks []scheduledTask
	if err := json.Unmarshal([]byte(body), &tasks); err != nil {
		t.Fatalf("listing is not valid JSON (%v):\n%s", err, body)
	}
	if len(tasks) != 2 {
		t.Fatalf("got %d tasks, want 2 (the watchdog must be excluded): %+v", len(tasks), tasks)
	}
	if tasks[0].MessageId != "<sooner@x>" {
		t.Errorf("tasks[0].MessageId = %q, want the soonest task <sooner@x>", tasks[0].MessageId)
	}
	if tasks[0].Crontab != "0 7 * * *" {
		t.Errorf("tasks[0].Crontab = %q, want %q", tasks[0].Crontab, "0 7 * * *")
	}
	// 12:00Z scheduled against -0500 is the 07:00 the user actually asked for.
	if tasks[0].NextRun != "2026-09-08T07:00:00-05:00" {
		t.Errorf("tasks[0].NextRun = %q, want it rendered in the task's own timezone (2026-09-08T07:00:00-05:00)", tasks[0].NextRun)
	}
	if tasks[0].ThreadId != "<root-sooner@x>" {
		t.Errorf("tasks[0].ThreadId = %q, want %q", tasks[0].ThreadId, "<root-sooner@x>")
	}
	// No timezone recorded ⇒ render as stored rather than dropping the field.
	if tasks[1].NextRun != "2026-09-10T15:00:00Z" {
		t.Errorf("tasks[1].NextRun = %q, want %q", tasks[1].NextRun, "2026-09-10T15:00:00Z")
	}
}
