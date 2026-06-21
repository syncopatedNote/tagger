# Issue-Driven AI Coding Agent — Temporal × Dagger

An issue (Jira ticket / GitHub issue) goes in; a pull request comes out. A
**Temporal** workflow orchestrates the long-running, fault-tolerant process,
while a **Dagger** containerised sandbox runs the actual LLM coding loop in
isolation. The coding agent is **multi-language**: the target toolchain (Go,
Python, … — extensible via the `agent_registry`) is selected per run by an
explicit override or by auto-detecting the repo's marker files.

## Architecture

```
        ┌──────────────────────── Temporal (orchestration, deterministic) ───────────────────────┐
        │                                                                                          │
input → │  CodingAgentWorkflow                                              human "supply-context" │ → PR URL
        │     │                                                              signal ▲ (on a gap)   │
        │     ├─ RunContextAgentActivity ─► requirements + Complete? ────────────┘                 │
        │     ├─ ResolveBaseCommitActivity ► BaseCommitSHA       (strings only; runs in parallel)  │
        │     ├─ DetectLanguageActivity ───► Language    (override wins; else probe marker files)  │
        │     ├─ RunCodingAgentActivity ───► BranchName + HeadCommitSHA   (strings only)           │
        │     └─ CreatePullRequestActivity ► PullRequestURL              (strings only)            │
        └─────────────────────────────────┬────────────────────────────────────────────────────────┘
                            ┌──────────────┴──────────────┐
                            ▼                              ▼
        ┌──── Dagger: context agent ────┐   ┌──── Dagger: coding agent ─────┐
        │  mcp-atlassian (read-only)    │   │  per-language workspace (go/   │
        │  Jira → Confluence → issues   │   │  python/…) + Context7          │ self-heals
        │  → JSON {requirements,complete}│   │  read → write → test → fix     │ token = Dagger Secret
        └───────────────────────────────┘   │  verify → push feature branch  │
                                            └────────────────────────────────┘
                                          │
                                          ▼
                                 Git remote = the data broker
```

### Two strict isolation rules

1. **Workflows are deterministic.** `workflows/` never imports or calls Dagger,
   the network, the clock, or randomness. It only sequences activities and
   passes tiny strings.
2. **All Dagger execution lives in activities.** Only `activities/context_agent_activity.go`,
   `activities/detect_language_activity.go`, and `activities/coding_agent_activity.go`
   open a Dagger connection.

### Claim-check pattern

Activities return **primitives** — a commit SHA, a branch name, a URL — never
source trees. The repository contents are brokered through the **Git remote**:
the Dagger activity pushes a branch and hands the workflow back only its name.
Temporal's event history therefore stays tiny.

## Layout

| Path                                    | Role                                                              |
| --------------------------------------- | ---------------------------------------------------------------- |
| `types/`                                | Dependency-free DTOs that travel between workflow and activities |
| `workflows/agent_workflow.go`           | `CodingAgentWorkflow` — orchestration, retry policy, gather/signal-halt + language-select |
| `activities/git_activities.go`          | `Activities` struct, `ResolveBaseCommitActivity`, `CreatePullRequestActivity` |
| `activities/context_agent_activity.go`  | `RunContextAgentActivity` — read-only Atlassian (Jira+Confluence) MCP crawl |
| `activities/detect_language_activity.go`| `DetectLanguageActivity` — probes repo root markers to pick the toolchain |
| `activities/coding_agent_activity.go`   | `RunCodingAgentActivity` — language-agnostic coding sandbox + Context7 MCP |
| `agent_registry/`                        | `Registry` (build-once catalog) + `agents/` (per-language `CodingAgent` toolchains: go, python) |
| `llm_factory/`                           | `Factory` (build-once provider catalog) + `providers/` (anthropic, openai, bedrock-via-LiteLLM) — resolves the Dagger LLM config |
| `dagger/toolbox/`                        | A Dagger module exposing `ReadFile`/`WriteFile`/`RunTests` as **named** LLM tools |
| `worker/`                               | Registers the workflow + activities and runs the worker          |
| `starter/`                              | Kicks off one workflow execution (CLI)                           |
| `server/`                               | Gin HTTP API: start / status / signal a run                      |

> See `robots.md` for the full architecture, rules, completeness policy, MCP wiring, and the human-in-the-loop signal contract.

## Tool surfacing: two approaches

Both make developer operations available to the LLM as schema'd tools:

- **Inline (used by the activity):** binding a `Container` to the `Env` exposes
  that container's operations (read file, write file, exec `go test`) to the
  model. Works in a plain SDK client with no codegen.
- **Named (`dagger/toolbox`):** a typed `Toolbox` object whose `ReadFile`,
  `WriteFile`, and `RunTests` methods surface as the `read_file`, `write_file`,
  and `run_tests` tools. Run it with `dagger call develop ...`. This is the
  idiomatic way to give the agent a tight, legible tool surface.

## Running it

Prerequisites: a Temporal server, the Dagger engine/CLI (≥ v0.19 for MCP), Go
1.23+, an LLM provider configured via the `llm_factory` (see below), and
Atlassian API tokens for the context phase.

### Choosing an LLM provider

The backend is selected once at startup by the **`llm_factory`** from
`LLM_PROVIDER` (default `anthropic`). The factory exists because the agent loop
runs *inside Dagger* (`client.LLM`), which picks its provider from engine
environment variables — so the factory's job is to validate the right
credentials and emit the model string + env contract the engine needs, not to
return an SDK client.

| `LLM_PROVIDER` | Required env                                                    | Notes                                                            |
| -------------- | --------------------------------------------------------------- | ---------------------------------------------------------------- |
| `anthropic`    | `ANTHROPIC_API_KEY`                                             | Default. Model from `AGENT_MODEL`/`ANTHROPIC_MODEL`.             |
| `openai`       | `OPENAI_API_KEY`                                               | Set `OPENAI_BASE_URL` to target OpenRouter / LiteLLM / local.   |
| `bedrock`      | `LLM_BEDROCK_PROXY_URL`, `LLM_BEDROCK_MODEL`                    | Amazon Bedrock via the bundled [LiteLLM proxy](https://docs.dagger.io/reference/configuration/llm/#amazon-bedrock-via-litellm-proxy) (OpenAI-compatible). |

Adding a provider is one file in `llm_factory/providers/` plus one line in
`llm_factory.New()`. See `.env.example` for the full per-provider knobs.

**Bedrock setup:** Dagger can't talk to Bedrock directly, so a LiteLLM proxy
(`docker-compose.yml` + `litellm_config.yaml`) fronts it as an OpenAI-compatible
API:

```sh
# put AWS creds in .env (AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_REGION)
docker compose --profile bedrock up -d litellm     # proxy on :4000
# then in .env: LLM_PROVIDER=bedrock,
#   LLM_BEDROCK_PROXY_URL=http://localhost:4000,
#   LLM_BEDROCK_MODEL=bedrock-claude-3-5-sonnet     # a model_name in litellm_config.yaml
```

Add Bedrock models by editing the `model_list` in `litellm_config.yaml`.

### Option A — Hybrid: Docker Compose infra + the worker on the host

This is the realistic way to run the whole flow. `docker-compose.yml` runs the
durable infra — Postgres-backed Temporal, the Temporal UI, and the Gin API
(built from `server/Dockerfile.server`, a multi-stage scratch image). The **worker runs
on the host**: it drives the Dagger agent loop, which spawns containers via a
Dagger engine, so it needs the Dagger CLI + a docker socket (running it on the
host avoids docker-in-docker).

> **Compose alone does NOT run a full job.** Without the host worker, a triggered
> run appears in the Temporal UI but every activity stays *Scheduled* forever —
> nothing is polling the task queue, and there is no Dagger engine. You must run
> both the compose infra **and** `go run ./worker`.

```sh
cp .env.example .env     # fill in GITHUB_TOKEN, the LLM provider, JIRA_*/CONFLUENCE_*

# 1. Infra (Anthropic/OpenAI providers)            → or use `make up`
docker compose up -d                     # postgres + temporal + ui + server
#   1b. Bedrock provider? start the proxy too:     → or `make up-bedrock`
#       docker compose --profile bedrock up -d

# 2. The worker, on the host                        → or `make worker`
go run ./worker          # host → Temporal at localhost:7233; engine spawns containers

# 3. Trigger a run (see "Triggering a run" below)   → or `make run ISSUE=… REPO=…`
```

→ Temporal UI `http://localhost:8233` · Gin API `http://localhost:8080` · **Swagger UI `http://localhost:8080/swagger/index.html`** · (Bedrock) LiteLLM `http://localhost:4000`

**Bedrock host-networking gotcha.** When the worker runs on the host but LiteLLM
runs in compose, the *Dagger engine* (not the worker) makes the OpenAI-compatible
call, and on macOS that engine is itself a container — so `localhost:4000` points
at the engine, not your host. Set
`LLM_BEDROCK_PROXY_URL=http://host.docker.internal:4000` (the `.env.example`
default) so the engine reaches the published proxy port. AWS creds and
`LLM_BEDROCK_MODEL` must also be set; the proxy needs `--profile bedrock`.

### Option B — everything on the host (no Docker)

```sh
go mod tidy
cp .env.example .env     # then fill in GITHUB_TOKEN, ANTHROPIC_API_KEY, JIRA_*/CONFLUENCE_*

# 1. Temporal dev server (separate terminal)
temporal server start-dev

# 2. Worker (reads .env)
go run ./worker

# 3a. Start a run via CLI (-lang is optional; omit to auto-detect the toolchain)
go run ./starter -issue "PROJ-123" -repo "https://github.com/owner/repo" -base main -lang python

# 3b. …or via the HTTP API ("language" is optional; omit to auto-detect)
go run ./server   # :8080
curl -X POST localhost:8080/v1/runs -H 'content-type: application/json' \
  -d '{"issue_reference":"PROJ-123","repo_url":"https://github.com/owner/repo","language":"python"}'
# if it halts for missing context, supply it:
curl -X POST localhost:8080/v1/runs/<workflow_id>/signal -H 'content-type: application/json' \
  -d '{"info":"design: https://your.atlassian.net/wiki/..."}'
```

### API docs (Swagger UI)

The Gin server serves interactive OpenAPI docs at
**`http://localhost:8080/swagger/index.html`** (raw spec at `/swagger/doc.json`).
The spec is generated from annotations on the handlers in `server/main.go` into
`server/docs/` by [`swaggo/swag`](https://github.com/swaggo/swag). Regenerate it
after changing a handler or its request/response types:

```sh
make swagger        # installs the swag CLI if missing, then runs `swag init`
```

`server/docs/` is committed so the image builds without the `swag` CLI; keep it
in sync via `make swagger` when you touch the API.

### Configuration

All config is via env vars, loaded from `.env` (see `.env.example`) or the real
environment. The full table — including the `JIRA_*` / `CONFLUENCE_*` Atlassian
settings, `AGENT_CONTEXT_MAX_LOOPS`, `ATLASSIAN_MCP_IMAGE`, and `HTTP_ADDR` — is
documented in `robots.md` §15.

Context is gathered via `RunContextAgentActivity`, which drives the read-only
mcp-atlassian MCP server. Supply missing context via the HTTP signal API or the
Temporal UI.

## Code quality

The repo ships pre-commit hooks that guard every commit and push.

**First-time setup** — run once after your initial commit:

```sh
make hooks
```

This runs `pre-commit install --install-hooks`, which wires both the
`pre-commit` and `pre-push` git hooks and pre-downloads all linter environments
(gitleaks, golangci-lint). Requires `pre-commit` (`brew install pre-commit`).

**Commit-time checks** (fast — blocks the commit):

| Stage | What it catches |
|---|---|
| `detect-private-key` | PEM private keys accidentally staged |
| `check-merge-conflict` | Unresolved `<<<<<<` markers |
| `check-added-large-files` | Files > 1 MB |
| `check-yaml` | Malformed YAML |
| `end-of-file-fixer` / `trailing-whitespace` | Whitespace noise |
| gitleaks | Secrets / credentials in staged diff |
| golangci-lint fmt | `gofmt` + `goimports` formatting |
| golangci-lint | `go vet` + standard linters (new code only) |
| go vet | Host-toolchain vet over the project package list |
| go mod tidy | Fails if `go.mod`/`go.sum` need updating |

**Push-time checks** (slower — blocks `git push`):

| Stage | What it catches |
|---|---|
| go test | Full test suite via `make test` |

Run all hooks manually without committing:

```sh
pre-commit run --all-files
```
