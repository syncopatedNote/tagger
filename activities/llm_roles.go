package activities

import (
	"fmt"
	"os"
	"strings"

	"github.com/syncopatedNote/tagger/llm_factory"
)

// ActivityRole identifies which agent a LLMConfig belongs to. It is the key
// into the llms map on Activities and the prefix for per-role env var overrides.
type ActivityRole string

const (
	RoleContext ActivityRole = "context"
	RoleCoding  ActivityRole = "coding"
)

// AllRoles is the single source of truth for supported roles. Adding a model
// for a new activity = one line here; nothing else in this file changes.
var AllRoles = []ActivityRole{RoleContext, RoleCoding}

// buildActivityLLMs resolves a LLMConfig for each role. The env var convention:
//
//	<ROLE>_LLM_PROVIDER  and  <ROLE>_LLM_MODEL  (e.g. CONTEXT_LLM_PROVIDER)
//
// Both fall back to the global LLM_PROVIDER / AGENT_MODEL when unset, so
// existing deployments that only set the global vars keep working unchanged.
func buildActivityLLMs(factory *llm_factory.Factory) (map[ActivityRole]llm_factory.LLMConfig, error) {
	globalProvider := os.Getenv("LLM_PROVIDER")
	globalModel := os.Getenv("AGENT_MODEL")
	out := make(map[ActivityRole]llm_factory.LLMConfig, len(AllRoles))
	for _, role := range AllRoles {
		prefix := strings.ToUpper(string(role))
		provider := getenv(prefix+"_LLM_PROVIDER", globalProvider)
		model := getenv(prefix+"_LLM_MODEL", globalModel)
		cfg, err := factory.CreateLLM(provider, model)
		if err != nil {
			return nil, fmt.Errorf("resolving LLM for role %q: %w", role, err)
		}
		out[role] = cfg
	}
	return out, nil
}
