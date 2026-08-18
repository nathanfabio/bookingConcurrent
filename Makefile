# Common tasks (CLAUDE.md §11). Run `make help` for the list.
#
# `make run` sources .env so the process sees the same variables you edit there.

SHELL := /bin/bash
.DEFAULT_GOAL := help

# Prefer golangci-lint on PATH; fall back to GOPATH/bin where `go install`
# puts it, so `make lint` works without extra PATH setup.
GOLANGCI_LINT := $(shell command -v golangci-lint 2>/dev/null)
ifeq ($(GOLANGCI_LINT),)
GOLANGCI_LINT := $(shell go env GOPATH)/bin/golangci-lint
endif

.PHONY: help
help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

.PHONY: up
up: ## Start all infra dependencies (Redis, Postgres, RabbitMQ, redis-commander)
	docker compose up -d

.PHONY: down
down: ## Stop infra dependencies (data volumes are kept)
	docker compose down

.PHONY: ps
ps: ## Show infra container status
	docker compose ps

.PHONY: logs
logs: ## Tail infra logs
	docker compose logs -f --tail=100

.PHONY: test
test: ## Run unit tests
	go test ./...

.PHONY: test-race
test-race: ## Run all tests with the race detector (CI runs this too)
	go test -race ./...

.PHONY: lint
lint: ## golangci-lint + repo hygiene guards (no secrets in source, domain purity)
	$(GOLANGCI_LINT) run ./...
	bash scripts/lint-guards.sh

.PHONY: run
run: ## Run the API locally (requires .env and `make up`)
	@test -f .env || { echo "no .env found — run: cp .env.example .env" >&2; exit 1; }
	set -a; source ./.env; set +a; exec go run ./cmd/api

.PHONY: tidy
tidy: ## go mod tidy
	go mod tidy
