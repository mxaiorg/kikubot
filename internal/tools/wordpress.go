package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"kikubot/internal/config"
)

// ── Gutenberg WordPress Tools ──────────────────────────────────────────────────────────

/*
This tool is designed to be used with WordPress. It provides detailed REST API
instructions for interacting with WordPress. It is specifically designed
for use with Gutenberg block-based WordPress sites.

To configure, be sure to set the following environment variables:
- WEBSITE_URL
- WORDPRESS_USER
- WORDPRESS_PASSWORD
The user should use an "Application Password". (Application Passwords are generated from the WordPress dashboard under Users > Your Profile > Application Passwords. They provide secure access to the REST API without exposing your main account credentials.)

The bash_exec tool is also added as it is required for WordPress tasks.

# Credential handling

The credentials are never interpolated into the tool result (and therefore
never enter the model context, the persisted thread memory, or the LLM
provider's logs). Instead the returned workflow has bash_exec compute the
Authorization header from the environment at execution time — bash_exec
inherits the container env, so a rotated application password takes effect
on the next call with no stale copies fossilised in saved history.

Before handing out the workflow at all, executeWordPress preflights the
credentials from Go (GET /wp-json/wp/v2/users/me). WordPress silently
downgrades an invalid or revoked application password to the anonymous
user, so a dead credential otherwise surfaces downstream as confusing
401 rest_cannot_create errors that the model then tries to "fix" with
alternative auth methods — a retry storm that burns the whole turn
budget. A failed preflight returns a terminal instruction to report and
stop, and the model never receives a workflow to experiment with.

# Runtime image requirements

The workflow returned by executeWordPress is shell-driven and assumes the
following binaries are present in the runtime image. Adding `wordpress` to
an agent's tool set without these will fail at runtime — bash_exec returns
`bash: not found` (or `curl: not found`) and the agent has no working path
to update the site:

  - bash    — executeBash invokes /bin/bash directly, not /bin/sh
  - curl    — used for every REST call in the workflow
  - jq      — used to extract .content.raw and to build update payloads

On Alpine, install with:

  apk add --no-cache bash curl jq

The base Dockerfile (./Dockerfile) installs these. Any image that strips
them down — minimal/scratch builds, or other agent images that get added
later — must keep them, or the wordpress tool's prescribed workflow won't
run.

When adding new shell-driven tools to this codebase, document their
runtime dependencies in a comment like this one and update the Dockerfile
in the same change. The agent has no way to recover from "binary missing"
beyond reporting it back to the user, so the dependency surface needs to
be tracked in the source tree, not discovered in production.
*/

// WordPressTool returns a lightweight tool that the model calls when it needs
// to interact with WordPress. The Execute function returns detailed REST API
// instructions on demand, keeping them out of the system prompt for unrelated
// requests. BashTool is also included as it is required.
func WordPressTool() []ToolDefinition {
	var tools []ToolDefinition
	tools = append(tools, ToolDefinition{
		Name:        "wordpress_tool",
		Description: "Call this tool to get WordPress REST API instructions and credentials. Call this BEFORE using bash_exec for any WordPress task.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"task": {
					"type": "string",
					"description": "Brief description of the WordPress task you need to perform (e.g. 'update news page', 'create new post')"
				}
			},
			"required": ["task"]
		}`),
		Execute:      executeWordPress,
		StaticSystem: "- The Website you have access to is the staging server at https://alexp270.sg-host.com",
	})

	tools = append(tools, BashTool())

	return tools
}

// wordpressPreflight verifies that the configured credentials actually
// authenticate against the site. Returns "" when auth is good; otherwise a
// terminal instruction for the model (report the problem and stop). The
// result is deliberately not cached: the check is one cheap GET per
// wordpress_tool call, and a live credential rotation should take effect
// immediately.
func wordpressPreflight(ctx context.Context, site, user, pass string) string {
	const stopDirective = "This is an operator/configuration problem, not something you can work around. " +
		"Do NOT retry, do NOT attempt alternative authentication methods (no -u, no wp-login.php, no cookies, no nonce), " +
		"and do NOT use bash_exec against the site. Report the problem to the requester via message_tool (or report_tool if available) and stop."

	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	url := strings.TrimRight(site, "/") + "/wp-json/wp/v2/users/me"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Sprintf("WordPress preflight failed: cannot build request for %s (%v). %s", site, err, stopDirective)
	}
	req.SetBasicAuth(user, pass)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Sprintf("WordPress preflight failed: %s is unreachable (%v). %s", site, err, stopDirective)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	// WordPress answers /users/me with the authenticated user's record. An
	// invalid application password is NOT rejected outright — the request is
	// treated as anonymous and answered with 401 rest_not_logged_in — so a
	// 200 with a real user id is the only acceptable outcome.
	var me struct {
		ID int `json:"id"`
	}
	if resp.StatusCode == http.StatusOK && json.Unmarshal(body, &me) == nil && me.ID > 0 {
		return ""
	}
	return fmt.Sprintf("WordPress credential check failed: GET /wp-json/wp/v2/users/me returned HTTP %d (%s). "+
		"The configured application password is invalid or has been revoked — WordPress is treating requests as anonymous, "+
		"so every write would fail with 401 rest_cannot_create. %s",
		resp.StatusCode, strings.TrimSpace(string(body)), stopDirective)
}

func executeWordPress(ctx context.Context, _ json.RawMessage) (string, error) {
	if config.WebSiteUrl == "" || config.WordPressUser == "" || config.WordPressPassword == "" {
		return "WordPress is not configured: one or more of WEBSITE_URL, WORDPRESS_USER, WORDPRESS_PASSWORD is missing from the environment. " +
			"This is an operator problem — report it to the requester via message_tool (or report_tool if available) and stop. " +
			"Do NOT attempt to reach the site by other means.", nil
	}

	if msg := wordpressPreflight(ctx, config.WebSiteUrl, config.WordPressUser, config.WordPressPassword); msg != "" {
		return msg, nil // non-fatal to the agent — surfaced as guidance, not an Execute error
	}

	// The credentials are referenced via the environment (inherited by
	// bash_exec), never inlined here — see the file comment.
	return fmt.Sprintf(`WordPress REST API — use bash_exec with curl.

Start EVERY bash_exec command with these two lines, exactly as written (the
credentials live in the environment; they are correct and pre-verified):

AUTH=$(printf '%%s:%%s' "$WORDPRESS_USER" "$WORDPRESS_PASSWORD" | base64 | tr -d '\n')
URL=%s

ABSOLUTE CONSTRAINTS (violating any of these is a critical error):
1. Auth: ONLY use -H "Authorization: Basic $AUTH" with $AUTH computed exactly as above. No -u, no --user, no wp-cli, no cookies, no nonce, no PUT method.
2. NEVER print, echo, decode, or otherwise expose $AUTH, $WORDPRESS_USER, or $WORDPRESS_PASSWORD in command output.
3. Tools: ONLY use curl, jq, and bash builtins. No python3, no find, no which.
4. ON ANY ERROR (401, 403, 500, etc.): STOP IMMEDIATELY. Do NOT retry with different auth. Do NOT try alternative methods. Report the error to the user with report_tool and move on.

CRITICAL — GUTENBERG BLOCK FORMAT:
This WordPress site uses the Gutenberg block editor. Content is stored as block markup
with comment delimiters like <!-- wp:group -->, <!-- wp:paragraph -->, etc.
- You MUST use context=edit to get the RAW block markup via .content.raw
- You MUST preserve all <!-- wp:... --> block comment delimiters exactly as they are.
- NEVER use .content.rendered — that is lossy HTML output that destroys block structure.
- When inserting new content, wrap it in proper block comments. Example paragraph:
  <!-- wp:paragraph -->
  <p><a href="https://example.com" target="_blank" rel="noreferrer noopener">Link text</a></p>
  <!-- /wp:paragraph -->

WORKFLOW (2-3 bash_exec calls total):

1. GET current raw block content:
   curl -s -H "Authorization: Basic $AUTH" "$URL/wp-json/wp/v2/pages/{id}?context=edit" | jq -r '.content.raw' > /tmp/wp_current.html
   head -30 /tmp/wp_current.html

2. BUILD payload and UPDATE in one bash_exec call:
   # Modify /tmp/wp_current.html with sed/bash to insert/edit content, then:
   jq -n --rawfile content /tmp/wp_current.html '{"content": $content}' > /tmp/wp_payload.json
   curl -s -H "Authorization: Basic $AUTH" -X POST "$URL/wp-json/wp/v2/pages/{id}" -H "Content-Type: application/json" -d @/tmp/wp_payload.json | jq '{id, status, modified}'

OTHER ENDPOINTS:
  List pages: curl -s -H "Authorization: Basic $AUTH" "$URL/wp-json/wp/v2/pages?per_page=50" | jq '.[] | {id, slug, title: .title.rendered}'
  List posts: curl -s -H "Authorization: Basic $AUTH" "$URL/wp-json/wp/v2/posts?per_page=50"
  Create post: curl -s -H "Authorization: Basic $AUTH" -X POST "$URL/wp-json/wp/v2/posts" -H "Content-Type: application/json" -d @/tmp/wp_payload.json

UPLOADING AN INBOUND ATTACHMENT TO THE MEDIA LIBRARY:
  If the user (or a coworker via X-Forwarded) attached an image/file you need to host, first
  materialise it to disk with save_attachment (the bytes are NOT readable from the inline
  image block you see in the conversation), then POST it:
    curl -s -H "Authorization: Basic $AUTH" -H "Content-Disposition: attachment; filename=NAME.png" \
         -H "Content-Type: image/png" --data-binary @/tmp/NAME.png "$URL/wp-json/wp/v2/media" | jq '{id, source_url}'
  Use the returned source_url anywhere a public image URL is required (Buffer drafts, post bodies, etc.).
`,
		config.WebSiteUrl), nil
}
