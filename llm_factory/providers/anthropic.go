package providers

import "fmt"

// anthropicProvider resolves the Anthropic backend. Dagger reads ANTHROPIC_API_KEY
// from the engine env and infers the Anthropic provider from a claude-* model id.
type anthropicProvider struct{}

// NewAnthropic returns the Anthropic provider resolver.
func NewAnthropic() Provider { return anthropicProvider{} }

func (anthropicProvider) Name() string { return ProviderAnthropic }

// Resolve reads ANTHROPIC_API_KEY (required) and the model. The model comes from
// modelOverride if set, else ANTHROPIC_MODEL, else a sensible default. The key is
// passed through in Env so the Dagger engine can authenticate.
func (anthropicProvider) Resolve(modelOverride string, getenv func(string) string) (LLMConfig, error) {
	apiKey := getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return LLMConfig{}, fmt.Errorf("provider %q requires ANTHROPIC_API_KEY", ProviderAnthropic)
	}

	model := firstNonEmpty(modelOverride, getenv("ANTHROPIC_MODEL"), "claude-opus-4-8")

	env := map[string]string{
		"ANTHROPIC_API_KEY": apiKey,
		// Dagger also keys off ANTHROPIC_MODEL when LLMOpts.Model is empty; we set
		// it so the engine env and the LLMOpts agree regardless of which Dagger
		// reads first.
		"ANTHROPIC_MODEL": model,
	}
	// Optional self-hosted / gateway endpoint for Anthropic-compatible proxies.
	if base := getenv("ANTHROPIC_BASE_URL"); base != "" {
		env["ANTHROPIC_BASE_URL"] = base
	}

	return LLMConfig{Provider: ProviderAnthropic, Model: model, Env: env}, nil
}
