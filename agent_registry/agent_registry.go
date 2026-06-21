// Package agent_registry is the startup-time catalog of supported coding-agent
// toolchains. It wires every available agent into a lookup table once (via New)
// and resolves a Language string to its CodingAgent on demand (via
// GetAgentByLanguage). It performs no I/O and holds no per-request state, so a
// single *Registry is built at worker boot and shared for the process lifetime.
package agent_registry

import (
	"fmt"
	"sort"
	"strings"

	"github.com/syncopatedNote/tagger/agent_registry/agents"
)

// Registry maps each supported Language to its CodingAgent implementation.
type Registry struct {
	byLang map[agents.Language]agents.CodingAgent
}

// New builds the registry, wiring in every supported agent. Call once at
// startup. Adding a language to the system is a single line here plus the
// agent's own file — no workflow or activity changes.
//
// JavaScript and Java are intentionally absent for now: detection recognises
// their marker files, but with no agent registered GetAgentByLanguage returns a
// clean "unsupported language" error rather than silently mis-building.
func New() *Registry {
	return &Registry{
		byLang: map[agents.Language]agents.CodingAgent{
			agents.LangGo:     agents.NewGoAgent(),
			agents.LangPython: agents.NewPythonAgent(),
		},
	}
}

// GetAgentByLanguage resolves a Language to its toolchain. The error names the
// supported set so callers (and humans reading the workflow failure) know what
// is available. Pure lookup — no I/O, safe to call on the hot path.
func (r *Registry) GetAgentByLanguage(lang agents.Language) (agents.CodingAgent, error) {
	a, ok := r.byLang[lang]
	if !ok {
		return nil, fmt.Errorf("unsupported language %q (supported: %s)", lang, r.Supported())
	}
	return a, nil
}

// Supported returns the registered languages, sorted, as a comma-joined string
// for error messages and diagnostics.
func (r *Registry) Supported() string {
	langs := make([]string, 0, len(r.byLang))
	for l := range r.byLang {
		langs = append(langs, string(l))
	}
	sort.Strings(langs)
	return strings.Join(langs, ", ")
}
