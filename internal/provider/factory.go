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
	// No key configured → return a harmless stub so the process can start and
	// stay up (the OpenRouter provider would otherwise log.Fatal here). The
	// poll loop detects config.LLMKeyMissing and replies with a demo notice
	// instead of ever calling the LLM. This is what makes the no-cost demo work
	// before the user pastes a key.
	if config.LLMKeyMissing {
		log.Println("no LLM API key configured — using stub provider (demo mode); set ANTHROPIC_API_KEY or OPENROUTER_API_KEY for real replies")
		return newStubProvider()
	}

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
