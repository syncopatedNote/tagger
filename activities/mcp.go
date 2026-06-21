package activities

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"dagger.io/dagger"
)

// Logical names for the MCP servers in the Activities.mcpServers registry. Each
// is the lookup key, the WithMCPServer label the LLM sees, and the prefix used
// for the server's Dagger secret names.
const (
	mcpAtlassian = "atlassian"
	mcpGitHub    = "github"
	mcpContext7  = "context7"
)

// mcpServerSpec declaratively describes an MCP server to run as a Dagger stdio
// service. It centralizes the build-container -> inject env/secrets ->
// expose-over-stdio pattern every MCP integration shares, so adding a server is
// a data change (one entry in newMCPRegistry) rather than another hand-rolled
// constructor.
//
// A spec is PURE DATA — it captures no Dagger client — so the registry is built
// once at startup and shared read-only across activity goroutines. build(client)
// turns a spec into a live service at activity time, using that activity's own
// per-call client.
//
// SECRET CUSTODY: Secrets holds worker-side plaintext values keyed by the env
// var name the server reads. build injects them via client.SetSecret +
// WithSecretVariable, so they are scrubbed from build caches and engine logs and
// never enter the LLM context — the model only ever sees the server's TOOLS.
type mcpServerSpec struct {
	Name          string            // logical name (see constants above)
	Image         string            // container image to run the server from
	Env           map[string]string // plain (non-secret) env vars
	Secrets       map[string]string // env var name -> secret value (worker-side plaintext)
	Args          []string          // the stdio command/args passed to AsService
	UseEntrypoint bool              // run Args via the image ENTRYPOINT vs as the command
	RequiredKeys  []string          // Env/Secrets keys that must be non-empty to be usable
}

// validate reports whether every RequiredKey resolves to a non-empty value in
// either Env or Secrets. It is the single source of truth for "is this server
// configured?"; the calling activity decides whether a missing server is fatal
// (a required source) or simply skipped (an optional one). The error names
// exactly what is missing, so a misconfiguration surfaces clearly in the Temporal
// event history.
func (s mcpServerSpec) validate() error {
	var missing []string
	for _, k := range s.RequiredKeys {
		if strings.TrimSpace(s.Env[k]) == "" && strings.TrimSpace(s.Secrets[k]) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("MCP server %q is missing required configuration: %s",
			s.Name, strings.Join(missing, ", "))
	}
	return nil
}

// build stands the spec up as a Dagger stdio service using the given
// (per-activity) client. Env and secret keys are applied in sorted order so the
// resulting container layers are deterministic and cache-friendly. Secrets are
// injected as Dagger secrets (scrubbed from caches and logs).
//
// Dagger's LLM.WithMCPServer speaks MCP over the service container's STDIO (not a
// network port), so every spec runs its server in stdio transport — see the
// per-server Args in newMCPRegistry. Running such a server as an HTTP/--port
// service makes the agent hang forever: Dagger writes `initialize` to stdin and
// the HTTP server never answers on stdout.
func (s mcpServerSpec) build(client *dagger.Client) *dagger.Service {
	c := client.Container().From(s.Image)

	for _, k := range sortedKeys(s.Env) {
		c = c.WithEnvVariable(k, s.Env[k])
	}
	for _, k := range sortedKeys(s.Secrets) {
		secret := client.SetSecret(s.Name+"-"+strings.ToLower(k), s.Secrets[k])
		c = c.WithSecretVariable(k, secret)
	}

	return c.AsService(dagger.ContainerAsServiceOpts{
		Args:          s.Args,
		UseEntrypoint: s.UseEntrypoint,
	})
}

// sortedKeys returns the keys of m in lexical order, for deterministic env
// layering (Go map iteration order is randomized).
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// newMCPRegistry builds the startup-time registry of every MCP server spec from
// the environment. Every server is ALWAYS registered; its availability is decided
// at activity time via spec.validate() (RequiredKeys), so a missing credential
// degrades that one server rather than crashing the worker. Read once at startup
// and shared read-only thereafter.
func newMCPRegistry() map[string]mcpServerSpec {
	return map[string]mcpServerSpec{
		// Atlassian (Jira + Confluence), read-only — the context agent's primary,
		// required source. READ_ONLY_MODE pins the tool surface to get/search only.
		mcpAtlassian: {
			Name:  mcpAtlassian,
			Image: getenv("ATLASSIAN_MCP_IMAGE", "ghcr.io/sooperset/mcp-atlassian:latest"),
			Env: map[string]string{
				"READ_ONLY_MODE":        "true",
				"JIRA_URL":              os.Getenv("JIRA_URL"),
				"JIRA_USERNAME":         os.Getenv("JIRA_USERNAME"),
				"JIRA_SSL_VERIFY":       getenv("JIRA_SSL_VERIFY", "true"),
				"CONFLUENCE_URL":        os.Getenv("CONFLUENCE_URL"),
				"CONFLUENCE_USERNAME":   os.Getenv("CONFLUENCE_USERNAME"),
				"CONFLUENCE_SSL_VERIFY": getenv("CONFLUENCE_SSL_VERIFY", "true"),
			},
			Secrets: map[string]string{
				"JIRA_API_TOKEN":       os.Getenv("JIRA_API_TOKEN"),
				"CONFLUENCE_API_TOKEN": os.Getenv("CONFLUENCE_API_TOKEN"),
			},
			Args:          []string{"--transport", "stdio", "-vv"},
			UseEntrypoint: true,
			RequiredKeys: []string{
				"JIRA_URL", "JIRA_USERNAME", "JIRA_API_TOKEN",
				"CONFLUENCE_URL", "CONFLUENCE_API_TOKEN",
			},
		},

		// GitHub, read-only — an OPTIONAL source for the context agent, attached only
		// when a GitHub token is present. GITHUB_READ_ONLY filters the surface to read
		// tools regardless of toolset, so the agent can fetch issues/PRs/files but
		// never mutate anything; GITHUB_TOOLSETS scopes it to the crawl's needs. The
		// PAT reuses the worker's GITHUB_TOKEN (read-only mode blocks writes whatever
		// the token's scopes).
		mcpGitHub: {
			Name:  mcpGitHub,
			Image: getenv("GITHUB_MCP_IMAGE", "ghcr.io/github/github-mcp-server"),
			Env: map[string]string{
				"GITHUB_READ_ONLY": "1",
				"GITHUB_TOOLSETS":  "issues,repos,pull_requests",
			},
			Secrets: map[string]string{
				"GITHUB_PERSONAL_ACCESS_TOKEN": os.Getenv("GITHUB_TOKEN"),
			},
			Args:          []string{"stdio"},
			UseEntrypoint: true,
			RequiredKeys:  []string{"GITHUB_PERSONAL_ACCESS_TOKEN"},
		},

		// Context7 docs — the coding agent's library/framework reference. No
		// credentials, so no RequiredKeys (always available). The node image runs npx
		// directly as the command, not via an entrypoint.
		mcpContext7: {
			Name:  mcpContext7,
			Image: "node:22-alpine",
			Args:  []string{"npx", "-y", "@upstash/context7-mcp"},
		},
	}
}
