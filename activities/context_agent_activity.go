package activities

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"dagger.io/dagger"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/syncopatedNote/tagger/types"
)

// contextAgentSystemPrompt drives the Atlassian context-gathering agent. The LLM
// is given the read-only mcp-atlassian tools (jira_get_issue, jira_search,
// confluence_get_page, confluence_search, ...) and asked to crawl from the
// seed Jira ticket out to its linked Confluence design and any further linked
// issues, then report a single distilled brief plus a completeness verdict.
//
// The completeness rules encoded here mirror the policy in robots.md:
//   - the Jira body must be non-empty / non-placeholder,
//   - at least one reachable Confluence design must be found,
//   - every referenced link that is followed must resolve (no 404 / access
//     denied).
//
// Missing acceptance criteria is recorded as a note but does NOT, by itself,
// make the bundle incomplete.
const contextAgentSystemPrompt = `You are a meticulous staff engineer gathering ALL the context needed to implement a ticket. You have read-only Jira and Confluence tools. You cannot and must not create, edit, transition, or comment on anything.

You are given:
- ` + "`issue`" + `: the seed Jira issue key (or URL).
- ` + "`supplements`" + `: zero or more pieces of context a human supplied on previous attempts to fill gaps you reported. Treat each as authoritative — follow any links or IDs it contains.

Crawl, in order:
1. Read the seed Jira issue: its description AND its comments (jira_get_issue).
2. From the issue body and comments, extract every Confluence link/page and read each one (confluence_get_page or confluence_search).
3. In each Confluence design, read the body AND its comments. If a comment or the body mentions further Jira/Git issue keys, read those issues too.
4. Fold in anything from ` + "`supplements`" + `.

Then decide completeness using these rules EXACTLY:
- INCOMPLETE if the Jira issue has an empty or placeholder description (just a title, "TBD", "see design", etc. with no real content).
- INCOMPLETE if you found NO Confluence design page anywhere in the crawl.
- INCOMPLETE if any link you tried to follow failed (404, not found, permission denied).
- Otherwise COMPLETE. (A design that merely lacks an explicit "acceptance criteria" heading is still COMPLETE — just note it.)

Return ONLY a single JSON object, no prose, no markdown fences, matching:
{
  "requirements": "<the full distilled, implementation-ready brief assembled from the ticket + design + linked issues>",
  "title": "<short PR-title-style summary>",
  "complete": <true|false>,
  "missing": ["<specific human-readable gap>", ...]
}

When complete is true, "missing" must be []. When false, "missing" must name exactly what a human needs to supply (e.g. "PROJ-123 links no Confluence design page" or "Confluence page 45678 returned 403 — grant access or provide the design").`

// RunContextAgentActivity gathers all implementation context for a ticket by
// driving an LLM that has the read-only mcp-atlassian tool surface. It crawls
// Jira -> Confluence -> linked issues and returns a distilled brief plus a
// completeness verdict.
//
// This activity NEVER halts to wait for a human. If context is missing it simply
// returns Complete=false with the gaps; the *workflow* owns the decision to halt
// on a "supply-context" signal and re-invoke this activity with the human's
// supplements folded into the input (see CodingAgentWorkflow). That boundary is
// load-bearing: activities cannot receive signals, only workflows can.
func (a *Activities) RunContextAgentActivity(ctx context.Context, in types.GatherContextInput) (types.GatherContextResult, error) {
	logger := activity.GetLogger(ctx)
	logger.Info("Gathering Atlassian context",
		"attempt", activity.GetInfo(ctx).Attempt, "issue", in.IssueReference, "supplements", len(in.Supplements))

	if strings.TrimSpace(in.IssueReference) == "" {
		return types.GatherContextResult{}, temporal.NewNonRetryableApplicationError(
			"IssueReference is required", "ValidationError", nil)
	}
	if err := a.atlassian.validate(); err != nil {
		return types.GatherContextResult{}, temporal.NewNonRetryableApplicationError(
			err.Error(), "ValidationError", nil)
	}

	// Long-ish call (an LLM crawl over a remote API): heartbeat so a dead worker
	// is detected via HeartbeatTimeout rather than the full StartToCloseTimeout.
	// The 30s interval keeps several heartbeats inside the 5m HeartbeatTimeout
	// window even if the SDK throttles flushes during the blocking Dagger call.
	stopHeartbeat := startHeartbeatWithProgress(ctx, 30*time.Second, logger)
	defer stopHeartbeat()

	client, err := connectDagger(ctx)
	if err != nil {
		return types.GatherContextResult{}, err
	}
	defer client.Close()
	logger.Info("Dagger engine connected", "attempt", activity.GetInfo(ctx).Attempt)

	// Stand up the mcp-atlassian server as a Dagger service. Credentials are
	// injected as Dagger SECRETS (scrubbed from cache/logs); READ_ONLY_MODE pins
	// the tool surface to read-only so the agent can never mutate Atlassian data.
	mcpService := a.atlassianMCPService(client)

	env := client.Env().
		WithStringInput("issue", in.IssueReference,
			"the seed Jira issue key or URL to gather context for").
		WithStringInput("supplements", strings.Join(in.Supplements, "\n---\n"),
			"human-supplied context from previous attempts; may be empty").
		WithStringOutput("result",
			"a single JSON object: {requirements, title, complete, missing[]}")

	agent := client.LLM(dagger.LLMOpts{
		Model:       a.LLM.Model,
		MaxAPICalls: a.MaxContextLoops,
	}).
		WithEnv(env).
		WithMCPServer("atlassian", mcpService).
		WithSystemPrompt(contextAgentSystemPrompt).
		WithPrompt("Gather the full context for `issue`, applying any `supplements`, and return the JSON result.")

	logger.Info("Context agent loop running", "model", a.LLM.Model, "maxAPICalls", a.MaxContextLoops)
	raw, err := agent.Env().Output("result").AsString(ctx)
	if err != nil {
		// A Bedrock streaming tool-use failure (modelStreamErrorException / "Model
		// produced invalid sequence as part of ToolUse") is effectively transient:
		// one malformed sampling of a tool call that Bedrock rejects mid-stream. A
		// fresh attempt usually samples a well-formed call, so surface it under a
		// RETRYABLE error type — the gather activity's RetryPolicy then bounds it at
		// MaximumAttempts. This is deliberately distinct from the agent genuinely
		// burning its MaxAPICalls budget, which stays non-retryable
		// (AgentExhaustedError) because a blind retry would just repeat it.
		if isTransientStreamError(err) {
			logger.Warn("Context agent hit a transient model stream error; will retry",
				"attempt", activity.GetInfo(ctx).Attempt, "error", err)
			return types.GatherContextResult{}, temporal.NewApplicationErrorWithCause(
				"context agent hit a transient model stream error", "ModelStreamError", err)
		}
		logger.Error("Context agent loop failed to produce output", "error", err)
		return types.GatherContextResult{}, temporal.NewNonRetryableApplicationError(
			"context agent did not complete", "AgentExhaustedError", err)
	}
	logger.Info("Context agent raw output", "raw", truncate(raw, 500))

	bundle, err := parseGatherResult(raw)
	if err != nil {
		logger.Error("Failed to parse context agent output", "error", err, "raw", truncate(raw, 500))
		return types.GatherContextResult{}, fmt.Errorf("parsing context agent output: %w", err)
	}

	logger.Info("Context gather verdict",
		"complete", bundle.Complete, "missing", bundle.Missing, "title", bundle.Title)
	return bundle, nil
}

// atlassianMCPService builds the read-only mcp-atlassian server as a Dagger
// Service. The Atlassian credentials are provided as Dagger secrets so they are
// scrubbed from build caches and engine logs and never surface to the LLM
// context — the LLM only ever sees the server's TOOLS, never its env.
func (a *Activities) atlassianMCPService(client *dagger.Client) *dagger.Service {
	jiraToken := client.SetSecret("jira-api-token", a.atlassian.JiraToken)
	confToken := client.SetSecret("confluence-api-token", a.atlassian.ConfluenceToken)

	return client.Container().
		From(a.atlassian.Image).
		// Read-only: only get/search tools are exposed; no create/update/transition.
		WithEnvVariable("READ_ONLY_MODE", "true").
		WithEnvVariable("JIRA_URL", a.atlassian.JiraURL).
		WithEnvVariable("JIRA_USERNAME", a.atlassian.JiraUsername).
		WithSecretVariable("JIRA_API_TOKEN", jiraToken).
		WithEnvVariable("JIRA_SSL_VERIFY", a.atlassian.JiraSSLVerify).
		WithEnvVariable("CONFLUENCE_URL", a.atlassian.ConfluenceURL).
		WithEnvVariable("CONFLUENCE_USERNAME", a.atlassian.ConfluenceUsername).
		WithSecretVariable("CONFLUENCE_API_TOKEN", confToken).
		WithEnvVariable("CONFLUENCE_SSL_VERIFY", a.atlassian.ConfluenceSSLVerify).
		// Dagger's LLM.WithMCPServer speaks MCP over the service container's STDIO,
		// NOT over a network port, so run the server in stdio transport: its
		// stdin/stdout carry the MCP JSON-RPC handshake. Running it as an HTTP
		// server (streamable-http/--port) makes the agent hang forever — Dagger
		// writes `initialize` to stdin and the HTTP server never answers on stdout.
		AsService(dagger.ContainerAsServiceOpts{
			Args:          []string{"--transport", "stdio", "-vv"},
			UseEntrypoint: true,
		})
}

// parseGatherResult decodes the agent's JSON verdict into a typed result. It is
// tolerant of the model wrapping the JSON in stray prose or ```json fences.
func parseGatherResult(raw string) (types.GatherContextResult, error) {
	s := extractJSONObject(raw)
	if s == "" {
		return types.GatherContextResult{}, fmt.Errorf("no JSON object found in output: %q", truncate(raw, 200))
	}
	var parsed struct {
		Requirements string   `json:"requirements"`
		Title        string   `json:"title"`
		Complete     bool     `json:"complete"`
		Missing      []string `json:"missing"`
	}
	if err := json.Unmarshal([]byte(s), &parsed); err != nil {
		return types.GatherContextResult{}, fmt.Errorf("invalid JSON: %w", err)
	}
	if strings.TrimSpace(parsed.Requirements) == "" && parsed.Complete {
		return types.GatherContextResult{}, fmt.Errorf("agent reported complete but returned empty requirements")
	}
	return types.GatherContextResult{
		Requirements: strings.TrimSpace(parsed.Requirements),
		Title:        strings.TrimSpace(parsed.Title),
		Complete:     parsed.Complete,
		Missing:      parsed.Missing,
	}, nil
}

// extractJSONObject returns the substring from the first '{' to the last '}',
// which strips any leading/trailing prose or markdown fences the model emitted.
func extractJSONObject(s string) string {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end < 0 || end < start {
		return ""
	}
	return s[start : end+1]
}

// transientStreamErrorMarkers identify a Bedrock streaming failure that is
// effectively transient rather than a genuine agent exhaustion: the model
// emitted a malformed tool-use block mid-stream and Bedrock aborted the stream
// (modelStreamErrorException / "Model produced invalid sequence as part of
// ToolUse"). A fresh agent attempt re-samples the tool call and usually
// succeeds, so these are worth a bounded retry. Matched case-insensitively
// against the surfaced Dagger/LiteLLM error string.
var transientStreamErrorMarkers = []string{
	"modelstreamerrorexception",
	"invalid sequence as part of tooluse",
}

// isTransientStreamError reports whether err is a retryable Bedrock streaming
// tool-use failure (see transientStreamErrorMarkers).
func isTransientStreamError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range transientStreamErrorMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
