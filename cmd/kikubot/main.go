package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"kikubot/internal/agents"
	"kikubot/internal/config"
	"kikubot/internal/dotenv"
	"kikubot/internal/services"
	"kikubot/internal/tools"
	"log"
	netmail "net/mail"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "kikubot/internal/tools_priv"

	"github.com/anthropics/anthropic-sdk-go"
)

var (
	agent     *agents.Agent
	agentsCfg *config.AgentsConfig
)

// Knowledge hot-reload state. baseSystem is the system prompt without the
// knowledge block; knowledgeKey is the agent's knowledge dir key; and
// knowledgeModTime is the newest knowledge-file mtime seen on the last load.
// reloadKnowledgeIfChanged re-appends the knowledge block when files change,
// so edits take effect without a rebuild or restart.
var (
	baseSystem       string
	knowledgeKey     string
	knowledgeModTime time.Time
	// knowledgeReloadMu serializes reloads so the 30s poll and an on-demand
	// SIGHUP trigger can't race on knowledgeModTime / the agent prompt.
	knowledgeReloadMu sync.Mutex
)

// emailRetryCounts tracks how many times each inbound Message-Id has been
// left unseen for retry. Process-local — survives across poll ticks but
// resets on container restart, which is intentional: a restart implies the
// operator may have fixed something. Bounded by MAX_EMAIL_RETRIES.
var emailRetryCounts = map[string]int{}

// pollerAgent is the slice of *agents.Agent that the per-email poll loop needs.
// Narrowing the dependency to an interface (rather than the concrete *Agent)
// lets developers swap the agent for a fake when exercising processNewEmails,
// without rewriting the loop.
type pollerAgent interface {
	ClearHistory()
	SetHistory([]anthropic.MessageParam)
	History() []anthropic.MessageParam
	HandleMessage(context.Context, string, *services.Email, int) error
}

// pollerDeps collects every external function the per-email processing loop
// calls. Bundling them behind a struct of function values turns the loop into
// a thin orchestrator: a developer who wants to alter one piece of core
// behaviour (memory loading, bounce sending, access control, …) overrides a
// single field instead of editing — and re-testing — the whole loop. The
// zero-value-friendly defaults live in defaultPollerDeps; production code calls
// processNewEmails(parent, emails, defaultPollerDeps()).
type pollerDeps struct {
	agent                     pollerAgent
	accessControl             func(context.Context, services.Email) error
	resolveThreadRoot         func(*services.Email) (string, error)
	getMemory                 func(string) (*services.Memory, error)
	memoryFromReferences      func(context.Context, []string) (*services.Memory, error)
	rememberThreadIndex       func(string, string, string) error
	saveMemoryHistory         func(context.Context, []anthropic.MessageParam, string) error
	stripAttachmentBlobs      func([]anthropic.MessageParam) []anthropic.MessageParam
	isAdminReviewThread       func(string) bool
	handleAdminReviewFollowUp func(context.Context, *services.Email, string) (bool, error)
	leaveUnreadForAdminReview func(context.Context, string, string)
	markSeen                  func(context.Context, []string) error
	sendBounce                func(context.Context, services.Email, string) error
	addToSenders              func([]string, string) []string
	handleAutoReply           func(context.Context, services.Email)
}

// defaultPollerDeps wires the abstraction to this codebase's real
// implementations. Admin-review handling is not implemented in this build, so
// those three fields point at local stubs (isAdminReviewThread always reports
// false, the follow-up hook is a no-op, and leaveUnreadForAdminReview just
// logs); see their definitions below.
func defaultPollerDeps() pollerDeps {
	return pollerDeps{
		agent:                     agent,
		accessControl:             agents.AccessControl,
		resolveThreadRoot:         services.ResolveThreadRoot,
		getMemory:                 services.GetMemory,
		memoryFromReferences:      services.MemoryFromReferences,
		rememberThreadIndex:       services.RememberThreadIndex,
		saveMemoryHistory:         services.SaveMemoryHistory,
		stripAttachmentBlobs:      agents.StripAttachmentBlobs,
		isAdminReviewThread:       isAdminReviewThread,
		handleAdminReviewFollowUp: handleAdminReviewFollowUp,
		leaveUnreadForAdminReview: leaveUnreadForAdminReview,
		markSeen:                  services.MarkSeen,
		sendBounce:                services.SendBounce,
		addToSenders:              services.AddToSenders,
		handleAutoReply:           handleAutoReply,
	}
}

func main() {
	log.SetFlags(log.Lshortfile)

	dotenv.LoadEnvFile()
	cfg, cfgErr := config.Load(agentsConfigPath())
	if cfgErr != nil {
		log.Fatalf("error loading agents.yaml: %v", cfgErr)
	}
	if cfg == nil {
		log.Fatalf("agents.yaml not found at %s — see CONFIGURATION.md", agentsConfigPath())
	}
	agentsCfg = cfg
	config.Apply(cfg)
	warnUncoveredExternals(cfg)
	services.InitDataPaths(config.InContainer)
	log.Printf("Agent, %s (%s), is alive!\n", config.AgentName, config.AgentEmail)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	initAgent()

	// SIGHUP forces an immediate knowledge-base reload — near-instant
	// propagation of configurator edits without waiting for the next poll.
	// `docker compose kill -s HUP <service>` (or `kill -HUP <pid>` in dev).
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			forceReloadKnowledge()
		}
	}()

	// Periodic prune of the Outlook Thread-Index fallback map. The index
	// grows by one entry per Outlook-originated thread; pruning drops
	// entries whose memory file no longer exists (e.g. operator cleanup).
	pruneStop := make(chan struct{})
	defer close(pruneStop)
	services.StartThreadIndexPruner(pruneStop, 6*time.Hour)

	// Primary event loop
	process(ctx)

	for {
		select {
		case <-ticker.C:
			process(ctx)
		case <-ctx.Done():
			log.Println("shutting down:", ctx.Err())
			return
		}
	}
}

func process(parent context.Context) {
	// Pick up any knowledge-base edits since the last poll (no restart needed).
	reloadKnowledgeIfChanged()

	// Check the email inbox for new messages
	//log.Println("checking email inbox...")
	emails, getErr := services.GetNewEmails(parent)
	if getErr != nil {
		log.Println("error getting new emails:", getErr)
		return
	}

	if len(emails) > 0 {
		processNewEmails(parent, emails, defaultPollerDeps())
	} else {
		//log.Println("no new emails found")
	}

	// Snooze handling
	for {
		if parent.Err() != nil {
			return
		}
		snooze, err := services.NextSnoozed()
		if err != nil {
			log.Println("error getting next snoozed:", err)
			break
		}
		if snooze == nil {
			break
		}
		// Watchdog entries own their own lifecycle (delete or reschedule) and
		// must not go through AdvanceOrDeleteSnooze.
		if snooze.Watchdog {
			runWatchdog(parent, snooze)
			continue
		}
		log.Println("snooze:", snooze.MessageId)
		timeout := time.Duration(config.AgentTimeout) * time.Second
		ctx, cancel := context.WithTimeout(parent, timeout)
		snoozeErr := agent.HandleSnooze(ctx, *snooze, config.MaxTurns)
		cancel()
		if snoozeErr != nil {
			log.Println("error handling snooze:", snoozeErr)
			break // stop processing snoozes this cycle; retry next poll
		}
		// Only advance/delete after successful execution
		if advErr := services.AdvanceOrDeleteSnooze(parent, snooze); advErr != nil {
			log.Println("error advancing snooze:", advErr)
		}
	}
}

// waitingWatchdogMaxFires bounds how many times a stuck-task watchdog will
// nudge a coordinator before giving up. Each nudge re-invokes the LLM, so this
// caps the cost (and noise) of a permanently-dead delegate.
const waitingWatchdogMaxFires = 2

// runWatchdog handles a fired stuck-task watchdog (see
// services.ArmWaitingWatchdog). It re-checks the thread and either stands down
// (the awaited reply arrived and the task is no longer waiting), nudges the
// coordinator to follow up or fall back to answering the requester, or — once
// the nudge budget is exhausted — gives up: marks the thread errored and
// notifies the immediate upstream so a delegation chain unwinds instead of
// hanging. It fully owns the snooze entry lifecycle.
func runWatchdog(parent context.Context, snooze *services.Snooze) {
	mem, memErr := services.GetMemory(snooze.ThreadId)
	if memErr != nil && !errors.Is(memErr, services.ErrMemoryNotFound) {
		log.Printf("watchdog: error reading memory for %s: %s — rescheduling", snooze.ThreadId, memErr)
		rescheduleWatchdog(parent, snooze)
		return
	}
	// Stand down: the reply arrived (or the task otherwise resolved).
	if mem == nil || mem.Status != services.MemoryStatus_Waiting {
		log.Printf("watchdog: thread %s no longer waiting — standing down", snooze.ThreadId)
		if delErr := snooze.DeleteSnooze(); delErr != nil {
			log.Printf("watchdog: error deleting stood-down entry: %s", delErr)
		}
		return
	}

	// Budget exhausted: give up and unwind.
	if snooze.Fires >= waitingWatchdogMaxFires {
		log.Printf("watchdog: thread %s still waiting after %d nudges — giving up", snooze.ThreadId, snooze.Fires)
		notice := fmt.Sprintf(
			"⚠️ Agent %s waited on a delegated sub-task for this thread and received no reply "+
				"after %d follow-ups. Marking the task as failed so it does not hang indefinitely. "+
				"Please review and resend if the work is still needed.\n",
			config.AgentName, snooze.Fires,
		)
		if emails, e := services.GetEmails(parent, []string{snooze.MessageId}); e == nil && len(emails) > 0 {
			if bErr := services.SendBounce(parent, emails[0], notice); bErr != nil {
				log.Printf("watchdog: error sending give-up notice: %s", bErr)
			}
		}
		if sErr := services.SetMemoryStatus(parent, services.MemoryStatus_Error, snooze.MessageId); sErr != nil {
			log.Printf("watchdog: error marking thread errored: %s", sErr)
		}
		if delErr := snooze.DeleteSnooze(); delErr != nil {
			log.Printf("watchdog: error deleting exhausted entry: %s", delErr)
		}
		return
	}

	// Nudge. Reschedule first (incremented, future deadline) so the entry
	// survives this turn and won't re-fire this cycle. If the coordinator
	// re-arms waiting during the nudge, ArmWaitingWatchdog overwrites this with
	// a fresh Fires=0 entry — progress resets the budget.
	snooze.Fires++
	rescheduleWatchdog(parent, snooze)

	agent.ClearHistory()
	agent.SetHistory(mem.History)
	nudge := services.Snooze{
		MessageId: snooze.MessageId,
		Description: "You previously set this task to 'waiting' and are still waiting on a coworker who " +
			"has not replied. Do NOT simply wait again. Either (a) send a brief follow-up to the coworker " +
			"you delegated to, or (b) if you already have enough to proceed, reply to the original requester " +
			"now with the best result you have (noting any missing piece) and mark the task complete. Do not " +
			"re-delegate the whole task from scratch.",
	}
	timeout := time.Duration(config.AgentTimeout) * time.Second
	ctx, cancel := context.WithTimeout(parent, timeout)
	hErr := agent.HandleSnooze(ctx, nudge, config.MaxTurns)
	cancel()
	if hErr != nil {
		log.Printf("watchdog: error nudging coordinator for %s: %s", snooze.ThreadId, hErr)
	}
	// Persist whatever the nudge produced (mirrors the inbound-email path).
	if sErr := services.SaveMemoryHistory(parent, agents.StripAttachmentBlobs(agent.History()), snooze.MessageId); sErr != nil {
		log.Printf("watchdog: error saving history for %s: %s", snooze.ThreadId, sErr)
	}
	// If the task resolved during the nudge (and wasn't re-armed as waiting),
	// drop the rescheduled entry so it doesn't wake again needlessly.
	if m2, _ := services.GetMemory(snooze.ThreadId); m2 == nil || m2.Status != services.MemoryStatus_Waiting {
		if delErr := snooze.DeleteSnooze(); delErr != nil {
			log.Printf("watchdog: error clearing resolved entry: %s", delErr)
		}
	}
}

// rescheduleWatchdog pushes a watchdog's next firing to now + the configured
// deadline and persists it (preserving Fires/Watchdog).
func rescheduleWatchdog(parent context.Context, snooze *services.Snooze) {
	snooze.UnSnooze = time.Now().Add(time.Duration(config.WaitingWatchdogMinutes) * time.Minute)
	if err := snooze.SaveSnooze(parent); err != nil {
		log.Printf("watchdog: error rescheduling %s: %s", snooze.ThreadId, err)
	}
}

// processNewEmails runs the per-email poll loop against a pollerDeps bundle.
// process() calls it with defaultPollerDeps(); the indirection lets each piece
// of core behaviour be overridden in isolation. A nil-agent deps falls back to
// defaults so a zero-value pollerDeps still works.
func processNewEmails(parent context.Context, emails []services.Email, deps pollerDeps) {
	if deps.agent == nil {
		deps = defaultPollerDeps()
	}
	if deps.handleAdminReviewFollowUp == nil {
		deps.handleAdminReviewFollowUp = handleAdminReviewFollowUp
	}
	var processed []string // Message-Ids to mark as seen
	for _, email := range emails {
		fmt.Println("NEW EMAIL:", email.MessageId)
		email.Senders = deps.addToSenders(email.Senders, email.From)

		// Auto-replies (bounces, out-of-office) MUST NOT reach the LLM.
		// Feeding them back causes the LLM to retry the task and create
		// a ping-pong loop. Handle them out-of-band and skip the LLM.
		if isAutoReply(email.AutoSubmitted) {
			deps.handleAutoReply(parent, email)
			if markErr := deps.markSeen(parent, []string{email.MessageId}); markErr != nil {
				log.Println("error marking auto-reply as seen:", markErr)
			}
			continue
		}

		// ACCESS CONTROL
		if aclErr := deps.accessControl(parent, email); aclErr != nil {
			log.Println("error checking access control:", aclErr)
			bounceMsg := fmt.Sprintf("🔒 Agent %s is not allowed to receive this email: %s\n", config.AgentName, aclErr.Error())
			bounceErr := deps.sendBounce(parent, email, bounceMsg)
			if bounceErr != nil {
				log.Println("error sending bounce:", bounceErr)
			}
			continue
		}

		// DEMO MODE — no LLM key configured. Reply with a templated notice
		// instead of invoking the LLM (which can't produce an answer without
		// a key). This is the "Hello World" moment for docker-compose-demo.yml:
		// the agent provably received the mail and replied, and the notice
		// tells the user how to unlock real responses. Mirrors the
		// out-of-band, never-call-the-LLM pattern used for auto-replies.
		if config.LLMKeyMissing {
			log.Printf("demo mode (no LLM key): replying with setup notice to %s", email.From)
			if replyErr := deps.sendBounce(parent, email, demoNoKeyNotice()); replyErr != nil {
				log.Println("error sending demo notice:", replyErr)
				// SendBounce marks seen only on success; mark manually so a
				// down SMTP server can't wedge us in an infinite retry loop.
				if markErr := deps.markSeen(parent, []string{email.MessageId}); markErr != nil {
					log.Println("error marking demo email as seen:", markErr)
				}
			}
			continue
		}
		var history []anthropic.MessageParam
		// Resolve the thread root with Outlook Thread-Index fallback so
		// Exchange-rewritten References don't orphan the inbound from
		// its existing memory file. Falls back to email.GetThreadRoot()
		// when no fingerprint matches, preserving behaviour for
		// non-MS clients.
		threadRoot, resolveErr := deps.resolveThreadRoot(&email)
		if resolveErr != nil {
			log.Println("error resolving thread root:", resolveErr)
			continue
		}
		// Check the memory queue for each new message
		// Based on the memory queue - load context for the agent
		memory, memoryErr := deps.getMemory(threadRoot)
		if memoryErr != nil && !errors.Is(memoryErr, services.ErrMemoryNotFound) {
			log.Println("error getting memory:", memoryErr)
			continue
		}
		if memory != nil {
			// Admin-review threads are escalated to a human and must not be
			// fed back to the LLM. This build stubs the flow (status is never
			// set), but the branch is wired through deps so a developer can
			// enable it by overriding the admin-review fields.
			if memory.Status == services.MemoryStatus_AdminReview {
				if handled, hookErr := deps.handleAdminReviewFollowUp(parent, &email, threadRoot); hookErr != nil {
					log.Println("error handling admin-review follow-up:", hookErr)
					deps.leaveUnreadForAdminReview(parent, email.MessageId, threadRoot)
				} else if handled {
					processed = append(processed, email.MessageId)
				} else {
					deps.leaveUnreadForAdminReview(parent, email.MessageId, threadRoot)
				}
				continue
			}
			history = memory.History
			memory.ClearStatus()
		} else {
			// Fill memory from the message thread
			memory, memoryErr = deps.memoryFromReferences(parent, email.References)
			if memoryErr != nil {
				log.Println("error building memory from references:", memoryErr)
				continue
			}
			// No email history to use, create a new memory
			if memory == nil {
				memory = &services.Memory{
					ThreadRoot: threadRoot,
				}
			}
		}

		// Record the Outlook fingerprint for this thread so future
		// replies with broken References can still find their way home.
		// Subject changes mid-thread will register a new index entry,
		// which is the right behaviour — a renamed conversation is
		// only safe to merge when both signals agree.
		if email.ThreadIndexConvID != "" {
			subj := email.ThreadTopic
			if subj == "" {
				subj = email.Subject
			}
			if rememberErr := deps.rememberThreadIndex(email.ThreadIndexConvID, subj, memory.ThreadRoot); rememberErr != nil {
				log.Printf("warning: thread index remember failed: %v", rememberErr)
			}
		}

		saveErr := deps.saveMemoryHistory(parent, memory.History, email.MessageId)
		if saveErr != nil {
			log.Println("error saving memory history:", saveErr)
			continue
		}

		// handle message
		deps.agent.ClearHistory()
		deps.agent.SetHistory(history)
		// Need enough time for MCP
		timeout := time.Duration(config.AgentTimeout) * time.Second
		ctx, cancel := context.WithTimeout(parent, timeout)
		err := deps.agent.HandleMessage(ctx, "", &email, config.MaxTurns)
		cancel()
		if err != nil {
			log.Println("error handling message:", err)
		}
		// Always save history — even on timeout/error the agent may have
		// completed useful work (tool calls, partial results) that we want
		// to preserve for the next attempt.
		//
		// StripAttachmentBlobs removes base64 PDF/image payloads before
		// persisting. The model already processed them in this run; the
		// EmailSummary block (filename, size) plus subsequent assistant
		// turns carry forward whatever was learned. Keeping the bytes on
		// disk just inflates memory files and re-bloats reload for
		// follow-up emails in the thread.
		err2 := deps.saveMemoryHistory(parent, deps.stripAttachmentBlobs(deps.agent.History()), email.MessageId)
		if err2 != nil {
			log.Println("error saving memory history:", err2)
		}
		if deps.isAdminReviewThread(threadRoot) {
			delete(emailRetryCounts, email.MessageId)
			deps.leaveUnreadForAdminReview(parent, email.MessageId, threadRoot)
			continue
		}
		if err != nil {
			// Max-turns is not retryable — the agent exhausted its budget
			// and a retry will just burn another one, creating an infinite
			// loop (especially for coordinators waiting on peers). Notify
			// the sender (so the delegation chain can unwind via
			// handleAutoReply instead of hanging) and mark seen.
			if errors.Is(err, agents.ErrMaxTurns) {
				delete(emailRetryCounts, email.MessageId)
				// If the agent already called set_task_status=complete, the
				// user has the real answer (typically from report_tool a
				// turn or two earlier). Suppress the failure notice — it
				// only confuses recipients who just got a successful reply.
				if mem, memErr := deps.getMemory(email.GetThreadRoot()); memErr == nil && mem != nil && mem.Status == services.MemoryStatus_Complete {
					log.Printf("max turns exhausted for email %s but task already marked complete — suppressing notice", email.MessageId)
					processed = append(processed, email.MessageId)
					continue
				}
				log.Printf("max turns exhausted for email %s — notifying sender and marking seen", email.MessageId)
				notice := fmt.Sprintf(
					"⚠️ Agent %s exhausted its turn budget (%d turns) while processing this task and could not complete it. Partial progress has been preserved in the thread history, but no final answer was produced.\n",
					config.AgentName, config.MaxTurns,
				)
				if bounceErr := deps.sendBounce(parent, email, notice); bounceErr != nil {
					log.Println("error sending max-turns notice:", bounceErr)
					// SendBounce marks seen on success; on failure mark
					// manually so we still break the loop.
					processed = append(processed, email.MessageId)
				}
				continue
			}
			// Don't mark as seen — transient errors will be retried next poll.
			// But cap retries so a deterministic-fail message can't loop
			// forever burning tokens (e.g. truncated tool calls, poisoned
			// memory, provider 5xx that never clears).
			emailRetryCounts[email.MessageId]++
			attempt := emailRetryCounts[email.MessageId]
			if attempt >= config.MaxEmailRetries {
				log.Printf("email %s exceeded retry budget (%d) — bouncing and marking seen",
					email.MessageId, config.MaxEmailRetries)
				notice := fmt.Sprintf(
					"⚠️ Agent %s could not process this email after %d attempts (last error: %v). "+
						"Marking it as seen to break the retry loop. Please review the agent logs "+
						"and resend if the underlying issue has been addressed.\n",
					config.AgentName, attempt, err,
				)
				if bounceErr := deps.sendBounce(parent, email, notice); bounceErr != nil {
					log.Println("error sending retry-budget notice:", bounceErr)
					// SendBounce marks seen on success; on failure mark
					// manually so we still break the loop.
					processed = append(processed, email.MessageId)
				}
				delete(emailRetryCounts, email.MessageId)
				continue
			}
			log.Printf("leaving email %s as unseen for retry (attempt %d/%d)",
				email.MessageId, attempt, config.MaxEmailRetries)
			continue
		}
		delete(emailRetryCounts, email.MessageId)
		processed = append(processed, email.MessageId)
	}
	// Mark successfully processed emails as seen
	if len(processed) > 0 {
		if markErr := deps.markSeen(parent, processed); markErr != nil {
			log.Println("error marking emails as seen:", markErr)
		}
	}
}

// --- Admin-review stubs ---------------------------------------------------
//
// The admin-review escalation flow (a thread parked for human review instead of
// being answered by the LLM) is part of the pollerDeps abstraction but is not
// implemented in this build. These stubs satisfy the abstraction so the loop
// compiles and stays close to the origin codebase; wire them to real
// implementations (and start setting services.MemoryStatus_AdminReview) to
// enable the flow.

// isAdminReviewThread reports whether a thread is parked for admin review.
// Stub: this build never sets MemoryStatus_AdminReview, so it always returns
// false. The check is kept structurally identical to the origin so enabling the
// flow is a one-function change.
func isAdminReviewThread(threadRoot string) bool {
	memory, err := services.GetMemory(threadRoot)
	if err != nil {
		if !errors.Is(err, services.ErrMemoryNotFound) {
			log.Printf("warning: could not inspect admin-review status for thread %s: %v", threadRoot, err)
		}
		return false
	}
	return memory != nil && memory.Status == services.MemoryStatus_AdminReview
}

// handleAdminReviewFollowUp processes a follow-up on an admin-review thread.
// Stub: the flow is not implemented, so it reports "not handled" with no error,
// which leaves the email for the caller's default (unread) handling.
func handleAdminReviewFollowUp(ctx context.Context, email *services.Email, threadRoot string) (bool, error) {
	return false, nil
}

// leaveUnreadForAdminReview leaves an escalated email unread so a human can act
// on it. Stub: MarkUnread is not implemented in this build, so it only logs.
func leaveUnreadForAdminReview(ctx context.Context, messageId, threadRoot string) {
	log.Printf("email %s in thread %s would be escalated for admin review, but admin review is not implemented in this build", messageId, threadRoot)
}

// demoNoKeyNotice is the templated reply sent when the agent is running without
// an LLM API key (config.LLMKeyMissing). It confirms the round-trip works and
// tells the user how to enable real responses. Used by the no-cost demo.
func demoNoKeyNotice() string {
	return "✅ Kikubot is alive — your email reached the agent and it replied. 🎉\n" +
		"\n" +
		"This is the no-cost demo running WITHOUT an LLM API key, so I can't generate\n" +
		"a real answer yet. To unlock full agent responses:\n" +
		"\n" +
		"  1. Get an Anthropic API key: https://console.anthropic.com/settings/keys\n" +
		"  2. Add it to configs/demo/secrets.env   →   ANTHROPIC_API_KEY=sk-ant-...\n" +
		"  3. Restart the demo:   docker compose -f docker-compose-demo.yml up -d\n" +
		"\n" +
		"Prefer a free model? Set OPENROUTER_API_KEY instead and switch llm_provider\n" +
		"to openrouter in configs/demo/agents.yaml. See the README \"Try the demo\"\n" +
		"section for the full walkthrough.\n" +
		"\n" +
		"— Kiku (demo)\n"
}

// isAutoReply reports whether an RFC 3834 Auto-Submitted value indicates a
// machine-generated message (anything other than unset or "no").
func isAutoReply(v string) bool {
	v = strings.TrimSpace(strings.ToLower(v))
	return v != "" && v != "no"
}

// truncateForLog returns s trimmed of surrounding whitespace and clipped to
// at most max bytes, appending an ellipsis marker if clipped. Used to log a
// readable excerpt of DSN / auto-reply bodies without spamming the journal.
func truncateForLog(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 || len(s) <= max {
		return s
	}
	// Back up to a rune boundary so we don't split a multibyte codepoint.
	cut := max
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + " …[truncated]"
}

// addressOnly returns the bare mailbox (no display name, lower-cased) from
// a From/To header value. Falls back to the trimmed input on parse failure.
func addressOnly(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if addr, err := netmail.ParseAddress(s); err == nil {
		return strings.ToLower(addr.Address)
	}
	return strings.ToLower(s)
}

// handleAutoReply deals with an incoming bounce/OOO out-of-band: it notifies
// the immediate upstream caller (one hop back up the delegation chain), so
// the rejection bubbles toward the originating user one agent at a time.
// It also deletes any pending snooze for the thread and records a terminal
// entry in memory. The LLM is not invoked.
func handleAutoReply(ctx context.Context, email services.Email) {
	log.Printf("auto-reply received on %s from %s (Auto-Submitted: %s) — bypassing LLM; propagating upstream and clearing pending state",
		email.MessageId, email.From, email.AutoSubmitted)
	if excerpt := truncateForLog(email.Content, 500); excerpt != "" {
		log.Printf("auto-reply body excerpt:\n%s", excerpt)
	}

	rootId, resolveErr := services.ResolveThreadRoot(&email)
	if resolveErr != nil {
		log.Printf("auto-reply: thread root resolve error: %v; falling back to References-only", resolveErr)
		rootId = email.GetThreadRoot()
	}

	// Walk References newest→oldest to find the most recent message that
	// lives in this agent's INBOX — that's the email we received from our
	// immediate upstream caller. In a Kiku→Gamma→Beta chain, Gamma finds
	// Kiku's delegation; Kiku (on the next hop) finds the user's original.
	var upstream *services.Email
	for i := len(email.References) - 1; i >= 0; i-- {
		refId := email.References[i]
		if refId == "" {
			continue
		}
		hits, err := services.GetEmails(ctx, []string{refId})
		if err != nil {
			log.Printf("auto-reply: error looking up reference %s: %v", refId, err)
			continue
		}
		if len(hits) > 0 {
			upstream = &hits[0]
			break
		}
	}

	// Sent-folder fallback: when a DSN from our own MTA bounces a message we
	// sent, its References point at the outbound — which lives in Sent, not
	// INBOX. Look up each reference in Sent, then walk THAT message's
	// References/In-Reply-To back into INBOX to find the real upstream.
	if upstream == nil {
		for i := len(email.References) - 1; i >= 0; i-- {
			refId := email.References[i]
			if refId == "" {
				continue
			}
			sent, err := services.GetSentEmail(ctx, refId)
			if err != nil {
				log.Printf("auto-reply: error looking up reference %s in Sent: %v", refId, err)
				continue
			}
			if sent == nil {
				continue
			}
			log.Printf("auto-reply: reference %s matched our own outbound in Sent; walking its thread for upstream", refId)
			// Candidate parents of the outbound, newest→oldest, with
			// In-Reply-To preferred.
			var candidates []string
			if sent.InReplyTo != "" {
				candidates = append(candidates, sent.InReplyTo)
			}
			for j := len(sent.References) - 1; j >= 0; j-- {
				candidates = append(candidates, sent.References[j])
			}
			for _, cand := range candidates {
				if cand == "" {
					continue
				}
				hits, err := services.GetEmails(ctx, []string{cand})
				if err != nil {
					log.Printf("auto-reply: error looking up outbound parent %s: %v", cand, err)
					continue
				}
				if len(hits) > 0 {
					upstream = &hits[0]
					break
				}
			}
			if upstream != nil {
				break
			}
		}
	}

	if upstream == nil {
		log.Printf("auto-reply: no upstream reference resolvable in INBOX or Sent (thread %s); nothing to notify", rootId)
	} else if strings.EqualFold(addressOnly(email.From), addressOnly(upstream.From)) {
		// OOO-loop guard: the auto-reply came from the same party we'd
		// notify. Don't ricochet it back.
		log.Printf("auto-reply: originator %s matches upstream %s; skipping notify to avoid loop",
			email.From, upstream.From)
	} else {
		subj := upstream.Subject
		if !strings.HasPrefix(strings.ToLower(subj), "re:") {
			subj = "Re: " + subj
		}
		content := fmt.Sprintf(
			"I was unable to complete your request. A downstream coworker declined part of the task with the following response:\n\n%s",
			strings.TrimSpace(email.Content),
		)
		notify := services.Email{
			To:            []string{upstream.From},
			Subject:       subj,
			Content:       content,
			InReplyTo:     upstream.MessageId,
			References:    append(upstream.References, upstream.MessageId),
			Date:          time.Now(),
			AutoSubmitted: "auto-replied",
		}
		if err := services.SendEmail(ctx, notify); err != nil {
			log.Println("error notifying upstream of auto-reply:", err)
		}
	}

	// Delete any snooze tied to this thread — the task can't complete.
	if rootId != "" {
		if snooze, err := services.FindSnoozeByThread(rootId); err == nil && snooze != nil {
			if delErr := snooze.DeleteSnooze(); delErr != nil {
				log.Println("error deleting snooze for aborted thread:", delErr)
			} else {
				log.Printf("cleared pending snooze for thread %s", rootId)
			}
		}
	}

	// Record a terminal note in memory so if the user follows up on this
	// thread, the LLM sees the prior failure instead of retrying blindly.
	if rootId != "" {
		memory, err := services.GetMemory(rootId)
		if err != nil && !errors.Is(err, services.ErrMemoryNotFound) {
			log.Println("error reading memory for auto-reply note:", err)
			return
		}
		if memory == nil {
			memory = &services.Memory{ThreadRoot: rootId}
		}
		note := anthropic.NewUserMessage(anthropic.NewTextBlock(
			fmt.Sprintf("[SYSTEM] Task aborted — coworker returned an auto-reply rejecting the request:\n\n%s",
				strings.TrimSpace(email.Content)),
		))
		memory.AddMessage([]anthropic.MessageParam{note})
		memory.ClearStatus()
		if saveErr := memory.SaveMemory(); saveErr != nil {
			log.Println("error saving memory with auto-reply note:", saveErr)
		}
	}
}

func initAgent() {
	// Setup agentTools
	agentTools := tools.CoreTools()

	cfg := agentsCfg
	if cfg != nil {
		if agentDef := cfg.FindAgent(config.AgentEmail); agentDef != nil {
			for _, key := range agentDef.Tools {
				t, ok := tools.LookupTools(key)
				if !ok {
					log.Printf("warning: unknown tool key %q for %s", key, config.AgentEmail)
					continue
				}
				agentTools = append(agentTools, t...)
			}
		}
	}

	// Deduplicate scripts by name
	agentTools = dedupTools(agentTools)
	if disabled := disabledToolNames(cfg, config.AgentEmail); len(disabled) > 0 {
		agentTools = filterDisabledTools(agentTools, disabled)
	}
	//for _, tool := range agentTools {
	//	log.Println("loaded tool:", tool.Name)
	//}

	var coworkerClause string
	if cfg != nil {
		coworkerClause = formatCoworkers(cfg.Peers(config.AgentEmail))
	}

	var system string

	if config.SysPrompt == "" {
		if coworkerClause != "" {
			// With coworkers
			system = fmt.Sprintf("You are a helpful agent that serves two groups — users and coworkers. Your role is to resolve tasks submitted by users, drawing on your coworkers below as needed.\n\n%s",
				coworkerClause)
		} else {
			// Alone
			system = "You are a helpful agent. Your job is to resolve tasks sent to you by users."
		}
	} else {
		// Custom agent system prompt
		system = config.SysPrompt
		if strings.Contains(strings.ToLower(system), "{{coworkers}}") {
			if coworkerClause != "" {
				re := regexp.MustCompile(`(?i)\{\{coworkers?}}`)
				system = re.ReplaceAllString(system, coworkerClause)
			} else {
				system = strings.ReplaceAll(system, "{{coworkers}}", "")
			}
		}
	}

	// Derive the agent key from the email local part (e.g.
	// "kiku@agents.mxhero.com" → "kiku") and stash the knowledge-free base
	// prompt so the knowledge block can be re-appended on later reloads.
	knowledgeKey = strings.ToLower(config.AgentEmail)
	if i := strings.Index(knowledgeKey, "@"); i > 0 {
		knowledgeKey = knowledgeKey[:i]
	}
	baseSystem = system

	agent = agents.NewAgent(agents.AgentConfig{
		ID:     fmt.Sprintf("%d", time.Now().Unix()),
		Role:   "worker",
		System: system,
	}, agentTools)

	// Load the knowledge base into the system prompt and record its mtime so
	// process() can hot-reload on edits without a restart.
	applyKnowledge()

	log.Printf("Agent %s initialized with %d scripts", config.AgentName, len(agentTools))
	//log.Printf("Agent initialized with system prompt:\n\n%s\n\n", system)
}

func dedupTools(toolList []tools.ToolDefinition) []tools.ToolDefinition {
	toolMap := make(map[string]tools.ToolDefinition)
	for _, tool := range toolList {
		toolMap[tool.Name] = tool
	}
	agentTools := make([]tools.ToolDefinition, 0, len(toolMap))
	for _, tool := range toolMap {
		agentTools = append(agentTools, tool)
	}
	return agentTools
}

// disabledToolNames collects the set of tool names to strip from an agent's
// toolset, merging common.disabled_tools with the agent's own disabled_tools.
// Names are matched against ToolDefinition.Name (e.g. "message_tool"), so this
// can remove normally-core tools as well as keyed ones.
func disabledToolNames(cfg *config.AgentsConfig, agentEmail string) map[string]bool {
	if cfg == nil {
		return nil
	}
	disabled := make(map[string]bool)
	for _, name := range cfg.Common.DisabledTools {
		name = strings.TrimSpace(name)
		if name != "" {
			disabled[name] = true
		}
	}
	if agentDef := cfg.FindAgent(agentEmail); agentDef != nil {
		for _, name := range agentDef.DisabledTools {
			name = strings.TrimSpace(name)
			if name != "" {
				disabled[name] = true
			}
		}
	}
	return disabled
}

// filterDisabledTools drops any tool whose Name is in the disabled set.
func filterDisabledTools(toolList []tools.ToolDefinition, disabled map[string]bool) []tools.ToolDefinition {
	if len(disabled) == 0 {
		return toolList
	}
	filtered := make([]tools.ToolDefinition, 0, len(toolList))
	for _, tool := range toolList {
		if disabled[tool.Name] {
			log.Printf("tool %q disabled by agents.yaml", tool.Name)
			continue
		}
		filtered = append(filtered, tool)
	}
	return filtered
}

// knowledgeBaseDir returns the path to the knowledge directory next to the
// running binary. In production (Docker) the binary and knowledge/ live in
// the same /app directory. During development it falls back to the directory
// containing the source file.
func knowledgeBaseDir() string {
	// First try next to the executable (production / Docker).
	exe, err := os.Executable()
	if err == nil {
		dir := filepath.Join(filepath.Dir(exe), "knowledge")
		if info, statErr := os.Stat(dir); statErr == nil && info.IsDir() {
			return dir
		}
	}
	// Fallback: next to this source file (go run / development).
	_, srcFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(srcFile), "knowledge")
}

// loadKnowledge reads all .md files from knowledge/common/ and
// knowledge/<agentKey>/ directories on disk, concatenates them, and returns a
// block suitable for appending to the system prompt. Files are sorted by name
// so you can control ordering with numeric prefixes (01_topic.md, 02_topic.md, …).
func loadKnowledge(agentKey string) string {
	baseDir := knowledgeBaseDir()

	var sections []string

	dirs := []string{"common"}
	if agentKey != "" {
		dirs = append(dirs, agentKey)
	}

	for _, dir := range dirs {
		dirPath := filepath.Join(baseDir, dir)
		entries, err := os.ReadDir(dirPath)
		if err != nil {
			// Directory doesn't exist — that's fine, skip it.
			continue
		}

		// Collect and sort .md filenames
		var mdFiles []string
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
				mdFiles = append(mdFiles, e.Name())
			}
		}
		sort.Strings(mdFiles)

		for _, name := range mdFiles {
			data, readErr := os.ReadFile(filepath.Join(dirPath, name))
			if readErr != nil {
				log.Printf("warning: could not read knowledge file %s/%s: %v", dirPath, name, readErr)
				continue
			}
			content := strings.TrimSpace(string(data))
			if content != "" {
				sections = append(sections, content)
			}
		}
	}

	if len(sections) == 0 {
		return ""
	}
	return strings.Join(sections, "\n\n---\n\n")
}

// applyKnowledge rebuilds the agent's system prompt from the cached base prompt
// plus the current on-disk knowledge files, and records the newest knowledge
// mtime so subsequent reloads can detect changes.
func applyKnowledge() {
	system := baseSystem
	if kb := loadKnowledge(knowledgeKey); kb != "" {
		system += kb
	}
	agent.SetSystem(system)
	knowledgeModTime = knowledgeMTime(knowledgeKey)
}

// knowledgeMTime returns the newest modification time across the agent's
// knowledge directories (common + <knowledgeKey>), including the directories
// themselves so that file additions and deletions register too. A zero time
// means no knowledge directory exists.
func knowledgeMTime(agentKey string) time.Time {
	baseDir := knowledgeBaseDir()
	dirs := []string{"common"}
	if agentKey != "" {
		dirs = append(dirs, agentKey)
	}
	var newest time.Time
	for _, dir := range dirs {
		dirPath := filepath.Join(baseDir, dir)
		// The directory's own mtime changes when files are added or removed.
		if info, err := os.Stat(dirPath); err == nil {
			if info.ModTime().After(newest) {
				newest = info.ModTime()
			}
		}
		entries, err := os.ReadDir(dirPath)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			if info, err := e.Info(); err == nil && info.ModTime().After(newest) {
				newest = info.ModTime()
			}
		}
	}
	return newest
}

// reloadKnowledgeIfChanged re-reads the knowledge base and updates the agent's
// system prompt when any knowledge file has changed since the last load. It is
// cheap to call every poll: unless a file moved it only stats directories.
// This lets configurator edits take effect without a rebuild or restart.
func reloadKnowledgeIfChanged() {
	knowledgeReloadMu.Lock()
	defer knowledgeReloadMu.Unlock()
	mt := knowledgeMTime(knowledgeKey)
	if !mt.After(knowledgeModTime) {
		return
	}
	applyKnowledge()
	log.Printf("knowledge base reloaded (newest mtime %s)", mt.Format(time.RFC3339))
}

// forceReloadKnowledge unconditionally re-reads the knowledge base, regardless
// of mtime. Wired to SIGHUP so an operator (or the configurator after writing
// files) can trigger near-instant propagation instead of waiting for the next
// poll. Safe to call concurrently with the poll-driven reload.
func forceReloadKnowledge() {
	knowledgeReloadMu.Lock()
	defer knowledgeReloadMu.Unlock()
	applyKnowledge()
	log.Printf("knowledge base reloaded on SIGHUP")
}

// agentsConfigPath resolves the path to agents.yaml.
// Priority: AGENTS_CONFIG env var > next to executable (production) > next to source (dev).
func agentsConfigPath() string {
	if p := os.Getenv("AGENTS_CONFIG"); p != "" {
		return p
	}
	// Next to executable (production / Docker)
	exe, err := os.Executable()
	if err == nil {
		p := filepath.Join(filepath.Dir(exe), "agents.yaml")
		if _, statErr := os.Stat(p); statErr == nil {
			return p
		}
	}
	// Fallback: walk up from this source file (go run / make dev) to the
	// repo root and look in configs/. main.go lives at cmd/kikubot/main.go,
	// so the repo root is two levels up.
	_, srcFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(srcFile), "..", "..", "configs", "agents.yaml")
}

// warnUncoveredExternals logs a startup warning for each `external:` peer
// whose address (or domain) is not covered by this agent's whitelist. The
// roster is outbound-only: it lets this agent SEND to a partner, but the
// partner's REPLY is still gated by AccessControl. In whitelist mode an
// uncovered external can be delegated to, yet its reply bounces at the ACL —
// which routes through handleAutoReply (so no loop), but the task silently
// dead-ends. Only meaningful in whitelist mode; an empty whitelist means
// inbound isn't whitelist-gated, so there's nothing to warn about.
func warnUncoveredExternals(cfg *config.AgentsConfig) {
	if cfg == nil || len(config.Whitelist) == 0 {
		return
	}
	covers := func(email string) bool {
		email = strings.ToLower(strings.TrimSpace(email))
		if email == "" {
			return true // nothing to check
		}
		domain := ""
		if at := strings.LastIndex(email, "@"); at >= 0 {
			domain = email[at+1:]
		}
		for _, w := range config.Whitelist {
			w = strings.ToLower(strings.TrimSpace(w))
			if strings.Contains(w, "@") {
				if w == email {
					return true
				}
			} else if w != "" && w == domain {
				return true
			}
		}
		return false
	}
	for _, e := range cfg.External {
		if e.Email == "" {
			continue
		}
		if !covers(e.Email) {
			log.Printf("warning: external peer %s is reachable (under `external:`) but not "+
				"covered by this agent's whitelist — its replies will bounce at ACL. "+
				"Add %q (or its domain) to %s's whitelist for two-way collaboration.",
				e.Email, e.Email, config.AgentEmail)
		}
	}
}

func formatCoworkers(peers []config.PromptPeer) string {
	if len(peers) == 0 {
		return ""
	}
	coworkerJson, err := json.MarshalIndent(peers, " ", "   ")
	if err != nil {
		log.Fatal(err)
	}
	guidance := "Collaborate with your coworkers using the message_tool tool."
	for _, p := range peers {
		if p.Scope == "external" {
			guidance += " Peers marked \"scope\": \"external\" run on other machines or domains: " +
				"reach them the same way via message_tool, but expect higher latency, " +
				"unknown capabilities, and no shared memory with them."
			break
		}
	}
	return fmt.Sprintf("Your coworkers are:\n\n%s\n\n%s", coworkerJson, guidance)
}
