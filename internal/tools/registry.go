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
	"snooze":               wrap(SnoozeTool),
	"unsnooze":             wrap(UnSnoozeTool),
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
}

// LookupTools returns the scripts for a given services key.
func LookupTools(key string) ([]ToolDefinition, bool) {
	factory, ok := registry[key]
	if !ok {
		return nil, false
	}
	return factory(), true
}
