package services

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// useTempSnoozeFile points the package-level snoozeFile at an isolated temp
// path for the duration of a test and restores it afterwards, so watchdog
// tests don't read or clobber a real snooze.json in the working directory.
func useTempSnoozeFile(t *testing.T) {
	t.Helper()
	orig := snoozeFile
	snoozeFile = filepath.Join(t.TempDir(), "snooze.json")
	t.Cleanup(func() { snoozeFile = orig })
}

const (
	wdThread = "<root@agents.example.com>"
	wdMsg    = "<inbound-42@agents.example.com>"
)

// TestArmWaitingWatchdog_Disabled: minutes <= 0 means the watchdog is turned
// off — arming must be a no-op and write nothing.
func TestArmWaitingWatchdog_Disabled(t *testing.T) {
	useTempSnoozeFile(t)

	if err := ArmWaitingWatchdog(context.Background(), wdMsg, wdThread, 0); err != nil {
		t.Fatalf("ArmWaitingWatchdog(minutes=0) error = %v", err)
	}
	got, err := FindWatchdogByThread(wdThread)
	if err != nil {
		t.Fatalf("FindWatchdogByThread error = %v", err)
	}
	if got != nil {
		t.Errorf("disabled watchdog wrote an entry: %+v, want none", got)
	}
}

// TestArmWaitingWatchdog_EmptyArgs: a missing message-id or thread-id (e.g. no
// trusted SourceEmail on ctx) must not arm anything rather than write a
// half-formed entry that can never replay.
func TestArmWaitingWatchdog_EmptyArgs(t *testing.T) {
	useTempSnoozeFile(t)

	if err := ArmWaitingWatchdog(context.Background(), "", wdThread, 60); err != nil {
		t.Fatalf("ArmWaitingWatchdog(empty msg) error = %v", err)
	}
	if err := ArmWaitingWatchdog(context.Background(), wdMsg, "", 60); err != nil {
		t.Fatalf("ArmWaitingWatchdog(empty thread) error = %v", err)
	}
	if got, _ := FindWatchdogByThread(wdThread); got != nil {
		t.Errorf("empty-arg arm wrote an entry: %+v, want none", got)
	}
}

// TestArmWaitingWatchdog_ArmsEntry: a normal arm writes a one-shot Watchdog
// entry keyed to the thread, carrying the inbound message-id for replay, a
// zero Fires counter, and a deadline at roughly now + minutes.
func TestArmWaitingWatchdog_ArmsEntry(t *testing.T) {
	useTempSnoozeFile(t)

	const minutes = 60
	before := time.Now()
	if err := ArmWaitingWatchdog(context.Background(), wdMsg, wdThread, minutes); err != nil {
		t.Fatalf("ArmWaitingWatchdog error = %v", err)
	}

	got, err := FindWatchdogByThread(wdThread)
	if err != nil {
		t.Fatalf("FindWatchdogByThread error = %v", err)
	}
	if got == nil {
		t.Fatalf("watchdog entry not written")
	}
	if !got.Watchdog {
		t.Errorf("Watchdog = false, want true")
	}
	if !got.Once {
		t.Errorf("Once = false, want true")
	}
	if got.MessageId != wdMsg {
		t.Errorf("MessageId = %q, want %q", got.MessageId, wdMsg)
	}
	if got.Fires != 0 {
		t.Errorf("Fires = %d, want 0", got.Fires)
	}
	lo := before.Add(minutes * time.Minute)
	hi := time.Now().Add(minutes*time.Minute + time.Minute)
	if got.UnSnooze.Before(lo) || got.UnSnooze.After(hi) {
		t.Errorf("UnSnooze = %v, want within [%v, %v]", got.UnSnooze, lo, hi)
	}
}

// seedScheduledTask writes a recurring scheduled task on wdThread keyed by the
// same Message-Id a scheduled run's watchdog carries (the replayed email), and
// returns it.
func seedScheduledTask(t *testing.T) Snooze {
	t.Helper()
	sched := Snooze{
		ThreadId:    wdThread,
		MessageId:   wdMsg,
		Crontab:     "0 6 * * *",
		Timezone:    "+0300",
		Description: "daily cycle",
		UnSnooze:    time.Now().Add(-time.Minute), // firing now
	}
	if err := SaveSnoozeFile([]Snooze{sched}); err != nil {
		t.Fatalf("seeding scheduled task: %v", err)
	}
	return sched
}

// TestArmWaitingWatchdog_CoexistsWithScheduledTask: a scheduled run that
// delegates and waits must still get a watchdog, and arming it must leave the
// schedule itself untouched even though both share thread and Message-Id.
func TestArmWaitingWatchdog_CoexistsWithScheduledTask(t *testing.T) {
	useTempSnoozeFile(t)
	sched := seedScheduledTask(t)

	if err := ArmWaitingWatchdog(context.Background(), wdMsg, wdThread, 60); err != nil {
		t.Fatalf("ArmWaitingWatchdog error = %v", err)
	}

	wd, err := FindWatchdogByThread(wdThread)
	if err != nil {
		t.Fatalf("FindWatchdogByThread error = %v", err)
	}
	if wd == nil {
		t.Fatalf("watchdog not armed on a thread that has a scheduled task")
	}
	got, err := FindSnoozeByThread(wdThread)
	if err != nil {
		t.Fatalf("FindSnoozeByThread error = %v", err)
	}
	if got == nil {
		t.Fatalf("scheduled task disappeared when the watchdog was armed")
	}
	if got.Watchdog || got.Crontab != sched.Crontab {
		t.Errorf("FindSnoozeByThread = %+v, want the untouched scheduled task", got)
	}
}

// TestAdvanceScheduledTask_KeepsWatchdog: the poll loop advances a schedule
// only after its run returns — by which time the run may have armed a
// watchdog. Advancing must not delete that watchdog.
func TestAdvanceScheduledTask_KeepsWatchdog(t *testing.T) {
	useTempSnoozeFile(t)
	sched := seedScheduledTask(t)
	if err := ArmWaitingWatchdog(context.Background(), wdMsg, wdThread, 60); err != nil {
		t.Fatalf("ArmWaitingWatchdog error = %v", err)
	}

	if err := AdvanceOrDeleteSnooze(context.Background(), &sched); err != nil {
		t.Fatalf("AdvanceOrDeleteSnooze error = %v", err)
	}

	if wd, _ := FindWatchdogByThread(wdThread); wd == nil {
		t.Errorf("advancing the scheduled task deleted the watchdog")
	}
	got, _ := FindSnoozeByThread(wdThread)
	if got == nil {
		t.Fatalf("scheduled task missing after advance")
	}
	if !got.UnSnooze.After(time.Now()) {
		t.Errorf("UnSnooze = %v, want the next tick in the future", got.UnSnooze)
	}
}

// TestDeleteSnooze_KindScoped: removing a watchdog (it stood down or gave up)
// must keep the schedule, and cancelling the schedule by Message-Id (what
// unsnooze_tool does) must keep the watchdog.
func TestDeleteSnooze_KindScoped(t *testing.T) {
	useTempSnoozeFile(t)
	seedScheduledTask(t)
	if err := ArmWaitingWatchdog(context.Background(), wdMsg, wdThread, 60); err != nil {
		t.Fatalf("ArmWaitingWatchdog error = %v", err)
	}

	wd, _ := FindWatchdogByThread(wdThread)
	if err := wd.DeleteSnooze(); err != nil {
		t.Fatalf("deleting watchdog: %v", err)
	}
	if got, _ := FindSnoozeByThread(wdThread); got == nil {
		t.Errorf("deleting the watchdog deleted the scheduled task")
	}

	if err := ArmWaitingWatchdog(context.Background(), wdMsg, wdThread, 60); err != nil {
		t.Fatalf("re-arming watchdog: %v", err)
	}
	if err := (&Snooze{MessageId: wdMsg}).DeleteSnooze(); err != nil {
		t.Fatalf("cancelling scheduled task: %v", err)
	}
	if got, _ := FindSnoozeByThread(wdThread); got != nil {
		t.Errorf("scheduled task still present after cancel: %+v", got)
	}
	if got, _ := FindWatchdogByThread(wdThread); got == nil {
		t.Errorf("cancelling the scheduled task deleted the watchdog")
	}
}

// TestAbortThreadSnoozes_KeepsRecurringSchedule: aborting a thread (a
// coworker's bounce or max-turns notice) clears its watchdog but keeps a
// recurring schedule, and leaves other threads alone.
func TestAbortThreadSnoozes_KeepsRecurringSchedule(t *testing.T) {
	useTempSnoozeFile(t)
	sched := seedScheduledTask(t)
	other := Snooze{ThreadId: "<other@agents.example.com>", MessageId: "<other@agents.example.com>", Once: true}
	if err := SaveSnoozeFile([]Snooze{sched, other}); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if err := ArmWaitingWatchdog(context.Background(), wdMsg, wdThread, 60); err != nil {
		t.Fatalf("ArmWaitingWatchdog error = %v", err)
	}

	n, err := AbortThreadSnoozes(wdThread)
	if err != nil {
		t.Fatalf("AbortThreadSnoozes error = %v", err)
	}
	if n != 1 {
		t.Errorf("removed = %d, want 1 (the watchdog)", n)
	}
	if wd, _ := FindWatchdogByThread(wdThread); wd != nil {
		t.Errorf("watchdog survived the abort: %+v", wd)
	}
	if got, _ := FindSnoozeByThread(wdThread); got == nil {
		t.Errorf("abort deleted the recurring schedule")
	}
	if got, _ := FindSnoozeByThread(other.ThreadId); got == nil {
		t.Errorf("abort touched another thread's task")
	}
}

// TestAbortThreadSnoozes_RemovesOneShot: a one-shot scheduled run on an
// aborted thread is cleared along with the watchdog.
func TestAbortThreadSnoozes_RemovesOneShot(t *testing.T) {
	useTempSnoozeFile(t)
	oneShot := Snooze{ThreadId: wdThread, MessageId: wdMsg, Once: true, UnSnooze: time.Now().Add(time.Hour)}
	if err := SaveSnoozeFile([]Snooze{oneShot}); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if err := ArmWaitingWatchdog(context.Background(), wdMsg, wdThread, 60); err != nil {
		t.Fatalf("ArmWaitingWatchdog error = %v", err)
	}

	n, err := AbortThreadSnoozes(wdThread)
	if err != nil {
		t.Fatalf("AbortThreadSnoozes error = %v", err)
	}
	if n != 2 {
		t.Errorf("removed = %d, want 2", n)
	}
	if all, _ := ReadSnoozeFile(); len(all) != 0 {
		t.Errorf("remaining entries = %+v, want none", all)
	}
}

// TestArmWaitingWatchdog_RefreshResetsFires: re-arming a thread that already
// has a watchdog (the agent made fresh progress and set waiting again) replaces
// the entry and resets the Fires counter to zero, so the nudge budget restarts.
func TestArmWaitingWatchdog_RefreshResetsFires(t *testing.T) {
	useTempSnoozeFile(t)

	stale := Snooze{
		ThreadId:  wdThread,
		MessageId: wdMsg,
		Once:      true,
		Watchdog:  true,
		Fires:     2,
		UnSnooze:  time.Now().Add(-time.Minute),
	}
	if err := SaveSnoozeFile([]Snooze{stale}); err != nil {
		t.Fatalf("seeding stale watchdog: %v", err)
	}

	if err := ArmWaitingWatchdog(context.Background(), wdMsg, wdThread, 60); err != nil {
		t.Fatalf("ArmWaitingWatchdog error = %v", err)
	}

	got, err := FindWatchdogByThread(wdThread)
	if err != nil {
		t.Fatalf("FindWatchdogByThread error = %v", err)
	}
	if got == nil {
		t.Fatalf("watchdog entry not written")
	}
	if !got.Watchdog {
		t.Errorf("Watchdog = false, want true")
	}
	if got.Fires != 0 {
		t.Errorf("Fires = %d, want 0 (refresh should reset the nudge budget)", got.Fires)
	}
	if got.UnSnooze.Before(time.Now()) {
		t.Errorf("UnSnooze = %v is in the past; refresh should push the deadline forward", got.UnSnooze)
	}
}

// TestArmWaitingWatchdog_NudgeKeepsFires: a follow-up sent during the
// watchdog's own nudge re-arms it (fresh deadline) but keeps the nudge count,
// so a delegate that never answers still reaches the give-up cap.
func TestArmWaitingWatchdog_NudgeKeepsFires(t *testing.T) {
	useTempSnoozeFile(t)

	nudged := Snooze{
		ThreadId:  wdThread,
		MessageId: wdMsg,
		Once:      true,
		Watchdog:  true,
		Fires:     1,
		UnSnooze:  time.Now().Add(time.Minute),
	}
	if err := SaveSnoozeFile([]Snooze{nudged}); err != nil {
		t.Fatalf("seeding nudged watchdog: %v", err)
	}

	ctx := WithWatchdogNudge(context.Background())
	if err := ArmWaitingWatchdog(ctx, wdMsg, wdThread, 60); err != nil {
		t.Fatalf("ArmWaitingWatchdog error = %v", err)
	}

	got, err := FindWatchdogByThread(wdThread)
	if err != nil {
		t.Fatalf("FindWatchdogByThread error = %v", err)
	}
	if got == nil {
		t.Fatalf("watchdog entry missing after re-arm")
	}
	if got.Fires != 1 {
		t.Errorf("Fires = %d, want 1 (a nudge's own follow-up is not progress)", got.Fires)
	}
	if !got.UnSnooze.After(time.Now().Add(30 * time.Minute)) {
		t.Errorf("UnSnooze = %v, want the deadline pushed out to ~now+60m", got.UnSnooze)
	}
}

// TestArmWaitingWatchdog_SingleEntryPerThread: arming must not accumulate
// duplicate entries for the same thread (SaveSnooze enforces one watchdog per thread).
func TestArmWaitingWatchdog_SingleEntryPerThread(t *testing.T) {
	useTempSnoozeFile(t)

	for i := 0; i < 3; i++ {
		if err := ArmWaitingWatchdog(context.Background(), wdMsg, wdThread, 60); err != nil {
			t.Fatalf("ArmWaitingWatchdog #%d error = %v", i, err)
		}
	}

	all, err := ReadSnoozeFile()
	if err != nil {
		t.Fatalf("ReadSnoozeFile error = %v", err)
	}
	count := 0
	for _, s := range all {
		if s.ThreadId == wdThread && s.Watchdog {
			count++
		}
	}
	if count != 1 {
		t.Errorf("entries for thread = %d, want 1", count)
	}
}
