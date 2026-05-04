package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"kikubot/internal/config"
)

// ── Gutenberg WordPress Tools ──────────────────────────────────────────────────────────

/*
This tool is designed to be used with WordPress. It provides detailed REST API
instructions and credentials for interacting with WordPress. It is specifically designed
for use with Gutenberg block-based WordPress sites.

To configure, be sure to set the following environment variables:
- WEBSITE_URL
- WORDPRESS_USER
- WORDPRESS_PASSWORD
The user should use an "Application Password". (Application Passwords are generated from the WordPress dashboard under Users > Your Profile > Application Passwords. They provide secure access to the REST API without exposing your main account credentials.)

The bash_exec tool is also added as it is required for WordPress tasks.

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
// instructions and credentials on demand, keeping them out of the system
// prompt for unrelated requests. BashTool is also included as it is required.
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

func executeWordPress(_ context.Context, _ json.RawMessage) (string, error) {
	// Pre-encode credentials to avoid shell-escaping issues with special characters in passwords.
	authHeader := base64.StdEncoding.EncodeToString(
		[]byte(config.WordPressUser + ":" + config.WordPressPassword),
	)

	return fmt.Sprintf(`WordPress REST API — use bash_exec with curl.

AUTH=%s
URL=%s

ABSOLUTE CONSTRAINTS (violating any of these is a critical error):
1. Auth: ONLY use -H "Authorization: Basic $AUTH". No -u, no --user, no wp-cli, no cookies, no nonce, no PUT method.
2. Tools: ONLY use curl, jq, and bash builtins. No python3, no find, no which.
3. ON ANY ERROR (401, 403, 500, etc.): STOP IMMEDIATELY. Do NOT retry with different auth. Do NOT try alternative methods. Report the error to the user with report_tool and move on.

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
`,
		authHeader,
		config.WebSiteUrl), nil
}
