package main

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"kikubot/internal/config"

	"gopkg.in/yaml.v3"
)

// mcpServersPath returns the configs/mcp_servers.yaml path under root — the
// remote (Streamable HTTP) MCP catalog the runtime loads independently of
// agents.yaml.
func mcpServersPath(root string) string {
	return filepath.Join(root, "configs", "mcp_servers.yaml")
}

// loadMCPServers reads the catalog. A missing file yields an empty list (a
// deployment may simply have no remote MCPs); only a parse error is returned.
func loadMCPServers(root string) ([]config.MCPServer, error) {
	return config.LoadMCPServers(mcpServersPath(root))
}

// mcpServersHeader is written above the generated YAML. The encoder drops any
// comments from the previous file, so the pointer to the documented example is
// re-emitted on every save.
const mcpServersHeader = `# mcp_servers.yaml — remote (Streamable HTTP) MCP servers, exposed to agents by key.
# Managed by the configurator (MCP Servers page). Field reference:
# configs/mcp_servers-example.yaml. Secrets are referenced by env-var NAME only;
# the values live in configs/secrets.env.
`

// saveMCPServers writes the catalog back to configs/mcp_servers.yaml. The file
// is rewritten in place (truncate, not rename) on purpose: docker-compose
// bind-mounts this file into every agent container, and a rename would leave
// the mount pointing at the old inode.
func saveMCPServers(root string, servers []config.MCPServer) error {
	path := mcpServersPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fsWriteError(filepath.Dir(path), err)
	}
	if servers == nil {
		servers = []config.MCPServer{} // render `mcp_servers: []`, not `null`
	}
	var buf strings.Builder
	buf.WriteString(mcpServersHeader)
	enc := yaml.NewEncoder(&strBuf{&buf})
	enc.SetIndent(2)
	if err := enc.Encode(config.MCPServersConfig{MCPServers: servers}); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return fsWriteError(path, os.WriteFile(path, []byte(buf.String()), 0o644))
}

// ensureMCPServersFile creates an empty catalog when none exists. The generated
// docker-compose.yml now mounts the whole configs/ directory, but older
// generated files bind-mount this file directly; if the host side were missing,
// Docker would create a directory in its place instead.
func ensureMCPServersFile(root string) error {
	_, err := os.Stat(mcpServersPath(root))
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return saveMCPServers(root, nil)
}

// mcpAuthModes are the canonical auth values offered by the form.
var mcpAuthModes = []string{"none", "bearer", "apikey", "oauth2"}

// normalizeMCPAuth folds the aliases the runtime accepts (static, api_key,
// oauth, …) onto the canonical form value. Unknown values pass through
// lowercased so validate can reject them.
func normalizeMCPAuth(a string) string {
	switch v := strings.ToLower(strings.TrimSpace(a)); v {
	case "", "none":
		return "none"
	case "bearer", "static":
		return "bearer"
	case "apikey", "api_key", "api-key":
		return "apikey"
	case "oauth2", "oauth":
		return "oauth2"
	default:
		return v
	}
}

// mcpHeaderPreview renders the request header a static-auth server will
// receive, mirroring the runtime's defaults (internal/tools/mcp_servers.go):
//
//	bearer → Authorization: Bearer <token>
//	apikey → Authorization: ApiKey <token>; custom header → raw token
func mcpHeaderPreview(s config.MCPServer) string {
	auth := normalizeMCPAuth(s.Auth)
	if auth != "bearer" && auth != "apikey" {
		return ""
	}
	header := strings.TrimSpace(s.Header)
	if header == "" {
		header = "Authorization"
	}
	scheme := strings.TrimSpace(s.Scheme)
	if s.Scheme == "" {
		switch {
		case auth == "apikey" && strings.EqualFold(header, "Authorization"):
			scheme = "ApiKey"
		case auth == "bearer":
			scheme = "Bearer"
		}
	}
	return strings.TrimSpace(header + ": " + strings.TrimSpace(scheme+" <token>"))
}

// mcpEnvNames lists the secrets.env variables a server references for its
// active auth mode.
func mcpEnvNames(s config.MCPServer) []string {
	var names []string
	add := func(n string) {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	switch normalizeMCPAuth(s.Auth) {
	case "bearer", "apikey":
		add(s.TokenEnv)
	case "oauth2":
		add(s.ClientIDEnv)
		add(s.ClientSecretEnv)
	}
	return names
}

// agentRef identifies an in-roster agent for display/links.
type agentRef struct {
	Name  string
	Email string
}

// agentsUsingKey returns the roster agents whose tools: list includes key.
func agentsUsingKey(r *roster, key string) []agentRef {
	var out []agentRef
	for _, a := range r.Agents {
		for _, t := range a.Tools {
			if strings.TrimSpace(t) == key {
				name := strings.TrimSpace(a.Name)
				if name == "" {
					name = a.Email
				}
				out = append(out, agentRef{Name: name, Email: a.Email})
				break
			}
		}
	}
	return out
}

func agentNames(refs []agentRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Name)
	}
	return out
}

func agentEmails(refs []agentRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Email)
	}
	return out
}

// oauthTokenMissing returns the agents in `users` that have no seeded OAuth
// token file for key. The runtime reads data/oauth/<key>.json inside the
// container, which docker-compose maps to ./data/<stem>/oauth/<key>.json on
// the host.
func oauthTokenMissing(root, key string, users []agentRef) []string {
	var missing []string
	for _, u := range users {
		p := filepath.Join(root, "data", emailStem(u.Email), "oauth", key+".json")
		if _, err := os.Stat(p); err != nil {
			missing = append(missing, u.Name)
		}
	}
	return missing
}

// mcpSummary is a row in the MCP servers list.
type mcpSummary struct {
	Key         string
	URL         string
	Auth        string
	Description string
	Header      string   // effective auth header preview (static modes only)
	EnvNames    []string // secrets.env variables this row references
	MissingEnv  []string // those with no (non-empty) value in secrets.env
	UsedBy      []agentRef
	// TokenMissing (oauth2 only) names agents using this server that lack a
	// seeded token file — the tool will be unavailable to them until seeded.
	TokenMissing []string
}

// listMCPServers returns the catalog annotated with credential, usage and
// OAuth-seed status, sorted by key.
func listMCPServers(root string) ([]mcpSummary, error) {
	servers, err := loadMCPServers(root)
	if err != nil {
		return nil, err
	}
	r, err := loadRoster(root)
	if err != nil {
		return nil, err
	}
	secrets := loadSecrets(root)
	out := make([]mcpSummary, 0, len(servers))
	for _, s := range servers {
		key := strings.TrimSpace(s.Key)
		if key == "" {
			continue
		}
		row := mcpSummary{
			Key:         key,
			URL:         strings.TrimSpace(s.URL),
			Auth:        normalizeMCPAuth(s.Auth),
			Description: strings.TrimSpace(s.Description),
			Header:      mcpHeaderPreview(s),
			EnvNames:    mcpEnvNames(s),
			UsedBy:      agentsUsingKey(r, key),
		}
		for _, n := range row.EnvNames {
			if v, _ := secrets.Get(n); strings.TrimSpace(v) == "" {
				row.MissingEnv = append(row.MissingEnv, n)
			}
		}
		if row.Auth == "oauth2" {
			row.TokenMissing = oauthTokenMissing(root, key, row.UsedBy)
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// mcpForm captures the user-submitted state of the Add/Edit MCP Server form.
// The *Env fields are env-var names (stored in mcp_servers.yaml); the *Value
// fields are the secrets themselves (stored in secrets.env under those names).
type mcpForm struct {
	OriginalKey string // empty for a new server

	Key             string
	URL             string
	Auth            string
	Description     string
	Header          string // bearer/apikey
	Scheme          string // bearer/apikey
	TokenEnv        string // bearer/apikey
	ClientIDEnv     string // oauth2
	ClientSecretEnv string // oauth2
	MetadataURL     string // oauth2

	// Secret values. Blank leaves the existing secrets.env entry untouched.
	TokenValue        string
	ClientIDValue     string
	ClientSecretValue string

	// Render-only.
	UsedBy       []agentRef `json:"-"`
	TokenMissing []string   `json:"-"`
}

var mcpKeyRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// validate runs input checks. Collision with a built-in registry key is
// checked against the parsed tool registry when the source tree is available;
// the runtime ignores such a row, so it's better caught here.
func (f *mcpForm) validate(root string) error {
	f.Key = strings.TrimSpace(f.Key)
	f.URL = strings.TrimSpace(f.URL)
	f.Auth = normalizeMCPAuth(f.Auth)
	if f.Key == "" {
		return fmt.Errorf("key is required")
	}
	if !mcpKeyRe.MatchString(f.Key) {
		return fmt.Errorf("key may only contain letters, digits, '_' and '-' (it prefixes the server's tool names)")
	}
	if f.URL == "" {
		return fmt.Errorf("URL is required")
	}
	u, err := url.Parse(f.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("URL must be an absolute http(s) URL")
	}
	validAuth := false
	for _, m := range mcpAuthModes {
		if f.Auth == m {
			validAuth = true
		}
	}
	if !validAuth {
		return fmt.Errorf("auth must be one of %s", strings.Join(mcpAuthModes, ", "))
	}
	envName := func(label, v string) error {
		v = strings.TrimSpace(v)
		if v == "" {
			return nil
		}
		if !isValidKey(v) {
			return fmt.Errorf("%s must be an environment variable name (letters, digits, '_'), got %q", label, v)
		}
		return nil
	}
	switch f.Auth {
	case "bearer", "apikey":
		if strings.TrimSpace(f.TokenEnv) == "" {
			return fmt.Errorf("auth %s requires a token env var name", f.Auth)
		}
		if err := envName("token env var", f.TokenEnv); err != nil {
			return err
		}
		if h := strings.TrimSpace(f.Header); h != "" && strings.ContainsAny(h, " :\r\n") {
			return fmt.Errorf("header must be a bare header name such as X-Api-Key")
		}
	case "oauth2":
		if strings.TrimSpace(f.ClientIDEnv) == "" {
			return fmt.Errorf("auth oauth2 requires a client ID env var name")
		}
		for label, v := range map[string]string{"client ID env var": f.ClientIDEnv, "client secret env var": f.ClientSecretEnv} {
			if err := envName(label, v); err != nil {
				return err
			}
		}
		if m := strings.TrimSpace(f.MetadataURL); m != "" {
			if mu, err := url.Parse(m); err != nil || (mu.Scheme != "http" && mu.Scheme != "https") || mu.Host == "" {
				return fmt.Errorf("metadata URL must be an absolute http(s) URL")
			}
		}
	}
	if infos, err := loadToolRegistry(root); err == nil {
		for _, t := range infos {
			if t.Key == f.Key && !t.MCP {
				return fmt.Errorf("key %q is already a built-in tool key; choose another", f.Key)
			}
		}
	}
	return nil
}

// toServer builds the YAML row, keeping only the fields that apply to the
// selected auth mode so the file stays readable.
func (f *mcpForm) toServer() config.MCPServer {
	s := config.MCPServer{
		Key:         strings.TrimSpace(f.Key),
		URL:         strings.TrimSpace(f.URL),
		Auth:        normalizeMCPAuth(f.Auth),
		Description: strings.TrimSpace(f.Description),
	}
	switch s.Auth {
	case "bearer", "apikey":
		s.TokenEnv = strings.TrimSpace(f.TokenEnv)
		s.Header = strings.TrimSpace(f.Header)
		s.Scheme = strings.TrimSpace(f.Scheme)
	case "oauth2":
		s.ClientIDEnv = strings.TrimSpace(f.ClientIDEnv)
		s.ClientSecretEnv = strings.TrimSpace(f.ClientSecretEnv)
		s.MetadataURL = strings.TrimSpace(f.MetadataURL)
	}
	return s
}

// newMCPForm seeds a blank form.
func newMCPForm() *mcpForm {
	return &mcpForm{Auth: "bearer"}
}

// loadMCPForm fills the form from mcp_servers.yaml, secrets.env and the roster.
func loadMCPForm(root, key string) (*mcpForm, error) {
	servers, err := loadMCPServers(root)
	if err != nil {
		return nil, err
	}
	idx := findMCPIndex(servers, key)
	if idx < 0 {
		return nil, fmt.Errorf("MCP server %q not found in configs/mcp_servers.yaml", key)
	}
	s := servers[idx]
	f := &mcpForm{
		OriginalKey:     strings.TrimSpace(s.Key),
		Key:             strings.TrimSpace(s.Key),
		URL:             s.URL,
		Auth:            normalizeMCPAuth(s.Auth),
		Description:     s.Description,
		Header:          s.Header,
		Scheme:          s.Scheme,
		TokenEnv:        s.TokenEnv,
		ClientIDEnv:     s.ClientIDEnv,
		ClientSecretEnv: s.ClientSecretEnv,
		MetadataURL:     s.MetadataURL,
	}
	secrets := loadSecrets(root)
	f.TokenValue, _ = secrets.Get(strings.TrimSpace(s.TokenEnv))
	f.ClientIDValue, _ = secrets.Get(strings.TrimSpace(s.ClientIDEnv))
	f.ClientSecretValue, _ = secrets.Get(strings.TrimSpace(s.ClientSecretEnv))
	if r, err := loadRoster(root); err == nil {
		f.UsedBy = agentsUsingKey(r, f.Key)
		if f.Auth == "oauth2" {
			f.TokenMissing = oauthTokenMissing(root, f.Key, f.UsedBy)
		}
	}
	return f, nil
}

// save persists the server to configs/mcp_servers.yaml and any non-blank
// secret values to configs/secrets.env. A key rename is propagated to every
// agent's tools: list in agents.yaml so assignments don't silently dangle.
// Returns the agents that reference the (new) key — the ones worth signalling
// to hot-reload.
func (f *mcpForm) save(root string) ([]agentRef, error) {
	if err := f.validate(root); err != nil {
		return nil, err
	}
	servers, err := loadMCPServers(root)
	if err != nil {
		return nil, err
	}
	def := f.toServer()
	for i, s := range servers {
		if strings.TrimSpace(s.Key) == def.Key && (f.OriginalKey == "" || i != findMCPIndex(servers, f.OriginalKey)) {
			return nil, fmt.Errorf("an MCP server with key %q already exists", def.Key)
		}
	}
	r, err := loadRoster(root)
	if err != nil {
		return nil, err
	}
	renamed := false
	if f.OriginalKey != "" && f.OriginalKey != def.Key {
		servers = removeMCP(servers, f.OriginalKey)
		renamed = renameToolKey(r, f.OriginalKey, def.Key)
	}
	servers = upsertMCP(servers, f.OriginalKey, def)
	if err := saveMCPServers(root, servers); err != nil {
		return nil, err
	}
	if renamed {
		if err := saveRoster(root, r); err != nil {
			return nil, fmt.Errorf("catalog saved but updating agents.yaml tool lists failed: %w", err)
		}
	}

	secrets := loadSecrets(root)
	changed := false
	set := func(name, value string) {
		name = strings.TrimSpace(name)
		if name == "" || strings.TrimSpace(value) == "" {
			return
		}
		if cur, _ := secrets.Get(name); cur == value {
			return
		}
		secrets.Set(name, value)
		changed = true
	}
	switch def.Auth {
	case "bearer", "apikey":
		set(def.TokenEnv, f.TokenValue)
	case "oauth2":
		set(def.ClientIDEnv, f.ClientIDValue)
		set(def.ClientSecretEnv, f.ClientSecretValue)
	}
	if changed {
		if err := saveSecrets(root, secrets); err != nil {
			return nil, fmt.Errorf("catalog saved but writing secrets.env failed: %w", err)
		}
	}
	return agentsUsingKey(r, def.Key), nil
}

// deleteMCPServer removes a row from the catalog and strips its key from every
// agent's tools: list. Returns the agents that referenced it.
func deleteMCPServer(root, key string) ([]agentRef, error) {
	key = strings.TrimSpace(key)
	servers, err := loadMCPServers(root)
	if err != nil {
		return nil, err
	}
	if findMCPIndex(servers, key) < 0 {
		return nil, fmt.Errorf("no MCP server with key %q", key)
	}
	r, err := loadRoster(root)
	if err != nil {
		return nil, err
	}
	users := agentsUsingKey(r, key)
	if err := saveMCPServers(root, removeMCP(servers, key)); err != nil {
		return nil, err
	}
	if len(users) > 0 {
		renameToolKey(r, key, "")
		if err := saveRoster(root, r); err != nil {
			return nil, fmt.Errorf("catalog saved but removing the key from agents.yaml failed: %w", err)
		}
	}
	return users, nil
}

// findMCPIndex returns the index of the server with key, or -1.
func findMCPIndex(servers []config.MCPServer, key string) int {
	key = strings.TrimSpace(key)
	for i, s := range servers {
		if strings.TrimSpace(s.Key) == key {
			return i
		}
	}
	return -1
}

// upsertMCP replaces the row keyed originalKey (or def.Key) in place, or
// appends when absent — preserving the file's row order across edits.
func upsertMCP(servers []config.MCPServer, originalKey string, def config.MCPServer) []config.MCPServer {
	if i := findMCPIndex(servers, originalKey); originalKey != "" && i >= 0 {
		servers[i] = def
		return servers
	}
	if i := findMCPIndex(servers, def.Key); i >= 0 {
		servers[i] = def
		return servers
	}
	return append(servers, def)
}

// removeMCP drops the row with key. No-op when absent.
func removeMCP(servers []config.MCPServer, key string) []config.MCPServer {
	if i := findMCPIndex(servers, key); i >= 0 {
		return append(servers[:i], servers[i+1:]...)
	}
	return servers
}

// renameToolKey rewrites `from` to `to` in every agent's tools: list; an empty
// `to` removes the key. Returns true when any agent changed.
func renameToolKey(r *roster, from, to string) bool {
	changed := false
	for i := range r.Agents {
		var out []string
		for _, t := range r.Agents[i].Tools {
			switch {
			case strings.TrimSpace(t) != from:
				out = append(out, t)
			case to != "":
				out = append(out, to)
				changed = true
			default:
				changed = true
			}
		}
		if len(out) != len(r.Agents[i].Tools) || changed {
			r.Agents[i].Tools = out
		}
	}
	return changed
}
