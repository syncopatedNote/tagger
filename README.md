# Tagger — Issue-to-PR AI Coding Agent

Tagger turns a tracked issue into a pull request. Point it at a Jira ticket and a target repository; it reads the requirements, writes the code, runs the tests, and opens a PR — autonomously, with a human-in-the-loop escape hatch when context is ambiguous.

## The problem

Modern engineering teams move fast, but the gap between a well-written issue and working code still requires significant human attention. Tagger bridges that gap by treating the issue as a specification and the pull request as the deliverable, automating the full cycle in a way that is auditable, resumable, and production-safe.

## How it works

Tagger runs as a durable, two-phase workflow:

```
                         ┌─────────────────────────────────┐
                         │   Temporal — durable workflow   │
                         │                                 │
  Issue reference  ─────►│  1. Context phase               │
                         │     Read Jira + Confluence,      │
  Human signal     ─────►│     extract requirements,        │
  (if needed)            │     flag ambiguity               │
                         │                                 │
                         │  2. Coding phase                │
                         │     LLM agent inside a          │
                         │     Dagger sandbox:             │
                         │     write → test → fix → push   │
                         │                                 │──► Pull Request
                         └─────────────────────────────────┘
```

**Context phase.** An LLM agent crawls Jira and Confluence via a read-only MCP connection and synthesises a structured requirements document. If a GitHub token is configured, it also optionally reads referenced GitHub issues, PRs, and files. If the requirements are incomplete, the workflow pauses and waits for a human signal before continuing — no silent hallucination of missing context.

**Coding phase.** A second LLM agent runs inside an isolated Dagger container with the target repository mounted and the appropriate language toolchain available. It reads, writes, and runs tests in a loop until the suite passes, then pushes a feature branch and opens a pull request.

**Orchestration.** Temporal provides the durable execution layer: automatic retries, full event history, and the ability to resume a run that was interrupted mid-flight. The workflow and the LLM execution are deliberately kept separate — the workflow handles sequencing and state; Dagger handles all non-deterministic, side-effectful work.

## Key design decisions

**Sandboxed execution.** Every coding action happens inside a reproducible Dagger container. The agent cannot affect the host machine, and the same run will produce the same filesystem state regardless of where the worker is running.

**Git as the data broker.** The workflow never passes source trees between steps. Instead, each phase pushes to the Git remote and hands back only a branch name or commit SHA. This keeps Temporal's event history compact and the system composable.

**Language-agnostic by design.** The agent selects the appropriate toolchain — Go, Python, or others — either from an explicit override or by probing the target repository's marker files. Adding a new language is a self-contained extension.

**Per-activity LLM configuration.** The context agent and coding agent can be pointed at different LLM backends or models independently. A cheaper, faster model is often sufficient for the Jira/Confluence crawl; the coding agent benefits from the most capable model available. Both fall back to a shared global setting when no per-role override is set.

**Pluggable LLM providers.** Anthropic, OpenAI-compatible endpoints, and Amazon Bedrock (via a bundled LiteLLM proxy) are supported out of the box.

## Requirements

- Go 1.26+
- [Dagger CLI](https://docs.dagger.io/install) v0.21.7 (required on the host only when running the worker locally via `make worker`; bundled inside the compose worker image)
- Docker (for the compose stack and Dagger engine)
- An LLM provider API key (Anthropic, OpenAI, or Bedrock)
- Atlassian API tokens (Jira + Confluence) for the context phase
- A GitHub token with permission to push branches and open pull requests

## Getting started

```sh
# 1. Configure
cp .env.example .env   # fill in your provider key, GitHub token, and Atlassian credentials

# 2. Start the full stack
make up
```

`make up` brings up the complete stack in Docker:

| Service | Purpose |
|---|---|
| `postgres` | Temporal's persistence backend |
| `temporal` | Temporal server (auto-provisions schema on first boot) |
| `temporal-ui` | Temporal web UI → http://localhost:8233 |
| `server` | Gin HTTP API for triggering and signalling runs → http://localhost:8080 |
| `litellm` | LiteLLM proxy fronting Amazon Bedrock as an OpenAI-compatible API → http://localhost:4000 |
| `dagger-engine` | Persistent Dagger engine with LLM credentials baked in at container start |
| `worker` | Temporal worker that drives the Dagger agent loop |

```sh
# 3. Trigger a run
make run ISSUE=PROJ-123 REPO=https://github.com/your-org/your-repo

# Optionally pin the language toolchain (skip auto-detection)
make run ISSUE=PROJ-123 REPO=https://github.com/your-org/your-repo LANGUAGE=python
```

The worker logs progress. When the run completes, the pull request URL is returned via the API and visible in the Temporal UI at `http://localhost:8233`.

### LLM providers

| Provider | `LLM_PROVIDER` | Notes |
|---|---|---|
| Anthropic | `anthropic` | Set `ANTHROPIC_API_KEY` in `.env`. |
| OpenAI-compatible | `openai` | Covers OpenAI, OpenRouter, LiteLLM, and local models — set `OPENAI_BASE_URL` for any compatible endpoint. |
| Amazon Bedrock | `bedrock` | Routed through the bundled LiteLLM proxy (always starts with `make up`). Add your AWS credentials and set `LLM_BEDROCK_MODEL` to a model name from `litellm_config.yaml`. |

### Per-activity model overrides

The context agent and coding agent can use different models. Set any combination in `.env`:

```sh
# Context agent — cheaper/faster model is usually sufficient for Jira/Confluence crawling
CONTEXT_LLM_PROVIDER=bedrock
CONTEXT_LLM_MODEL=nova-lite

# Coding agent — most capable model for implementation
CODING_LLM_PROVIDER=bedrock
CODING_LLM_MODEL=claude-sonnet-4.5
```

Unset variables fall back to the global `LLM_PROVIDER` / `AGENT_MODEL`. Existing deployments that only set the global vars keep working unchanged.

### Running the worker locally

To run the worker on the host instead of in compose (useful during development):

```sh
# Start the supporting stack (without the worker service)
make up

# Run the worker on the host — picks up .env automatically
make worker
```

The worker connects to the compose-managed `dagger-engine` container via `_EXPERIMENTAL_DAGGER_RUNNER_HOST` in `.env`. The host must have the Dagger CLI v0.21.7 installed and a Docker socket available.

### Supplying missing context

If the context agent can't gather enough information from the Jira ticket, the workflow halts and waits. Supply the missing context via the API:

```sh
curl -X POST localhost:8080/v1/runs/<workflow_id>/signal \
  -H 'content-type: application/json' \
  -d '{"info":"design doc: https://your.atlassian.net/wiki/...."}'
```

Or use the "Send Signal" button in the Temporal UI at `http://localhost:8233`.

## Contributing

Install the git hooks after your first clone:

```sh
make hooks
```

This wires pre-commit checks (secret scanning, formatting, linting, `go mod tidy`) and a pre-push test gate. Requires [`pre-commit`](https://pre-commit.com) (`brew install pre-commit`). Run checks at any time without committing:

```sh
pre-commit run --all-files
```
