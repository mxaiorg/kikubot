package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// knowledgeFile is one Markdown file in a knowledge directory.
type knowledgeFile struct {
	Name    string
	Content string
}

// knowledgeView is the render model for the knowledge editor partial
// (templates/knowledge.html). It is embedded in the Agent Defaults and
// Add/Edit Agent pages and is also rendered standalone as the HTMX swap
// target after a save/delete.
type knowledgeView struct {
	Scope string // "common" or an agent email — round-tripped in every form
	Title string // human heading
	Dir   string // display path, e.g. configs/knowledge/common
	Files []knowledgeFile
	Draft knowledgeFile // pre-fill for the "add file" form (used to preserve input on error)

	// Inline flash, shown above the editor (the editor is swapped on its own
	// via HTMX, so it can't rely on the page-level cookie flash).
	Flash     string
	FlashKind string // success|error
}

// knowledgeScopeCommon is the directory holding knowledge shared by every agent.
const knowledgeScopeCommon = "common"

// knowledgeDirName maps a scope to its directory name under configs/knowledge/.
// scope "common" (or empty) → "common"; otherwise the lowercased local-part of
// the agent email, matching loadKnowledge's agentKey convention in
// cmd/kikubot/main.go.
func knowledgeDirName(scope string) string {
	scope = strings.TrimSpace(scope)
	if scope == "" || strings.EqualFold(scope, knowledgeScopeCommon) {
		return knowledgeScopeCommon
	}
	return emailStem(scope)
}

// knowledgeDir returns the absolute path to the knowledge directory for a scope.
func knowledgeDir(root, scope string) string {
	return filepath.Join(root, "configs", "knowledge", knowledgeDirName(scope))
}

// knowledgeDirDisplay returns the repo-relative directory for the scope, for
// display in the UI.
func knowledgeDirDisplay(scope string) string {
	return "configs/knowledge/" + knowledgeDirName(scope)
}

// validKnowledgeName normalises and validates a knowledge filename. A missing
// .md extension is appended. Path separators and dotfiles are rejected so a
// submitted name can never escape the knowledge directory.
func validKnowledgeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("filename is required")
	}
	if !strings.HasSuffix(strings.ToLower(name), ".md") {
		name += ".md"
	}
	if name != filepath.Base(name) || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("filename must not contain path separators")
	}
	if strings.HasPrefix(name, ".") {
		return "", fmt.Errorf("filename must not start with a dot")
	}
	return name, nil
}

// listKnowledge returns the .md files in a scope's directory, sorted by name —
// the same order the runtime concatenates them into the system prompt
// (numeric prefixes like 01_, 02_ control ordering). A missing directory is
// not an error; it yields no files.
func listKnowledge(root, scope string) ([]knowledgeFile, error) {
	dir := knowledgeDir(root, scope)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var files []knowledgeFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(dir, e.Name()))
		if readErr != nil {
			return nil, readErr
		}
		files = append(files, knowledgeFile{Name: e.Name(), Content: string(data)})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return files, nil
}

// saveKnowledgeFile creates or updates a knowledge file. When oldName is set
// and differs from name, the file is renamed (used to re-order via numeric
// prefix). Saving never silently clobbers a different existing file.
func saveKnowledgeFile(root, scope, oldName, name, content string) error {
	name, err := validKnowledgeName(name)
	if err != nil {
		return err
	}
	dir := knowledgeDir(root, scope)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fsWriteError(dir, err)
	}
	// Normalise to LF and guarantee a single trailing newline.
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.TrimRight(content, "\n") + "\n"

	oldName = strings.TrimSpace(oldName)
	target := filepath.Join(dir, name)
	if oldName == "" || oldName != name {
		// Creating a new file, or renaming onto a new name: refuse to
		// overwrite an unrelated existing file.
		if _, statErr := os.Stat(target); statErr == nil {
			return fmt.Errorf("a file named %q already exists", name)
		}
	}
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		return fsWriteError(target, err)
	}
	if oldName != "" && oldName != name {
		// Rename: drop the old file after the new one is written.
		if _, vErr := validKnowledgeName(oldName); vErr == nil {
			_ = os.Remove(filepath.Join(dir, oldName))
		}
	}
	return nil
}

// deleteKnowledgeFile removes a knowledge file by name.
func deleteKnowledgeFile(root, scope, name string) error {
	name, err := validKnowledgeName(name)
	if err != nil {
		return err
	}
	target := knowledgeDir(root, scope) + string(os.PathSeparator) + name
	if err := os.Remove(target); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fsWriteError(target, err)
	}
	return nil
}

// buildKnowledgeView assembles the editor view for a scope.
func buildKnowledgeView(root, scope string) knowledgeView {
	v := knowledgeView{
		Scope: scope,
		Dir:   knowledgeDirDisplay(scope),
	}
	if knowledgeDirName(scope) == knowledgeScopeCommon {
		v.Title = "Common knowledge"
	} else {
		v.Title = "Agent knowledge"
	}
	files, err := listKnowledge(root, scope)
	if err != nil {
		v.Flash, v.FlashKind = "Could not read knowledge files: "+err.Error(), "error"
		return v
	}
	v.Files = files
	return v
}
