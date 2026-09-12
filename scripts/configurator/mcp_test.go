package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kikubot/internal/config"
)

// tempRoot builds a throwaway project root with a roster whose agents
// reference the given MCP key, and no source tree (so the built-in-key
// collision check is skipped).
func tempRoot(t *testing.T, key string) string {
	t.Helper()
	root := t.TempDir()
	r := &roster{}
	r.Agents = []config.AgentDef{
		{Name: "Kiku", Email: "kiku@agents.example.com", Role: "Coordinator", Description: "d", Tools: []string{"report", key}},
		{Name: "Beta", Email: "beta@agents.example.com", Role: "Worker", Description: "d", Tools: []string{"report"}},
	}
	if err := saveRoster(root, r); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestMCPFormSaveRoundTrip(t *testing.T) {
	root := tempRoot(t, "tavily_mcp")
	f := &mcpForm{
		Key: "tavily_mcp", URL: "https://mcp.tavily.com/mcp", Auth: "bearer",
		TokenEnv: "TAVILY_API_KEY", TokenValue: "tvly-secret", Description: "Tavily search",
		// oauth2-only fields must be dropped for a bearer row.
		ClientIDEnv: "IGNORED",
	}
	users, err := f.save(root)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(users) != 1 || users[0].Name != "Kiku" {
		t.Fatalf("users = %+v, want Kiku", users)
	}
	servers, err := loadMCPServers(root)
	if err != nil || len(servers) != 1 {
		t.Fatalf("load: %v, %d servers", err, len(servers))
	}
	got := servers[0]
	if got.Key != "tavily_mcp" || got.Auth != "bearer" || got.TokenEnv != "TAVILY_API_KEY" || got.ClientIDEnv != "" {
		t.Errorf("row = %+v", got)
	}
	if v, _ := loadSecrets(root).Get("TAVILY_API_KEY"); v != "tvly-secret" {
		t.Errorf("secret = %q", v)
	}
	b, _ := os.ReadFile(mcpServersPath(root))
	if !strings.HasPrefix(string(b), "# mcp_servers.yaml") || !strings.Contains(string(b), "mcp_servers:\n  - key: tavily_mcp") {
		t.Errorf("unexpected file:\n%s", b)
	}

	// Blank secret value leaves the stored one untouched.
	f2, err := loadMCPForm(root, "tavily_mcp")
	if err != nil {
		t.Fatal(err)
	}
	if f2.TokenValue != "tvly-secret" {
		t.Errorf("form token = %q", f2.TokenValue)
	}
	f2.TokenValue = ""
	f2.Description = "changed"
	if _, err := f2.save(root); err != nil {
		t.Fatal(err)
	}
	if v, _ := loadSecrets(root).Get("TAVILY_API_KEY"); v != "tvly-secret" {
		t.Errorf("secret after blank save = %q", v)
	}
}

func TestMCPFormRenamePropagatesToAgents(t *testing.T) {
	root := tempRoot(t, "old_mcp")
	if err := saveMCPServers(root, []config.MCPServer{{Key: "old_mcp", URL: "https://x.example.com", Auth: "none"}}); err != nil {
		t.Fatal(err)
	}
	f, err := loadMCPForm(root, "old_mcp")
	if err != nil {
		t.Fatal(err)
	}
	f.Key = "new_mcp"
	if _, err := f.save(root); err != nil {
		t.Fatal(err)
	}
	servers, _ := loadMCPServers(root)
	if len(servers) != 1 || servers[0].Key != "new_mcp" {
		t.Fatalf("servers = %+v", servers)
	}
	r, _ := loadRoster(root)
	if got := strings.Join(r.Agents[0].Tools, ","); got != "report,new_mcp" {
		t.Errorf("Kiku tools = %q", got)
	}
	if got := strings.Join(r.Agents[1].Tools, ","); got != "report" {
		t.Errorf("Beta tools = %q", got)
	}
}

func TestDeleteMCPServerStripsKey(t *testing.T) {
	root := tempRoot(t, "gone_mcp")
	if err := saveMCPServers(root, []config.MCPServer{
		{Key: "gone_mcp", URL: "https://x.example.com", Auth: "none"},
		{Key: "kept_mcp", URL: "https://y.example.com", Auth: "none"},
	}); err != nil {
		t.Fatal(err)
	}
	users, err := deleteMCPServer(root, "gone_mcp")
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0].Name != "Kiku" {
		t.Errorf("users = %+v", users)
	}
	servers, _ := loadMCPServers(root)
	if len(servers) != 1 || servers[0].Key != "kept_mcp" {
		t.Errorf("servers = %+v", servers)
	}
	r, _ := loadRoster(root)
	if got := strings.Join(r.Agents[0].Tools, ","); got != "report" {
		t.Errorf("Kiku tools = %q", got)
	}
	if _, err := deleteMCPServer(root, "nope"); err == nil {
		t.Error("deleting an unknown key should fail")
	}
}

func TestMCPFormValidate(t *testing.T) {
	root := t.TempDir()
	base := func() *mcpForm {
		return &mcpForm{Key: "ok_mcp", URL: "https://mcp.example.com/mcp", Auth: "bearer", TokenEnv: "X_TOKEN"}
	}
	cases := []struct {
		name string
		mut  func(*mcpForm)
		want string // substring of the error, "" for valid
	}{
		{"valid bearer", func(*mcpForm) {}, ""},
		{"valid none", func(f *mcpForm) { f.Auth = "none"; f.TokenEnv = "" }, ""},
		{"valid oauth2", func(f *mcpForm) { f.Auth = "oauth2"; f.ClientIDEnv = "CID"; f.ClientSecretEnv = "CSEC" }, ""},
		{"alias auth", func(f *mcpForm) { f.Auth = "api_key" }, ""},
		{"missing key", func(f *mcpForm) { f.Key = "" }, "key is required"},
		{"bad key chars", func(f *mcpForm) { f.Key = "my mcp" }, "letters, digits"},
		{"bad url", func(f *mcpForm) { f.URL = "mcp.example.com" }, "absolute http"},
		{"unknown auth", func(f *mcpForm) { f.Auth = "basic" }, "auth must be one of"},
		{"bearer no env", func(f *mcpForm) { f.TokenEnv = "" }, "requires a token env"},
		{"bad env name", func(f *mcpForm) { f.TokenEnv = "my-token" }, "environment variable name"},
		{"bad header", func(f *mcpForm) { f.Header = "X-Api-Key: foo" }, "bare header name"},
		{"oauth2 no client id", func(f *mcpForm) { f.Auth = "oauth2" }, "requires a client ID"},
		{"oauth2 bad metadata", func(f *mcpForm) { f.Auth = "oauth2"; f.ClientIDEnv = "CID"; f.MetadataURL = "nope" }, "metadata URL"},
	}
	for _, tc := range cases {
		f := base()
		tc.mut(f)
		err := f.validate(root)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: got %v, want error containing %q", tc.name, err, tc.want)
		}
	}
}

func TestMCPFormDuplicateKeyRejected(t *testing.T) {
	root := tempRoot(t, "a_mcp")
	if err := saveMCPServers(root, []config.MCPServer{{Key: "a_mcp", URL: "https://a.example.com"}}); err != nil {
		t.Fatal(err)
	}
	f := &mcpForm{Key: "a_mcp", URL: "https://b.example.com", Auth: "none"}
	if _, err := f.save(root); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("got %v, want duplicate-key error", err)
	}
	// Editing the same row under its own key is fine.
	f.OriginalKey = "a_mcp"
	if _, err := f.save(root); err != nil {
		t.Errorf("self-edit: %v", err)
	}
}

func TestMCPHeaderPreview(t *testing.T) {
	for _, tc := range []struct {
		s    config.MCPServer
		want string
	}{
		{config.MCPServer{Auth: "bearer"}, "Authorization: Bearer <token>"},
		{config.MCPServer{Auth: "apikey"}, "Authorization: ApiKey <token>"},
		{config.MCPServer{Auth: "apikey", Header: "X-Api-Key"}, "X-Api-Key: <token>"},
		{config.MCPServer{Auth: "bearer", Scheme: "Token"}, "Authorization: Token <token>"},
		{config.MCPServer{Auth: "oauth2"}, ""},
		{config.MCPServer{Auth: "none"}, ""},
	} {
		if got := mcpHeaderPreview(tc.s); got != tc.want {
			t.Errorf("%+v → %q, want %q", tc.s, got, tc.want)
		}
	}
}

func TestListMCPServersStatus(t *testing.T) {
	root := tempRoot(t, "box_mcp")
	if err := saveMCPServers(root, []config.MCPServer{
		{Key: "box_mcp", URL: "https://mcp.box.com", Auth: "oauth2", ClientIDEnv: "BOX_CLIENT_ID", ClientSecretEnv: "BOX_CLIENT_SECRET"},
		{Key: "mxmcp", URL: "https://mcp.example.com", Auth: "apikey", TokenEnv: "MXMCP_API_KEY"},
	}); err != nil {
		t.Fatal(err)
	}
	s := loadSecrets(root)
	s.Set("BOX_CLIENT_ID", "id")
	s.Set("MXMCP_API_KEY", "k")
	if err := saveSecrets(root, s); err != nil {
		t.Fatal(err)
	}
	rows, err := listMCPServers(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Key != "box_mcp" || rows[1].Key != "mxmcp" {
		t.Fatalf("rows = %+v", rows)
	}
	box := rows[0]
	if strings.Join(box.MissingEnv, ",") != "BOX_CLIENT_SECRET" {
		t.Errorf("box missing env = %v", box.MissingEnv)
	}
	if len(box.UsedBy) != 1 || box.UsedBy[0].Name != "Kiku" {
		t.Errorf("box used by = %+v", box.UsedBy)
	}
	if strings.Join(box.TokenMissing, ",") != "Kiku" {
		t.Errorf("box token missing = %v", box.TokenMissing)
	}
	// Seed the token file where the container volume maps it; status clears.
	p := filepath.Join(root, "data", "kiku", "oauth", "box_mcp.json")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte("{}"), 0o600)
	rows, _ = listMCPServers(root)
	if len(rows[0].TokenMissing) != 0 {
		t.Errorf("token missing after seed = %v", rows[0].TokenMissing)
	}
	if len(rows[1].MissingEnv) != 0 || len(rows[1].UsedBy) != 0 || rows[1].Header != "Authorization: ApiKey <token>" {
		t.Errorf("mxmcp row = %+v", rows[1])
	}
}

func TestEnsureMCPServersFile(t *testing.T) {
	root := t.TempDir()
	if err := ensureMCPServersFile(root); err != nil {
		t.Fatal(err)
	}
	servers, err := loadMCPServers(root)
	if err != nil || len(servers) != 0 {
		t.Fatalf("load after ensure: %v, %d", err, len(servers))
	}
	// Existing content is never clobbered.
	_ = saveMCPServers(root, []config.MCPServer{{Key: "k", URL: "https://k.example.com"}})
	if err := ensureMCPServersFile(root); err != nil {
		t.Fatal(err)
	}
	servers, _ = loadMCPServers(root)
	if len(servers) != 1 {
		t.Errorf("ensure clobbered the file: %+v", servers)
	}
}
