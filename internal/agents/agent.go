package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"kikubot/internal/config"
	"kikubot/internal/provider"
	"kikubot/internal/services"
	"kikubot/internal/tools"
	"log"
	netmail "net/mail"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// ErrMaxTurns is returned when HandleMessage exhausts its turn budget without
// the model emitting an end_turn stop reason. The caller should treat this as
// non-retryable — re-running the same message will just burn another budget.
var ErrMaxTurns = errors.New("agent hit max turns")

// Agent is a single node in the cluster. It holds its own conversation
// history, tool set, and LLM provider.
type Agent struct {
	cfg          AgentConfig
	provider     provider.Provider
	tools        []tools.ToolDefinition
	toolIndex    map[string]tools.ToolDefinition
	history      []anthropic.MessageParam
	lastResponse string
}

func NewAgent(cfg AgentConfig, agentTools []tools.ToolDefinition) *Agent {
	idx := make(map[string]tools.ToolDefinition, len(agentTools))
	for _, t := range agentTools {
		idx[t.Name] = t
	}
	return &Agent{
		cfg:       cfg,
		provider:  provider.NewProvider(),
		tools:     agentTools,
		toolIndex: idx,
	}
}

func (a *Agent) RegisterTool(td tools.ToolDefinition) {
	a.tools = append(a.tools, td)
	a.toolIndex[td.Name] = td
}

func (a *Agent) SetHistory(history []anthropic.MessageParam) {
	a.history = history
}

func (a *Agent) History() []anthropic.MessageParam { return a.history }

func (a *Agent) ClearHistory() {
	a.history = make([]anthropic.MessageParam, 0)
	a.lastResponse = ""
}

func (a *Agent) LastResponse() string { return a.lastResponse }

// HandleMessage runs the agent's agentic loop for one inbound message.
// It returns any outbound messages produced by send_message tool calls.
func (a *Agent) HandleMessage(ctx context.Context, preSys string, email *services.Email, maxTurns int) error {
	// Append the incoming message to conversation history
	msg, msgErr := email.UserMessage()
	if msgErr != nil {
		return fmt.Errorf("error parsing email: %w", msgErr)
	}
	a.history = append(a.history, *msg)

	// Collect tool-contributed system instructions, splitting by stability.
	// StaticSystem entries are identical across all emails → go in the
	// cached prefix. Dynamic System() entries may depend on email state →
	// volatile, not cached.
	var staticToolInstructions []string
	var dynamicToolInstructions []string
	for _, toolItem := range a.tools {
		if toolItem.StaticSystem != "" {
			staticToolInstructions = append(staticToolInstructions, toolItem.StaticSystem)
		}
		if toolItem.System != nil {
			toolSystem, toolErr := toolItem.System(*email)
			if toolErr != nil {
				log.Printf("error parsing tool system: %s", toolErr)
				continue
			}
			if toolSystem != "" {
				dynamicToolInstructions = append(dynamicToolInstructions, toolSystem)
			}
		}
	}

	// Stable system: base agent prompt + any static tool instructions.
	// Identical across all emails for this agent.
	var stable strings.Builder
	if preSys != "" {
		stable.WriteString(preSys)
		stable.WriteString("\n\n")
	}
	stable.WriteString(a.cfg.System)
	if len(staticToolInstructions) > 0 {
		stable.WriteString("\n\n# Tool Instructions\n\n")
		stable.WriteString(strings.Join(staticToolInstructions, "\n\n"))
	}
	stableSystem := stable.String()

	// Volatile system: per-email tool instructions + Message-Id reference.
	// Volatile system: per-email tool instructions. Message-Id and X-Senders
	// are NOT exposed to the LLM — scripts recover them from ctx via
	// services.SourceEmail(), which the LLM cannot alter.
	var volatile strings.Builder
	if len(dynamicToolInstructions) > 0 {
		volatile.WriteString("# Per-Email Tool Instructions\n\n")
		volatile.WriteString(strings.Join(dynamicToolInstructions, "\n\n"))
	}
	volatileSystem := volatile.String()

	//log.Printf("SYSTEM MESSAGE (stable, %d chars ≈ %d tokens):\n%s", len(stableSystem), len(stableSystem)/4, stableSystem)
	if volatileSystem != "" {
		log.Printf("SYSTEM MESSAGE (volatile, %d chars ≈ %d tokens):\n%s",
			len(volatileSystem), len(volatileSystem)/4, volatileSystem)
	}

	// Build provider-specific tool params from our tool definitions
	toolDefs := toToolDefs(a.tools)
	sdkTools, serverToolNames := a.provider.BuildTools(toolDefs, config.LlmModel)
	serverToolSet := make(map[string]bool, len(serverToolNames))
	for _, name := range serverToolNames {
		serverToolSet[name] = true
	}

	// Stash the inbound email on the context so scripts can recover the trusted
	// origin (for ACL enforcement, authoritative Message-Id / X-Senders, etc.)
	// without depending on LLM-provided headers.
	ctx = services.WithSourceEmail(ctx, email)

	for turn := 0; turn < maxTurns; turn++ {
		// Check context before making the API call so we don't fire a
		// request we already know will fail.
		if ctx.Err() != nil {
			return fmt.Errorf("context cancelled before turn %d: %w", turn, ctx.Err())
		}

		// Trim history if it exceeds the configured character limit.
		if config.MaxHistoryChars > 0 {
			a.history = trimHistory(a.history, config.MaxHistoryChars)
		}

		params := provider.MessageParams{
			Model:          config.LlmModel,
			MaxTokens:      config.MaxTokens,
			SystemStable:   stableSystem,
			SystemVolatile: volatileSystem,
			Messages:       a.history,
			Tools:          sdkTools,
		}

		resp, err := a.provider.CreateMessage(ctx, params)
		if err != nil {
			return fmt.Errorf("api call failed: %w", err)
		}

		// Sanitize any tool_use blocks whose Input was truncated by the
		// model's output-token limit. The raw bytes are replaced with "{}"
		// so they survive marshalling into history; the original IDs are
		// returned so we can short-circuit Execute and emit a specific
		// guidance result instead of dispatching invalid JSON to the tool.
		truncated := sanitizeTruncatedToolInputs(resp)

		// Convert the response to a history param
		a.history = append(a.history, a.provider.ToHistoryParam(resp))

		// Process all content blocks
		var toolResults []anthropic.ContentBlockParamUnion
		for _, block := range resp.Content {
			switch block.Type {
			case "text":
				a.lastResponse = block.Text
				log.Printf("  📝 %s: %s", a.cfg.ID, block.Text)

			case "tool_use":
				if reason, isTrunc := truncated[block.ID]; isTrunc {
					log.Printf("  ✂️  %s: tool_use %q input truncated (%s); returning guidance",
						a.cfg.ID, block.Name, reason)
					toolResults = append(toolResults, a.provider.NewToolResult(
						block.ID,
						"Your previous tool input was truncated by the model output limit "+
							"and could not be parsed. The payload is too large to emit "+
							"inline. Reduce the size: send long content as a file "+
							"attachment, split the request into multiple smaller calls, "+
							"or summarize. Do NOT retry the same call verbatim.",
						true,
					))
					continue
				}

				td, ok := a.toolIndex[block.Name]
				if !ok {
					toolResults = append(toolResults, a.provider.NewToolResult(
						block.ID, fmt.Sprintf("unknown tool: %s", block.Name), true,
					))
					continue
				}

				result, execErr := td.Execute(ctx, block.Input)
				isError := execErr != nil
				if isError {
					result = execErr.Error()
				}

				original := len(result)
				result = truncateToolResult(result, config.MaxToolResultChars)
				if len(result) < original {
					log.Printf("  ✂️  %s: truncated %s result %d → %d chars",
						a.cfg.ID, block.Name, original, len(result))
				}

				toolResults = append(toolResults, a.provider.NewToolResult(block.ID, result, isError))

			case "server_tool_use":
				log.Printf("  🌐 %s: server tool: %s", a.cfg.ID, block.Name)

			case "web_search_tool_result", "code_execution_tool_result", "bash_code_execution_tool_result":
				// Server-side result — already processed by the API, nothing to feed back.

			default:
				log.Printf("  ℹ️ %s: unhandled block type: %s", a.cfg.ID, block.Type)
			}
		}

		// If the model stopped naturally (no tool use), we're done
		if resp.StopReason == "end_turn" {
			return nil
		}

		// Feed tool results back as a user message (SDK convention)
		if len(toolResults) > 0 {
			a.history = append(a.history, anthropic.NewUserMessage(toolResults...))
		}
	}

	return fmt.Errorf("agent %s hit max turns (%d): %w", a.cfg.ID, maxTurns, ErrMaxTurns)
}

func (a *Agent) HandleSnooze(ctx context.Context, snooze services.Snooze, maxTurns int) error {
	emails, emailErr := services.GetEmails(ctx, []string{snooze.MessageId})
	if emailErr != nil {
		log.Printf("error getting email: %s", emailErr)
		return emailErr
	}
	if len(emails) == 0 {
		return fmt.Errorf("no snoozed email found with Message-Id: %s", snooze.MessageId)
	}

	preSys := "IMPORTANT: This email is being replayed as a previously scheduled task. " +
		"The scheduling has already been handled — do NOT schedule, snooze, or defer anything. " +
		"Do NOT mention scheduling in your response. " +
		"Execute the task described below immediately."

	if snooze.Description != "" {
		preSys += "\n\nTask: " + snooze.Description
	}

	// Temporarily remove snooze scripts from the agent's tool set
	originalTools := a.tools
	a.tools = tools.WithoutTool(tools.WithoutTool(a.tools, "snooze_tool"), "unsnooze_tool")
	handleErr := a.HandleMessage(ctx, preSys, &emails[0], maxTurns)
	if handleErr != nil {
		log.Printf("error handling message: %s", handleErr)
		return handleErr
	}
	// Restore original scripts
	a.tools = originalTools
	return nil
}

// ── Helpers ────────────────────────────────────────────────────────────

// toToolDefs converts tools.ToolDefinition slices to the provider-neutral
// ToolDef type, avoiding circular imports.
func toToolDefs(defs []tools.ToolDefinition) []provider.ToolDef {
	out := make([]provider.ToolDef, len(defs))
	for i, d := range defs {
		out[i] = provider.ToolDef{
			Name:        d.Name,
			Description: d.Description,
			InputSchema: d.InputSchema,
		}
	}
	return out
}

// historySize returns the approximate character count of serialized history.
func historySize(history []anthropic.MessageParam) int {
	b, err := json.Marshal(history)
	if err != nil {
		return 0
	}
	return len(b)
}

// trimHistory reduces serialized history below maxChars by dropping oldest
// messages. It always starts at a "safe cutpoint": a user message with no
// tool_result blocks (otherwise the leading tool_result would reference a
// tool_use in a trimmed assistant turn and the API returns 400).
//
// The "anchor" is the index of the most recent user message authored by a
// human (sender not in global.AgentEmails). trimHistory will never cut above
// the anchor — losing the active user request causes coordinators to forget
// the task and re-delegate to peers. When an anchor is present, the chosen
// cutpoint is restricted to indices >= anchor; among those, the oldest whose
// tail fits is preferred, falling back to the anchor itself otherwise (even
// if the tail still exceeds maxChars — better an oversize call than losing
// the task context).
//
// If no anchor exists (e.g. thread composed entirely of peer traffic, or
// AgentEmails not initialized), behaviour falls back to the pre-anchor logic:
// oldest cutpoint whose tail fits, else newest.
func trimHistory(history []anthropic.MessageParam, maxChars int) []anthropic.MessageParam {
	size := historySize(history)
	if size <= maxChars {
		return history
	}

	log.Printf("history size %d chars exceeds limit %d, trimming oldest messages", size, maxChars)

	// Collect safe cutpoint indices (ascending).
	var cutpoints []int
	for i, m := range history {
		if m.Role == anthropic.MessageParamRoleUser && !hasToolResult(m) {
			cutpoints = append(cutpoints, i)
		}
	}

	if len(cutpoints) == 0 {
		log.Printf("no safe cutpoint in history; leaving untrimmed (%d chars)", size)
		return history
	}

	// Find the anchor: the newest human-authored user message. Restrict
	// candidate cutpoints to >= anchor so we never trim away the active
	// user request.
	anchor := findAnchor(history)
	candidates := cutpoints
	if anchor >= 0 {
		filtered := candidates[:0:0]
		for _, idx := range cutpoints {
			if idx >= anchor {
				filtered = append(filtered, idx)
			}
		}
		if len(filtered) > 0 {
			candidates = filtered
		}
	}

	// Prefer the oldest candidate whose tail fits. Otherwise use the
	// oldest candidate (which is the anchor itself when pinning) — keeps
	// the active request visible even if the tail still exceeds maxChars.
	chosen := candidates[0]
	for _, idx := range candidates {
		if historySize(history[idx:]) <= maxChars {
			chosen = idx
			break
		}
	}

	trimmed := history[chosen:]
	log.Printf("history trimmed to %d messages (%d chars), starting at index %d (anchor=%d)",
		len(trimmed), historySize(trimmed), chosen, anchor)

	// If we're still over budget — typically because the anchor-pinned tail
	// is itself oversize — compress the largest tool_result blocks in the
	// tail. Deterministic, preserves tool_use ↔ tool_result pairing, and
	// the assistant turns that follow have already digested the raw output.
	if historySize(trimmed) > maxChars {
		trimmed = compressToolResults(trimmed, maxChars)
	}
	return trimmed
}

// compressToolResults shrinks history to fit maxChars by replacing the
// largest tool_result text payloads with a short stub. It works on a deep
// copy of the slice so callers' references are untouched, walks blocks
// from largest to smallest, and stops as soon as the serialized history
// fits. Tool_use ↔ tool_result pairing is preserved (the result block
// stays present, just with truncated content).
func compressToolResults(history []anthropic.MessageParam, maxChars int) []anthropic.MessageParam {
	// Deep-copy the messages and content slices we may mutate.
	out := make([]anthropic.MessageParam, len(history))
	for i, m := range history {
		newContent := make([]anthropic.ContentBlockParamUnion, len(m.Content))
		copy(newContent, m.Content)
		m.Content = newContent
		out[i] = m
	}

	type ref struct {
		msgIdx, blockIdx, size int
	}
	var refs []ref
	for i, m := range out {
		for j, block := range m.Content {
			if block.OfToolResult == nil {
				continue
			}
			size := 0
			for _, c := range block.OfToolResult.Content {
				if c.OfText != nil {
					size += len(c.OfText.Text)
				}
			}
			if size > 0 {
				refs = append(refs, ref{i, j, size})
			}
		}
	}

	// Largest first.
	for i := 0; i < len(refs); i++ {
		for j := i + 1; j < len(refs); j++ {
			if refs[j].size > refs[i].size {
				refs[i], refs[j] = refs[j], refs[i]
			}
		}
	}

	const stubFmt = "[compressed for context window: %d chars elided]"
	for _, r := range refs {
		if historySize(out) <= maxChars {
			break
		}
		tr := out[r.msgIdx].Content[r.blockIdx].OfToolResult
		stub := fmt.Sprintf(stubFmt, r.size)
		fixed := *tr
		fixed.Content = []anthropic.ToolResultBlockParamContentUnion{
			{OfText: &anthropic.TextBlockParam{Text: stub}},
		}
		out[r.msgIdx].Content[r.blockIdx].OfToolResult = &fixed
	}

	final := historySize(out)
	if final > maxChars {
		log.Printf("history still %d chars after compressing tool results (limit %d); proceeding oversize",
			final, maxChars)
	} else {
		log.Printf("history compressed to %d chars by eliding tool_result content", final)
	}
	return out
}

// findAnchor returns the index of the most recent user-role, non-tool_result
// message whose embedded email JSON was sent by a non-agent address. Returns
// -1 if no such message exists or AgentEmails has not been populated.
func findAnchor(history []anthropic.MessageParam) int {
	if len(config.AgentEmails) == 0 {
		return -1
	}
	for i := len(history) - 1; i >= 0; i-- {
		m := history[i]
		if m.Role != anthropic.MessageParamRoleUser || hasToolResult(m) {
			continue
		}
		from := extractFromField(m)
		if from == "" {
			continue
		}
		if !config.AgentEmails[strings.ToLower(from)] {
			return i
		}
	}
	return -1
}

// extractFromField pulls the bare "from" address out of the first text block
// of a user message, assuming the block holds an EmailSummary JSON produced
// by Email.UserMessage. The stored From is a formatted RFC 5322 address
// (e.g. `"Kiku" <kiku@agents.mxhero.com>`), so we parse it down to the bare
// mailbox before returning. Returns "" if the block isn't parseable JSON or
// has no from.
func extractFromField(m anthropic.MessageParam) string {
	for _, block := range m.Content {
		if block.OfText == nil {
			continue
		}
		var probe struct {
			From string `json:"from"`
		}
		if err := json.Unmarshal([]byte(block.OfText.Text), &probe); err != nil {
			return ""
		}
		if probe.From == "" {
			return ""
		}
		if addr, err := netmail.ParseAddress(probe.From); err == nil {
			return addr.Address
		}
		return probe.From
	}
	return ""
}

// truncateToolResult clamps result to maxChars, preserving valid UTF-8 and
// appending a marker so the model knows content was dropped. A non-positive
// maxChars disables truncation. The marker itself is accounted for in the
// budget so the final string is always <= maxChars.
func truncateToolResult(result string, maxChars int) string {
	if maxChars <= 0 || len(result) <= maxChars {
		return result
	}
	const marker = "\n\n[... tool result truncated ...]"
	budget := maxChars - len(marker)
	if budget <= 0 {
		return result[:maxChars]
	}
	// Back up to a rune boundary so we don't cut a multibyte codepoint.
	for budget > 0 && (result[budget]&0xC0) == 0x80 {
		budget--
	}
	return result[:budget] + marker
}

// sanitizeTruncatedToolInputs walks resp.Content and replaces any tool_use
// block whose Input is not valid JSON with the empty object "{}". This
// happens when the model hits its output-token limit mid-tool-call: the
// streamed JSON arguments are cut off, and the raw bytes would otherwise
// (a) fail to dispatch via json.Unmarshal in the tool, and (b) corrupt
// the saved history because json.RawMessage.MarshalJSON validates.
//
// Returns a map of tool_use ID → reason for the truncation so the caller
// can emit a specific tool_result instead of running Execute.
func sanitizeTruncatedToolInputs(resp *provider.MessageResponse) map[string]string {
	if resp == nil {
		return nil
	}
	truncated := map[string]string{}
	for i := range resp.Content {
		block := &resp.Content[i]
		if block.Type != "tool_use" {
			continue
		}
		raw := bytes.TrimSpace(block.Input)
		if len(raw) == 0 {
			block.Input = json.RawMessage(`{}`)
			continue
		}
		if json.Valid(raw) {
			continue
		}
		reason := "invalid JSON"
		if resp.StopReason == "max_tokens" {
			reason = "stop_reason=max_tokens — output limit hit mid-tool-call"
		}
		truncated[block.ID] = reason
		block.Input = json.RawMessage(`{}`)
	}
	return truncated
}

// hasToolResult reports whether the message contains any tool_result blocks.
// A leading user message with tool_result blocks is invalid because the
// tool_use they reference would be in a prior (trimmed) assistant message.
func hasToolResult(m anthropic.MessageParam) bool {
	for _, block := range m.Content {
		if block.OfToolResult != nil {
			return true
		}
	}
	return false
}
