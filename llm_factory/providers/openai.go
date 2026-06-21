package providers

import "fmt"

// openAIProvider resolves the OpenAI backend, or any OpenAI-COMPATIBLE gateway
// (OpenRouter, a LiteLLM proxy, a local server) when OPENAI_BASE_URL is set.
// Dagger reads OPENAI_API_KEY / OPENAI_BASE_URL / OPENAI_MODEL from the engine
// env and speaks the OpenAI wire protocol.
type openAIProvider struct{}

// NewOpenAI returns the OpenAI (and OpenAI-compatible) provider resolver.
func NewOpenAI() Provider { return openAIProvider{} }

func (openAIProvider) Name() string { return ProviderOpenAI }

// Resolve reads OPENAI_API_KEY (required) and the model (modelOverride, else
// OPENAI_MODEL, else gpt-4o). OPENAI_BASE_URL is passed through when present so
// the same provider serves OpenAI proper and any OpenAI-compatible gateway.
func (openAIProvider) Resolve(modelOverride string, getenv func(string) string) (LLMConfig, error) {
	apiKey := getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return LLMConfig{}, fmt.Errorf("provider %q requires OPENAI_API_KEY", ProviderOpenAI)
	}

	model := firstNonEmpty(modelOverride, getenv("OPENAI_MODEL"), "gpt-4o")

	env := map[string]string{
		"OPENAI_API_KEY": apiKey,
		"OPENAI_MODEL":   model,
	}
	// Point Dagger at an alternative OpenAI-compatible endpoint (OpenRouter,
	// LiteLLM, local) when configured. Absent => the real OpenAI API.
	if base := getenv("OPENAI_BASE_URL"); base != "" {
		env["OPENAI_BASE_URL"] = base
	}
	// Azure OpenAI deployments additionally need an API version.
	if ver := getenv("OPENAI_AZURE_VERSION"); ver != "" {
		env["OPENAI_AZURE_VERSION"] = ver
	}

	return LLMConfig{Provider: ProviderOpenAI, Model: model, Env: env}, nil
}
