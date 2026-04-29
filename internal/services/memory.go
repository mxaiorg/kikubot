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
	// Wrap each persisted message as raw JSON so the SDK never re-serialises
	// the content blocks through its own (potentially buggy) ToParam path.
	// Older history may contain server-side tool-result blocks whose
	// `content` field was corrupted by the SDK streaming accumulator (see
	// provider/anthropic.go) — drop those messages entirely since we can't
	// reconstruct them.
	for _, msgRaw := range raw.History {
		if messageHasCorruptServerResult(msgRaw) {
			continue
		}
		sanitized := stripCitationsFromMessage(msgRaw)
		m.History = append(m.History, param.Override[anthropic.MessageParam](sanitized))
	}
	// Trim any leading user message that references a now-dropped tool_use,
	// and trim leading tool_result-only user messages that no longer have a
	// preceding assistant tool_use to answer.
	m.History = trimOrphanedLeadingMessages(m.History)
	return &m, nil
}

// messageHasCorruptServerResult reports whether the message contains a
// server-side tool-result block whose `content` field was serialized by the
// buggy SDK accumulator (identifiable by the Go-style union field
// `OfWebSearchResultBlockArray` leaking into the JSON).
func messageHasCorruptServerResult(raw json.RawMessage) bool {
	var msg struct {
		Content []json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return false
	}
	for _, block := range msg.Content {
		var peek struct {
			Type    string          `json:"type"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(block, &peek); err != nil {
			continue
		}
		switch peek.Type {
		case "code_execution_tool_result", "bash_code_execution_tool_result",
			"web_search_tool_result":
			if bytes.Contains(peek.Content, []byte(`"OfWebSearchResultBlockArray"`)) {
				return true
			}
		}
	}
	return false
}

// trimOrphanedLeadingMessages drops leading messages that would be invalid
// after earlier messages were removed: a leading non-user message, or a
// leading user message consisting of tool_result blocks whose matching
// tool_use lives in a dropped assistant turn.
func trimOrphanedLeadingMessages(history []anthropic.MessageParam) []anthropic.MessageParam {
	for len(history) > 0 {
		if history[0].Role != anthropic.MessageParamRoleUser {
			history = history[1:]
			continue
		}
		hasToolResult := false
		for _, b := range history[0].Content {
			if b.OfToolResult != nil {
				hasToolResult = true
				break
			}
		}
		if hasToolResult {
			history = history[1:]
			continue
		}
		break
	}
	return history
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
