package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"kikubot/internal/config"
	"net/http"
	"net/url"
	"strings"
)

// ── Helpjuice Knowledge Base Tools ──────────────────────────────────────
//
// FAQ scripts (optimised — LLM provides only new content, Go handles splicing):
//   - helpjuice_faq        — read the FAQ article
//   - helpjuice_faq_append — append a Q&A to a section (no full-body regen)
//   - helpjuice_update     — generic article update by ID
//
// Requires env vars: HELPJUICE_API_KEY, HELPJUICE_ACCOUNT (subdomain).

const (
	//faqArticleID = 3743923 // test article
	faqArticleID = 3743408 // production article
)

func HelpjuiceTools() []ToolDefinition {
	return []ToolDefinition{
		helpjuiceFAQRead(),
		helpjuiceFAQAppend(),
		//helpjuiceUpdateTool(),
	}
}

func helpjuiceFAQRead() ToolDefinition {
	return ToolDefinition{
		Name:        "helpjuice_faq",
		Description: "Read the Helpjuice KB (knowledge base) FAQ (frequently asked questions).",
		InputSchema: []byte(`{"type":"object"}`),
		Execute: func(_ context.Context, _ json.RawMessage) (string, error) {
			// Fetch the FAQ article body directly — eliminates search + get round trips.
			reqURL := fmt.Sprintf("%s/articles/%d", helpjuiceBaseURL(), faqArticleID)
			body, err := helpjuiceRequest(http.MethodGet, reqURL, nil)
			if err != nil {
				return "", fmt.Errorf("fetching FAQ article: %w", err)
			}
			return string(body), nil
		},
	}
}

// helpjuiceFAQAppend lets the LLM provide only the new Q&A content and a
// section name. The Go code fetches the current body, finds the section,
// appends the new Q&A at the end of that section, and pushes the update.
// This avoids the LLM having to regenerate the entire article body as
// output tokens — which was causing context deadline timeouts.
func helpjuiceFAQAppend() ToolDefinition {
	return ToolDefinition{
		Name:        "helpjuice_faq_append",
		Description: `Append a new Q&A entry to a section of the Helpjuice FAQ article. The tool fetches the current article, splices in the new Q&A at the end of the specified section, and updates the article.`,
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"section": {
					"type": "string",
					"description": "Which FAQ section to append to. Use a keyword: GENERAL, TECHNOLOGY, IMPLEMENTATION, or GENERATIVE AI."
				},
				"question": {
					"type": "string",
					"description": "The new FAQ question text (without ## prefix)"
				},
				"answer": {
					"type": "string",
					"description": "The answer content (plain text or markdown)"
				}
			},
			"required": ["section", "question", "answer"]
		}`),
		Execute:      executeHelpjuiceFAQAppend,
		StaticSystem: "- You only need to provide the section, question, and answer — NOT the full article body. Sections: \"GENERAL\", \"TECHNOLOGY\", \"IMPLEMENTATION\", \"GENERATIVE AI\".",
	}
}

// sectionKeywords maps user-facing keywords to unique substrings that appear
// in the section heading regardless of whether the body is markdown or HTML.
// We search case-insensitively for these in the body.
var sectionKeywords = map[string]string{
	"GENERAL":        "GENERAL FAQ",
	"TECHNOLOGY":     "TECHNOLOGY / ARCHITECTURE / SECURITY",
	"IMPLEMENTATION": "IMPLEMENTATION",
	"GENERATIVE AI":  "GENERATIVE AI",
}

// preCodePrefix and preCodeSuffix are the HTML wrapper Helpjuice uses to
// store raw markdown in the article body field.
const (
	preCodePrefix = `<pre><code class="language-markdown">`
	preCodeSuffix = `</code></pre>`
)

func executeHelpjuiceFAQAppend(_ context.Context, input json.RawMessage) (string, error) {
	var p struct {
		Section  string `json:"section"`
		Question string `json:"question"`
		Answer   string `json:"answer"`
	}
	if err := json.Unmarshal(input, &p); err != nil {
		return "", fmt.Errorf("parsing input: %w", err)
	}

	// 1. Resolve the section keyword.
	sectionKey := strings.ToUpper(strings.TrimSpace(p.Section))
	keyword, ok := sectionKeywords[sectionKey]
	if !ok {
		return "", fmt.Errorf("unknown section %q — use one of: GENERAL, TECHNOLOGY, IMPLEMENTATION, GENERATIVE AI", p.Section)
	}

	// 2. Fetch current article body.
	reqURL := fmt.Sprintf("%s/articles/%d", helpjuiceBaseURL(), faqArticleID)
	raw, err := helpjuiceRequest(http.MethodGet, reqURL, nil)
	if err != nil {
		return "", fmt.Errorf("fetching FAQ article: %w", err)
	}

	body, err := extractBody(raw)
	if err != nil {
		return "", err
	}

	// 3. Unwrap the <pre><code class="language-markdown"> wrapper so we can
	// work with plain markdown, then rewrap before saving.
	md, err := unwrapMarkdown(body)
	if err != nil {
		return "", err
	}

	// 4. Find the target section by keyword (case-insensitive).
	mdLower := strings.ToLower(md)
	sectionIdx := strings.Index(mdLower, strings.ToLower(keyword))
	if sectionIdx < 0 {
		return "", fmt.Errorf("could not find section containing %q", keyword)
	}

	// 5. Find the end of this section: the next "---" separator that appears
	// after the section heading line, or end of string.
	afterHeader := mdLower[sectionIdx:]
	newlineIdx := strings.Index(afterHeader, "\n")
	if newlineIdx < 0 {
		newlineIdx = 0
	}
	sepIdx := strings.Index(afterHeader[newlineIdx:], "\n---")
	var insertPos int
	if sepIdx >= 0 {
		insertPos = sectionIdx + newlineIdx + sepIdx
	} else {
		insertPos = len(md)
	}

	// 6. Build and splice in the new Q&A markdown block.
	newQA := fmt.Sprintf("\n\n## %s\n\n%s",
		strings.TrimSpace(p.Question), strings.TrimSpace(p.Answer))
	md = md[:insertPos] + newQA + md[insertPos:]

	// 7. Rewrap and push.
	updatedBody := preCodePrefix + md + preCodeSuffix
	payload, _ := json.Marshal(map[string]any{
		"article": map[string]any{"body": updatedBody},
	})

	reqURL = fmt.Sprintf("%s/articles/%d", helpjuiceBaseURL(), faqArticleID)
	result, err := helpjuiceRequest(http.MethodPut, reqURL, payload)
	if err != nil {
		return "", fmt.Errorf("updating FAQ article: %w", err)
	}

	return fmt.Sprintf("Successfully appended Q&A to section %q. Question: %s\nAPI response: %s",
		sectionKey, p.Question, string(result)), nil
}

// unwrapMarkdown strips the <pre><code class="language-markdown"> wrapper
// that Helpjuice uses to store raw markdown in the body field.
func unwrapMarkdown(body string) (string, error) {
	trimmed := strings.TrimSpace(body)
	if !strings.HasPrefix(trimmed, preCodePrefix) {
		return "", fmt.Errorf("article body does not start with expected <pre><code> wrapper (got: %.80s)", trimmed)
	}
	inner := strings.TrimPrefix(trimmed, preCodePrefix)
	inner = strings.TrimSuffix(inner, preCodeSuffix)
	return inner, nil
}

// extractBody pulls the article body from the Helpjuice v3 GET /articles/:id
// response, which nests it at article.answer.body.
func extractBody(raw []byte) (string, error) {
	var resp struct {
		Article struct {
			Answer struct {
				Body string `json:"body"`
			} `json:"answer"`
		} `json:"article"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("parsing API response: %w", err)
	}
	if resp.Article.Answer.Body == "" {
		return "", fmt.Errorf("article.answer.body is empty in API response (first 300 chars: %.300s)", string(raw))
	}
	return resp.Article.Answer.Body, nil
}

func helpjuiceBaseURL() string {
	return fmt.Sprintf("https://%s.helpjuice.com/api/v3", config.HelpjuiceAccount)
}

// ── Search ──────────────────────────────────────────────────────────────

func helpjuiceSearchTool() ToolDefinition {
	return ToolDefinition{
		Name:        "helpjuice_search",
		Description: "Search the Helpjuice knowledge base for articles matching a query. Returns article IDs, names, and slugs.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"query": {
					"type": "string",
					"description": "Search query string"
				},
				"category_id": {
					"type": "integer",
					"description": "Optional category ID to filter results"
				}
			},
			"required": ["query"]
		}`),
		Execute: executeHelpjuiceSearch,
	}
}

func executeHelpjuiceSearch(_ context.Context, input json.RawMessage) (string, error) {
	var p struct {
		Query      string `json:"query"`
		CategoryID *int   `json:"category_id,omitempty"`
	}
	if err := json.Unmarshal(input, &p); err != nil {
		return "", fmt.Errorf("parsing input: %w", err)
	}

	params := url.Values{}
	params.Set("query", p.Query)
	if p.CategoryID != nil {
		params.Set("category_id", fmt.Sprintf("%d", *p.CategoryID))
	}

	reqURL := fmt.Sprintf("%s/search?%s", helpjuiceBaseURL(), params.Encode())
	body, err := helpjuiceRequest(http.MethodGet, reqURL, nil)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// ── Get Article ─────────────────────────────────────────────────────────

func helpjuiceGetTool() ToolDefinition {
	return ToolDefinition{
		Name:        "helpjuice_get",
		Description: "Fetch the full content of a Helpjuice article by ID. Use after helpjuice_search to read an article before updating it.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"id": {
					"type": "integer",
					"description": "The article ID"
				},
				"processed": {
					"type": "boolean",
					"description": "If true, returns fully rendered HTML. Default false (raw content)."
				}
			},
			"required": ["id"]
		}`),
		Execute: executeHelpjuiceGet,
	}
}

func executeHelpjuiceGet(_ context.Context, input json.RawMessage) (string, error) {
	var p struct {
		ID        int  `json:"id"`
		Processed bool `json:"processed"`
	}
	if err := json.Unmarshal(input, &p); err != nil {
		return "", fmt.Errorf("parsing input: %w", err)
	}

	reqURL := fmt.Sprintf("%s/articles/%d", helpjuiceBaseURL(), p.ID)
	if p.Processed {
		reqURL += "?processed=true"
	}

	body, err := helpjuiceRequest(http.MethodGet, reqURL, nil)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// ── Update Article ──────────────────────────────────────────────────────

func helpjuiceUpdateTool() ToolDefinition {
	return ToolDefinition{
		Name:        "helpjuice_update",
		Description: "Update a Helpjuice article by ID. Provide only the fields you want to change. Updatable fields: name, description, codename, visibility_id (0=internal, 1=public, 2=private, 4=URL-only), body (HTML content), published (bool), category_ids, user_ids, group_ids.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"id": {
					"type": "integer",
					"description": "The article ID to update"
				},
				"name": {
					"type": "string",
					"description": "New article title"
				},
				"body": {
					"type": "string",
					"description": "New article body content"
				},
				"description": {
					"type": "string",
					"description": "New article description"
				},
				"published": {
					"type": "boolean",
					"description": "Whether the article is published"
				},
				"visibility_id": {
					"type": "integer",
					"description": "0=internal, 1=public, 2=private, 4=URL-only"
				},
				"codename": {
					"type": "string",
					"description": "New article codename"
				},
				"category_ids": {
					"type": "array",
					"items": {"type": "integer"},
					"description": "Category IDs to assign"
				}
			},
			"required": ["id"]
		}`),
		Execute: executeHelpjuiceUpdate,
	}
}

func executeHelpjuiceUpdate(_ context.Context, input json.RawMessage) (string, error) {
	var p struct {
		ID           int    `json:"id"`
		Name         string `json:"name,omitempty"`
		Body         string `json:"body,omitempty"`
		Description  string `json:"description,omitempty"`
		Published    *bool  `json:"published,omitempty"`
		VisibilityID *int   `json:"visibility_id,omitempty"`
		Codename     string `json:"codename,omitempty"`
		CategoryIDs  []int  `json:"category_ids,omitempty"`
	}
	if err := json.Unmarshal(input, &p); err != nil {
		return "", fmt.Errorf("parsing input: %w", err)
	}

	// Build the article payload with only the fields that were provided.
	article := make(map[string]any)
	if p.Name != "" {
		article["name"] = p.Name
	}
	if p.Body != "" {
		article["body"] = p.Body
	}
	if p.Description != "" {
		article["description"] = p.Description
	}
	if p.Published != nil {
		article["published"] = *p.Published
	}
	if p.VisibilityID != nil {
		article["visibility_id"] = *p.VisibilityID
	}
	if p.Codename != "" {
		article["codename"] = p.Codename
	}
	if p.CategoryIDs != nil {
		article["category_ids"] = p.CategoryIDs
	}

	if len(article) == 0 {
		return "", fmt.Errorf("no fields to update — provide at least one field besides id")
	}

	payload := map[string]any{"article": article}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshaling payload: %w", err)
	}

	reqURL := fmt.Sprintf("%s/articles/%d", helpjuiceBaseURL(), p.ID)
	body, err := helpjuiceRequest(http.MethodPut, reqURL, payloadBytes)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// ── HTTP helper ─────────────────────────────────────────────────────────

func helpjuiceRequest(method, reqURL string, payload []byte) ([]byte, error) {
	var bodyReader io.Reader
	if payload != nil {
		bodyReader = bytes.NewReader(payload)
	}

	//log.Printf("helpjuiceRequest: %s %s", method, reqURL)
	//log.Printf("payload: %s", string(payload))

	req, err := http.NewRequest(method, reqURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Authorization", config.HelpjuiceAPIKey)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("helpjuice request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("helpjuice API error (HTTP %d): %s", resp.StatusCode, string(body))
	}

	return body, nil
}
