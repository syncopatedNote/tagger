// Command worker runs the Temporal worker: it polls the task queue and executes
// the CodingAgentWorkflow and its activities.
//
// Required environment:
//   - GITHUB_TOKEN          credential for clone/push/PR (worker-side only)
//   - an LLM provider's credentials (see below), read by the Dagger engine for
//     the LLM. The llm_factory resolves these at startup from LLM_PROVIDER; the
//     resolved env is exported onto this process so the Dagger engine sees it.
//
// LLM provider (see llm_factory; LLM_PROVIDER defaults to "anthropic"):
//   - anthropic: ANTHROPIC_API_KEY (+ optional ANTHROPIC_MODEL, ANTHROPIC_BASE_URL)
//   - openai:    OPENAI_API_KEY (+ optional OPENAI_MODEL, OPENAI_BASE_URL)
//   - bedrock:   LLM_BEDROCK_PROXY_URL, LLM_BEDROCK_MODEL (+ optional
//     LLM_BEDROCK_PROXY_KEY) — Bedrock via a LiteLLM OpenAI-compatible proxy
//
// Atlassian context gathering (see activities.NewActivities):
//   - JIRA_URL, JIRA_USERNAME, JIRA_API_TOKEN, CONFLUENCE_URL,
//     CONFLUENCE_USERNAME, CONFLUENCE_API_TOKEN
//
// Optional environment (see activities.NewActivities):
//   - LLM_PROVIDER, AGENT_MODEL, AGENT_MAX_LOOPS, AGENT_CONTEXT_MAX_LOOPS,
//     AGENT_GIT_IMAGE, AGENT_SIMULATE_PR, ATLASSIAN_MCP_IMAGE
//   - TEMPORAL_HOSTPORT     defaults to localhost:7233
//
// All of the above can be supplied via a .env file in the working directory.
package main

import (
	"log"
	"os"

	"github.com/joho/godotenv"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/syncopatedNote/tagger/activities"
	"github.com/syncopatedNote/tagger/workflows"
)

func main() {
	// Load .env into the process environment if present. A missing .env is fine
	// (CI / production inject real env vars), so the error is intentionally
	// ignored — godotenv.Load never overrides variables already set in the env.
	_ = godotenv.Load()

	hostPort := os.Getenv("TEMPORAL_HOSTPORT")
	if hostPort == "" {
		hostPort = client.DefaultHostPort // localhost:7233
	}

	c, err := client.Dial(client.Options{HostPort: hostPort})
	if err != nil {
		log.Fatalf("unable to create Temporal client: %v", err)
	}
	defer c.Close()

	w := worker.New(c, workflows.TaskQueue, worker.Options{})

	// Register the workflow (deterministic) and the activity set (side-effecting).
	w.RegisterWorkflow(workflows.CodingAgentWorkflow)
	w.RegisterActivity(activities.NewActivities())

	log.Printf("worker listening on task queue %q (temporal=%s)", workflows.TaskQueue, hostPort)
	if err := w.Run(worker.InterruptCh()); err != nil {
		log.Fatalf("worker stopped: %v", err)
	}
}
