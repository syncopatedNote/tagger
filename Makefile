# Convenience targets for the full compose stack (all services including the
# worker and Dagger engine) or for running just the worker on the host.
#
# Full stack:   make up        (docker compose up -d — runs everything)
# Local worker: make worker    (go run ./worker — useful during dev; requires
#               the dagger-engine compose service to already be up so that
#               _EXPERIMENTAL_DAGGER_RUNNER_HOST in .env resolves correctly)

# Load .env so targets (e.g. the worker) see the same config the containers do.
ifneq (,$(wildcard .env))
include .env
export
endif

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: up
up: ## Start core infra (postgres + temporal + ui + server)
	docker compose up -d
	@echo "Temporal UI → http://localhost:8233   |   API → http://localhost:8080"

.PHONY: up-bedrock
up-bedrock: ## Start core infra AND the LiteLLM Bedrock proxy (:4000)
	docker compose --profile bedrock up -d
	@echo "LiteLLM proxy → http://localhost:4000"

.PHONY: worker
worker: ## Run the worker on the host (polls Temporal, runs Dagger activities)
	go run ./worker

.PHONY: logs
logs: ## Tail logs for all compose services
	docker compose logs -f

.PHONY: ps
ps: ## Show compose service status
	docker compose ps

.PHONY: down
down: ## Stop all compose services (keeps the postgres volume)
	docker compose --profile bedrock down

.PHONY: clean
clean: ## Stop services AND delete the postgres volume (wipes Temporal history)
	docker compose --profile bedrock down -v

# --- one-off triggers -------------------------------------------------------
# Note: LANGUAGE= (not LANG=) — LANG is the shell's locale env var and would
# collide with `export` above, silently sending the locale as the language.

# Usage: make run ISSUE=PROJ-123 REPO=https://github.com/owner/repo [LANGUAGE=python]
.PHONY: run
run: ## Trigger a run via the HTTP API (ISSUE=, REPO=, optional LANGUAGE=)
	@test -n "$(ISSUE)" || { echo "ISSUE= is required"; exit 1; }
	@test -n "$(REPO)"  || { echo "REPO= is required"; exit 1; }
	curl -sS -X POST localhost:8080/v1/runs \
		-H 'content-type: application/json' \
		-d '{"issue_reference":"$(ISSUE)","repo_url":"$(REPO)"$(if $(LANGUAGE),$(comma)"language":"$(LANGUAGE)")}' \
		| sed 's/.*/  → &/'

# --- docs -------------------------------------------------------------------

# Regenerate the OpenAPI spec + Swagger UI assets (server/docs) from the
# annotations in server/main.go. Run after changing any handler or its types.
# The swag CLI version MUST match the swaggo/swag library in go.mod (v1.16.x).
.PHONY: swagger
swagger: ## Regenerate Swagger docs (server/docs) from handler annotations
	@command -v swag >/dev/null 2>&1 || { echo "installing swag CLI…"; go install github.com/swaggo/swag/cmd/swag@v1.16.4; }
	cd server && swag init --generalInfo main.go --output ./docs --parseDependency --parseInternal
	@echo "Swagger UI → http://localhost:8080/swagger/index.html (when the server is running)"

# --- checks -----------------------------------------------------------------

# Explicit package list: dagger/toolbox is a separate Dagger module that
# requires `dagger develop` codegen before it compiles — exclude it everywhere.
GO_PKGS := \
	./activities/... \
	./workflows/... \
	./worker/... \
	./server/... \
	./types/... \
	./agent_registry/... \
	./llm_factory/...

.PHONY: build
build: ## Build all packages (excludes dagger/toolbox)
	go build $(GO_PKGS)

.PHONY: vet
vet: ## Run go vet over all packages
	go vet $(GO_PKGS)

.PHONY: test
test: ## Run tests (excludes dagger/toolbox)
	go test $(GO_PKGS)

.PHONY: verify
verify: build vet test ## Build + vet + test the Go module (excludes the dagger/toolbox module)

.PHONY: hooks
hooks: ## Install git pre-commit and pre-push hooks (run once after cloning)
	@command -v pre-commit >/dev/null 2>&1 || { echo "pre-commit not found — brew install pre-commit"; exit 1; }
	pre-commit install --install-hooks

comma := ,
