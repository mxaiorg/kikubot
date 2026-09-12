package tools

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"kikubot/internal/config"

	"github.com/mark3labs/mcp-go/client/transport"
)

// mcpRegistered tracks the registry keys that came from the mcp_servers table,
// so a later RegisterMCPServers (hot reload) can drop the ones that no longer
// exist. Guarded by mcpRegisteredMu; the registry maps themselves have their
// own lock.
var (
	mcpRegisteredMu sync.Mutex
	mcpRegistered   = map[string]bool{}
)

// RegisterMCPServers turns the declarative `mcp_servers:` table from
// mcp_servers.yaml into registry entries — one tool key per server — so adding
// a remote MCP (static-bearer or OAuth2) is config-only, with no new Go. Call
// at startup, after config.Apply and InitOAuthDir, and before initAgent so the
// keys are resolvable when an agent's tools are assembled.
//
// The call has replace semantics: keys registered by a previous call that are
// absent from (or invalid in) the new table are unregistered, so the registry
// always mirrors the current file. That is what lets a running agent pick up
// mcp_servers.yaml edits on hot reload. Local/static registry keys are never
// touched — a table row cannot shadow a built-in tool key.
//
// A misconfigured entry (unknown auth type, missing required env names) is
// logged and skipped rather than fatal, so one broken row can't take the agent
// down. A valid entry whose credentials are simply unset at runtime registers
// fine but yields no tools (also logged) — same graceful-degradation contract
// as the hand-written MCP factories.
func RegisterMCPServers(servers []config.MCPServer) {
	mcpRegisteredMu.Lock()
	defer mcpRegisteredMu.Unlock()

	next := make(map[string]bool, len(servers))
	for _, srv := range servers {
		key := strings.TrimSpace(srv.Key)
		if key == "" || strings.TrimSpace(srv.URL) == "" {
			log.Printf("[mcp] skipping mcp_servers entry with empty key/url: %+v", srv)
			continue
		}
		if HasTool(key) && !mcpRegistered[key] {
			log.Printf("[mcp] %s: key collides with a built-in tool — row ignored", key)
			continue
		}
		factory, err := mcpServerFactory(srv)
		if err != nil {
			log.Printf("[mcp] %s: %v — tool will be unavailable", key, err)
			continue
		}
		Register(key, factory, strings.TrimSpace(srv.Description))
		next[key] = true
	}
	for key := range mcpRegistered {
		if !next[key] {
			Unregister(key)
			log.Printf("[mcp] %s: removed from mcp_servers.yaml — tool unregistered", key)
		}
	}
	mcpRegistered = next
}

// mcpServerFactory builds the registry factory for one server entry. The auth
// mode is validated here (errors abort registration of just this key); the
// returned closure is what runs at tool-assembly time and degrades to nil tools
// if credentials are missing.
func mcpServerFactory(srv config.MCPServer) (func() []ToolDefinition, error) {
	name := strings.TrimSpace(srv.Key)
	url := strings.TrimSpace(srv.URL)

	switch strings.ToLower(strings.TrimSpace(srv.Auth)) {
	case "", "none":
		return func() []ToolDefinition {
			t, err := MCPBridge(name, url, "")
			return mcpBridgeOrLog(name, t, err)
		}, nil

	case "bearer", "static", "apikey", "api_key", "api-key":
		tokenEnv := strings.TrimSpace(srv.TokenEnv)
		if tokenEnv == "" {
			return nil, fmt.Errorf("auth=%q requires token_env", srv.Auth)
		}
		header, scheme := staticHeaderDefaults(srv)
		return func() []ToolDefinition {
			token := strings.TrimSpace(os.Getenv(tokenEnv))
			if token == "" {
				log.Printf("[mcp] %s: %s is empty — tool unavailable", name, tokenEnv)
				return nil
			}
			// "<scheme> <token>" with a scheme, or the raw token without one.
			// TrimSpace collapses the leading gap when scheme is empty.
			value := strings.TrimSpace(scheme + " " + token)
			t, err := MCPBridgeHeaders(name, url, map[string]string{header: value})
			return mcpBridgeOrLog(name, t, err)
		}, nil

	case "oauth2", "oauth":
		clientIDEnv := strings.TrimSpace(srv.ClientIDEnv)
		if clientIDEnv == "" {
			return nil, fmt.Errorf("auth=%q requires client_id_env", srv.Auth)
		}
		clientSecretEnv := strings.TrimSpace(srv.ClientSecretEnv)
		metadataURL := strings.TrimSpace(srv.MetadataURL)
		return func() []ToolDefinition {
			clientID := strings.TrimSpace(os.Getenv(clientIDEnv))
			if clientID == "" {
				log.Printf("[mcp] %s: %s is empty — tool unavailable", name, clientIDEnv)
				return nil
			}
			cfg := transport.OAuthConfig{
				ClientID:              clientID,
				ClientSecret:          strings.TrimSpace(os.Getenv(clientSecretEnv)),
				TokenStore:            NewFileTokenStore(name),
				AuthServerMetadataURL: metadataURL,
			}
			t, err := MCPBridgeOAuth(name, url, cfg)
			return mcpBridgeOrLog(name, t, err)
		}, nil

	default:
		return nil, fmt.Errorf("unknown auth type %q (want none|bearer|apikey|oauth2)", srv.Auth)
	}
}

// staticHeaderDefaults resolves the request header name and scheme prefix for a
// static-header auth mode (bearer/apikey), applying mode-aware defaults that an
// explicit header:/scheme: in the table overrides.
//
//	bearer → Authorization: Bearer <token>
//	apikey → Authorization: ApiKey <token>   (default header)
//	apikey + custom header → "<Header>: <token>"  (raw token, no scheme)
func staticHeaderDefaults(srv config.MCPServer) (header, scheme string) {
	header = strings.TrimSpace(srv.Header)
	if header == "" {
		header = "Authorization"
	}
	isAuthorization := strings.EqualFold(header, "Authorization")

	scheme = strings.TrimSpace(srv.Scheme)
	if srv.Scheme == "" { // unset (not just empty after trim) → apply mode default
		switch strings.ToLower(strings.TrimSpace(srv.Auth)) {
		case "apikey", "api_key", "api-key":
			if isAuthorization {
				scheme = "ApiKey"
			} // custom header → raw token, no scheme
		default: // bearer / static
			scheme = "Bearer"
		}
	}
	return header, scheme
}

// mcpBridgeOrLog unwraps a bridge result, logging (and dropping to nil) on
// error so a transient connect failure at startup doesn't crash tool assembly.
func mcpBridgeOrLog(name string, tools []ToolDefinition, err error) []ToolDefinition {
	if err != nil {
		log.Printf("[mcp] %s bridge error: %v", name, err)
		return nil
	}
	return tools
}
