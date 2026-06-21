# ROBOTS.md — Development Context, Rules, and Policy for the Issue-Driven AI Coding Agent

This file is the authoritative reference for any agent, developer, or automated system working on or inside this codebase. It describes what the system does, how it is structured, what the rules are, and what is absolutely forbidden. Read it fully before making any change.

---

## 1. What This System Does

This system takes a Jira ticket and a repository URL and produces a pull request. The pipeline is automated, with one deliberate human checkpoint:

1. **Gather context** — an LLM with read-only Atlassian (Jira + Confluence) tools crawls from the seed Jira ticket out to its linked Confluence design and any further linked issues, and distills an implementation brief. If context is missing, the workflow **halts and waits for a human** to supply it (see §11).
2. **Resolve the base commit** — pin the base branch to an immutable commit SHA (runs in parallel with step 1).
3. **Select the toolchain** — choose the coding-agent language: an explicit override wins, else detect it from the repo's root marker files (see §13b).
4. **Write the code** — a second LLM, inside a containerized sandbox with the repo mounted and Context7 docs tools, writes code, runs tests, fixes failures, and iterates until the suite passes. The sandbox (image, test command, persona) is the one selected in step 3.
5. **Publish** — push the finished branch and open a pull request.

There are **two LLM agents** (context-gathering and coding), each in its own Temporal activity, each driven by Dagger, each with its own MCP tool surface. The only human touchpoint is the optional context-supplement signal in step 1.

---

## 2. Repository Layout

```
tagger/
├── types/                          ← shared data types; zero dependencies
│   └── types.go
├── workflows/                      ← Temporal workflow (deterministic only)
│   └── agent_workflow.go           ← CodingAgentWorkflow + gather/signal-halt loop
├── activities/                     ← all side effects; the only I/O layer
│   ├── git_activities.go           ← ResolveBaseCommitActivity + CreatePullRequestActivity + atlassianConfig
│   ├── context_agent_activity.go   ← RunContextAgentActivity (Atlassian MCP crawl)
│   ├── detect_language_activity.go ← DetectLanguageActivity (root-marker probe)
│   └── coding_agent_activity.go    ← RunCodingAgentActivity (language-agnostic coding loop + Context7 MCP)
├── agent_registry/                 ← startup-time catalog of per-language coding agents
│   ├── agent_registry.go           ← Registry: New() + GetAgentByLanguage()
│   └── agents/                     ← dependency-light toolchain catalog (no Dagger/Temporal imports)
│       ├── coding_agent_interface.go ← CodingAgent interface, Language consts, prompt template
│       ├── go_agent.go             ← goAgent (golang:1.23, go test ./...)
│       └── python_agent.go         ← pythonAgent (python:3.12, pytest)
├── llm_factory/                    ← startup-time catalog of LLM providers
│   ├── llm_factory.go              ← Factory: New() + CreateLLM() → LLMConfig
│   └── providers/                  ← dependency-light provider catalog (no Dagger/Temporal imports)
│       ├── provider_interface.go   ← Provider interface, LLMConfig, provider consts
│       ├── anthropic.go            ← anthropicProvider (ANTHROPIC_API_KEY)
│       ├── openai.go               ← openAIProvider (OPENAI_API_KEY, +BASE_URL for gateways)
│       └── bedrock.go              ← bedrockProvider (Bedrock via LiteLLM, OpenAI-compatible)
├── worker/
│   └── main.go                     ← boots the Temporal worker process
├── starter/
│   └── main.go                     ← submits one workflow execution, blocks on result (CLI)
├── server/
│   ├── main.go                     ← Gin HTTP API: start / status / signal a run (+ Swagger annotations)
│   ├── docs/                       ← generated OpenAPI spec + Swagger UI (swag init); committed
│   └── Dockerfile.server           ← multi-stage build → scratch image for the server
├── dagger/toolbox/                 ← optional Dagger module; named LLM tools (built by `dagger develop`)
│   ├── dagger.json
│   └── main.go
├── docker-compose.yml              ← stack: postgres + temporal + temporal-ui + server (+ litellm under `bedrock` profile)
├── litellm_config.yaml             ← LiteLLM model_list (Bedrock → OpenAI-compatible)
├── .dockerignore                   ← keeps .env/.git/artifacts out of the build context
├── .env.example                    ← copy to .env; documents every variable
├── .gitignore                      ← .env is ignored
├── go.mod
├── README.md
└── robots.md                       ← this file
```

**Dependency direction is strictly one-way and must stay that way:**

```
worker  → workflows, activities
starter → workflows, types
server  → workflows, types
workflows → activities (name resolution only), types
activities → types, agent_registry, llm_factory, dagger.io/dagger, go.temporal.io/sdk
agent_registry → agent_registry/agents
agent_registry/agents → nothing (no Dagger, no Temporal — pure toolchain data)
llm_factory → llm_factory/providers
llm_factory/providers → nothing (no Dagger, no Temporal — pure config: strings + maps)
types → nothing
```

If you find yourself importing `dagger.io/dagger` from `workflows/`, stop immediately. That is a violation. The same applies to `agent_registry/agents` and `llm_factory/providers`: they must stay pure, dependency-light catalogs (plain strings, argv slices, env maps). The `activities` layer translates that data into Dagger objects — the import arrow never points back.

> **Module path:** `github.com/syncopatedNote/tagger`. **Dagger:** `v0.21.7` (MCP requires ≥ v0.19.0). **Temporal:** `v1.31.0`. **Go:** 1.26.

---

## 3. The Two Absolute Rules

These are non-negotiable architectural invariants. Violating either causes silent correctness failures that are very hard to debug.

### Rule 1: Workflow code must be deterministic

Temporal replays workflow code from its event history during recovery. If the workflow produces a different outcome on replay than it did on the original run, Temporal raises a non-determinism error and the workflow is permanently stuck.

**The `workflows/` package must never:**
- Import `dagger.io/dagger` or any Dagger type
- Import `net`, `net/http`, `os/exec`, `os` (except via `workflow.GetLogger`)
- Call `time.Now()` — use `workflow.Now(ctx)` instead
- Use `math/rand` or `crypto/rand`
- Read environment variables directly
- Spawn goroutines (except via `workflow.Go`)
- Write to the filesystem

**The `workflows/` package may:**
- Call `workflow.ExecuteActivity(...)` and `workflow.ExecuteChildWorkflow(...)`
- Call `workflow.WithActivityOptions(ctx, ...)`, `workflow.GetLogger(ctx)`
- Call pure helper functions (string manipulation, struct construction)
- Call `workflow.Now(ctx)` for timestamps
- Import and reference types from `types/` and method signatures from `activities/`

### Rule 2: All side effects live in activities

`activities/` is the only layer allowed to:
- Open network connections
- Spawn subprocesses (`os/exec`)
- Read environment variables for secrets
- Call `time.Now()` or use randomness
- Connect to the Dagger engine
- Push to Git remotes

If you need to do something side-effecting in the workflow, extract it into a new activity method on `Activities` and call it via `workflow.ExecuteActivity`.

---

## 4. The Claim-Check Pattern

**Source code, file contents, and directories must never appear in Temporal payloads.**

Temporal persists every activity input and output to an event history stored in its database. Putting large blobs there causes:
- Unbounded event history growth
- Slow replay during recovery
- Expensive storage costs
- Payload size limit errors

Instead, use Git as the data broker:

```
Activity returns a SHA  →  "abc123f7..."         (40 chars)
Activity returns a branch  →  "agent/proj-123-a4b2"  (20 chars)
Activity returns a URL  →  "https://github.com/.../pull/42"
```

The actual working tree lives in the Git remote. Activities read from and write to Git; Temporal sees only the pointers.

**Enforcement rule:** Every field in `types/types.go` must be a short primitive — `string`, `int`, `bool`. If you are adding a `[]byte`, a file path containing file contents, or a struct with more than ~10 fields, you are probably violating claim-check. Use Git instead.

---

## 5. Data Types Reference (`types/types.go`)

All types are dependency-free. They serialize to JSON via Temporal's default codec.

| Type | Direction | Key fields |
|---|---|---|
| `CodingAgentInput` | Workflow input | `IssueReference`, `RepoURL`, `BaseBranch`, `Language` (optional override) |
| `GatherContextInput` | Context agent input | `IssueReference`, `Supplements []string` |
| `GatherContextResult` | Context agent output | `Requirements`, `Title`, `Complete bool`, `Missing []string` |
| `ContextSupplement` | Signal payload | `Info` (the human-supplied missing context) |
| `ResolveBaseCommitInput` | Activity input | `RepoURL`, `BaseBranch` |
| `ResolveBaseCommitResult` | Activity output | `BaseCommitSHA` |
| `DetectLanguageInput` | Detection input | `RepoURL`, `BaseCommitSHA` |
| `DetectLanguageResult` | Detection output | `Language` (e.g. `"go"`, `"python"`) |
| `RunCodingAgentInput` | Coding agent input | `RepoURL`, `BaseCommitSHA`, `Requirements`, `IssueReference`, `Language` |
| `RunCodingAgentResult` | Coding agent output | `BranchName`, `HeadCommitSHA` |
| `CreatePullRequestInput` | PR activity input | `RepoURL`, `BaseBranch`, `FeatureBranch`, `Title`, `Body` |
| `CreatePullRequestResult` | PR activity output | `PullRequestURL`, `Number` |
| `CodingAgentResult` | Workflow output | `PullRequestURL`, `BranchName`, `HeadCommitSHA` |

`Supplements` and `Missing` are `[]string` of *short* strings (URLs, IDs, one-line gaps) — they remain claim-check-safe. When adding a new field, ask: "Is this a pointer to data, or the data itself?" Only pointers belong here.

---

## 6. Workflow Activity Options Policy

Each activity has distinct timeout and retry settings. These are not arbitrary — they reflect the cost and recoverability of each operation.

### Infra activities — `infraRetry`

Used for `ResolveBaseCommitActivity`, `DetectLanguageActivity`, and `CreatePullRequestActivity`. These are fast (seconds), idempotent, and fail mostly for transient network reasons.

```go
StartToCloseTimeout: 2 * time.Minute
RetryPolicy:
  InitialInterval:    2s
  BackoffCoefficient: 2.0
  MaximumInterval:    1min
  MaximumAttempts:    5
  NonRetryableErrorTypes: ["ValidationError"]
```

### Context agent — `RunContextAgentActivity`

An LLM crawl over the Atlassian APIs. Long-ish, but cheaper than the coding loop. It never retries an exhausted or validation failure — those flow up to the workflow's gather/signal loop, which decides whether to halt for a human.

```go
StartToCloseTimeout: 15 * time.Minute
HeartbeatTimeout:    2 * time.Minute
RetryPolicy:
  InitialInterval:    5s
  BackoffCoefficient: 2.0
  MaximumInterval:    1min
  MaximumAttempts:    3
  NonRetryableErrorTypes: ["ValidationError", "AgentExhaustedError"]
```

> Note: `Complete=false` is **not an error** — the activity returns successfully with a verdict. The *workflow* halts for a signal (§11). The retry policy here only governs genuine failures (engine crash, malformed output).

### Coding agent — `RunCodingAgentActivity`

Long-running (up to 45 minutes) and expensive. Retry policy is deliberately tight.

```go
StartToCloseTimeout: 45 * time.Minute   // wall-clock cap; ctx cancels Dagger on timeout
HeartbeatTimeout:    2 * time.Minute    // detects a dead/stuck worker fast
RetryPolicy:
  InitialInterval:    10s
  BackoffCoefficient: 2.0
  MaximumInterval:    2min
  MaximumAttempts:    2                 // one retry max; LLM loops are expensive
  NonRetryableErrorTypes: ["ValidationError", "AgentExhaustedError"]
```

`AgentExhaustedError` means the LLM used all its loop budget without passing tests. A blind retry will almost certainly do the same thing. Surface it for human triage instead.

**Rule:** Never increase `MaximumAttempts` on the coding agent above 3 without explicit human sign-off. Each attempt can consume up to 45 minutes of compute and 25 LLM API calls.

---

## 7. Error Classification Policy

Errors in this system fall into two classes:

### Retryable (plain `error` or `fmt.Errorf`)

- Network timeouts and transient HTTP failures
- Dagger engine connection failures
- `git push` auth failures (token might be stale, retryable on next attempt)
- GitHub API 5xx responses

Return these as plain Go errors. Temporal will retry according to the activity's `RetryPolicy`.

### Non-Retryable (`temporal.NewNonRetryableApplicationError`)

These must be marked with a type string that matches the workflow's `NonRetryableErrorTypes` list.

| Scenario | Type string | Why non-retryable |
|---|---|---|
| Empty `IssueReference` or `RepoURL` | `"ValidationError"` | Input is wrong; retrying won't fix it |
| Branch not found in `git ls-remote` | `"ValidationError"` | Branch doesn't exist; retrying won't create it |
| Invalid `RepoURL` format | `"ValidationError"` | Parse failure; retrying won't fix it |
| GitHub returned HTTP 422 | `"ValidationError"` | PR already exists or nothing to compare |
| Missing `GITHUB_TOKEN` on worker | `"ValidationError"` | Config error; retrying won't provide the token |
| LLM loop hit `MaxAPICalls` cap | `"AgentExhaustedError"` | Model ran out of budget; retry wastes money |
| Post-agent `go test ./...` still fails | `"AgentExhaustedError"` | Code doesn't work; same result on retry |

**Rule:** When writing a new activity, classify every error path explicitly. "I'm not sure" is not an acceptable answer — default to non-retryable and document why.

---

## 8. Secret Custody Policy

The GitHub token is the most sensitive value in the system. These rules apply to it and to any future credential (SSH key, API key, database password).

**Where it lives:**
- `os.Getenv("GITHUB_TOKEN")` is called exactly once, in `NewActivities()`, at worker startup
- It is stored in the unexported `Activities.githubToken` field (lowercase = package-private in Go)
- It never appears in a log line, a return value, a workflow input, or a Temporal event history entry

**How it enters containers:**
- `client.SetSecret("github-token", token)` registers it as a Dagger secret
- `container.WithSecretVariable("GITHUB_TOKEN", secret)` injects it into the container at runtime
- Dagger scrubs secrets from build-cache layers and from its operation logs automatically
- The secret is injected only into the **pusher container** — the separate `alpine/git` container that does the commit and push

**What the LLM workspace container never has:**
- Any secret variable
- The `.git` directory (which may contain cached credentials)
- Any network credential of any kind

**Rule:** The agent workspace container and the pusher container must always be separate `*dagger.Container` instances. Never call `WithSecretVariable` on a container that has been or will be bound to a Dagger `Env` as an LLM tool. Credentials and LLM workspace are mutually exclusive.

**For SSH keys:** use `client.SetSecret` + `container.WithMountedSecret("/root/.ssh/id_ed25519", secret)` on the pusher container only. Set `GIT_SSH_COMMAND=ssh -i /root/.ssh/id_ed25519 -o IdentitiesOnly=yes` to prevent fallback to agent-forwarded keys.

---

## 9. The LLM Loop — How Self-Healing Works

The agent in `RunCodingAgentActivity` runs an iterative loop inside the Dagger engine:

```
1. Model receives: requirements (string), issue ref (string), workspace (container)
2. Model calls tool: read_file("/src/main.go") → returns file contents
3. Model calls tool: write_file("/src/main.go", <new contents>) → returns updated container
4. Model calls tool: exec("go test ./...") → returns stdout+stderr regardless of exit code
5. If tests fail → model reads the output, patches the code, runs tests again
6. If tests pass → model sets the `completed` output and stops
```

The self-healing mechanism depends on one critical detail: `go test ./...` failures must be returned to the model as tool results, not as pipeline errors. This is achieved via `Expect: dagger.ReturnTypeAny` in the toolbox module and via the inline container binding in the main activity (Dagger returns output for any exit code when the container is bound as an LLM tool).

**What the model is told it cannot do (system prompt rules):**
1. Explore before editing — read relevant files first
2. Make the smallest change that satisfies requirements
3. Run `go test ./...` after every change
4. Fix implementation failures; never delete or weaken tests
5. Set `completed` only when `go test ./...` passes cleanly
6. No network credentials — push is handled by a separate stage

**Verification gate (mandatory):** After the model declares completion, the activity independently re-runs `go test ./...` on the completed container. The model's self-report is never trusted. If this independent gate fails, `AgentExhaustedError` is returned.

---

## 10. Dagger Caching Semantics

Understanding the cache model is essential for debugging slow runs and unexpected re-execution.

**Content-addressed cache:** Every Dagger operation is a DAG node keyed by a hash of its inputs. Two workers issuing the same `go mod download` over the same module graph hit the same cache key. The cache lives in the Dagger Engine daemon, not in the worker process.

**Named cache volumes:** `client.CacheVolume("go-mod")` and `client.CacheVolume("go-build")` are named volumes in the engine. They persist across sessions and are shared by all workers connected to the same engine. This is how Go module and build caches are preserved between runs.

**Cache busting for the pusher:** The pusher container includes `WithEnvVariable("CACHEBUST", time.Now().UTC().Format(time.RFC3339Nano))`. This injects a unique value that changes on every run, preventing Dagger from reusing a cached clone from a previous attempt. Without this, Temporal retries could push a stale working tree.

**Rule:** Never add `CACHEBUST` to the agent workspace container — that would defeat the module/build cache and make every run cold. Only the pusher container, which must always do a fresh clone, gets the cache bust.

**Distributed caching:** If workers run on multiple machines, they share cache only if they point at a shared remote Dagger Engine or if the engine is configured with remote cache export/import (registry-backed `cache-from`/`cache-to`). A local engine per machine means per-host caches with no sharing.

---

## 11. Heartbeat Policy

The coding agent (`RunCodingAgentActivity`) runs for up to 45 minutes, and the context agent (`RunContextAgentActivity`) for up to 15 — both heartbeat. Without heartbeats, Temporal cannot distinguish between "the worker is busy doing valid work" and "the worker is dead." The `HeartbeatTimeout: 2min` setting means Temporal expects a heartbeat at least every 2 minutes.

The heartbeat is implemented as a background goroutine that fires every 20 seconds:

```go
stopHeartbeat := startHeartbeat(ctx, 20*time.Second)
defer stopHeartbeat()
```

**Rule:** Any new long-running activity (one with `StartToCloseTimeout > 5min`) must implement heartbeating. The pattern is: start a ticker goroutine calling `activity.RecordHeartbeat(ctx, ...)` on interval, return a stop function, and defer the stop function at the top of the activity.

**Heartbeat data:** `activity.RecordHeartbeat(ctx, "dagger agent running")` accepts an optional details payload. For more granular observability, pass a struct describing current progress (e.g., loop iteration count, last tool called). This data is visible in the Temporal UI and accessible on retry via `activity.GetHeartbeatDetails`.

---

## 12. The Nil Receiver Pattern

The workflow imports `activities` to resolve method names:

```go
var a *activities.Activities
workflow.ExecuteActivity(ctx, a.RunContextAgentActivity, input)
```

`a` is `nil`. This is intentional. `a.RunContextAgentActivity` is a method value — in Go, a method value on a nil pointer is valid as long as the method is never actually called through it (and it isn't — Temporal's SDK extracts the function name and dispatches to the real registered instance on the worker).

**Why this matters:** It keeps the workflow package free of concrete dependencies. If the workflow imported and instantiated a real `*Activities`, it would pull in the Dagger SDK, breaking the determinism boundary. The nil receiver gives type safety and Temporal name registration without runtime coupling.

**Rule:** Do not change this to a non-nil instance, a function pointer, or a string name. The nil receiver pattern is the correct way to reference activities from a Temporal workflow in Go.

---

## 13. Atlassian Context Gathering & the Completeness Policy

Context is gathered by an **LLM crawl**, not by deterministic Go code. `RunContextAgentActivity` (in `context_agent_activity.go`) stands up the `ghcr.io/sooperset/mcp-atlassian` server as a Dagger service, binds it to the LLM via `WithMCPServer("atlassian", ...)`, and prompts the model to crawl: Jira ticket (body + comments) → linked Confluence pages (body + comments) → any further Jira/Git issues referenced along the way. The model returns a single JSON verdict.

**The mcp-atlassian server runs in `READ_ONLY_MODE=true`.** Only get/search tools are exposed (`jira_get_issue`, `jira_search`, `confluence_get_page`, `confluence_search`). The agent can never create, update, transition, or comment. **Rule:** do not remove `READ_ONLY_MODE`; the gathering agent must never mutate Atlassian data.

### Completeness policy (what triggers a halt)

The agent reports `Complete: false` and the workflow halts for a human signal (§16, Human-in-the-Loop) when **any** of these hold:

1. **Empty/placeholder Jira body** — the ticket has no real description (just a title, "TBD", "see design").
2. **No Confluence design found** — the crawl turned up zero Confluence pages.
3. **A broken/inaccessible link** — a referenced Confluence page or issue returned 404 / permission-denied.

A design that merely lacks an explicit "acceptance criteria" heading is **still complete** (recorded as a note, not a halt). This policy lives in the agent's system prompt in `context_agent_activity.go` — keep it and this section in sync.

### The output contract

The agent must return **only** a JSON object: `{requirements, title, complete, missing[]}`. The activity parses this (tolerant of stray prose / ```json fences). Malformed output is a retryable error; a `complete:true` with empty `requirements` is rejected. When `complete:false`, `missing[]` names exactly what a human must supply.

### Swapping providers

To target a different tracker, change the MCP image (`ATLASSIAN_MCP_IMAGE`) and adjust the system prompt's tool names. The Go code is provider-agnostic — it only spawns a service and reads a JSON verdict.

---

## 13b. Multi-Language Coding Agents (the `CodingAgent` interface)

`RunCodingAgentActivity` is **one** language-agnostic activity. Everything that varies per language — base image, dependency caches, warm-up command, test command, and the system-prompt persona — is supplied by a `CodingAgent` implementation. The shared loop (Dagger connect → mount → env-bind → self-heal → verify → publish) never changes.

### The interface (`agent_registry/agents/coding_agent_interface.go`)

```go
type CodingAgent interface {
    Language() Language        // "go", "python", ...
    BaseImage() string         // "golang:1.23", "python:3.12"
    CacheMounts() []CacheMount // {Path, Volume} pairs (plain data, no Dagger types)
    WarmupExec() []string      // argv; nil = skip
    TestExec() []string        // the verification-gate suite
    Persona() string           // "senior Go engineer"
}
```

`BuildSystemPrompt(agent)` interpolates `Persona()` and the test command into the otherwise-shared prompt — rules 1–7 are identical across languages.

**The `agents` package is a pure catalog: it imports neither Dagger nor Temporal.** It returns plain strings and argv slices; the activity translates `CacheMounts()` into `client.CacheVolume(...)`, etc. This keeps the import arrow one-way (`activities → agent_registry → agents`) and the agents trivially unit-testable.

### The registry (`agent_registry/agent_registry.go`)

A startup-time catalog built **once** by `New()` and queried per-request by `GetAgentByLanguage(lang)`:

- `New()` is called once in `NewActivities()`; the `*Registry` lives on the `Activities` struct for the worker's lifetime. It is **not** rebuilt per activity.
- `GetAgentByLanguage` is a pure map lookup; an unknown language returns an error naming the supported set, which the activity surfaces as a non-retryable `ValidationError`.

**Currently registered:** `go`, `python`. **Recognised by detection but not yet built:** `javascript`, `java` — these resolve to a clean "unsupported language" failure rather than mis-building. Adding one is a single registry line plus its `*_agent.go` file; no workflow or activity change.

### Language selection (override → detect → fail)

Two distinct steps in two distinct places — this split is load-bearing:

1. **The workflow decides the language *string*** (`selectLanguage`): an explicit `input.Language` override wins with no activity call; otherwise `DetectLanguageActivity` runs. A failure to determine the language is fatal — we never guess.
2. **The activity resolves the string → a `CodingAgent` instance** via the registry. This is the only place a `CodingAgent` exists; the workflow stays dependency-light and only routes on a primitive.

### `DetectLanguageActivity`

A Temporal activity (it does I/O — reaches the Git remote) but **functionally deterministic**: same tree in, same language out. It lists the repository root at the **pinned `BaseCommitSHA`** (the exact tree the coding agent will build — no race with a moving branch) and probes marker files in a **fixed priority order**:

| Priority | Marker | Language |
|---|---|---|
| 1 | `go.mod` | go |
| 2 | `pyproject.toml`, `requirements.txt`, `setup.py` | python |
| 3 | `package.json` | javascript |
| 4 | `pom.xml`, `build.gradle`, `build.gradle.kts` | java |

First match wins, so a Go repo with a tooling `package.json` resolves to `go`. No marker → non-retryable `ValidationError` ("supply the language explicitly"). Per-subdirectory languages in a monorepo are out of scope for now (documented seam).

---

## 13c. LLM Providers (the `llm_factory`)

Both agents run their loop **inside Dagger** via `client.LLM(dagger.LLMOpts{Model, MaxAPICalls})`. Dagger's `LLMOpts` exposes **only** `Model` and `MaxAPICalls` — there is no `Provider`, `APIKey`, or `BaseURL` field. Dagger selects the backend by reading **engine environment variables** (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `OPENAI_BASE_URL`, …) and inferring the provider from the model string.

So — unlike a Python `LLMFactory` that returns a client object you call yourself — the `llm_factory` here returns **config**, not a client. There is nothing in this codebase that would call a returned client; the call happens inside Dagger.

### `LLMConfig` — what the factory produces

```go
type LLMConfig struct {
    Provider string            // resolved provider name (logging/diagnostics)
    Model    string            // → dagger.LLMOpts.Model
    Env      map[string]string // the provider env contract the Dagger engine must see
}
```

`Model` feeds `dagger.LLMOpts.Model`; `Env` is exported onto the **worker process** in `NewActivities()` so the (embedded/local) Dagger engine the activities connect to resolves the right backend.

### The provider interface (`llm_factory/providers/provider_interface.go`)

Each provider reads the env vars it owns (via an **injected** `getenv` for testability), validates the required credentials, and emits an `LLMConfig`. **The `providers` package imports neither Dagger nor Temporal** — pure config (strings + maps), exactly like `agent_registry/agents`. A missing credential is an error, never a guessed empty value.

### The factory (`llm_factory/llm_factory.go`)

Built **once** by `New()` (in `NewActivities()`), queried by `CreateLLM(provider, modelOverride)`:

- `provider` empty → the factory's default (`LLM_PROVIDER`, else `anthropic`). Case-insensitive.
- `modelOverride` (from `AGENT_MODEL`) wins over the provider's model env var.
- An unknown provider, or a provider missing its credentials, is **fatal at startup** (`log.Fatal` in `NewActivities`) — a worker that can't reach an LLM can do no useful work, and failing here beats failing inside the first activity attempt.

### Supported providers

| `LLM_PROVIDER` | Required env | Maps to (Dagger sees) |
|---|---|---|
| `anthropic` (default) | `ANTHROPIC_API_KEY` (+ opt `ANTHROPIC_MODEL`, `ANTHROPIC_BASE_URL`) | `ANTHROPIC_*` |
| `openai` | `OPENAI_API_KEY` (+ opt `OPENAI_MODEL`, `OPENAI_BASE_URL`) | `OPENAI_*` — `OPENAI_BASE_URL` also serves OpenRouter / LiteLLM / local |
| `bedrock` | `LLM_BEDROCK_PROXY_URL`, `LLM_BEDROCK_MODEL` (+ opt `LLM_BEDROCK_PROXY_KEY`) | `OPENAI_BASE_URL`/`OPENAI_API_KEY`/`OPENAI_MODEL` |

**Bedrock is the OpenAI provider pointed at a LiteLLM proxy.** Dagger has no native Bedrock transport; the [documented path](https://docs.dagger.io/reference/configuration/llm/#amazon-bedrock-via-litellm-proxy) is a LiteLLM proxy whose `model_list` maps a friendly `model_name` → `bedrock/<model id>` with `drop_params: true` (Bedrock rejects OpenAI's `seed`/`parallel_tool_calls`). The factory translates `LLM_BEDROCK_*` into the OpenAI-compatible env vars and requires the proxy URL (a Bedrock setup with no proxy is misconfigured, not "real OpenAI").

The proxy is **bundled**: `docker-compose.yml` runs `ghcr.io/berriai/litellm` with `litellm_config.yaml` mounted as its `model_list`, and AWS creds (`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` / `AWS_REGION`) passed to the proxy container — never to this app. `docker compose up -d litellm` → `LLM_BEDROCK_PROXY_URL=http://localhost:4000`. Add Bedrock models by editing `model_list` in `litellm_config.yaml`; each `model_name` is a valid `LLM_BEDROCK_MODEL`.

Adding a provider is one file in `llm_factory/providers/` plus one line in `New()`. Secrets in `LLMConfig.Env` are masked by `providers.RedactedEnv` for safe logging.

---

## 14. Toolbox Module vs. Inline Container Binding

There are two ways tools surface to the LLM in this system:

### Inline Container Binding (used by `RunCodingAgentActivity`)

The workspace `*dagger.Container` is bound to the `Env` directly:
```go
env := client.Env().
    WithContainerInput("workspace", workspace, "description")
```
Dagger exposes the Container's generic API — read file, write file, exec command — as LLM tools. No codegen required. Works with a plain SDK import. Tool names match Dagger's Container method names.

### Named Tools via Dagger Module (`dagger/toolbox/`)

A typed Go struct is bound instead:
```go
env := dag.Env().
    WithToolboxInput("workspace", t, "description").
    WithToolboxOutput("completed", "description")
```
Dagger derives a JSON schema from each public method of `Toolbox`. Tools are named `read_file`, `write_file`, `run_tests` — domain-specific and legible. Built with `dagger develop`. Requires the Dagger CLI.

**When to use which:** The inline approach is simpler and has no build step. The named-tools approach gives the LLM a tighter, more constrained surface and is easier to audit (you can read exactly what operations the model is allowed to perform). For production use, prefer the toolbox approach.

---

## 15. Configuration Reference

Configuration is via environment variables, supplied through a `.env` file (loaded by `godotenv.Load()` in `worker`, `starter`, and `server`) or real env vars (which always win). Copy `.env.example` to `.env` to start. CLI flags: `starter`'s `-issue`, `-repo`, `-base`, and `-lang` (optional language override).

| Variable | Default | Required | Description |
|---|---|---|---|
| `GITHUB_TOKEN` | — | Yes (for real runs) | Clone/push/PR credential. Worker-side only; never in Temporal |
| `LLM_PROVIDER` | `anthropic` | No | LLM backend: `anthropic`, `openai`, or `bedrock`. Resolved by the `llm_factory` (see §13c) |
| `AGENT_MODEL` | provider default | No | Per-run model override passed to Dagger (both agents); empty → the provider's model env var / default |
| `ANTHROPIC_API_KEY` | — | If `anthropic` | Anthropic key (+ opt `ANTHROPIC_MODEL`, `ANTHROPIC_BASE_URL`). Visible to the Dagger engine |
| `OPENAI_API_KEY` | — | If `openai` | OpenAI key (+ opt `OPENAI_MODEL`, `OPENAI_BASE_URL` for OpenRouter / LiteLLM / local) |
| `LLM_BEDROCK_PROXY_URL` | — | If `bedrock` | LiteLLM proxy base URL fronting Bedrock as an OpenAI-compatible API |
| `LLM_BEDROCK_MODEL` | — | If `bedrock` | LiteLLM `model_name` (matches the proxy's `config.yml`) |
| `LLM_BEDROCK_PROXY_KEY` | `not-needed` | No | LiteLLM master key, if the proxy is authenticated |
| `AGENT_MAX_LOOPS` | `25` | No | Coding-agent tool round-trip cap (Dagger-level) |
| `AGENT_CONTEXT_MAX_LOOPS` | `20` | No | Context-agent tool round-trip cap (Dagger-level) |
| `AGENT_GIT_IMAGE` | `alpine/git:latest` | No | Image for the credentialed push step |
| `AGENT_SIMULATE_PR` | `true` | No | `true` = skip live GitHub API; returns synthetic URL |

> Per-language workspace images (`golang:1.23`, `python:3.12`, …) are **not** env vars — they live in the agent registry (`agent_registry/agents/`) and are chosen by the detected/overridden language at runtime. See §13b.
| `ATLASSIAN_MCP_IMAGE` | `ghcr.io/sooperset/mcp-atlassian:latest` | No | Read-only Atlassian MCP server image |
| `JIRA_URL` | — | Yes | Jira base URL |
| `JIRA_USERNAME` | — | Yes | Jira account email |
| `JIRA_API_TOKEN` | — | Yes | Jira API token → Dagger secret |
| `JIRA_SSL_VERIFY` | `true` | No | TLS verification for Jira |
| `CONFLUENCE_URL` | — | Yes | Confluence base URL (`.../wiki`) |
| `CONFLUENCE_USERNAME` | — | Yes | Confluence account email |
| `CONFLUENCE_API_TOKEN` | — | Yes | Confluence API token → Dagger secret |
| `CONFLUENCE_SSL_VERIFY` | `true` | No | TLS verification for Confluence |
| `TEMPORAL_HOSTPORT` | `localhost:7233` | No | Temporal frontend gRPC address |
| `HTTP_ADDR` | `:8080` | No | Bind address for the Gin server |
| `API_AUTH_REQUIRED` | `false` | No | When `true`, the auth middleware rejects requests without `Authorization` |

**Rule:** Adding a new configuration value means adding it to `NewActivities()` (loaded once, stored on the struct), not reading `os.Getenv` inline inside an activity method. Inline `os.Getenv` in activity code is hard to test and inconsistent with how the rest of config is handled. The HTTP server reads its own `HTTP_ADDR`/`TEMPORAL_HOSTPORT` directly since it has no `Activities` struct.

---

## 16. Human-in-the-Loop: the Signal/Halt Pattern

The single most important control-flow rule in this system:

> **An activity cannot wait for a human. Only a workflow can.**

Activities are "do one thing and return." They cannot receive signals, pause, or block for human input — doing so holds a worker slot hostage and fights the heartbeat/timeout machinery. Workflows are the durable, suspendable layer: a workflow can block on a signal channel for **days** at zero compute cost, because Temporal persists the state and rehydrates on signal.

This is why context completeness is **computed in the activity** (`RunContextAgentActivity` returns `Complete`/`Missing`) but **acted on in the workflow** (`gatherContextWithSignals`):

```go
for {
    result = ExecuteActivity(RunContextAgentActivity, {issue, supplements})
    if result.Complete { break }
    // halt — costs nothing while waiting
    signalCh.Receive(ctx, &supplement)          // SupplyContextSignal = "supply-context"
    supplements = append(supplements, supplement.Info)
}
```

**The signal contract:**
- Signal name: `SupplyContextSignal` = `"supply-context"` (exported from `workflows`).
- Payload: `types.ContextSupplement{ Info string }` — short text only (a URL, an ID, a sentence).
- Senders: the Temporal Web UI's "Send Signal" button, or `POST /v1/runs/:id/signal` (§18).
- Each supplement accumulates; the next gather attempt sees all prior supplements.

**Rule:** Never try to make an activity block for human input (no polling loops, no `time.Sleep` waiting for a DB flag). If a new step needs human approval, model it as a workflow signal exactly like this one.

---

## 17. MCP Server Integration

Both LLM agents get external tools via Dagger's MCP support (`LLM.WithMCPServer(name, *Service)`, **available in Dagger ≥ v0.19.0** — this is why the project is pinned to v0.21.7). The pattern is identical in both activities:

1. Build a container from the MCP server's image.
2. Expose its port and run it over an HTTP transport.
3. Turn it into a service with `Container.AsService()`.
4. Bind it: `client.LLM(...).WithMCPServer("<name>", service)`.

| Agent | MCP server | Image | Mode |
|---|---|---|---|
| `RunContextAgentActivity` | Atlassian (Jira + Confluence) | `ghcr.io/sooperset/mcp-atlassian` | **read-only** |
| `RunCodingAgentActivity` | Context7 (library docs) | `node:22-alpine` + `@upstash/context7-mcp` | n/a (public docs) |

**Why these and only these:** The coding agent already has the repo mounted in its workspace, so a GitHub/code-reading MCP would be redundant and would waste `MaxAPICalls` budget. Context7 adds current library docs the clone can't provide. The context agent needs Atlassian to reach the tracker.

**Budget impact:** every MCP tool call counts against the agent's `MaxAPICalls`. Account for some lookups when setting `AGENT_MAX_LOOPS` / `AGENT_CONTEXT_MAX_LOOPS`.

**Rule:** Any MCP server that needs credentials must receive them as **Dagger secrets** (`SetSecret` + `WithSecretVariable`), never plain env strings — the LLM sees the server's *tools*, never its env. The Atlassian tokens follow this rule; Context7 needs none.

---

## 18. The HTTP API (`server/`)

A **stateless** Gin adapter over the Temporal client — Temporal holds all durable state, so the server needs no database. There is **no native Temporal end-user UI** for starting business workflows; the Web UI (`:8233`) is an operator/debug console only. This API is the supported way to trigger and steer runs from a frontend.

| Route | Purpose |
|---|---|
| `POST /v1/runs` | Start a run. Body: `{issue_reference, repo_url, base_branch?}`. Returns `{workflow_id, run_id, status_url}` (202). |
| `GET /v1/runs/:id` | Describe a run; includes the `result` once closed. |
| `POST /v1/runs/:id/signal` | Supply missing context to a halted run. Body: `{info}`. This is the production human-in-the-loop bridge (§16). |
| `GET /healthz` | Liveness. |
| `GET /swagger/*` | Interactive OpenAPI docs (Swagger UI) at `/swagger/index.html`. |

**Swagger / OpenAPI.** Handlers carry `swaggo` annotations (`@Summary`, `@Param`, `@Success`, …); each returns a **typed** response struct (`startRunResponse`, `runStatusResponse`, `errorResponse`, …) rather than an untyped `gin.H`, so the generated schema is accurate. `swag init` (via `make swagger`) writes the spec + UI assets into `server/docs/`, which is **committed** so the scratch image builds without the `swag` CLI. The CLI version must match the `swaggo/swag` library in `go.mod` (v1.16.x) or `docs.go` won't compile. Regenerate `server/docs/` whenever a handler or its types change.

`authMiddleware()` is a chokepoint stub for OAuth/JWT — every `/v1` route passes through it. When `API_AUTH_REQUIRED=true` it rejects requests lacking an `Authorization` header; wire real token validation (e.g. `golang.org/x/oauth2` + JWKS) inside it. **Rule:** keep auth as this single middleware seam; do not scatter per-route checks.

Workflow IDs are `coding-agent-{issue}-{unixtime}` so re-runs of the same ticket are allowed. To instead reject duplicate concurrent runs, use the issue key alone as the ID and Temporal will dedupe.

---

## 19. What the Agent Must Never Do

This section is addressed directly to any automated agent (LLM or otherwise) making changes to this repository.

**Never violate the determinism boundary.** Do not import Dagger, `net`, `os/exec`, or any I/O package from `workflows/`. Not even indirectly through a shared utility package.

**Never put credentials in Temporal payloads.** Do not add a `Token`, `Password`, `APIKey`, or any secret string to any type in `types/types.go` or as a field in workflow/activity inputs or outputs.

**Never skip the independent verification gate.** The `go test ./...` re-run after the agent completes is not optional. Do not remove it, comment it out, or replace it with the agent's self-reported result. The gate exists because the model can be wrong about whether its code compiles and passes.

**Never weaken the test suite.** When writing or modifying code in a target repository, do not delete tests, comment them out, add `t.Skip()`, lower test assertions, or otherwise reduce coverage to make tests pass. Fix the implementation.

**Never add `WithSecretVariable` to the agent workspace container.** Credentials belong only in the pusher container. If you find yourself needing a credential inside the LLM's workspace, stop and re-examine the architecture.

**Never use `git add -A` or `git commit --amend` from inside the agent workspace.** The agent produces a modified directory; the separate pusher container handles all Git operations. The agent workspace has no `.git` directory and should never acquire one.

**Never increase `MaximumAttempts` on the coding agent (`RunCodingAgentActivity`) above 3** without explicit human sign-off. Each attempt is expensive.

**Never bypass the `NonRetryableErrorTypes` lists.** If you are adding a new error scenario, classify it explicitly. Do not silently convert a validation failure into a retryable error.

---

## 20. How to Add a New Activity

1. Add the method to the `Activities` struct in `activities/` (in an appropriate file, or a new file)
2. Give it the signature `func (a *Activities) MyNewActivity(ctx context.Context, in types.MyInput) (types.MyOutput, error)`
3. Add corresponding input/output types to `types/types.go` (short primitives only)
4. In `workflows/agent_workflow.go`, add a new `workflow.WithActivityOptions` block and `workflow.ExecuteActivity` call, sequenced correctly
5. The nil receiver `var a *activities.Activities` will automatically expose the new method — no additional registration is needed; `w.RegisterActivity(activities.NewActivities())` registers all methods on the struct

Do not register individual activity functions. Always register the whole struct via `w.RegisterActivity`. This keeps registration and the Activities struct in sync automatically.

---

## 21. Running and Verifying the System

```sh
# One-time: create your local env file and fill in the tokens
cp .env.example .env

# Compile the app packages (dagger/toolbox is excluded — built by the Dagger CLI)
go build ./activities/ ./workflows/ ./worker/ ./starter/ ./server/ ./types/

# Vet for common errors
go vet ./activities/ ./workflows/ ./worker/ ./starter/ ./server/ ./types/

# Start Temporal dev server (separate terminal; UI at localhost:8233)
temporal server start-dev

# Start the worker (reads .env: GitHub token, Atlassian creds, LLM key)
go run ./worker

# Option A — trigger a run via the CLI
go run ./starter -issue "PROJ-123" -repo "https://github.com/owner/repo" -base main

# Option B — trigger via the HTTP API
go run ./server                                   # separate terminal, :8080
curl -X POST localhost:8080/v1/runs -H 'content-type: application/json' \
  -d '{"issue_reference":"PROJ-123","repo_url":"https://github.com/owner/repo"}'
# If the run halts for missing context, supply it:
curl -X POST localhost:8080/v1/runs/<workflow_id>/signal -H 'content-type: application/json' \
  -d '{"info":"design doc: https://your.atlassian.net/wiki/...."}'

# Build the toolbox Dagger module (requires Dagger CLI)
cd dagger/toolbox && dagger develop
```

### Running via Docker Compose

`docker-compose.yml` brings up the durable stack — **postgres → temporal (auto-setup) → temporal-ui → server** — plus the **litellm** proxy under the optional `bedrock` profile:

```sh
docker compose up -d                     # postgres + temporal + ui + server
docker compose --profile bedrock up -d   # …also the LiteLLM Bedrock proxy
go run ./worker                          # the worker still runs on the HOST
```

**The worker is deliberately NOT containerised in compose.** It drives the Dagger agent loop, which needs a Dagger engine and a docker socket; running it on the host keeps that wiring simple and avoids docker-in-docker. The **server**, by contrast, is a thin Temporal client (no Dagger import), so it builds CGO-free via `server/Dockerfile.server` (multi-stage → `scratch`, ~static binary + CA certs) and containerises cleanly. Inside the compose network services reach Temporal as `temporal:7233`; on the host it's `localhost:7233`. `.dockerignore` keeps `.env`, `.git`, and the `dagger/` module out of the build context (secret hygiene + the toolbox's generated code doesn't compile under plain `go build`).

`AGENT_SIMULATE_PR=true` (the default) skips the live GitHub API call. The Atlassian context phase still needs valid `JIRA_*` / `CONFLUENCE_*` tokens and a reachable Dagger engine.

The Temporal UI at `http://localhost:8233` shows real-time workflow execution, per-activity inputs/outputs, the full event history, retry counts, and error details — and is where an operator can manually send the `supply-context` signal.

---

## 22. Dependency Versions

| Dependency | Version | Role |
|---|---|---|
| `go.temporal.io/sdk` | `v1.31.0` | Workflow orchestration, activity execution, retry/timeout |
| `dagger.io/dagger` | `v0.21.7` | Container execution, LLM Env + **MCP** (`WithMCPServer`, ≥ v0.19.0), secrets, cache volumes, services |
| `github.com/gin-gonic/gin` | `v1.10.x` | HTTP API framework (`server/`) |
| `github.com/swaggo/gin-swagger` + `swaggo/files` | `v1.6.x` / `v1.0.x` | Serves Swagger UI at `/swagger/*` |
| `github.com/swaggo/swag` | `v1.16.x` | OpenAPI generation; **CLI must match this library version** |
| `github.com/joho/godotenv` | `v1.5.x` | `.env` loading for worker/starter/server |
| Go toolchain | `1.26` | Module language version |

When upgrading either SDK, re-read the changelog for:
- Temporal: any changes to replay behavior, serialization format, or activity registration
- Dagger: any changes to `LLMOpts`, `Env` API, `WithMCPServer`, `AsService`, or `SetSecret`/`WithSecretVariable`

Run `go mod tidy` after any dependency change. Both `go build` and `go vet` must return exit 0 (on the app package list above) before committing.
