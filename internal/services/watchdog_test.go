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
	got, err := FindSnoozeByThread(wdThread)
	if err != nil {
		t.Fatalf("FindSnoozeByThread error = %v", err)
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
	if got, _ := FindSnoozeByThread(wdThread); got != nil {
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

	got, err := FindSnoozeByThread(wdThread)
	if err != nil {
		t.Fatalf("FindSnoozeByThread error = %v", err)
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

// TestArmWaitingWatchdog_DoesNotClobberRealSnooze: when a genuine (non-watchdog)
// snooze already owns the thread — e.g. a user-scheduled recurring task — the
// watchdog must stand down rather than overwrite it, since that snooze will
// re-trigger processing on its own.
func TestArmWaitingWatchdog_DoesNotClobberRealSnooze(t *testing.T) {
	useTempSnoozeFile(t)

	real := Snooze{
		ThreadId:    wdThread,
		MessageId:   "<scheduled@agents.example.com>",
		Crontab:     "0 9 * * *",
		Description: "daily report",
		UnSnooze:    time.Now().Add(24 * time.Hour),
	}
	if err := SaveSnoozeFile([]Snooze{real}); err != nil {
		t.Fatalf("seeding real snooze: %v", err)
	}

	if err := ArmWaitingWatchdog(context.Background(), wdMsg, wdThread, 60); err != nil {
		t.Fatalf("ArmWaitingWatchdog error = %v", err)
	}

	got, err := FindSnoozeByThread(wdThread)
	if err != nil {
		t.Fatalf("FindSnoozeByThread error = %v", err)
	}
	if got == nil {
		t.Fatalf("real snooze disappeared")
	}
	if got.Watchdog {
		t.Errorf("real snooze was overwritten by a watchdog entry")
	}
	if got.MessageId != real.MessageId {
		t.Errorf("MessageId = %q, want %q (real snooze should be untouched)", got.MessageId, real.MessageId)
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

	got, err := FindSnoozeByThread(wdThread)
	if err != nil {
		t.Fatalf("FindSnoozeByThread error = %v", err)
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

// TestArmWaitingWatchdog_SingleEntryPerThread: arming must not accumulate
// duplicate entries for the same thread (SaveSnooze enforces one-per-thread).
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
		if s.ThreadId == wdThread {
			count++
		}
	}
	if count != 1 {
		t.Errorf("entries for thread = %d, want 1", count)
	}
}
