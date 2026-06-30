package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ── Box CLI Tools ───────────────────────────────────────────────────────
// Wraps the Box CLI (@box/cli) via the generic CLI bridge.
// Auth is configured by running "box configure:environments:add /path/to/services.json"
// before first use (e.g. in Docker entrypoint).

// This is a more directed version of the Box CLI.
// For a more general CLI integration see cli_helper.go and CLINavigator.

// Limit to most important fields
var boxFields = "--fields=type,id,name,description,created_by,shared_link"

func boxConfig() CLIToolConfig {
	return CLIToolConfig{
		Prefix:   "box",
		Command:  "npx",
		BaseArgs: []string{"-y", "@box/cli"},
		Timeout:  30,
	}
}

// BoxCLI returns Box tool definitions.
func BoxCLI() []ToolDefinition {
	cfg := boxConfig()

	// Verify the CLI is reachable at startup
	if _, err := CLIExec(cfg, []string{"--version"}); err != nil {
		log.Println("box cli not available:", err)
		return nil
	}
	log.Println("[box] CLI bridge initialized")

	return []ToolDefinition{
		boxSearchTool(cfg),
		boxFileGetTool(cfg),
		boxFolderListTool(cfg),
		boxFileDownloadTool(cfg),
		boxReadTextTool(cfg),
		boxSharedItemGetTool(cfg),
	}
}

// ── box__search ─────────────────────────────────────────────────────────

func boxSearchTool(cfg CLIToolConfig) ToolDefinition {
	return ToolDefinition{
		Name:        "box__search",
		Description: "Full-text search across all Box content (files, folders, web links). Returns matching items with metadata.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"query": {
					"type": "string",
					"description": "The search query string"
				},
				"file_extensions": {
					"type": "string",
					"description": "Comma-separated file extensions to filter by (e.g. pdf,docx)"
				},
				"type": {
					"type": "string",
					"enum": ["file", "folder", "web_link"],
					"description": "Limit results to a specific item type"
				},
				"ancestor_folder_id": {
					"type": "string",
					"description": "Limit search to items within this folder ID"
				},
				"limit": {
					"type": "integer",
					"description": "Max results to return (default 20)"
				}
			},
			"required": ["query"]
		}`),
		Execute: func(_ context.Context, input json.RawMessage) (string, error) {
			var p struct {
				Query            string `json:"query"`
				FileExtensions   string `json:"file_extensions"`
				Type             string `json:"type"`
				AncestorFolderID string `json:"ancestor_folder_id"`
				Limit            int    `json:"limit"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", fmt.Errorf("parsing input: %w", err)
			}

			args := []string{"search", p.Query, "--json", boxFields}
			if p.FileExtensions != "" {
				args = append(args, "--file-extensions", p.FileExtensions)
			}
			if p.Type != "" {
				args = append(args, "--type", p.Type)
			}
			if p.AncestorFolderID != "" {
				args = append(args, "--ancestor-folder-ids", p.AncestorFolderID)
			}
			if p.Limit > 0 {
				args = append(args, "--limit", fmt.Sprintf("%d", p.Limit))
			}

			return CLIExec(cfg, args)
		},
	}
}

// ── box__file_get ───────────────────────────────────────────────────────

func boxFileGetTool(cfg CLIToolConfig) ToolDefinition {
	return ToolDefinition{
		Name:        "box__file_get",
		Description: "Get metadata for a Box file by its ID. Returns name, size, owner, dates, shared link info, and more.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"file_id": {
					"type": "string",
					"description": "The Box file ID"
				}
			},
			"required": ["file_id"]
		}`),
		Execute: func(_ context.Context, input json.RawMessage) (string, error) {
			var p struct {
				FileID string `json:"file_id"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", fmt.Errorf("parsing input: %w", err)
			}

			return CLIExec(cfg, []string{"files:get", p.FileID, "--json", boxFields})
		},
		StaticSystem: "- When sharing a file from Box, always prefer Shared and Direct Download links\n" +
			"- Use box__file_get to download files from Box only if you need them for immediate use, " +
			"analysis, or requested as an attachment\n" +
			"- Downloading a file does not make it available to the user. " +
			"The file needs to be sent back to the user either as a link or as an email attachment\n",
	}
}

// ── box__folder_list ────────────────────────────────────────────────────

func boxFolderListTool(cfg CLIToolConfig) ToolDefinition {
	return ToolDefinition{
		Name:        "box__folder_list",
		Description: "List items (files and subfolders) in a Box folder. Use folder ID '0' for the root folder.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"folder_id": {
					"type": "string",
					"description": "The Box folder ID (use '0' for root)"
				},
				"limit": {
					"type": "integer",
					"description": "Max items to return (default 20)"
				}
			},
			"required": ["folder_id"]
		}`),
		Execute: func(_ context.Context, input json.RawMessage) (string, error) {
			var p struct {
				FolderID string `json:"folder_id"`
				Limit    int    `json:"limit"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", fmt.Errorf("parsing input: %w", err)
			}

			args := []string{"folders:items", p.FolderID, "--json", boxFields}
			if p.Limit > 0 {
				args = append(args, "--limit", fmt.Sprintf("%d", p.Limit))
			}

			return CLIExec(cfg, args)
		},
	}
}

// ── box__shared_item_get ────────────────────────────────────────────────

func boxSharedItemGetTool(cfg CLIToolConfig) ToolDefinition {
	return ToolDefinition{
		Name: "box__shared_item_get",
		Description: "Resolve a Box shared link URL to its underlying item and return that item's properties " +
			"(type, id, name, owner, shared link info). Box shared links look like " +
			"\"https://company-name.box.com/s/<token>\" — they have an '/s/' path segment instead of a numeric " +
			"item ID. Use this to turn such a link into a concrete item ID, then pass that id to " +
			"box__file_download (for files) or box__folder_list (for folders).",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"url": {
					"type": "string",
					"description": "The Box shared link URL (e.g. https://company-name.box.com/s/<token>). Must contain an '/s/' segment."
				},
				"password": {
					"type": "string",
					"description": "Password for the shared link, if it is password-protected"
				}
			},
			"required": ["url"]
		}`),
		Execute: func(_ context.Context, input json.RawMessage) (string, error) {
			var p struct {
				URL      string `json:"url"`
				Password string `json:"password"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", fmt.Errorf("parsing input: %w", err)
			}
			if p.URL == "" {
				return "", fmt.Errorf("url is required")
			}

			// Include permissions so the model can see can_download before
			// attempting a download (preview-only links cannot be fetched).
			args := []string{"shared-links:get", p.URL, "--json",
				"--fields=type,id,name,extension,size,description,created_by,shared_link,permissions"}
			if p.Password != "" {
				args = append(args, "--password", p.Password)
			}

			return CLIExec(cfg, args)
		},
		StaticSystem: "- Box shared/preview links look like \"https://company-name.box.com/s/<token>\" — they use an '/s/' " +
			"path segment and do NOT contain the numeric item ID that the other Box tools require.\n" +
			"- To download a file from a shared (preview) link: first call box__shared_item_get with the " +
			"\"https://.../s/...\" URL to resolve it to the underlying item, then take the \"id\" from the " +
			"result and pass it to box__file_download (or box__folder_list if the resolved type is a folder).\n" +
			"- Before downloading, check the resolved item's \"permissions.can_download\". If it is false, the " +
			"shared link is PREVIEW-ONLY: the binary cannot be downloaded via the API — do not try, and do not " +
			"fall back to download_file (which would only grab the HTML preview page). If you only need the file's " +
			"CONTENTS (to read, summarize, or answer questions), use box__read_text instead — it works under " +
			"preview rights even when download is blocked. Only if the requester specifically needs the binary " +
			"file itself, tell them the link is preview-only and the owner must enable download on the shared link.\n" +
			"- IMPORTANT: when downloading a file that came from a shared link, also pass that original " +
			"\"https://.../s/...\" URL to box__file_download as its shared_link argument. Files reached through a " +
			"shared link are usually NOT directly accessible by id, so a plain box__file_download (id only) will " +
			"return 'not_found'. The shared_link argument is what authorizes the download.\n" +
			"- NEVER fetch a \"https://.../s/...\" URL with the generic download_file tool — that returns the Box " +
			"HTML preview page, not the actual file. Always go through box__shared_item_get + box__file_download.\n",
	}
}

// ── box__file_download ──────────────────────────────────────────────────

func boxFileDownloadTool(cfg CLIToolConfig) ToolDefinition {
	return ToolDefinition{
		Name: "box__file_download",
		Description: "Download a file from Box to local disk and return its local path. Pass that path to " +
			"message_tool's attachments[].path to send the file. If the file came from a shared/preview link " +
			"(\"https://.../s/<token>\"), also pass that URL as shared_link — files reached via a shared link are " +
			"usually not directly downloadable by id, and the shared_link is what authorizes the download.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"file_id": {
					"type": "string",
					"description": "The Box file ID to download (e.g. the \"id\" returned by box__shared_item_get)"
				},
				"shared_link": {
					"type": "string",
					"description": "The original Box shared link URL (\"https://.../s/<token>\") the file was reached through. Required when the file is not directly accessible by id."
				},
				"shared_link_password": {
					"type": "string",
					"description": "Password for the shared link, if it is password-protected"
				},
				"as_user": {
					"type": "string",
					"description": "Optional Box user ID to act as (e.g. the file owner's created_by.id from box__file_get / box__search). Use this when the file is owned by an enterprise user but not shared via a link — the service account impersonates that user to download. Must be the numeric user ID, not an email."
				}
			},
			"required": ["file_id"]
		}`),
		Execute: func(_ context.Context, input json.RawMessage) (string, error) {
			var p struct {
				FileID             string `json:"file_id"`
				SharedLink         string `json:"shared_link"`
				SharedLinkPassword string `json:"shared_link_password"`
				AsUser             string `json:"as_user"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", fmt.Errorf("parsing input: %w", err)
			}
			if p.FileID == "" {
				return "", fmt.Errorf("file_id is required")
			}

			// Persist downloads under a stable per-file temp dir so message_tool
			// can attach them later by path (don't inline base64 — it truncates
			// mid-stream).
			destDir := filepath.Join(os.TempDir(), "box-download-"+p.FileID)
			if err := os.MkdirAll(destDir, 0o755); err != nil {
				return "", fmt.Errorf("creating download dir: %w", err)
			}

			var filePath string
			var err error
			if p.SharedLink != "" || p.AsUser != "" {
				// The service account often has no direct collaboration on the item,
				// so files:download by id 404s. Fetch the content endpoint with the
				// appropriate auth context instead: the BoxApi shared_link header
				// (link-based access) and/or the As-User header (impersonate the
				// owning enterprise user).
				filePath, err = boxDownloadViaHTTP(cfg, p.FileID, p.SharedLink, p.SharedLinkPassword, p.AsUser, destDir)
			} else {
				filePath, err = boxDownloadByID(cfg, p.FileID, destDir)
			}
			if err != nil {
				return "", err
			}

			info, err := os.Stat(filePath)
			if err != nil {
				return "", fmt.Errorf("stat downloaded file: %w", err)
			}

			result := struct {
				Filename string `json:"filename"`
				Path     string `json:"path"`
				Size     int64  `json:"size"`
			}{
				Filename: filepath.Base(filePath),
				Path:     filePath,
				Size:     info.Size(),
			}

			out, _ := json.Marshal(result)
			return string(out), nil
		},
		StaticSystem: "- box__file_download saves the file locally and returns its path; attach it by passing that " +
			"path to message_tool's attachments[].path. Do not try to inline the file contents yourself.\n" +
			"- If a plain box__file_download (id only) returns 'not_found'/404, the file is owned by a user the " +
			"service account doesn't collaborate with. Don't give up: if you have a shared link, pass it as " +
			"shared_link; otherwise look up the owner's numeric user id (created_by.id from box__file_get or " +
			"box__search) and pass it as as_user to download by impersonating the owner.\n",
	}
}

// boxDownloadByID downloads a file the service account can access directly,
// using the Box CLI. Returns the saved file's path.
func boxDownloadByID(cfg CLIToolConfig, fileID, destDir string) (string, error) {
	if _, err := CLIExec(cfg, []string{"files:download", fileID, "--destination", destDir, "--overwrite"}); err != nil {
		return "", fmt.Errorf("downloading file: %w", err)
	}
	entries, err := os.ReadDir(destDir)
	if err != nil || len(entries) == 0 {
		return "", fmt.Errorf("no file found after download")
	}
	return filepath.Join(destDir, entries[0].Name()), nil
}

// boxDownloadViaHTTP downloads file content that the Box CLI's files:download
// cannot reach because it has no way to pass extra auth context. We call the
// content endpoint directly with a fresh service-account token plus, as needed,
// the BoxApi: shared_link header (Box's documented mechanism for link-based
// content access) and/or the As-User header (impersonate the owning enterprise
// user — requires the app's "Manage app users" scope). Works for items the
// service account doesn't otherwise own.
func boxDownloadViaHTTP(cfg CLIToolConfig, fileID, sharedLink, password, asUser, destDir string) (string, error) {
	token, err := boxAccessToken(cfg)
	if err != nil {
		return "", fmt.Errorf("getting box access token: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	url := fmt.Sprintf("https://api.box.com/2.0/files/%s/content", fileID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("building request: %w", err)
	}
	boxSetAuth(req, token, sharedLink, password, asUser)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("requesting file content: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		if resp.StatusCode == http.StatusForbidden {
			// Download is denied (can_download=false). This is terminal — either
			// the shared link is preview-only, or this service account lacks
			// download permission (e.g. it is external to the file's Box
			// enterprise and an enterprise policy blocks external downloads). No
			// API path can fetch the bytes; make it explicit so the model doesn't
			// fall back to scraping the HTML preview page.
			return "", fmt.Errorf("box denied download of file %s (HTTP 403, can_download=false). The shared "+
				"link is preview-only, or this Box service account isn't permitted to download (commonly because "+
				"it's external to the file's enterprise). A Box admin must enable download on the link or grant "+
				"this account download permission. Do not retry or fall back to download_file. If you only need to "+
				"read the file's contents (not send the binary), use box__read_text instead — it works under "+
				"preview rights even when download is blocked. (Box response: %s)",
				fileID, strings.TrimSpace(string(body)))
		}
		return "", fmt.Errorf("box content endpoint returned HTTP %d for file %s via shared link: %s",
			resp.StatusCode, fileID, strings.TrimSpace(string(body)))
	}

	filename := boxFilenameFromResponse(resp, fileID)
	destPath := filepath.Join(destDir, filename)
	out, err := os.Create(destPath)
	if err != nil {
		return "", fmt.Errorf("creating file: %w", err)
	}
	defer out.Close()

	if _, err := io.Copy(out, resp.Body); err != nil {
		return "", fmt.Errorf("writing file: %w", err)
	}
	return destPath, nil
}

// boxAccessToken returns a service-account access token via the Box CLI.
func boxAccessToken(cfg CLIToolConfig) (string, error) {
	out, err := CLIExec(cfg, []string{"tokens:get"})
	if err != nil {
		return "", err
	}
	// tokens:get prints just the token (a JWT has no spaces); take the last
	// whitespace-separated field to tolerate any "Token: ..." style label.
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", fmt.Errorf("empty token from box tokens:get")
	}
	return fields[len(fields)-1], nil
}

// boxFilenameFromResponse derives a filename from the Content-Disposition header,
// falling back to the file id.
func boxFilenameFromResponse(resp *http.Response, fileID string) string {
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if _, params, err := mime.ParseMediaType(cd); err == nil {
			if name := params["filename"]; name != "" {
				return filepath.Base(name)
			}
		}
	}
	return "box-file-" + fileID
}

// ── box__read_text ──────────────────────────────────────────────────────

func boxReadTextTool(cfg CLIToolConfig) ToolDefinition {
	return ToolDefinition{
		Name: "box__read_text",
		Description: "Read the text content of a Box file (xlsx, docx, pptx, pdf, txt, …) using Box's extracted-text " +
			"representation. Use this to summarize, search, or answer questions about a file's contents. Unlike " +
			"box__file_download, this works under PREVIEW rights, so it succeeds even when a file's download is " +
			"blocked by a shared-link or enterprise security policy. If the file came from a shared/preview link, " +
			"pass that URL as shared_link.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"file_id": {
					"type": "string",
					"description": "The Box file ID (e.g. the \"id\" returned by box__shared_item_get)"
				},
				"shared_link": {
					"type": "string",
					"description": "The original Box shared link URL (\"https://.../s/<token>\") the file was reached through, if any."
				},
				"shared_link_password": {
					"type": "string",
					"description": "Password for the shared link, if it is password-protected"
				},
				"as_user": {
					"type": "string",
					"description": "Optional Box user ID to act as (e.g. the file owner's created_by.id). Use when there is no shared_link."
				}
			},
			"required": ["file_id"]
		}`),
		Execute: func(_ context.Context, input json.RawMessage) (string, error) {
			var p struct {
				FileID             string `json:"file_id"`
				SharedLink         string `json:"shared_link"`
				SharedLinkPassword string `json:"shared_link_password"`
				AsUser             string `json:"as_user"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", fmt.Errorf("parsing input: %w", err)
			}
			if p.FileID == "" {
				return "", fmt.Errorf("file_id is required")
			}
			return boxReadText(cfg, p.FileID, p.SharedLink, p.SharedLinkPassword, p.AsUser)
		},
		StaticSystem: "- To read or summarize the CONTENTS of a Box file without needing the binary, use box__read_text. " +
			"It returns the file's extracted text and works under preview rights even when box__file_download is " +
			"blocked by a download-disabled / preview-only policy.\n",
	}
}

// boxRepEntry is one entry in a Box file's representations list.
type boxRepEntry struct {
	Representation string `json:"representation"`
	Status         struct {
		State string `json:"state"`
	} `json:"status"`
	Info struct {
		URL string `json:"url"`
	} `json:"info"`
	Content struct {
		URLTemplate string `json:"url_template"`
	} `json:"content"`
}

// boxRepresentations is the subset of the Box file representations response we need.
type boxRepresentations struct {
	Representations struct {
		Entries []boxRepEntry `json:"entries"`
	} `json:"representations"`
}

// boxReadText fetches a file's extracted-text representation. It resolves the
// representation (polling if Box reports it as still generating), then downloads
// the text content. Auth context (shared link or as-user) is forwarded on every
// request so it works for link-shared or non-owned items.
func boxReadText(cfg CLIToolConfig, fileID, sharedLink, password, asUser string) (string, error) {
	token, err := boxAccessToken(cfg)
	if err != nil {
		return "", fmt.Errorf("getting box access token: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	metaURL := fmt.Sprintf("https://api.box.com/2.0/files/%s?fields=representations", fileID)
	entry, err := boxResolveExtractedText(ctx, token, metaURL, sharedLink, password, asUser, fileID)
	if err != nil {
		return "", err
	}

	contentURL := strings.Replace(entry.Content.URLTemplate, "{+asset_path}", "", 1)
	if contentURL == "" {
		return "", fmt.Errorf("box returned no content URL for the extracted text of file %s", fileID)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, contentURL, nil)
	if err != nil {
		return "", fmt.Errorf("building content request: %w", err)
	}
	boxSetAuth(req, token, sharedLink, password, asUser)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching extracted text: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("box extracted-text endpoint returned HTTP %d for file %s: %s",
			resp.StatusCode, fileID, strings.TrimSpace(string(body)))
	}

	text, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading extracted text: %w", err)
	}
	if len(text) == 0 {
		return "", fmt.Errorf("box extracted text for file %s was empty", fileID)
	}
	return string(text), nil
}

// boxResolveExtractedText requests the file's representations and returns the
// extracted_text entry once it is in the "success" state, polling its info URL a
// few times if Box is still generating it.
func boxResolveExtractedText(ctx context.Context, token, metaURL, sharedLink, password, asUser, fileID string) (boxRepEntry, error) {
	var zero boxRepEntry

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metaURL, nil)
	if err != nil {
		return zero, fmt.Errorf("building representations request: %w", err)
	}
	boxSetAuth(req, token, sharedLink, password, asUser)
	req.Header.Set("x-rep-hints", "[extracted_text]")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return zero, fmt.Errorf("requesting representations: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return zero, fmt.Errorf("box representations endpoint returned HTTP %d for file %s: %s",
			resp.StatusCode, fileID, strings.TrimSpace(string(body)))
	}

	var reps boxRepresentations
	if err := json.NewDecoder(resp.Body).Decode(&reps); err != nil {
		return zero, fmt.Errorf("decoding representations: %w", err)
	}

	var entry boxRepEntry
	found := false
	for _, e := range reps.Representations.Entries {
		if e.Representation == "extracted_text" {
			entry, found = e, true
			break
		}
	}
	if !found {
		return zero, fmt.Errorf("no extracted_text representation is available for file %s "+
			"(the file type may not support text extraction)", fileID)
	}

	// Poll the info URL while Box generates the representation.
	for i := 0; entry.Status.State != "success" && i < 5; i++ {
		if entry.Status.State == "none" {
			return zero, fmt.Errorf("box has no extracted text for file %s", fileID)
		}
		if entry.Info.URL == "" {
			break
		}
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-time.After(1500 * time.Millisecond):
		}

		pReq, err := http.NewRequestWithContext(ctx, http.MethodGet, entry.Info.URL, nil)
		if err != nil {
			return zero, fmt.Errorf("building representation poll request: %w", err)
		}
		boxSetAuth(pReq, token, sharedLink, password, asUser)
		pResp, err := http.DefaultClient.Do(pReq)
		if err != nil {
			return zero, fmt.Errorf("polling representation: %w", err)
		}
		var polled boxRepEntry
		dErr := json.NewDecoder(pResp.Body).Decode(&polled)
		pResp.Body.Close()
		if dErr != nil {
			return zero, fmt.Errorf("decoding representation poll: %w", dErr)
		}
		entry = polled
	}

	if entry.Status.State != "success" {
		return zero, fmt.Errorf("box extracted text for file %s was not ready (state: %q)", fileID, entry.Status.State)
	}
	return entry, nil
}

// boxSetAuth applies the bearer token plus optional shared-link or as-user
// context headers to a Box API request.
func boxSetAuth(req *http.Request, token, sharedLink, password, asUser string) {
	req.Header.Set("Authorization", "Bearer "+token)
	if sharedLink != "" {
		v := "shared_link=" + sharedLink
		if password != "" {
			v += "&shared_link_password=" + password
		}
		req.Header.Set("BoxApi", v)
	}
	if asUser != "" {
		req.Header.Set("As-User", asUser)
	}
}
