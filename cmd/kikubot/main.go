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
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

var agent *agents.Agent

// emailRetryCounts tracks how many times each inbound Message-Id has been
// left unseen for retry. Process-local — survives across poll ticks but
// resets on container restart, which is intentional: a restart implies the
// operator may have fixed something. Bounded by MAX_EMAIL_RETRIES.
var emailRetryCounts = map[string]int{}

func main() {
	log.SetFlags(log.Lshortfile)

	dotenv.LoadEnvFile()
	config.LoadEnv()
	services.InitDataPaths(config.InContainer)
	log.Printf("Agent, %s (%s), is alive!\n", config.AgentName, config.AgentEmail)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	initAgent()

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
	// Check the email inbox for new messages
	//log.Println("checking email inbox...")
	emails, getErr := services.GetNewEmails(parent)
	if getErr != nil {
		log.Println("error getting new emails:", getErr)
		return
	}

	if len(emails) > 0 {
		var processed []string // Message-Ids to mark as seen
		for _, email := range emails {
			log.Println("new email:", email.MessageId)
			email.Senders = services.AddToSenders(email.Senders, email.From)

			// Auto-replies (bounces, out-of-office) MUST NOT reach the LLM.
			// Feeding them back causes the LLM to retry the task and create
			// a ping-pong loop. Handle them out-of-band and skip the LLM.
			if isAutoReply(email.AutoSubmitted) {
				handleAutoReply(parent, email)
				if markErr := services.MarkSeen(parent, []string{email.MessageId}); markErr != nil {
					log.Println("error marking auto-reply as seen:", markErr)
				}
				continue
			}

			// ACCESS CONTROL
			if aclErr := agents.AccessControl(parent, email); aclErr != nil {
				log.Println("error checking access control:", aclErr)
				bounceMsg := fmt.Sprintf("🔒 Agent %s is not allowed to receive this email: %s\n", config.AgentName, aclErr.Error())
				bounceErr := services.SendBounce(parent, email, bounceMsg)
				if bounceErr != nil {
					log.Println("error sending bounce:", bounceErr)
				}
				continue
			}
			var history []anthropic.MessageParam
			// Check the memory queue for each new message
			// Based on the memory queue - load context for the agent
			memory, memoryErr := services.GetMemory(email.GetThreadRoot())
			if memoryErr != nil && !errors.Is(memoryErr, services.ErrMemoryNotFound) {
				log.Println("error getting memory:", memoryErr)
				continue
			}
			if memory != nil {
				history = memory.History
				memory.ClearStatus()
			} else {
				// Fill memory from the message thread
				memory, memoryErr = services.MemoryFromReferences(parent, email.References)
				if memoryErr != nil {
					log.Println("error building memory from references:", memoryErr)
					continue
				}
				// No email history to use, create a new memory
				if memory == nil {
					memory = &services.Memory{
						ThreadRoot: email.GetThreadRoot(),
					}
				}
			}

			saveErr := services.SaveMemoryHistory(parent, memory.History, email.MessageId)
			if saveErr != nil {
				log.Println("error saving memory history:", saveErr)
				continue
			}

			// handle message
			agent.ClearHistory()
			agent.SetHistory(history)
			// Need enough time for MCP
			timeout := time.Duration(config.AgentTimeout) * time.Second
			ctx, cancel := context.WithTimeout(parent, timeout)
			err := agent.HandleMessage(ctx, "", &email, config.MaxTurns)
			cancel()
			if err != nil {
				log.Println("error handling message:", err)
			}
			// Always save history — even on timeout/error the agent may have
			// completed useful work (tool calls, partial results) that we want
			// to preserve for the next attempt.
			err2 := services.SaveMemoryHistory(parent, agent.History(), email.MessageId)
			if err2 != nil {
				log.Println("error saving memory history:", err2)
			}
			if err != nil {
				// Max-turns is not retryable — the agent exhausted its budget
				// and a retry will just burn another one, creating an infinite
				// loop (especially for coordinators waiting on peers). Notify
				// the sender (so the delegation chain can unwind via
				// handleAutoReply instead of hanging) and mark seen.
				if errors.Is(err, agents.ErrMaxTurns) {
					delete(emailRetryCounts, email.MessageId)
					log.Printf("max turns exhausted for email %s — notifying sender and marking seen", email.MessageId)
					notice := fmt.Sprintf(
						"⚠️ Agent %s exhausted its turn budget (%d turns) while processing this task and could not complete it. Partial progress has been preserved in the thread history, but no final answer was produced.\n",
						config.AgentName, config.MaxTurns,
					)
					if bounceErr := services.SendBounce(parent, email, notice); bounceErr != nil {
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
					if bounceErr := services.SendBounce(parent, email, notice); bounceErr != nil {
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
			if markErr := services.MarkSeen(parent, processed); markErr != nil {
				log.Println("error marking emails as seen:", markErr)
			}
		}
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

	rootId := email.GetThreadRoot()

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

	// Load agent configuration from YAML
	cfg, cfgErr := config.Load(agentsConfigPath())
	if cfgErr != nil {
		log.Fatalf("error loading agents services: %v", cfgErr)
	}

	if cfg != nil {
		if agentDef := cfg.FindAgent(config.AgentEmail); agentDef != nil {
			for _, key := range agentDef.Tools {
				t, ok := tools.LookupTools(key)
				if !ok {
					log.Printf("warning: unknown tool key %q in services for %s", key, config.AgentEmail)
					continue
				}
				agentTools = append(agentTools, t...)
			}
		} else {
			log.Printf("warning: no agent services found for %s, using core scripts only", config.AgentEmail)
		}
		// Populate the known-agent-email set so trimHistory can distinguish
		// peer replies from human requests (the "anchor") when picking a
		// cutpoint. Self is included so that our own sent copies, if ever
		// reflected back in history, are classified as agent traffic too.
		config.AgentEmails = make(map[string]bool, len(cfg.Agents))
		for _, a := range cfg.Agents {
			if a.Email != "" {
				config.AgentEmails[strings.ToLower(a.Email)] = true
			}
		}
	} else {
		log.Println("warning: agents.yaml not found, using core scripts only")
	}

	// Deduplicate scripts by name
	agentTools = dedupTools(agentTools)

	var coworkerClause string
	if cfg != nil {
		coworkerClause = formatCoworkers(cfg.Coworkers(config.AgentEmail))
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

	// Load knowledge base files into the system prompt.
	// Derive the agent key from the email local part (e.g. "kiku@agents.mxhero.com" → "kiku").
	agentKey := strings.ToLower(config.AgentEmail)
	if i := strings.Index(agentKey, "@"); i > 0 {
		agentKey = agentKey[:i]
	}
	if kb := loadKnowledge(agentKey); kb != "" {
		system += kb
	}

	agent = agents.NewAgent(agents.AgentConfig{
		ID:     fmt.Sprintf("%d", time.Now().Unix()),
		Role:   "worker",
		System: system,
	}, agentTools)

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

func formatCoworkers(coworkers []config.AgentDef) string {
	if len(coworkers) == 0 {
		return ""
	}
	coworkerJson, err := json.MarshalIndent(coworkers, " ", "   ")
	if err != nil {
		log.Fatal(err)
	}
	return fmt.Sprintf("Your coworkers are:\n\n%s\n\n%s",
		coworkerJson,
		"Collaborate with your coworkers using the message_tool tool.",
	)
}
