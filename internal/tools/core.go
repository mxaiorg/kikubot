package tools

func CoreTools() []ToolDefinition {
	var agentTools []ToolDefinition

	agentTools = append(agentTools, SetTaskStatusTool())
	agentTools = append(agentTools, MessageTool())
	agentTools = append(agentTools, MboxSearchTool())
	agentTools = append(agentTools, SaveAttachmentTool())

	return agentTools
}
