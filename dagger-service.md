# Why the Dagger engine runs as a dedicated compose service

## The short version

The Dagger engine — not the worker — makes every LLM API call. The engine reads
its LLM credentials from its own environment variables at startup, before any
worker process connects. If the engine is auto-provisioned on demand (the Dagger
default), it starts with a blank environment and can never see credentials the
worker sets afterwards. Running the engine as a compose service (`dagger-engine`)
with the credentials baked into its `environment:` block is the only reliable
way to get the right secrets into the right process at the right time.

---

## Why the Dagger engine holds the credentials, not the worker

When the Go worker calls `client.LLM(dagger.LLMOpts{Model: "nova-pro"})`, it is
not making an LLM API call itself. It is declaring a lazy computation graph inside
the Dagger engine. The engine — a long-running daemon process — is what ultimately
opens the TCP connection to the LLM API, sends the prompt, and receives the
response. The worker is a thin orchestrator that hands instructions to the engine
and reads results back.

Because the engine owns the network call, it is also the process that must hold
the credentials. Dagger's `dagger.LLMOpts` struct exposes only two fields: `Model`
and `MaxAPICalls`. There is no `APIKey`, no `BaseURL`, no `Provider` field. The
engine selects its LLM backend exclusively by reading environment variables present
in its own process:

| Dagger expects on the engine | What it means |
|---|---|
| `OPENAI_BASE_URL` | LiteLLM proxy URL (your Bedrock gateway) |
| `OPENAI_API_KEY` | LiteLLM master key |
| `OPENAI_MODEL` | Model name the proxy routes to Bedrock |
| `ANTHROPIC_API_KEY` | Direct Anthropic key (if using that provider) |

These must be in the **engine's** environment. Setting them on the worker process
has no effect on the engine's process — they are separate OS processes, possibly
in separate containers.

---

## The timing problem with auto-provisioned engines

By default, when the Dagger Go SDK calls `dagger.Connect()`, it:

1. Checks if a Dagger engine container is already running.
2. If not, pulls `registry.dagger.io/engine:<version>` and starts a new container.
3. Connects to that fresh container.

That freshly started container has a completely blank environment — no
`OPENAI_BASE_URL`, no keys, nothing. At this point the worker hasn't even had a
chance to set them, and even if it could, `os.Setenv` in the worker process only
affects the worker's own environment. There is no mechanism for a worker to inject
environment variables into a container it didn't start.

The result: `client.LLM(...)` inside the agent loop reaches an engine that has no
configured LLM backend and fails immediately.

---

## How the compose service solves it

The `dagger-engine` service in `docker-compose.yml` is started **before** any
worker runs, with all credentials set in its `environment:` block:

```yaml
dagger-engine:
  image: registry.dagger.io/engine:v0.21.7
  environment:
    OPENAI_BASE_URL:   ${LLM_BEDROCK_PROXY_URL}   # → http://litellm:4000
    OPENAI_API_KEY:    ${LLM_BEDROCK_PROXY_KEY}   # → sk-tagger-local-dev
    OPENAI_MODEL:      ${LLM_BEDROCK_MODEL}        # → nova-pro
    ANTHROPIC_API_KEY: ${ANTHROPIC_API_KEY:-}
```

These values come from your `.env` file at `docker compose up` time and are baked
into the engine container's process environment before it accepts any connections.
The engine is ready with credentials before the first workflow even starts.

The worker is then told to connect to this specific engine instead of
auto-provisioning a new one, via:

```
_EXPERIMENTAL_DAGGER_RUNNER_HOST=docker-container://tagger-dagger-engine
```

---

## What `docker-container://` is and how it works

`_EXPERIMENTAL_DAGGER_RUNNER_HOST` is a URI the Dagger CLI reads at startup. The
scheme selects a connection driver; the host is the target identifier. When the
scheme is `docker-container://`, the driver:

1. Shells out to the `docker` CLI binary.
2. Runs `docker exec -i tagger-dagger-engine /usr/local/bin/dagger session ...`
3. That exec'd process, running **inside** the already-running engine container,
   becomes the session endpoint.
4. The Dagger SDK communicates with it over the exec's stdin/stdout pipe.

No new container is started. No new engine process is launched. The SDK slots into
the engine that was already running with the right credentials.

This is why `docker-cli` must be installed in the worker container. The Dagger CLI
binary is present (pre-baked via `_EXPERIMENTAL_DAGGER_CLI_BIN`), and the Docker
socket is mounted at `/var/run/docker.sock`, but the socket alone is not enough —
the `docker-container://` driver needs the `docker` executable to run `docker exec`.
Without it the driver's availability check fails and you get:

```
start engine: driver for scheme "docker-container" was not available
```

---

## The full credential flow, end to end

```
.env file (on host)
  LLM_BEDROCK_PROXY_URL=http://litellm:4000
  LLM_BEDROCK_PROXY_KEY=sk-tagger-local-dev
  LLM_BEDROCK_MODEL=nova-pro
         │
         │  docker compose up
         ▼
tagger-dagger-engine container
  OPENAI_BASE_URL=http://litellm:4000   ← baked in at container start
  OPENAI_API_KEY=sk-tagger-local-dev
  OPENAI_MODEL=nova-pro
         │
         │  engine is running and credential-aware before any workflow starts
         │
tagger-worker container
  _EXPERIMENTAL_DAGGER_RUNNER_HOST=docker-container://tagger-dagger-engine
  /var/run/docker.sock  (mounted)
  /usr/local/bin/dagger (Dagger CLI, pre-baked)
  /usr/bin/docker       (Docker CLI, needed by docker-container:// driver)
         │
         │  workflow triggers → activity calls connectDagger()
         │  → dagger.Connect() forks the Dagger CLI subprocess
         │  → CLI reads _EXPERIMENTAL_DAGGER_RUNNER_HOST
         │  → docker-container:// driver runs: docker exec -i tagger-dagger-engine ...
         │  → session process runs INSIDE tagger-dagger-engine (credential-aware)
         │  → SDK communicates via stdio pipe
         ▼
tagger-litellm container
  (engine calls http://litellm:4000 using OPENAI_BASE_URL it already had)
         │
         ▼
Amazon Bedrock (nova-pro)
```

The worker also receives `LLM_BEDROCK_*` env vars in the compose file, but for a
different purpose: `llm_factory` reads them at startup to resolve the model name
string passed to `dagger.LLMOpts{Model: a.LLM.Model}`. The factory produces no SDK
client and makes no API calls — it only supplies the model string. The engine uses
its own pre-baked env vars for the actual backend call. The two sets of vars are
independent; the worker's copies never flow into the engine.

---

## Why `litellm` resolves correctly from inside the engine

The engine container is on the same Docker compose network as `litellm`. Docker's
embedded DNS resolves service names to container IPs within that network, so
`http://litellm:4000` routes correctly from the engine to the LiteLLM proxy. If
you were running the worker on the host (via `go run ./worker`) and the engine were
also on the host, you would use `http://localhost:4000` instead. The compose setup
keeps everything on the same network, so the service name always works.

---

## Summary of why each piece exists

| Piece | Why it exists |
|---|---|
| `dagger-engine` compose service | Starts the engine with credentials before any worker connects |
| `environment:` block on `dagger-engine` | Bakes `OPENAI_*` vars into the engine's process at container start |
| `_EXPERIMENTAL_DAGGER_RUNNER_HOST` | Tells the SDK/CLI to reuse the running engine instead of auto-provisioning a blank one |
| `docker-container://` scheme | Connects by exec-ing into the named container via Docker, reusing its process and environment |
| `docker-cli` in the worker image | Required by the `docker-container://` driver to run `docker exec` |
| `/var/run/docker.sock` mount on worker | Gives the Docker CLI in the worker access to the Docker daemon to exec into the engine |
