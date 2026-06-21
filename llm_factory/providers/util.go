package providers

import (
	"sort"
	"strings"
)

// firstNonEmpty returns the first non-empty string in vals, or "" if all empty.
// Providers use it for the override -> env -> default model precedence.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// secretEnvKeys are the LLMConfig.Env keys whose values are credentials and must
// never be logged. RedactedEnv masks these; everything else (base urls, model
// names, regions) is safe to print for diagnostics.
var secretEnvKeys = map[string]bool{
	"ANTHROPIC_API_KEY": true,
	"OPENAI_API_KEY":    true,
	"GEMINI_API_KEY":    true,
}

// RedactedEnv returns a log-safe copy of env: secret values are replaced with a
// fixed mask, non-secret values are kept. Keys are returned sorted via the
// returned string so log lines are stable.
func RedactedEnv(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(k)
		b.WriteString("=")
		if secretEnvKeys[k] {
			b.WriteString("***")
		} else {
			b.WriteString(env[k])
		}
	}
	return b.String()
}
