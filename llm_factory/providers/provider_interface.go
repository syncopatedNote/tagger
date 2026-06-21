// Package providers defines the per-LLM-provider configuration resolvers used by
// the llm_factory. Each provider knows the exact environment-variable contract
// the Dagger engine reads to talk to that backend, validates that the required
// credentials are present, and emits a uniform LLMConfig.
//
// Why this shape (and not a returned SDK client like a Python LLMFactory):
// this application NEVER calls an LLM SDK directly. The whole agent loop runs
// inside the Dagger engine via `client.LLM(dagger.LLMOpts{Model, MaxAPICalls})`.
// Dagger's LLMOpts exposes only Model and MaxAPICalls — it has no Provider /
// APIKey / BaseURL field. Dagger selects the provider by reading ENVIRONMENT
// VARIABLES on the engine (ANTHROPIC_API_KEY, OPENAI_API_KEY, OPENAI_BASE_URL,
// …) and inferring the backend from the model string.
//
// So a provider here produces exactly "whatever dagger.LLM expects": the model
// string for LLMOpts.Model, plus the provider env vars the engine must see. The
// activities pass Model straight into dagger.LLMOpts; the worker exports Env onto
// the engine process so `client.LLM` resolves to the chosen backend.
//
// This package imports neither Dagger nor Temporal — it is pure config (strings
// and maps). The activity layer translates an LLMConfig into Dagger calls.
package providers

// Provider names. These are the values an operator puts in LLM_PROVIDER (or the
// per-run override) to pick a backend.
const (
	// ProviderAnthropic talks to the Anthropic API directly (claude-* models).
	// This is the historical default for the app.
	ProviderAnthropic = "anthropic"
	// ProviderOpenAI talks to the OpenAI API, or any OpenAI-COMPATIBLE gateway
	// (OpenRouter, a LiteLLM proxy, a local server) when OPENAI_BASE_URL is set.
	ProviderOpenAI = "openai"
	// ProviderBedrock talks to Amazon Bedrock. Dagger has no native Bedrock
	// transport; the official path is a LiteLLM proxy that exposes Bedrock as an
	// OpenAI-compatible API. From Dagger's point of view this is therefore just
	// the OpenAI provider pointed at the proxy's base URL.
	ProviderBedrock = "bedrock"
)

// LLMConfig is the resolved, provider-agnostic result of selecting a provider.
// It is exactly what the activity needs to drive a Dagger LLM:
//
//   - Model is passed verbatim to dagger.LLMOpts.Model.
//   - Env is the set of environment variables the Dagger engine must have for
//     `client.LLM` to resolve to this provider (api key, base url, model name,
//     …). The worker exports these onto the engine process before connecting.
//
// Env never contains a Temporal-visible value: it is built worker-side from the
// process environment and stays there. It is intentionally a plain map so it can
// be logged with secrets redacted (see Redacted) and diffed in tests.
type LLMConfig struct {
	// Provider is the resolved provider name (one of the Provider* constants),
	// kept for logging and diagnostics.
	Provider string
	// Model is the model identifier handed to dagger.LLMOpts.Model. For Bedrock
	// via LiteLLM this is the proxy's `model_name`, NOT the raw bedrock/... id.
	Model string
	// Env is the provider's environment-variable contract for the Dagger engine.
	Env map[string]string
}

// Provider resolves a provider's configuration from the worker environment. An
// implementation reads the env vars it owns, validates the required ones are
// present, and returns the LLMConfig the Dagger engine needs.
//
// getenv is injected (rather than calling os.Getenv directly) so providers are
// trivially unit-testable with a fake environment and never reach for global
// process state of their own.
type Provider interface {
	// Name returns the provider's canonical name (a Provider* constant).
	Name() string
	// Resolve builds the LLMConfig for this provider. modelOverride, when
	// non-empty, takes precedence over the provider's model env var. getenv is
	// the environment accessor (os.Getenv in production). A missing required
	// credential is returned as an error — never guessed or defaulted to empty.
	Resolve(modelOverride string, getenv func(string) string) (LLMConfig, error)
}
