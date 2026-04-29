package agents

// Message is the inter-agent communication envelope.
type Message struct {
	From    string `json:"from"`
	To      string `json:"to,omitempty"`
	Content string `json:"content"`
}

// AgentConfig defines how to spawn an agent node.
type AgentConfig struct {
	ID     string // Unique agent identifier
	Role   string // "planner", "worker", etc. — for your own routing logic
	System string // System prompt defining the agent's personality/role
	//Tools  []string // Which registered tool names this agent can access
}
