# Kikubot Tools

Kikubot tools provide various capabilities that are distributed across your agents. By distributing tools across multiple agents, agents can better perform their tasks.

## Creating Tools

There are a couple helper functions to make it easier to create tools.

- cli_helper.go - Helper functions for using command line tools
  - box_cli.go is an example of a tool that uses the cli_helper.go
- mcp_helper.go - Helper functions for adding MCPs
  - provides helper functions for local and remote MCPs
    - mxmcp.go is an example of a remote MCP usage
    - salesforce_mcp.go is an example of a local MCP usage

Agent tools can supplement the agent's system prompt to provide additional instructions. System prompts can be static or dynamic. Dynamic system prompts take the email being processed as an input. This allows you to inject per email specific instructions into the system prompt. See `types.go` for more information.

LLMs execute tools via the tool's Execute function. This function takes a context which carries the email being processed. The reason this is done is to provide a check on LLM input that may be incorrect. 

## Tips for Creating Tools

With AI development tools, like Claude Code, you can create a tool that wraps almost any REST API in a matter of minutes. The Vimeo tool was created with the following prompt in Claude Code:

> Create a tool for listing and searching Vimeo videos from an authenticated Vimeo account (using an API key passed in via environment). Use Vimeo's API https://developer.vimeo.com/api/reference to implement the API. Keep functionality to read-only operations so that the tool can help the LLM to search for videos and provide video links and other information. You might use the existing helpjuice.go tool as a reference.

A few minutes later, the tool was created. Thanks, Claude!

⚠️ Once you have a tool, be sure to:
1. register it in registry.go and then add it to your agent definitions in your `agents.yaml` file. 
2. Also, remember to pass in your API key as an environment variable.

🤔 You might be wondering why we created a Vimeo tool instead of using an existing Vimeo MCP tool. The reason is that the Vimeo MCP tools implements much more capability than we need. As a result, it consumes a lot more tokens than required for our case.
