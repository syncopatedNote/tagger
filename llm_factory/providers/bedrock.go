package providers

import "fmt"

// bedrockProvider resolves Amazon Bedrock. Dagger has NO native Bedrock
// transport; the officially documented path is a LiteLLM proxy that exposes
// Bedrock models as an OpenAI-compatible API:
//
//	https://docs.dagger.io/reference/configuration/llm/#amazon-bedrock-via-litellm-proxy
//
// So from Dagger's point of view "bedrock" is just the OpenAI provider pointed at
// the proxy. The operator runs LiteLLM with a config.yml whose model_list maps a
// friendly `model_name` (e.g. "bedrock-claude-3-5-sonnet") to a Bedrock model id
// (e.g. "bedrock/us.anthropic.claude-3-5-sonnet-20241022-v2:0", with
// `drop_params: true` — Bedrock rejects OpenAI's seed / parallel_tool_calls).
//
// This provider therefore emits OpenAI-compatible env vars, but unlike the plain
// openAIProvider it REQUIRES the proxy base URL: a Bedrock setup with no proxy
// endpoint is misconfigured, not "the real OpenAI API".
//
// Required worker env:
//   - LLM_BEDROCK_PROXY_URL : the LiteLLM proxy base URL (e.g. http://127.0.0.1:4000)
//   - LLM_BEDROCK_MODEL     : the LiteLLM model_name (matches config.yml), unless
//     a per-run model override is supplied.
//
// Optional:
//   - LLM_BEDROCK_PROXY_KEY : the LiteLLM master key. Dagger requires SOME
//     OPENAI_API_KEY value even when the proxy is unauthenticated, so a
//     placeholder is used if unset.
type bedrockProvider struct{}

// NewBedrock returns the Bedrock-via-LiteLLM provider resolver.
func NewBedrock() Provider { return bedrockProvider{} }

func (bedrockProvider) Name() string { return ProviderBedrock }

// Resolve maps the Bedrock/LiteLLM settings onto the OpenAI-compatible env vars
// Dagger expects. The proxy URL is mandatory; the model is the LiteLLM
// model_name (override wins). The proxy key is optional and defaults to a
// placeholder because Dagger insists on a non-empty OPENAI_API_KEY.
func (bedrockProvider) Resolve(modelOverride string, getenv func(string) string) (LLMConfig, error) {
	proxyURL := getenv("LLM_BEDROCK_PROXY_URL")
	if proxyURL == "" {
		return LLMConfig{}, fmt.Errorf(
			"provider %q requires LLM_BEDROCK_PROXY_URL (the LiteLLM proxy that fronts Bedrock as an OpenAI-compatible API)",
			ProviderBedrock)
	}

	model := firstNonEmpty(modelOverride, getenv("LLM_BEDROCK_MODEL"))
	if model == "" {
		return LLMConfig{}, fmt.Errorf(
			"provider %q requires a model: set LLM_BEDROCK_MODEL to the LiteLLM model_name (e.g. \"bedrock-claude-3-5-sonnet\") or pass a model override",
			ProviderBedrock)
	}

	// LiteLLM may run unauthenticated locally; Dagger still requires a non-empty
	// OPENAI_API_KEY, so fall back to a harmless placeholder.
	proxyKey := getenv("LLM_BEDROCK_PROXY_KEY")
	if proxyKey == "" {
		proxyKey = "not-needed"
	}

	env := map[string]string{
		"OPENAI_BASE_URL": proxyURL,
		"OPENAI_API_KEY":  proxyKey,
		"OPENAI_MODEL":    model,
	}

	return LLMConfig{Provider: ProviderBedrock, Model: model, Env: env}, nil
}
