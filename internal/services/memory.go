package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

type MemoryStatus string

const (
	MemoryStatus_Waiting  MemoryStatus = "waiting"
	MemoryStatus_Complete MemoryStatus = "complete"
	MemoryStatus_Error    MemoryStatus = "error"
	// MemoryStatus_AdminReview is a stub status referenced by the pollerDeps
	// admin-review abstraction (see cmd/kikubot/main.go). The admin-review flow
	// is not implemented in this build, so this status is never set — it exists
	// only so the abstraction compiles and stays close to the origin codebase.
	MemoryStatus_AdminReview MemoryStatus = "admin_review"
)

var ErrMemoryNotFound = errors.New("memory not found")

// memoryDir and snoozeFile default to paths relative to the working directory.
// InitDataPaths moves them under /app/data when running in a container so
// they survive image rebuilds via a Docker volume.
var memoryDir = "memory"

// InitDataPaths sets persistent data paths for container environments.
// Call after global.LoadEnv().
func InitDataPaths(inContainer bool) {
	if inContainer {
		snoozeFile = "data/snooze.json"
		memoryDir = "data/memory"
		xeroDir = "data/xero"
	}
}

func (s MemoryStatus) String() string {
	return string(s)
}

type Memory struct {
	ThreadRoot    string                   `json:"threadRoot,omitempty"` // Index
	History       []anthropic.MessageParam `json:"history,omitempty"`
	Status        MemoryStatus             `json:"status,omitempty"`
	StatusUpdated *time.Time               `json:"statusUpdated,omitempty"`
}

func ensureMemoryDir() error {
	return os.MkdirAll(memoryDir, 0755)
}

// rawMemory is used for reading history as raw JSON messages so we can
// wrap each one with param.Override, bypassing SDK serialization bugs.
type rawMemory struct {
	ThreadRoot    string            `json:"threadRoot,omitempty"`
	History       []json.RawMessage `json:"history,omitempty"`
	Status        MemoryStatus      `json:"status,omitempty"`
	StatusUpdated *time.Time        `json:"statusUpdated,omitempty"`
}

func readMemoryFile(threadRoot string) (*Memory, error) {
	path := filepath.Join(memoryDir, msgIDToFilename(threadRoot))
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrMemoryNotFound
		}
		return nil, err
	}
	var raw rawMemory
	if err2 := json.Unmarshal(data, &raw); err2 != nil {
		return nil, err2
	}
	m := Memory{
		ThreadRoot:    raw.ThreadRoot,
		Status:        raw.Status,
		StatusUpdated: raw.StatusUpdated,
	}
	// Sanitize and trim raw history BEFORE wrapping. The wrapping below uses
	// param.Override which zeroes the public Role/Content fields (the raw
	// JSON is held opaquely for serialisation only) — so any field-based
	// check applied to a wrapped message misfires. Historical bug: doing the
	// orphan-trim on wrapped messages saw Role=="" on every message and
	// dropped the entire history on every load.
	//
	// Order of operations:
	//   1. Strip corrupt server-side tool-result blocks (a known SDK
	//      streaming-accumulator bug) — we can't reconstruct them — along with
	//      the server_tool_use calls that produced them. Only when that empties
	//      a message is the whole message dropped. Regular tool_use blocks in
	//      the same turn (e.g. a message_tool call whose tool_result lives in
	//      the next message) are preserved, so we don't orphan their pairing.
	//   2. Sanitise citations / server_tool_use that the API would reject,
	//      and drop unusable thinking blocks (empty signature or empty body)
	//      that the API rejects on re-submission.
	//   3. Trim leading messages that would be invalid after (1) removed
	//      earlier turns (orphaned tool_results, non-user leads).
	//   4. Wrap each surviving raw message via param.Override so the SDK
	//      doesn't re-serialise it through its own (potentially buggy)
	//      ToParam path.
	var sanitized []json.RawMessage
	for _, msgRaw := range raw.History {
		stripped, keep := stripCorruptServerResults(msgRaw)
		if !keep {
			continue
		}
		clean := stripCitationsFromMessage(stripped)
		clean = stripUnusableThinking(clean)
		sanitized = append(sanitized, clean)
	}
	sanitized = trimOrphanedLeadingMessagesRaw(sanitized)
	for _, msgRaw := range sanitized {
		m.History = append(m.History, param.Override[anthropic.MessageParam](msgRaw))
	}
	return &m, nil
}

// isCorruptServerResult reports whether a content block is a server-side
// tool-result block whose `content` field was serialized by the buggy SDK
// accumulator (identifiable by the Go-style union field
// `OfWebSearchResultBlockArray` leaking into the JSON). The type set must stay
// in sync with ToHistoryParam/needsOriginalRaw in provider/anthropic.go —
// text_editor_/bash_code_execution results arrive via the code execution that
// backs web_search_20260209's dynamic filtering.
func isCorruptServerResult(blockType string, content json.RawMessage) bool {
	switch blockType {
	case "code_execution_tool_result", "bash_code_execution_tool_result",
		"text_editor_code_execution_tool_result",
		"web_search_tool_result", "web_fetch_tool_result":
		return bytes.Contains(content, []byte(`"OfWebSearchResultBlockArray"`))
	}
	return false
}

// stripCorruptServerResults removes corrupt server-side tool-result blocks and
// the server_tool_use blocks that produced them from a single serialized
// message. These blocks persist without the pre-accumulate rawJSON, so on
// reload they carry the SDK's re-marshalled union fields and the API rejects
// them. Regular tool_use/tool_result blocks (e.g. a message_tool call whose
// result lives in the next message) are left untouched so their cross-message
// pairing survives. Returns the rewritten message and whether any content
// remains — keep is false when stripping empties the message, so the caller
// drops it entirely.
func stripCorruptServerResults(raw json.RawMessage) (json.RawMessage, bool) {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return raw, true // unparseable — leave for downstream handling
	}
	contentRaw, ok := msg["content"]
	if !ok {
		return raw, true
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(contentRaw, &blocks); err != nil {
		return raw, true
	}

	// First pass: flag corrupt result blocks and record the tool_use_ids they
	// reference, so the paired server_tool_use calls can be dropped too (a
	// server_tool_use with no result is itself rejected by the API).
	corrupt := make([]bool, len(blocks))
	orphanedCallIDs := map[string]bool{}
	var anyCorrupt bool
	for i, b := range blocks {
		var peek struct {
			Type      string          `json:"type"`
			Content   json.RawMessage `json:"content"`
			ToolUseID string          `json:"tool_use_id"`
		}
		if err := json.Unmarshal(b, &peek); err != nil {
			continue
		}
		if isCorruptServerResult(peek.Type, peek.Content) {
			corrupt[i] = true
			anyCorrupt = true
			if peek.ToolUseID != "" {
				orphanedCallIDs[peek.ToolUseID] = true
			}
		}
	}
	if !anyCorrupt {
		return raw, len(blocks) > 0
	}

	// Second pass: keep everything except the corrupt results and their
	// producing server_tool_use calls.
	kept := make([]json.RawMessage, 0, len(blocks))
	for i, b := range blocks {
		if corrupt[i] {
			continue
		}
		var peek struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if err := json.Unmarshal(b, &peek); err == nil &&
			peek.Type == "server_tool_use" && orphanedCallIDs[peek.ID] {
			continue
		}
		kept = append(kept, b)
	}
	if len(kept) == 0 {
		return nil, false
	}
	newContent, err := json.Marshal(kept)
	if err != nil {
		return raw, true
	}
	msg["content"] = newContent
	out, err := json.Marshal(msg)
	if err != nil {
		return raw, true
	}
	return out, true
}

// trimOrphanedLeadingMessagesRaw drops leading messages that would be
// invalid as the first turn of a re-submitted conversation: a leading
// non-user message, or a leading user message composed entirely of
// tool_result blocks whose matching tool_use lives in a now-dropped
// assistant turn.
//
// Operates on raw JSON because this runs on the load path before
// param.Override wrapping (the wrapper hides Role/Content from any
// field-based check). A message that fails to parse as a MessageParam
// shape is treated as orphaned.
func trimOrphanedLeadingMessagesRaw(history []json.RawMessage) []json.RawMessage {
	for len(history) > 0 {
		role, hasToolResult, ok := peekRoleAndToolResult(history[0])
		if !ok {
			history = history[1:]
			continue
		}
		if role != string(anthropic.MessageParamRoleUser) {
			history = history[1:]
			continue
		}
		if hasToolResult {
			history = history[1:]
			continue
		}
		break
	}
	return history
}

// peekRoleAndToolResult inspects a raw JSON-encoded MessageParam and
// returns its role plus whether any of its content blocks is a
// tool_result. ok is false when the JSON does not parse as a message.
func peekRoleAndToolResult(raw json.RawMessage) (role string, hasToolResult bool, ok bool) {
	var probe struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return "", false, false
	}
	for _, b := range probe.Content {
		if b.Type == "tool_result" {
			hasToolResult = true
			break
		}
	}
	return probe.Role, hasToolResult, true
}

// serverToolUseAllowedFields are the only keys the API accepts on a
// server_tool_use content block when it appears in a request. Everything else
// — including zero-valued fields the SDK accidentally serialized (citations,
// text, signature, thinking, data, content, tool_use_id …) — must be stripped
// before re-submission.
var serverToolUseAllowedFields = map[string]bool{
	"type":          true,
	"id":            true,
	"input":         true,
	"name":          true,
	"cache_control": true,
	"caller":        true,
}

// stripCitationsFromMessage sanitizes a serialized message so it can be
// re-submitted to the Anthropic API:
//   - text blocks: drop "citations" (web-search citations sometimes carry
//     empty URLs that the API rejects, and they aren't needed for continuity).
//   - server_tool_use blocks: keep only the fields the API accepts on input.
//     The SDK's streaming accumulator re-marshals ContentBlockUnion with all
//     union-variant fields zero-filled; passing those back triggers 400s like
//     "server_tool_use.citations: Extra inputs are not permitted".
func stripCitationsFromMessage(raw json.RawMessage) json.RawMessage {
	var msg struct {
		Role    string            `json:"role"`
		Content []json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return raw // not a message shape — return as-is
	}

	changed := false
	for i, block := range msg.Content {
		var peek struct {
			Type      string          `json:"type"`
			Citations json.RawMessage `json:"citations"`
		}
		if err := json.Unmarshal(block, &peek); err != nil {
			continue
		}

		switch peek.Type {
		case "text":
			if len(peek.Citations) == 0 {
				continue
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal(block, &m); err != nil {
				continue
			}
			delete(m, "citations")
			if fixed, err := json.Marshal(m); err == nil {
				msg.Content[i] = fixed
				changed = true
			}

		case "server_tool_use":
			var m map[string]json.RawMessage
			if err := json.Unmarshal(block, &m); err != nil {
				continue
			}
			blockChanged := false
			for k := range m {
				if !serverToolUseAllowedFields[k] {
					delete(m, k)
					blockChanged = true
				}
			}
			if blockChanged {
				if fixed, err := json.Marshal(m); err == nil {
					msg.Content[i] = fixed
					changed = true
				}
			}
		}
	}

	if !changed {
		return raw
	}
	if out, err := json.Marshal(msg); err == nil {
		return out
	}
	return raw
}

// stripUnusableThinking drops thinking / redacted_thinking blocks that the API
// will reject when the message is re-submitted:
//   - thinking blocks with an empty signature (the cryptographic token is
//     required and is verified) or an empty thinking body (a non-empty
//     signature cannot validate against empty content);
//   - redacted_thinking blocks with empty data.
//
// These arise from the SDK streaming accumulator splitting a thinking block's
// signature and body across interleaved deltas (fixed upstream by the
// index-based accumulator, but already persisted in older memory files). A
// single bad block wedges the whole thread on every reload, so we drop it.
//
// The block is only removed when at least one other content block survives —
// a message must never be left with empty content. Runs on raw JSON (load
// path, before param.Override wrapping), mirroring stripCitationsFromMessage.
func stripUnusableThinking(raw json.RawMessage) json.RawMessage {
	var msg struct {
		Role    string            `json:"role"`
		Content []json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return raw // not a message shape — return as-is
	}

	kept := make([]json.RawMessage, 0, len(msg.Content))
	dropped := false
	for _, block := range msg.Content {
		var peek struct {
			Type      string `json:"type"`
			Signature string `json:"signature"`
			Thinking  string `json:"thinking"`
			Data      string `json:"data"`
		}
		if err := json.Unmarshal(block, &peek); err != nil {
			kept = append(kept, block)
			continue
		}
		unusable := false
		switch peek.Type {
		case "thinking":
			unusable = strings.TrimSpace(peek.Signature) == "" || strings.TrimSpace(peek.Thinking) == ""
		case "redacted_thinking":
			unusable = strings.TrimSpace(peek.Data) == ""
		}
		if unusable {
			dropped = true
			continue
		}
		kept = append(kept, block)
	}

	// Nothing to drop, or dropping would empty the message — leave it intact.
	if !dropped || len(kept) == 0 {
		return raw
	}
	msg.Content = kept
	if out, err := json.Marshal(msg); err == nil {
		return out
	}
	return raw
}

func writeMemoryFile(m *Memory) error {
	if err := ensureMemoryDir(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		// Most common cause is a tool_use block whose Input is invalid
		// JSON (model truncated mid-call, prior to the agent-loop fix).
		// Strip those before giving up — losing one block is much better
		// than losing the whole save and re-running the entire task.
		if cleaned := sanitizeHistoryRawJSON(m.History); cleaned != nil {
			m.History = cleaned
			data, err = json.MarshalIndent(m, "", "  ")
		}
		if err != nil {
			return fmt.Errorf("memory marshal failed even after sanitization: %w", err)
		}
	}
	path := filepath.Join(memoryDir, msgIDToFilename(m.ThreadRoot))
	return os.WriteFile(path, data, 0644)
}

// toolUseInputValid reports whether a ToolUseBlockParam.Input value will
// successfully marshal to JSON. The Input field is typed `any`; the
// providers in this codebase set it to a json.RawMessage, which fails
// MarshalJSON when the bytes are not valid JSON (e.g. truncated tool
// arguments from a max_tokens stop). nil and other valid Go values are
// considered fine.
func toolUseInputValid(in any) bool {
	switch v := in.(type) {
	case nil:
		return true
	case json.RawMessage:
		raw := bytes.TrimSpace(v)
		return len(raw) > 0 && json.Valid(raw)
	case []byte:
		raw := bytes.TrimSpace(v)
		return len(raw) > 0 && json.Valid(raw)
	default:
		return true
	}
}

// sanitizeHistoryRawJSON walks history and repairs any json.RawMessage
// fields that are not valid JSON. The only known offender is
// ContentBlockParamUnion.OfToolUse.Input — when the model hits its output
// token limit mid-tool-call, the streamed Arguments are cut off and the
// resulting RawMessage fails MarshalJSON validation.
//
// Returns a sanitized copy when changes were made, or nil when there was
// nothing to fix (caller should propagate the original marshal error).
func sanitizeHistoryRawJSON(history []anthropic.MessageParam) []anthropic.MessageParam {
	changed := false
	cleaned := make([]anthropic.MessageParam, len(history))
	for i, msg := range history {
		newContent := make([]anthropic.ContentBlockParamUnion, len(msg.Content))
		for j, block := range msg.Content {
			if block.OfToolUse != nil {
				if !toolUseInputValid(block.OfToolUse.Input) {
					fixed := *block.OfToolUse
					fixed.Input = json.RawMessage(`{}`)
					block.OfToolUse = &fixed
					changed = true
				}
			}
			newContent[j] = block
		}
		msg.Content = newContent
		cleaned[i] = msg
	}
	if !changed {
		return nil
	}
	return cleaned
}

func SetMemoryStatus(ctx context.Context, status MemoryStatus, messageId string) error {
	emails, emailErr := GetEmails(ctx, []string{messageId})
	if emailErr != nil {
		return fmt.Errorf("error getting email: %w", emailErr)
	}
	if len(emails) == 0 {
		return fmt.Errorf("no email found with Message-Id: %s", messageId)
	}

	threadRoot := emails[0].GetThreadRoot()
	memory, err := GetMemory(threadRoot)
	if err != nil && !errors.Is(err, ErrMemoryNotFound) {
		return err
	}
	if memory == nil {
		memory = &Memory{
			ThreadRoot: threadRoot,
		}
	}

	memory.Status = status
	memory.StatusUpdated = new(time.Now())
	return memory.SaveMemory()
}

func SaveMemoryHistory(ctx context.Context, history []anthropic.MessageParam, messageId string) error {
	emails, emailErr := GetEmails(ctx, []string{messageId})
	if emailErr != nil {
		return fmt.Errorf("error getting email: %w", emailErr)
	}
	if len(emails) == 0 {
		return fmt.Errorf("no email found with Message-Id: %s", messageId)
	}

	memory, err := GetMemory(emails[0].GetThreadRoot())
	if err != nil && !errors.Is(err, ErrMemoryNotFound) {
		return fmt.Errorf("error getting memory: %w", err)
	}
	if memory == nil {
		memory = &Memory{
			ThreadRoot: emails[0].GetThreadRoot(),
		}
	}
	memory.History = history

	return memory.SaveMemory()
}

// GetMemory returns the Memory struct for the given threadRoot.
// ThreadRoot is the unique identifier for the thread. It is the first
// message-id of the References header or the message-id if there is no
// References header. If the entry does not exist, it returns ErrMemoryNotFound.
func GetMemory(threadRoot string) (*Memory, error) {
	return readMemoryFile(threadRoot)
}

// MemoryFromReferences will generate a Memory struct from email messages and save
// it to the memory directory. Returns the Memory struct and any error encountered.
func MemoryFromReferences(ctx context.Context, references []string) (*Memory, error) {
	// Fetch all referenced emails in a single batch, group by thread root
	fetched, err := GetEmails(ctx, references)
	if err != nil {
		return nil, fmt.Errorf("fetching referenced emails: %w", err)
	}

	if len(fetched) == 0 {
		return nil, nil
	}

	root := fetched[0].GetThreadRoot()

	// Build a Memory with history in chronological order
	var history []anthropic.MessageParam
	for _, email := range fetched {
		msg, err2 := email.UserMessage()
		if err2 != nil {
			continue
		}
		history = append(history, *msg)
	}

	// Memory status information is not set
	m := Memory{
		ThreadRoot: root,
		History:    history,
	}
	if err3 := m.SaveMemory(); err3 != nil {
		return nil, fmt.Errorf("saving memory for thread %s: %w", root, err3)
	}

	return &m, nil
}

func (p *Memory) ClearStatus() {
	p.Status = ""
	p.StatusUpdated = nil
}

func (p *Memory) SaveMemory() error {
	return writeMemoryFile(p)
}

func (p *Memory) AddMessage(messages []anthropic.MessageParam) {
	p.History = append(p.History, messages...)
}

func msgIDToFilename(msgID string) string {
	// Strip surrounding < > if present
	s := strings.TrimPrefix(strings.TrimSuffix(msgID, ">"), "<")
	// PathEscape only encodes truly problematic chars (e.g. rare "/" in local part)
	return fmt.Sprintf("%s.json", url.PathEscape(s))
}

func filenameToMsgID(filename string) (string, error) {
	// Strip directory and extension
	base := filepath.Base(filename)
	name := strings.TrimSuffix(base, filepath.Ext(base))

	s, err := url.PathUnescape(name)
	if err != nil {
		return "", err
	}
	return "<" + s + ">", nil
}
