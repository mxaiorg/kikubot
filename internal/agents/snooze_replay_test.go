package agents

import (
	"strings"
	"testing"
	"time"

	"kikubot/internal/services"
)

func TestSnoozeReplayContent_ScheduledRun(t *testing.T) {
	email := services.Email{
		Date:    time.Date(2026, 9, 10, 9, 3, 18, 0, time.FixedZone("", 3*3600)),
		Content: "Yes, set up a daily run for every 6am Cyprus time.",
	}
	snooze := services.Snooze{
		Description: "Daily Bazaraki cycle — triage, analyst, scout, report",
		Timezone:    "+0300",
	}
	now := time.Date(2026, 9, 11, 3, 0, 13, 0, time.UTC)

	got := snoozeReplayContent(snooze, email, now)

	for _, want := range []string{
		"AUTOMATED SCHEDULED RUN",
		"Current time: Fri, 11 Sep 2026 06:00 +0300",
		"Task to perform now: Daily Bazaraki cycle — triage, analyst, scout, report",
		"do not treat this as a duplicate",
		"--- Replayed email ---\nYes, set up a daily run for every 6am Cyprus time.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("replay content missing %q:\n%s", want, got)
		}
	}
}

func TestSnoozeReplayContent_Watchdog(t *testing.T) {
	email := services.Email{Content: "original"}
	snooze := services.Snooze{Watchdog: true, Description: "send a follow-up"}

	got := snoozeReplayContent(snooze, email, time.Now())

	if !strings.Contains(got, "AUTOMATED FOLLOW-UP") || !strings.Contains(got, "Instruction: send a follow-up") {
		t.Errorf("watchdog replay framing wrong:\n%s", got)
	}
	if strings.Contains(got, "SCHEDULED RUN") || strings.Contains(got, "from the start") {
		t.Errorf("watchdog replay must not tell the model to restart the task:\n%s", got)
	}
}
