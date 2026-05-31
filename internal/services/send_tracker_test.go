package services

import (
	"context"
	"sync"
	"testing"
)

// TestSendTracker_BarePermissive: outside an agent invocation (no tracker
// on ctx), DeliveryCount returns 0, MarkDelivered is a no-op, and
// HasSendTracker returns false. This is what keeps CLI/test/script
// callers of set_task_status from tripping the gate.
func TestSendTracker_BarePermissive(t *testing.T) {
	ctx := context.Background()
	if HasSendTracker(ctx) {
		t.Errorf("HasSendTracker on bare ctx = true, want false")
	}
	if got := DeliveryCount(ctx); got != 0 {
		t.Errorf("DeliveryCount on bare ctx = %d, want 0", got)
	}
	// Must not panic on bare ctx.
	MarkDelivered(ctx)
	if got := DeliveryCount(ctx); got != 0 {
		t.Errorf("DeliveryCount after MarkDelivered on bare ctx = %d, want 0", got)
	}
}

// TestSendTracker_DecoratedCounts: WithSendTracker attaches a fresh
// tracker; MarkDelivered increments; DeliveryCount reflects the count.
// This is the path set_task_status's gate relies on.
func TestSendTracker_DecoratedCounts(t *testing.T) {
	ctx := WithSendTracker(context.Background())
	if !HasSendTracker(ctx) {
		t.Fatalf("HasSendTracker after WithSendTracker = false, want true")
	}
	if got := DeliveryCount(ctx); got != 0 {
		t.Errorf("DeliveryCount on fresh tracker = %d, want 0", got)
	}
	MarkDelivered(ctx)
	if got := DeliveryCount(ctx); got != 1 {
		t.Errorf("DeliveryCount after 1 MarkDelivered = %d, want 1", got)
	}
	MarkDelivered(ctx)
	MarkDelivered(ctx)
	if got := DeliveryCount(ctx); got != 3 {
		t.Errorf("DeliveryCount after 3 MarkDelivered = %d, want 3", got)
	}
}

// TestSendTracker_FreshPerInvocation: calling WithSendTracker again on
// an already-decorated ctx yields a NEW tracker. This matters because
// HandleSnooze invokes HandleMessage, which calls WithSendTracker —
// each fire must start at zero, otherwise a snooze handler could be
// gated open by a previous turn's deliveries.
func TestSendTracker_FreshPerInvocation(t *testing.T) {
	first := WithSendTracker(context.Background())
	MarkDelivered(first)
	MarkDelivered(first)
	if got := DeliveryCount(first); got != 2 {
		t.Fatalf("first.DeliveryCount = %d, want 2", got)
	}

	second := WithSendTracker(first)
	if got := DeliveryCount(second); got != 0 {
		t.Errorf("second.DeliveryCount = %d, want 0 (fresh tracker should not inherit)", got)
	}
	// Parent still reads its own count — child does not affect parent.
	if got := DeliveryCount(first); got != 2 {
		t.Errorf("first.DeliveryCount after creating second = %d, want 2", got)
	}
}

// TestSendTracker_ConcurrentIncrement: tool dispatch is currently
// serial within a turn, but the tracker uses atomics so we don't have
// to revisit this if dispatch ever parallelises (or a tool spawns
// goroutines). Spam from N goroutines and verify the total.
func TestSendTracker_ConcurrentIncrement(t *testing.T) {
	ctx := WithSendTracker(context.Background())
	const goroutines = 50
	const perGoroutine = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				MarkDelivered(ctx)
			}
		}()
	}
	wg.Wait()
	if got, want := DeliveryCount(ctx), goroutines*perGoroutine; got != want {
		t.Errorf("DeliveryCount after concurrent increment = %d, want %d", got, want)
	}
}
