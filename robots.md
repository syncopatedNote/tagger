# ROBOTS.md — Agent Context for the Tagger Codebase

Read this file in full before making any change. It is written for you, an AI coding agent. Every section is a directive, not an explanation. Where rationale appears, it is one line — enough to prevent a specific mistake, no more.

---

## 1. What This System Does (30 seconds)

A Temporal workflow takes a Jira issue key and a GitHub repo URL and produces a pull request:

1. **Context agent** — LLM with read-only Atlassian MCP tools crawls Jira → Confluence → linked issues, returns a distilled requirements brief. If context is missing, the workflow halts and waits for a human signal.
2. **Coding agent** — LLM inside a Dagger container with the repo mounted writes code, runs the test suite, fixes failures, iterates until the suite passes, then pushes a branch.
3. **PR** — the workflow opens a pull request for that branch.

Two agents. Two Temporal activities. One human checkpoint. Everything else is automatic.

---

## 2. Repository Map

| Path | What it is | Rule when touching it |
|---|---|---|
| `types/types.go` | All DTOs that cross activity boundaries | Fields must be short primitives only — no blobs, no file contents, no slices larger than a few strings |
| `workflows/agent_workflow.go` | The Temporal workflow | No I/O of any kind. No Dagger. No `os`, `net`, `rand`, `time.Now()`. See §4 |
| `activities/activity_base.go` | `Activities` struct + `NewActivities()` | All config is read here once at startup. Never call `os.Getenv` inside an activity method |
| `activities/llm_roles.go` | Per-activity LLM role map | Add one line to `AllRoles` when adding a new agent activity |
| `activities/coding_agent_activity.go` | The Dagger sandbox coding loop | Language-agnostic. Never hardcode language-specific logic here |
| `activities/context_agent_activity.go` | Atlassian MCP crawl | Never mutate Atlassian data. `READ_ONLY_MODE=true` must stay |
| `activities/mcp.go` | MCP server registry | Add new MCP servers here as `mcpServerSpec` entries |
| `agent_registry/agents/coding_agent_interface.go` | `CodingAgent` interface + `BuildSystemPrompt` | No Dagger or Temporal imports. Returns plain data only |
| `agent_registry/agents/go_agent.go` | Go toolchain config | No logic — only image name, cache mounts, warmup argv, test argv, persona string |
| `agent_registry/agents/python_agent.go` | Python toolchain config | Same. Warmup installs deps; see §10 for the exact logic |
| `agent_registry/agent_registry.go` | Startup catalog of agents | Register new language agents here |
| `llm_factory/providers/` | Per-provider LLM config | No Dagger or Temporal imports. Returns `LLMConfig` (strings + maps) only |
| `llm_factory/llm_factory.go` | Factory that resolves provider → config | Wire new providers here |
| `server/main.go` | Gin HTTP API | Thin Temporal client. No Dagger. Regenerate `server/docs/` after changing handlers |
| `worker/main.go` | Boots the Temporal worker | Registers the whole `Activities` struct — never register individual functions |
| `litellm_config.yaml` | LiteLLM proxy model list | Add Bedrock models here. Every entry needs `drop_params: true` |
| `docker-compose.yml` | Full stack (7 services) | See §11 for the service dependency chain |

**Module path:** `github.com/syncopatedNote/tagger` · **Go:** 1.26.1 · **Dagger:** v0.21.7 · **Temporal:** v1.31.0

---

## 3. Hard Invariants — Never Break These

**1. No I/O in `workflows/`.** Temporal replays workflow code from its event history. Any non-determinism (network call, clock read, random value, env var read, goroutine outside `workflow.Go`) causes a permanent non-determinism error on replay. The workflow may only call `workflow.ExecuteActivity`, `workflow.ExecuteChildWorkflow`, `workflow.Now(ctx)`, `workflow.Go`, pure helper functions, and types from `types/`.

**2. No source blobs in Temporal payloads.** Every field in `types/types.go` must be a short primitive — a SHA, a branch name, a URL, a short string. Never add a `[]byte`, file contents, or a directory. The Git remote is the data broker; Temporal sees only pointers.

**3. No credentials in the agent workspace container.** The LLM workspace container and the pusher container must be separate `*dagger.Container` instances. Never call `WithSecretVariable` on a container that has been or will be bound to an `Env` as an LLM tool.

**4. No git commands inside the agent workspace.** The workspace is built from `client.Git(...).Tree()` — a plain directory with no `.git`. Git operations always fail there. The pusher container (a separate alpine/git container with credentials) handles all commits and pushes.

**5. The verification gate is mandatory.** After the coding agent declares completion, the activity independently re-runs the toolchain's test command on the completed container. Never remove, comment out, or skip this gate.

**6. `agent_registry/agents/` and `llm_factory/providers/` must stay import-free.** These packages return plain strings and maps. They never import Dagger, Temporal, or any package that does I/O. The `activities` layer is the only place that translates their output into Dagger objects.

---

## 4. Dependency Direction

```
worker  → workflows, activities
server  → workflows, types
workflows → activities (method name resolution only via nil receiver), types
activities → types, agent_registry, llm_factory, dagger.io/dagger, go.temporal.io/sdk
agent_registry → agent_registry/agents
agent_registry/agents → (nothing)
llm_factory → llm_factory/providers
llm_factory/providers → (nothing)
types → (nothing)
```

The arrow never points back. If you need a Dagger type in `workflows/`, you are wrong — extract the work into an activity. If you need a Dagger type in `agent_registry/agents/`, you are wrong — return plain data and let `activities` translate it.

**The nil receiver pattern.** The workflow references activities like this:

```go
var a *activities.Activities
workflow.ExecuteActivity(ctx, a.RunCodingAgentActivity, input)
```

`a` is nil. This is correct and intentional. It gives the SDK a typed method value (for name registration) without importing any concrete dependency. Do not change it to a non-nil instance or a string name.

---

## 5. Error Classification

Every error path must be classified. "I'm not sure" defaults to non-retryable.

| Scenario | Type | How to return |
|---|---|---|
| Missing required input field | `"ValidationError"` | `temporal.NewNonRetryableApplicationError(msg, "ValidationError", nil)` |
| Unknown language / provider | `"ValidationError"` | Same |
| Branch / repo not found | `"ValidationError"` | Same |
| Missing token / credential on worker | `"ValidationError"` | Same |
| Agent hit `MaxAPICalls` cap | `"AgentExhaustedError"` | `temporal.NewNonRetryableApplicationError(msg, "AgentExhaustedError", err)` |
| Test suite still fails after agent completes | `"AgentExhaustedError"` | Same |
| Transient network / HTTP 5xx | *(retryable)* | Plain `fmt.Errorf(...)` — Temporal retries via the activity's `RetryPolicy` |
| Dagger engine connection failure | *(retryable)* | Plain `fmt.Errorf(...)` |
| `git push` auth failure | *(retryable)* | Plain `fmt.Errorf(...)` |

`NonRetryableErrorTypes` lists in the workflow must include `"ValidationError"` and `"AgentExhaustedError"` for every activity that can produce them.

---

## 6. How to Extend the System

### Add a new coding-agent language

1. Create `agent_registry/agents/<lang>_agent.go`. Implement `CodingAgent`:
   - `Language()` — return a new `Language` constant defined in `coding_agent_interface.go`
   - `BaseImage()` — the toolchain image (e.g. `"node:22"`)
   - `CacheMounts()` — `[]CacheMount` for dependency caches; may be empty
   - `WarmupExec()` — argv to pre-fetch dependencies; `nil` skips warmup
   - `TestExec()` — argv for the test suite (e.g. `[]string{"npm", "test"}`)
   - `Persona()` — role string for the system prompt (e.g. `"senior Node.js engineer"`)
2. Add the constant to the `Language` block in `coding_agent_interface.go`.
3. Register it in `agent_registry/agent_registry.go`: one line in `New()`.
4. Add it to the detection priority table in `detect_language_activity.go`.

No workflow or activity changes. No changes to `BuildSystemPrompt`.

### Add a new LLM provider

1. Create `llm_factory/providers/<name>.go`. Implement `Provider`:
   - `Name()` — lowercase string (e.g. `"vertexai"`)
   - `Resolve(modelOverride string, getenv func(string) string) (LLMConfig, error)` — read credentials via `getenv`, validate them (return error if missing), return `LLMConfig{Provider, Model, Env}` where `Env` holds the vars the Dagger engine needs
2. Register it in `llm_factory/llm_factory.go`: one line in `New()`.
3. Add the env vars to `.env.example` and `docker-compose.yml` (worker + dagger-engine services).

No workflow, activity, or agent changes.

### Add a new Temporal activity

1. Add a method to the `Activities` struct in `activities/` (new file or existing):
   ```go
   func (a *Activities) MyActivity(ctx context.Context, in types.MyInput) (types.MyOutput, error)
   ```
2. Add `MyInput` and `MyOutput` to `types/types.go` — short primitives only.
3. In `workflows/agent_workflow.go`, add `workflow.WithActivityOptions(...)` and `workflow.ExecuteActivity(ctx, a.MyActivity, input)` in the correct sequence.
4. If the activity is long-running (`StartToCloseTimeout > 5min`), add heartbeating:
   ```go
   stopHeartbeat := startHeartbeat(ctx, 20*time.Second)
   defer stopHeartbeat()
   ```
5. No registration change needed — `w.RegisterActivity(activities.NewActivities())` registers all methods automatically.

### Add a new MCP server

Add an entry to `newMCPRegistry()` in `activities/mcp.go`:

```go
"myserver": {
    Name:          "myserver",
    Image:         "ghcr.io/org/myserver:latest",
    Secrets:       map[string]string{"MY_API_KEY": os.Getenv("MY_API_KEY")},
    Args:          []string{"stdio"},
    UseEntrypoint: true,
    RequiredKeys:  []string{"MY_API_KEY"},
},
```

Then in the activity, call `a.mcpServers["myserver"].build(client)` and pass it to `agent.WithMCPServer(...)`. If the server is optional (like GitHub), check `spec.validate() == nil` before attaching.

### Add a new agent role (per-activity LLM)

In `activities/llm_roles.go`:
```go
const RoleMyAgent ActivityRole = "myagent"  // add constant
var AllRoles = []ActivityRole{RoleContext, RoleCoding, RoleMyAgent}  // add to slice
```

In the new activity: `a.llms[RoleMyAgent].Model`. The env var convention resolves automatically: `MYAGENT_LLM_PROVIDER` / `MYAGENT_LLM_MODEL`, falling back to `LLM_PROVIDER` / `AGENT_MODEL`.

---

## 7. Secret Custody Rules

- `GITHUB_TOKEN` is read once in `NewActivities()`, stored in `Activities.githubToken` (unexported). It never appears in a log, a return value, a workflow input, or a Temporal event history entry.
- All MCP server credentials are injected via `client.SetSecret(name, value)` + `container.WithSecretVariable(name, secret)`. Dagger scrubs secrets from build-cache layers and engine logs.
- The GitHub token enters containers only via `WithSecretVariable` on the **pusher container** — the separate `alpine/git` container that commits and pushes. The LLM workspace container never has it.
- For any new credential: `os.Getenv` once in `NewActivities()` → store on `Activities` → pass to `client.SetSecret()` at activity time. Never `os.Getenv` inside an activity method.

---

## 8. The Coding Agent Workspace

What the agent gets:
- A container built from the toolchain's `BaseImage()`
- The target repository source mounted at `/src` (no `.git` — this is a plain directory tree, not a git clone)
- Dependency caches pre-mounted
- Warmup command already run
- Context7 MCP server attached for library docs

What the agent must never do (enforced via system prompt):
- Run any git command (`git add`, `git commit`, `git branch`, `git push`, etc.) — the workspace has no `.git` and all git operations will fail
- Set the `completed` output before the full test suite passes
- Delete, weaken, skip, or comment out tests
- Rely on network credentials — the workspace container has none

The agent's first action must always be a recursive directory listing from `/src` to understand the full project structure before reading any specific file or making any edit.

The pusher container (separate from the workspace, credential-isolated) handles all git work after the agent completes.

---

## 9. Per-Activity LLM Configuration

Each agent role resolves its own `LLMConfig` at startup via `buildActivityLLMs` in `activities/llm_roles.go`.

Resolution order for each role:
1. `<ROLE>_LLM_PROVIDER` / `<ROLE>_LLM_MODEL` (role-specific override)
2. `LLM_PROVIDER` / `AGENT_MODEL` (global fallback)

| Role constant | Env prefix | Activity |
|---|---|---|
| `RoleContext` | `CONTEXT_` | `RunContextAgentActivity` |
| `RoleCoding` | `CODING_` | `RunCodingAgentActivity` |

In activity code: `a.llms[RoleContext].Model`, `a.llms[RoleCoding].Model`. Never access `a.LLM` — that field no longer exists.

---

## 10. Python Agent Warmup

`pythonAgent.WarmupExec()` runs before the agent loop. It fails hard on error — a broken environment is caught here, not inside the agent loop where it wastes the entire call budget.

Priority order (first match wins, others skipped):
1. `requirements.txt` present → `pip install -r requirements.txt`
2. `pyproject.toml` present → `pip install -e .`
3. Neither present → exit non-zero; activity fails before agent starts

`pyproject.toml` projects declare dependencies inside the toml file; `pip install -e .` installs both the package and its dependencies. Do not add `|| true` to either command.

---

## 11. Compose Stack

`docker compose up -d` starts all seven services. There are no profiles.

| Service | Image | Purpose |
|---|---|---|
| `postgres` | `postgres:16-alpine` | Temporal persistence backend |
| `temporal` | `temporalio/auto-setup:1.26` | Temporal server; auto-provisions schema on first boot |
| `temporal-ui` | `temporalio/ui:2.31.2` | Web UI at `:8233` |
| `litellm` | `litellm/litellm:v1.83.14-stable` | LiteLLM proxy fronting Bedrock; static IP `172.28.1.10` |
| `dagger-engine` | `registry.dagger.io/engine:v0.21.7` | Persistent Dagger engine with LLM env vars baked in at start |
| `server` | built from `server/Dockerfile.server` | Gin HTTP API at `:8080` |
| `worker` | built from `worker/Dockerfile.worker` | Temporal worker; connects to `dagger-engine` via Docker socket |

**Critical:** the `dagger-engine` image tag must match `dagger.io/dagger` in `go.mod`. A version mismatch causes an immediate connection error. The engine reads its LLM env vars (`OPENAI_BASE_URL`, `ANTHROPIC_API_KEY`, etc.) at container start — they cannot be set after the fact. The engine uses `extra_hosts` to resolve `litellm` by static IP because it rewrites `/etc/resolv.conf` at startup and compose DNS doesn't stick.

---

## 12. MCP Servers

| Logical name | Attached to | Image | Credentials | Optional? |
|---|---|---|---|---|
| `atlassian` | context agent | `ghcr.io/sooperset/mcp-atlassian` | `JIRA_API_TOKEN`, `CONFLUENCE_API_TOKEN` (Dagger secrets) | No — missing config fails the activity |
| `github` | context agent | `ghcr.io/github/github-mcp-server` | `GITHUB_TOKEN` (Dagger secret) | Yes — attached only when `GITHUB_TOKEN` is set |
| `context7` | coding agent | `node:22-alpine` | None | No — always attached |

`atlassian` is read-only (`READ_ONLY_MODE=true`). `github` is read-only (`GITHUB_READ_ONLY=1`, `GITHUB_TOOLSETS=issues,repos,pull_requests`). Never remove these flags.

Every MCP tool call counts against `MaxAPICalls`. The coding agent has `CODING_AGENT_MAX_LOOPS` (default 50); the context agent has `AGENT_CONTEXT_MAX_LOOPS` (default 20).

---

## 13. Configuration Reference

| Variable | Default | Required | Notes |
|---|---|---|---|
| `GITHUB_TOKEN` | — | Yes | Clone/push/PR. Worker-side only; never in Temporal |
| `LLM_PROVIDER` | `anthropic` | No | Global fallback: `anthropic`, `openai`, `bedrock` |
| `AGENT_MODEL` | provider default | No | Global model fallback; superseded by role-specific vars |
| `CONTEXT_LLM_PROVIDER` | — | No | Provider for context agent; falls back to `LLM_PROVIDER` |
| `CONTEXT_LLM_MODEL` | — | No | Model for context agent; falls back to `AGENT_MODEL` |
| `CODING_LLM_PROVIDER` | — | No | Provider for coding agent; falls back to `LLM_PROVIDER` |
| `CODING_LLM_MODEL` | — | No | Model for coding agent; falls back to `AGENT_MODEL` |
| `ANTHROPIC_API_KEY` | — | If `anthropic` | + opt `ANTHROPIC_MODEL`, `ANTHROPIC_BASE_URL` |
| `OPENAI_API_KEY` | — | If `openai` | + opt `OPENAI_MODEL`, `OPENAI_BASE_URL` |
| `LLM_BEDROCK_PROXY_URL` | — | If `bedrock` | LiteLLM proxy URL (e.g. `http://litellm:4000`) |
| `LLM_BEDROCK_MODEL` | — | If `bedrock` | A `model_name` from `litellm_config.yaml` |
| `LLM_BEDROCK_PROXY_KEY` | `not-needed` | No | LiteLLM master key |
| `AGENT_MAX_LOOPS` | `25` | No | Global fallback loop cap; superseded by `CODING_AGENT_MAX_LOOPS` |
| `CODING_AGENT_MAX_LOOPS` | `50` | No | Coding agent tool round-trip cap; falls back to `AGENT_MAX_LOOPS` |
| `AGENT_CONTEXT_MAX_LOOPS` | `20` | No | Context agent tool round-trip cap |
| `AGENT_GIT_IMAGE` | `alpine/git:latest` | No | Image for the credentialed push step |
| `AGENT_SIMULATE_PR` | `true` | No | `true` = skip live GitHub PR API |
| `ATLASSIAN_MCP_IMAGE` | `ghcr.io/sooperset/mcp-atlassian:latest` | No | Atlassian MCP server image |
| `GITHUB_MCP_IMAGE` | `ghcr.io/github/github-mcp-server` | No | GitHub MCP server image |
| `JIRA_URL` | — | Yes | Jira base URL |
| `JIRA_USERNAME` | — | Yes | Jira account email |
| `JIRA_API_TOKEN` | — | Yes | Jira API token → Dagger secret |
| `JIRA_SSL_VERIFY` | `true` | No | |
| `CONFLUENCE_URL` | — | Yes | Confluence base URL |
| `CONFLUENCE_USERNAME` | — | Yes | Confluence account email |
| `CONFLUENCE_API_TOKEN` | — | Yes | Confluence API token → Dagger secret |
| `CONFLUENCE_SSL_VERIFY` | `true` | No | |
| `TEMPORAL_HOSTPORT` | `localhost:7233` | No | Temporal gRPC address (host dev); compose sets `temporal:7233` directly |
| `HTTP_ADDR` | `:8080` | No | Gin server bind address |
| `API_AUTH_REQUIRED` | `false` | No | When `true`, `/v1` routes require `Authorization` header |
| `_EXPERIMENTAL_DAGGER_RUNNER_HOST` | — | Yes (host dev) | Set to `docker-container://tagger-dagger-engine` when running worker on host |

Per-language workspace images (`golang:1.23`, `python:3.12`, …) are not env vars — they live in `agent_registry/agents/` and are selected at runtime.

---

## 14. What You Must Never Do

- Import `dagger.io/dagger`, `net`, `net/http`, `os/exec`, or `os` (beyond `os.Getenv` in `NewActivities`) from `workflows/`
- Put a secret, token, password, or any credential into a `types/` struct or a Temporal payload
- Call `os.Getenv` inside an activity method — read config once in `NewActivities()` and store it on `Activities`
- Add `WithSecretVariable` to the LLM workspace container
- Run git commands inside the agent workspace
- Remove or skip the independent post-agent verification gate
- Delete, comment out, or weaken tests in a target repository to make them pass
- Increase `MaximumAttempts` on `RunCodingAgentActivity` above 3 without explicit human sign-off
- Silence dependency install errors with `|| true` in `WarmupExec` — fail hard so broken envs surface before the agent loop starts
- Add a `bedrock` (or any) profile to `docker-compose.yml` — all services are always-on; there are no profiles
