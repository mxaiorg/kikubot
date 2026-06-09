package tools

import (
	"context"
	"testing"
)

// Covers the public report/task extension seam (hooks.go): register throwaway
// hooks, exercise the fire points, restore state on cleanup.
func TestReportLifecycleHooks_FireToRegisteredObservers(t *testing.T) {
	prevReport := reportSentHooks
	prevComplete := taskCompleteHooks
	reportSentHooks = nil
	taskCompleteHooks = nil
	t.Cleanup(func() {
		reportSentHooks = prevReport
		taskCompleteHooks = prevComplete
	})

	var gotReport SentReport
	reportSent := 0
	completed := 0
	RegisterReportSentHook(func(_ context.Context, r SentReport) {
		reportSent++
		gotReport = r
	})
	RegisterTaskCompleteHook(func(_ context.Context) { completed++ })

	fireReportSent(context.Background(), SentReport{To: "a@b.com", Message: "done"})
	fireTaskComplete(context.Background())

	if reportSent != 1 || completed != 1 {
		t.Fatalf("hooks fired report=%d complete=%d, want 1/1", reportSent, completed)
	}
	if gotReport.To != "a@b.com" || gotReport.Message != "done" {
		t.Fatalf("report payload not propagated: %#v", gotReport)
	}
}
