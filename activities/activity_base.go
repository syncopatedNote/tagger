// Package activities contains the side-effecting, non-deterministic Temporal
// activities. This is the ONLY layer allowed to touch the network, the
// filesystem, secrets, and the Dagger engine.
//
// This file is the foundation every activity is built on: the shared Activities
// dependency bundle and its NewActivities constructor (which reads all config
// from the environment ONCE at startup), plus the read-only Atlassian connection
// config the context agent consumes.
//
// The activities themselves live in sibling files:
//   - git_activities.go        ResolveBaseCommitActivity, CreatePullRequestActivity
//   - context_agent_activity.go RunContextAgentActivity (read-only Atlassian crawl)
//   - detect_language_activity.go DetectLanguageActivity
//   - coding_agent_activity.go  RunCodingAgentActivity (the sandboxed coding loop)
//
// Cross-cutting helpers (Dagger connect, heartbeats, env + string utilities) live
// in common_activities.go.
package activities

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/syncopatedNote/tagger/agent_registry"
	"github.com/syncopatedNote/tagger/llm_factory"
)

// atlassianConfig holds the connection settings for the read-only mcp-atlassian
// server used by RunContextAgentActivity. Tokens are kept here (worker-side
// only) and injected into the MCP container as Dagger secrets — never logged,
// never placed in a Temporal payload.
type atlassianConfig struct {
	Image string // mcp-atlassian image, e.g. ghcr.io/sooperset/mcp-atlassian:latest

	JiraURL       string
	JiraUsername  string
	JiraToken     string
	JiraSSLVerify string

	ConfluenceURL       string
	ConfluenceUsername  string
	ConfluenceToken     string
	ConfluenceSSLVerify string
}

// validate ensures the minimum Atlassian settings are present before we try to
// stand up the MCP server. Missing config is a non-retryable setup error.
func (c atlassianConfig) validate() error {
	var missing []string
	if strings.TrimSpace(c.JiraURL) == "" {
		missing = append(missing, "JIRA_URL")
	}
	if strings.TrimSpace(c.JiraUsername) == "" {
		missing = append(missing, "JIRA_USERNAME")
	}
	if strings.TrimSpace(c.JiraToken) == "" {
		missing = append(missing, "JIRA_API_TOKEN")
	}
	if strings.TrimSpace(c.ConfluenceURL) == "" {
		missing = append(missing, "CONFLUENCE_URL")
	}
	if strings.TrimSpace(c.ConfluenceToken) == "" {
		missing = append(missing, "CONFLUENCE_API_TOKEN")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing Atlassian configuration: %s", strings.Join(missing, ", "))
	}
	return nil
}

// Activities bundles the dependencies/configuration shared by every activity.
//
// SECRET CUSTODY (see Verification Q3): githubToken is loaded from the worker's
// environment at startup and lives ONLY here, on the worker. It is never placed
// in a workflow input, an activity result, or anything Temporal persists to its
// event history. Activities read it directly from this struct.
type Activities struct {
	// LLM is the resolved LLM backend config (provider, model, and the engine
	// env contract) selected once at startup by the llm_factory. Its Model feeds
	// dagger.LLMOpts.Model; the Dagger engine reads the matching provider env
	// vars (already present in the worker process) to pick the backend.
	LLM llm_factory.LLMConfig
	// MaxAgentLoops caps the tool-calling iterations the CODING agent may run in
	// one attempt — the Dagger-level runaway guard for the expensive loop.
	MaxAgentLoops int
	// MaxContextLoops caps the tool-calling iterations the ATLASSIAN context
	// agent may run. The crawl is cheaper than coding, so this is smaller.
	MaxContextLoops int
	// GitImage is a small image containing git, used only for the credentialed
	// push step (e.g. "alpine/git:latest").
	GitImage string

	// githubToken is the credential used for cloning private repos, pushing the
	// feature branch, and opening the PR. Held worker-side only.
	githubToken string
	// simulatePR, when true, skips the live GitHub API call and returns a
	// synthetic URL — handy for local runs, demos, and tests.
	simulatePR bool
	// atlassian holds the read-only mcp-atlassian connection config consumed by
	// RunContextAgentActivity. Tokens stay worker-side; see context_agent_activity.go.
	atlassian atlassianConfig
	// agents is the startup-time catalog of per-language coding-agent toolchains.
	// RunCodingAgentActivity resolves in.Language to a CodingAgent through it.
	// Built once in NewActivities and shared for the worker's lifetime.
	agents *agent_registry.Registry
}

// NewActivities builds an Activities instance from environment configuration.
//
// It resolves the LLM backend up front via the llm_factory: the provider comes
// from LLM_PROVIDER (default anthropic) and the model from AGENT_MODEL (a
// per-provider env var, e.g. ANTHROPIC_MODEL, is also honoured by the provider).
// A misconfigured backend (unknown provider or missing credentials) is fatal at
// startup — a worker that cannot reach an LLM can do no useful work, and failing
// here is far clearer than failing inside the first activity attempt.
func NewActivities() *Activities {
	llm, err := llm_factory.New().CreateLLM(os.Getenv("LLM_PROVIDER"), os.Getenv("AGENT_MODEL"))
	if err != nil {
		log.Fatalf("LLM provider configuration error: %v", err)
	}
	// Export the provider's env contract onto THIS process so the Dagger engine
	// the activities connect to (embedded/local) reads the right backend. This is
	// load-bearing for derived configs like Bedrock, where the factory translates
	// LLM_BEDROCK_* into OPENAI_BASE_URL / OPENAI_API_KEY / OPENAI_MODEL that were
	// never in the raw environment. os.Setenv here (startup, single goroutine) is
	// safe; never call it from inside an activity.
	for k, v := range llm.Env {
		if err := os.Setenv(k, v); err != nil {
			log.Fatalf("exporting LLM env %s: %v", k, err)
		}
	}

	return &Activities{
		LLM:             llm,
		MaxAgentLoops:   getenvInt("AGENT_MAX_LOOPS", 25),
		MaxContextLoops: getenvInt("AGENT_CONTEXT_MAX_LOOPS", 20),
		GitImage:        getenv("AGENT_GIT_IMAGE", "alpine/git:latest"),
		githubToken:     os.Getenv("GITHUB_TOKEN"),
		simulatePR:      getenv("AGENT_SIMULATE_PR", "true") == "true",
		agents:          agent_registry.New(),
		atlassian: atlassianConfig{
			Image:               getenv("ATLASSIAN_MCP_IMAGE", "ghcr.io/sooperset/mcp-atlassian:latest"),
			JiraURL:             os.Getenv("JIRA_URL"),
			JiraUsername:        os.Getenv("JIRA_USERNAME"),
			JiraToken:           os.Getenv("JIRA_API_TOKEN"),
			JiraSSLVerify:       getenv("JIRA_SSL_VERIFY", "true"),
			ConfluenceURL:       os.Getenv("CONFLUENCE_URL"),
			ConfluenceUsername:  os.Getenv("CONFLUENCE_USERNAME"),
			ConfluenceToken:     os.Getenv("CONFLUENCE_API_TOKEN"),
			ConfluenceSSLVerify: getenv("CONFLUENCE_SSL_VERIFY", "true"),
		},
	}
}
