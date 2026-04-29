package provider

import (
	"kikubot/internal/config"
	"log"
	"strings"
)

// NewProvider returns the appropriate Provider implementation based on
// the LLM_PROVIDER environment variable.
//
//   - "anthropic" (default) → uses the Anthropic Messages API directly
//   - "openrouter"          → uses the OpenRouter API (OpenAI-compatible)
//
// As a convenience, if LLM_PROVIDER is unset but the model string starts
// with a provider prefix (e.g. "anthropic/claude-…"), OpenRouter is
// selected automatically since Anthropic's own API doesn't use prefixed
// model names.
func NewProvider() Provider {
	prov := strings.ToLower(config.LlmProvider)

	switch prov {
	case "openrouter":
		log.Println("using OpenRouter provider")
		return NewOpenRouterProvider()
	case "anthropic", "":
		log.Println("using Anthropic provider")
		return NewAnthropicProvider()
	default:
		log.Printf("unknown LLM_PROVIDER %q, falling back to Anthropic", prov)
		return NewAnthropicProvider()
	}
}
