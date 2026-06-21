package llm_factory

import (
	"testing"

	"github.com/syncopatedNote/tagger/llm_factory/providers"
)

func TestCreateLLM_DefaultsToAnthropic(t *testing.T) {
	t.Setenv("LLM_PROVIDER", "")
	t.Setenv("ANTHROPIC_API_KEY", "sk-a")

	cfg, err := New().CreateLLM("", "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != providers.ProviderAnthropic {
		t.Fatalf("empty provider should default to anthropic, got %q", cfg.Provider)
	}
}

func TestCreateLLM_ExplicitProviderWins(t *testing.T) {
	t.Setenv("LLM_PROVIDER", "anthropic")
	t.Setenv("OPENAI_API_KEY", "sk-o")

	// Explicit "openai" arg overrides the LLM_PROVIDER default.
	cfg, err := New().CreateLLM("openai", "gpt-4o-mini")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != providers.ProviderOpenAI {
		t.Fatalf("provider = %q", cfg.Provider)
	}
	if cfg.Model != "gpt-4o-mini" {
		t.Fatalf("model override ignored: %q", cfg.Model)
	}
}

func TestCreateLLM_DefaultProviderFromEnv(t *testing.T) {
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("OPENAI_API_KEY", "sk-o")

	cfg, err := New().CreateLLM("", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Provider != providers.ProviderOpenAI {
		t.Fatalf("LLM_PROVIDER should set the default, got %q", cfg.Provider)
	}
}

func TestCreateLLM_UnknownProvider(t *testing.T) {
	if _, err := New().CreateLLM("does-not-exist", ""); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestCreateLLM_CaseInsensitive(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-a")
	cfg, err := New().CreateLLM("ANTHROPIC", "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != providers.ProviderAnthropic {
		t.Fatalf("provider name should be case-insensitive, got %q", cfg.Provider)
	}
}
