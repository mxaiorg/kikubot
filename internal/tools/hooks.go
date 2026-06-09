package tools

import "context"

// This file is the designed extension surface for build-gated ("private")
// packages that need to react to public tool flows without editing public
// function bodies. Public code fires the hooks below at stable call sites;
// a package compiled into the binary (e.g. internal/tools_priv) registers
// callbacks from an init(). Every hook is a no-op in a build that registers
// nothing, so the default behaviour is unchanged.
//
// The rule this enforces: company-specific behaviour is *registered*, never
// edited into public function bodies — which is what turns each downstream
// customisation into a perennial merge conflict. Adding a new integration
// point means adding a Register* function here plus one fire call at the
// relevant public site, not sprinkling bespoke calls through public files.

// SentReport is the read-only view of an outgoing report handed to report-sent
// hooks. It carries only the fields a notifier might need, decoupling
// observers from the internal sendMailMsg shape.
type SentReport struct {
	To        string
	Cc        string
	InReplyTo string
	Subject   string
	Message   string
}

var (
	reportSentHooks   []func(ctx context.Context, report SentReport)
	taskCompleteHooks []func(ctx context.Context)
)

// RegisterReportSentHook registers a callback fired after a report is
// successfully delivered (report_tool / report_strict_tool). No-op when nothing
// registers. Hooks run synchronously in registration order on the send path, so
// they must be cheap and must not block.
func RegisterReportSentHook(fn func(ctx context.Context, report SentReport)) {
	reportSentHooks = append(reportSentHooks, fn)
}

// RegisterTaskCompleteHook registers a callback fired when a task transitions
// to "complete" via set_task_status. No-op when nothing registers.
func RegisterTaskCompleteHook(fn func(ctx context.Context)) {
	taskCompleteHooks = append(taskCompleteHooks, fn)
}

func fireReportSent(ctx context.Context, report SentReport) {
	for _, h := range reportSentHooks {
		h(ctx, report)
	}
}

func fireTaskComplete(ctx context.Context) {
	for _, h := range taskCompleteHooks {
		h(ctx)
	}
}
