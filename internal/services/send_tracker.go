package services

import (
	"context"
	"sync/atomic"
)

// SendTracker counts successful outbound deliveries within a single
// HandleMessage invocation. It is the substrate for the "did the agent
// actually send anything?" forcing function on set_task_status — a turn
// that writes a reply only as assistant text (never invoking a send tool)
// must be prevented from being closed out as `waiting` or `complete`,
// because in that state the recipient never received the reply and the
// agent is stuck waiting for a response to a message it never delivered.
//
// One tracker is attached to the context at the top of HandleMessage,
// shared by every tool call in that invocation, and discarded when the
// invocation returns. Snooze handling also runs through HandleMessage,
// so a fresh tracker is established for each snooze fire as well.
type SendTracker struct {
	n int64
}

type sendTrackerKey struct{}

// WithSendTracker returns a derived context carrying a fresh SendTracker.
// Call once at the top of HandleMessage (and once per HandleSnooze fire,
// which goes through HandleMessage). Subsequent calls overwrite the
// tracker — intentional, so the count always reflects a single
// invocation.
func WithSendTracker(ctx context.Context) context.Context {
	return context.WithValue(ctx, sendTrackerKey{}, &SendTracker{})
}

// MarkDelivered increments the tracker on ctx. Call from a send tool's
// Execute path *after* the underlying SMTP delivery succeeds — never
// before, so a failed send doesn't unlock set_task_status's gate.
//
// Safe to call when no tracker is on ctx (e.g. from a script or test):
// it's a no-op in that case, so callers don't need to special-case it.
func MarkDelivered(ctx context.Context) {
	if t, _ := ctx.Value(sendTrackerKey{}).(*SendTracker); t != nil {
		atomic.AddInt64(&t.n, 1)
	}
}

// DeliveryCount returns the number of successful deliveries recorded on
// ctx for this invocation. Returns 0 when no tracker is present so that
// gates default to "permissive" outside a HandleMessage turn — tests,
// scripts, and direct CLI tool invocations shouldn't trip the gate.
func DeliveryCount(ctx context.Context) int {
	if t, _ := ctx.Value(sendTrackerKey{}).(*SendTracker); t != nil {
		return int(atomic.LoadInt64(&t.n))
	}
	return 0
}

// HasSendTracker reports whether ctx was decorated with WithSendTracker.
// Use this in a gate before consulting DeliveryCount so the gate is
// active only inside a real agent invocation: outside one, callers (CLI,
// tests, ad-hoc scripts) shouldn't have set_task_status refuse them just
// because they never wired up the tracker.
func HasSendTracker(ctx context.Context) bool {
	_, ok := ctx.Value(sendTrackerKey{}).(*SendTracker)
	return ok
}
