// Package llm_factory is the startup-time catalog of supported LLM providers. It
// wires every available provider into a lookup table once (via New) and resolves
// a provider name to a concrete LLMConfig on demand (via CreateLLM).
//
// It is the single seam where "which LLM backend" is decided. It mirrors the
// Python LLMFactory.create_llm(provider, model, ...) idea, but adapted to how
// THIS app runs LLMs: the agent loop executes inside the Dagger engine via
// `client.LLM(dagger.LLMOpts{Model})`, and Dagger selects the provider from
// engine ENVIRONMENT VARIABLES. So CreateLLM does not return an SDK client — it
// returns an LLMConfig: the model string for dagger.LLMOpts plus the env vars the
// Dagger engine must see. See providers/provider_interface.go for the full
// rationale.
//
// The factory performs no I/O and holds no per-request state, so a single
// *Factory is built at worker boot and shared for the process lifetime — exactly
// like agent_registry.Registry.
package llm_factory

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/syncopatedNote/tagger/llm_factory/providers"
)

// LLMConfig is re-exported so callers depend only on this package, not on the
// providers subpackage, for the result type.
type LLMConfig = providers.LLMConfig

// Factory maps each supported provider name to its resolver.
type Factory struct {
	byName map[string]providers.Provider
	// defaultProvider is used when CreateLLM is called with an empty provider
	// name (the common case: env-driven default).
	defaultProvider string
}

// New builds the factory, wiring in every supported provider. Call once at
// startup. Adding a provider is a single line here plus the provider's own file
// in providers/ — no activity or workflow changes.
//
// The default provider is read from LLM_PROVIDER (falling back to "anthropic",
// the app's historical backend) so existing deployments keep working unchanged.
func New() *Factory {
	all := []providers.Provider{
		providers.NewAnthropic(),
		providers.NewOpenAI(),
		providers.NewBedrock(),
	}
	byName := make(map[string]providers.Provider, len(all))
	for _, p := range all {
		byName[p.Name()] = p
	}

	def := os.Getenv("LLM_PROVIDER")
	if def == "" {
		def = providers.ProviderAnthropic
	}

	return &Factory{byName: byName, defaultProvider: strings.ToLower(def)}
}

// CreateLLM resolves a provider+model into an LLMConfig the Dagger LLM can use.
//
//   - provider: the backend name (anthropic, openai, bedrock). Empty => the
//     factory's default provider (LLM_PROVIDER, else anthropic). Case-insensitive.
//   - modelOverride: a per-run model id that wins over the provider's model env
//     var. Empty => the provider picks from its env / default.
//
// An unknown provider, or a provider missing its required credentials, is an
// error — the factory never silently falls back to a different backend. The
// environment is read via os.Getenv (the worker process env, which is also the
// Dagger engine env when the worker connects an embedded/local engine).
func (f *Factory) CreateLLM(provider, modelOverride string) (LLMConfig, error) {
	name := strings.ToLower(strings.TrimSpace(provider))
	if name == "" {
		name = f.defaultProvider
	}

	p, ok := f.byName[name]
	if !ok {
		return LLMConfig{}, fmt.Errorf("unsupported LLM provider %q (supported: %s)", name, f.Supported())
	}
	return p.Resolve(modelOverride, os.Getenv)
}

// Supported returns the registered provider names, sorted, comma-joined — for
// error messages and diagnostics.
func (f *Factory) Supported() string {
	names := make([]string, 0, len(f.byName))
	for n := range f.byName {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
