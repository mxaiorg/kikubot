package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"kikubot/internal/config"
	"log"
	"net/http"
	"net/url"
	"strings"
)

// ── Vimeo API Tools ─────────────────────────────────────────────────────
//
// Read-only REST client for the Vimeo API (https://developer.vimeo.com/api/reference)
// against the authenticated account identified by config.VimeoApiKey.
//
// Tools:
//   - vimeo_search          — search/list videos in the authenticated account
//   - vimeo_get_video       — fetch a single video's details by ID
//   - vimeo_list_showcases  — list the account's showcases (albums)
//   - vimeo_showcase_videos — list videos within a showcase (album)

const (
	vimeoBaseURL = "https://api.vimeo.com"
	vimeoAccept  = "application/vnd.vimeo.*+json;version=3.4"

	// Compact field selection — keeps responses small enough for the LLM
	// while still surfacing the bits useful for sharing or describing a video.
	vimeoVideoFields = "uri,name,description,duration,created_time,modified_time," +
		"link,player_embed_url,pictures.base_link,stats.plays,tags.name,privacy.view"

	vimeoShowcaseFields = "uri,name,description,created_time,modified_time," +
		"link,metadata.connections.videos.total,pictures.base_link,privacy.view"
)

func Vimeo() []ToolDefinition {
	if strings.TrimSpace(config.VimeoApiKey) == "" {
		log.Println("[vimeo] VIMEO_API_KEY not set — Vimeo tools disabled")
		return nil
	}
	log.Println("[vimeo] REST client initialized")

	return []ToolDefinition{
		vimeoSearchTool(),
		vimeoGetVideoTool(),
		vimeoListShowcasesTool(),
		vimeoShowcaseVideosTool(),
	}
}

// ── vimeo_search ────────────────────────────────────────────────────────

func vimeoSearchTool() ToolDefinition {
	return ToolDefinition{
		Name: "vimeo_search",
		Description: "Search or list videos in the authenticated Vimeo account. " +
			"Returns video IDs, titles, descriptions, duration, share links, " +
			"embed URLs, thumbnails, play counts, and tags. " +
			"Omit the query to list all videos in date order.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"query": {
					"type": "string",
					"description": "Optional full-text search term. If omitted, lists videos sorted by the chosen sort field."
				},
				"sort": {
					"type": "string",
					"enum": ["default", "alphabetical", "date", "duration", "last_user_action_event_date", "modified_time", "plays", "likes", "comments"],
					"description": "How to order results. Default is by relevance when query is set, by date otherwise."
				},
				"direction": {
					"type": "string",
					"enum": ["asc", "desc"],
					"description": "Sort direction (default desc)."
				},
				"page": {
					"type": "integer",
					"description": "Page number (1-indexed, default 1)."
				},
				"per_page": {
					"type": "integer",
					"description": "Results per page, max 100 (default 25)."
				}
			}
		}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var p struct {
				Query     string `json:"query"`
				Sort      string `json:"sort"`
				Direction string `json:"direction"`
				Page      int    `json:"page"`
				PerPage   int    `json:"per_page"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", fmt.Errorf("parsing input: %w", err)
			}

			q := url.Values{}
			q.Set("fields", vimeoVideoFields)
			if p.Query != "" {
				q.Set("query", p.Query)
			}
			if p.Sort != "" {
				q.Set("sort", p.Sort)
			}
			if p.Direction != "" {
				q.Set("direction", p.Direction)
			}
			if p.Page > 0 {
				q.Set("page", fmt.Sprintf("%d", p.Page))
			}
			if p.PerPage > 0 {
				if p.PerPage > 100 {
					p.PerPage = 100
				}
				q.Set("per_page", fmt.Sprintf("%d", p.PerPage))
			}

			body, err := vimeoRequest(ctx, http.MethodGet, "/me/videos?"+q.Encode())
			if err != nil {
				return "", err
			}
			return string(body), nil
		},
		StaticSystem: "- Vimeo video URIs are of the form `/videos/<id>`; the numeric `<id>` is what other Vimeo tools accept.\n" +
			"- Prefer the `link` field when sharing a video with users; use `player_embed_url` only for embedding.\n",
	}
}

// ── vimeo_get_video ─────────────────────────────────────────────────────

func vimeoGetVideoTool() ToolDefinition {
	return ToolDefinition{
		Name: "vimeo_get_video",
		Description: "Fetch full metadata for a single Vimeo video by ID or URI. " +
			"Returns title, description, duration, share link, embed URL, thumbnail, " +
			"privacy settings, and play stats.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"video_id": {
					"type": "string",
					"description": "The Vimeo video ID (e.g. \"123456789\") or full URI (\"/videos/123456789\")."
				}
			},
			"required": ["video_id"]
		}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var p struct {
				VideoID string `json:"video_id"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", fmt.Errorf("parsing input: %w", err)
			}
			id := normalizeVimeoID(p.VideoID)
			if id == "" {
				return "", fmt.Errorf("video_id is required")
			}

			path := fmt.Sprintf("/videos/%s?fields=%s", url.PathEscape(id), url.QueryEscape(vimeoVideoFields))
			body, err := vimeoRequest(ctx, http.MethodGet, path)
			if err != nil {
				return "", err
			}
			return string(body), nil
		},
	}
}

// ── vimeo_list_showcases ────────────────────────────────────────────────

func vimeoListShowcasesTool() ToolDefinition {
	return ToolDefinition{
		Name: "vimeo_list_showcases",
		Description: "List the authenticated account's showcases (albums). " +
			"Useful for discovering curated groups of videos before drilling into them with vimeo_showcase_videos.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"query": {
					"type": "string",
					"description": "Optional search term to filter showcases by name."
				},
				"page": {
					"type": "integer",
					"description": "Page number (1-indexed, default 1)."
				},
				"per_page": {
					"type": "integer",
					"description": "Results per page, max 100 (default 25)."
				}
			}
		}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var p struct {
				Query   string `json:"query"`
				Page    int    `json:"page"`
				PerPage int    `json:"per_page"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", fmt.Errorf("parsing input: %w", err)
			}

			q := url.Values{}
			q.Set("fields", vimeoShowcaseFields)
			if p.Query != "" {
				q.Set("query", p.Query)
			}
			if p.Page > 0 {
				q.Set("page", fmt.Sprintf("%d", p.Page))
			}
			if p.PerPage > 0 {
				if p.PerPage > 100 {
					p.PerPage = 100
				}
				q.Set("per_page", fmt.Sprintf("%d", p.PerPage))
			}

			body, err := vimeoRequest(ctx, http.MethodGet, "/me/albums?"+q.Encode())
			if err != nil {
				return "", err
			}
			return string(body), nil
		},
	}
}

// ── vimeo_showcase_videos ───────────────────────────────────────────────

func vimeoShowcaseVideosTool() ToolDefinition {
	return ToolDefinition{
		Name:        "vimeo_showcase_videos",
		Description: "List the videos contained in a Vimeo showcase (album) by showcase ID or URI.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"showcase_id": {
					"type": "string",
					"description": "The showcase (album) ID (e.g. \"12345678\") or full URI (\"/albums/12345678\")."
				},
				"query": {
					"type": "string",
					"description": "Optional full-text search within this showcase."
				},
				"sort": {
					"type": "string",
					"enum": ["default", "alphabetical", "date", "duration", "modified_time", "plays", "likes", "comments", "added_first", "added_last", "manual"],
					"description": "How to order results."
				},
				"direction": {
					"type": "string",
					"enum": ["asc", "desc"],
					"description": "Sort direction (default desc)."
				},
				"page": {
					"type": "integer",
					"description": "Page number (1-indexed, default 1)."
				},
				"per_page": {
					"type": "integer",
					"description": "Results per page, max 100 (default 25)."
				}
			},
			"required": ["showcase_id"]
		}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var p struct {
				ShowcaseID string `json:"showcase_id"`
				Query      string `json:"query"`
				Sort       string `json:"sort"`
				Direction  string `json:"direction"`
				Page       int    `json:"page"`
				PerPage    int    `json:"per_page"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", fmt.Errorf("parsing input: %w", err)
			}
			id := normalizeShowcaseID(p.ShowcaseID)
			if id == "" {
				return "", fmt.Errorf("showcase_id is required")
			}

			q := url.Values{}
			q.Set("fields", vimeoVideoFields)
			if p.Query != "" {
				q.Set("query", p.Query)
			}
			if p.Sort != "" {
				q.Set("sort", p.Sort)
			}
			if p.Direction != "" {
				q.Set("direction", p.Direction)
			}
			if p.Page > 0 {
				q.Set("page", fmt.Sprintf("%d", p.Page))
			}
			if p.PerPage > 0 {
				if p.PerPage > 100 {
					p.PerPage = 100
				}
				q.Set("per_page", fmt.Sprintf("%d", p.PerPage))
			}

			path := fmt.Sprintf("/me/albums/%s/videos?%s", url.PathEscape(id), q.Encode())
			body, err := vimeoRequest(ctx, http.MethodGet, path)
			if err != nil {
				return "", err
			}
			return string(body), nil
		},
	}
}

// ── HTTP helper ─────────────────────────────────────────────────────────

func vimeoRequest(ctx context.Context, method, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, vimeoBaseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+config.VimeoApiKey)
	req.Header.Set("Accept", vimeoAccept)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vimeo request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("vimeo API error (HTTP %d): %s", resp.StatusCode, string(body))
	}
	return body, nil
}

// normalizeVimeoID accepts either "12345678" or "/videos/12345678" and
// returns just the numeric ID portion.
func normalizeVimeoID(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "/videos/")
	s = strings.TrimPrefix(s, "videos/")
	return s
}

// normalizeShowcaseID accepts either "12345678" or "/albums/12345678".
func normalizeShowcaseID(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "/albums/")
	s = strings.TrimPrefix(s, "albums/")
	return s
}
