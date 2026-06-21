# Tagger — Issue-to-PR AI Coding Agent

Tagger turns a tracked issue into a pull request. Point it at a Jira ticket or GitHub issue and a target repository; it reads the requirements, writes the code, runs the tests, and opens a PR — autonomously, with a human-in-the-loop escape hatch when context is ambiguous.

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

**Context phase.** An LLM agent crawls Jira and Confluence via a read-only MCP connection and synthesises a structured requirements document. If the requirements are incomplete, the workflow pauses and waits for a human signal before continuing — no silent hallucination of missing context.

**Coding phase.** A second LLM agent runs inside an isolated Dagger container with the target repository mounted and the appropriate language toolchain available. It reads, writes, and runs tests in a loop until the suite passes, then pushes a feature branch and opens a pull request.

**Orchestration.** Temporal provides the durable execution layer: automatic retries, full event history, and the ability to resume a run that was interrupted mid-flight. The workflow and the LLM execution are deliberately kept separate — the workflow handles sequencing and state; Dagger handles all non-deterministic, side-effectful work.

## Key design decisions

**Sandboxed execution.** Every coding action happens inside a reproducible Dagger container. The agent cannot affect the host machine, and the same run will produce the same filesystem state regardless of where the worker is running.

**Git as the data broker.** The workflow never passes source trees between steps. Instead, each phase pushes to the Git remote and hands back only a branch name or commit SHA. This keeps Temporal's event history compact and the system composable.

**Language-agnostic by design.** The agent selects the appropriate toolchain — Go, Python, or others — either from an explicit override or by probing the target repository's marker files. Adding a new language is a self-contained extension.

**Pluggable LLM providers.** Both the context agent and the coding agent resolve their backend at startup. Anthropic, OpenAI-compatible endpoints, and Amazon Bedrock are supported out of the box.

## Requirements

- Go 1.23+
- [Dagger CLI](https://docs.dagger.io/install) ≥ v0.19
- A running [Temporal](https://docs.temporal.io/self-hosted-guide) server
- An LLM provider API key (Anthropic, OpenAI, or Bedrock)
- Atlassian API tokens (Jira + Confluence) for the context phase
- A GitHub token with permission to push branches and open pull requests

## Getting started

```sh
# 1. Configure
cp .env.example .env   # fill in your provider key, GitHub token, and Atlassian credentials

# 2. Start the infrastructure (Temporal + API server)
make up

# 3. Start the worker (runs on the host — needs the Dagger CLI and a Docker socket)
make worker

# 4. Trigger a run
make run ISSUE=PROJ-123 REPO=https://github.com/your-org/your-repo
```

The worker logs progress. When the run completes, the pull request URL is returned via the API and visible in the Temporal UI at `http://localhost:8233`.

### LLM providers

| Provider | Notes |
|---|---|
| Anthropic | Default. Set your API key in `.env`. |
| OpenAI-compatible | Covers OpenAI, OpenRouter, LiteLLM, and local models — set the base URL to point at any compatible endpoint. |
| Amazon Bedrock | Routed through a bundled LiteLLM proxy. Start with `make up-bedrock` and add your AWS credentials to `.env`. |

### Running without Docker

A fully local setup is supported for development: run a Temporal dev server on the host, start the worker directly, and trigger runs via the CLI or the HTTP API. See `.env.example` for the full configuration reference.

## Contributing

Install the git hooks after your first commit:

```sh
make hooks
```

This wires pre-commit checks (secret scanning, formatting, linting, `go mod tidy`) and a pre-push test gate. Requires [`pre-commit`](https://pre-commit.com) (`brew install pre-commit`). Run checks at any time without committing:

```sh
pre-commit run --all-files
```
