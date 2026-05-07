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
	"snooze":               SnoozeTools,
	"mxmcp":                MxMCP,
	"salesforce_mcp":       SalesforceMCP,
	"buffer_mcp":           BufferMCP,
	"anthropic_web_search": wrap(AnthropicWebSearch),
	"wordpress":            WordPressTool,
	"helpjuice":            HelpjuiceTools,
	"box_cli":              BoxCLI,
	"download":             wrap(DownloadTool),
	"file_text":            wrap(FileTextTool),
	"bash":                 wrap(BashTool),
	"xero_mcp":             XeroMCP,
	"tavily_mcp":           TavilyMCP,
	"vimeo":                Vimeo,
}

// LookupTools returns the scripts for a given services key.
func LookupTools(key string) ([]ToolDefinition, bool) {
	factory, ok := registry[key]
	if !ok {
		return nil, false
	}
	return factory(), true
}

// registryDescriptions provides human-friendly summaries for tools whose
// factories build their ToolDefinitions dynamically (typically MCP bridges
// that fetch tool lists from a remote server, so there are no Description
// literals in this package for the configurator's AST walker to find). The
// configurator dashboard reads this map to populate chip tooltips. Update
// when adding a new MCP-style factory or when its scope shifts.
var registryDescriptions = map[string]string{
	"mxmcp":          "mxHERO Mail2Cloud Advanced — searches and retrieves email across an organization's archived email accounts.",
	"salesforce_mcp": "Salesforce CRM — query and update accounts, contacts, opportunities, leads, and other Salesforce records.",
	"buffer_mcp":     "Buffer social media — draft, schedule, and publish posts across connected social channels.",
	"tavily_mcp":     "Tavily web search — runs web searches and returns extracted content for downstream reasoning.",
	"vimeo":          "List Vimeo video library: descriptions, links, etc.",
	"xero_mcp":       "Xero accounting — read and update invoices, contacts, transactions, and other Xero records.",
}
