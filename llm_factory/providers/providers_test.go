package providers

import "testing"

// fakeEnv returns a getenv func backed by a map, so providers can be exercised
// against a controlled environment with no global process state.
func fakeEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestAnthropicResolve(t *testing.T) {
	p := NewAnthropic()

	t.Run("requires api key", func(t *testing.T) {
		if _, err := p.Resolve("", fakeEnv(nil)); err == nil {
			t.Fatal("expected error when ANTHROPIC_API_KEY is missing")
		}
	})

	t.Run("override beats env beats default", func(t *testing.T) {
		cfg, err := p.Resolve("claude-from-override", fakeEnv(map[string]string{
			"ANTHROPIC_API_KEY": "sk-x",
			"ANTHROPIC_MODEL":   "claude-from-env",
		}))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Model != "claude-from-override" {
			t.Fatalf("override should win, got %q", cfg.Model)
		}
		if cfg.Env["ANTHROPIC_API_KEY"] != "sk-x" {
			t.Fatalf("api key not propagated: %v", cfg.Env)
		}
		if cfg.Provider != ProviderAnthropic {
			t.Fatalf("provider = %q", cfg.Provider)
		}
	})

	t.Run("default model when nothing set", func(t *testing.T) {
		cfg, err := p.Resolve("", fakeEnv(map[string]string{"ANTHROPIC_API_KEY": "sk-x"}))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Model == "" {
			t.Fatal("expected a default model")
		}
	})
}

func TestOpenAIResolve(t *testing.T) {
	p := NewOpenAI()

	if _, err := p.Resolve("", fakeEnv(nil)); err == nil {
		t.Fatal("expected error when OPENAI_API_KEY is missing")
	}

	cfg, err := p.Resolve("gpt-x", fakeEnv(map[string]string{
		"OPENAI_API_KEY":  "sk-o",
		"OPENAI_BASE_URL": "https://openrouter.ai/api/v1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "gpt-x" {
		t.Fatalf("model = %q", cfg.Model)
	}
	if cfg.Env["OPENAI_BASE_URL"] != "https://openrouter.ai/api/v1" {
		t.Fatalf("base url not propagated for OpenAI-compatible gateway: %v", cfg.Env)
	}
}

func TestBedrockResolve(t *testing.T) {
	p := NewBedrock()

	t.Run("requires proxy url", func(t *testing.T) {
		// No proxy => misconfigured, even if a model is present.
		if _, err := p.Resolve("bedrock-claude", fakeEnv(map[string]string{})); err == nil {
			t.Fatal("expected error when LLM_BEDROCK_PROXY_URL is missing")
		}
	})

	t.Run("requires a model", func(t *testing.T) {
		_, err := p.Resolve("", fakeEnv(map[string]string{
			"LLM_BEDROCK_PROXY_URL": "http://127.0.0.1:4000",
		}))
		if err == nil {
			t.Fatal("expected error when no model is resolvable")
		}
	})

	t.Run("maps onto OpenAI-compatible env", func(t *testing.T) {
		cfg, err := p.Resolve("", fakeEnv(map[string]string{
			"LLM_BEDROCK_PROXY_URL": "http://127.0.0.1:4000",
			"LLM_BEDROCK_MODEL":     "bedrock-claude-3-5-sonnet",
			"LLM_BEDROCK_PROXY_KEY": "litellm-master",
		}))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Provider != ProviderBedrock {
			t.Fatalf("provider = %q", cfg.Provider)
		}
		// From Dagger's view this is the OpenAI provider pointed at the proxy.
		if cfg.Env["OPENAI_BASE_URL"] != "http://127.0.0.1:4000" {
			t.Fatalf("OPENAI_BASE_URL = %q", cfg.Env["OPENAI_BASE_URL"])
		}
		if cfg.Env["OPENAI_MODEL"] != "bedrock-claude-3-5-sonnet" {
			t.Fatalf("OPENAI_MODEL = %q", cfg.Env["OPENAI_MODEL"])
		}
		if cfg.Env["OPENAI_API_KEY"] != "litellm-master" {
			t.Fatalf("OPENAI_API_KEY = %q", cfg.Env["OPENAI_API_KEY"])
		}
	})

	t.Run("placeholder key when proxy is unauthenticated", func(t *testing.T) {
		cfg, err := p.Resolve("m", fakeEnv(map[string]string{
			"LLM_BEDROCK_PROXY_URL": "http://127.0.0.1:4000",
		}))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Env["OPENAI_API_KEY"] == "" {
			t.Fatal("Dagger requires a non-empty OPENAI_API_KEY; placeholder expected")
		}
	})
}

func TestRedactedEnv(t *testing.T) {
	got := RedactedEnv(map[string]string{
		"OPENAI_API_KEY":  "super-secret",
		"OPENAI_BASE_URL": "http://proxy:4000",
		"OPENAI_MODEL":    "m",
	})
	if want := "OPENAI_API_KEY=*** OPENAI_BASE_URL=http://proxy:4000 OPENAI_MODEL=m"; got != want {
		t.Fatalf("redaction mismatch:\n got %q\nwant %q", got, want)
	}
}
