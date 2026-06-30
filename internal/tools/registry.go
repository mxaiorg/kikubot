package tools

// registry maps services-friendly string keys to tool factory functions.
// CoreTools are not included here — they are always added unconditionally.

type toolFactory func() []ToolDefinition

// wrap adapts a single-tool factory to the slice-returning signature.
func wrap(fn func() ToolDefinition) toolFactory {
	return func() []ToolDefinition { return []ToolDefinition{fn()} }
}

var registry = map[string]toolFactory{
	"report":               wrap(ReportTool),
	"report_strict":        wrap(ReportStrictTool),
	"snooze":               SnoozeTools,
	"salesforce_mcp":       SalesforceMCP,
	"anthropic_web_search": wrap(AnthropicWebSearch),
	"wordpress":            WordPressTool,
	"helpjuice":            HelpjuiceTools,
	"box_cli":              BoxCLI,
	"download":             wrap(DownloadTool),
	"file_text":            wrap(FileTextTool),
	"bash":                 wrap(BashTool),
	"xero_mcp":             XeroMCP,
	"xero_api":             XeroAPI,
	"vimeo":                Vimeo,
	"nuki":                 Nuki,
	"supabase":             Supabase,
	"weather":              Weather,
}

// LookupTools returns the scripts for a given services key.
func LookupTools(key string) ([]ToolDefinition, bool) {
	factory, ok := registry[key]
	if !ok {
		return nil, false
	}
	return factory(), true
}

// Register adds a tool factory to the registry under the given key. Intended
// for use from build-tagged packages (see internal/tools_priv) that contribute
// company-specific tools without putting them in the public registry literal.
// Call from an init() so the tool is available before agent construction.
// An empty description is allowed; supply one for factories that build their
// ToolDefinitions dynamically (e.g. MCP bridges) so the configurator dashboard
// has something to display.
func Register(key string, factory func() []ToolDefinition, description string) {
	if key == "" || factory == nil {
		return
	}
	registry[key] = factory
	if description != "" {
		registryDescriptions[key] = description
	}
}

// registryDescriptions provides human-friendly summaries for tools whose
// factories build their ToolDefinitions dynamically (typically MCP bridges
// that fetch tool lists from a remote server, so there are no Description
// literals in this package for the configurator's AST walker to find). The
// configurator dashboard reads this map to populate chip tooltips. Update
// when adding a new MCP-style factory or when its scope shifts.
var registryDescriptions = map[string]string{
	"salesforce_mcp": "Salesforce CRM — query and update accounts, contacts, opportunities, leads, and other Salesforce records.",
	"vimeo":          "List Vimeo video library: descriptions, links, etc.",
	"xero_mcp":       "Xero accounting via the upstream xero-mcp-server (Custom Connection apps; AU/NZ/UK/US only).",
	"xero_api":       "Xero accounting via direct REST (WebApp OAuth, works anywhere). Currently exposes invoices and contacts. Run cmd/xero-bootstrap once to seed the refresh token.",
	"nuki":           "Manage Nuki device accounts and keypad codes (a reduced set of API commands)",
	"supabase":       "Supabase/PostgREST CRUD — select, insert, update, upsert, or delete rows on any table the API key has access to.",
}
