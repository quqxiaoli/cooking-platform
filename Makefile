# Makefile — cooking-platform
# Usage: make <target>
# Requires: go 1.23+, docker, docker compose v2, golangci-lint, migrate (golang-migrate)

APP_NAME   := cooking-platform
BINARY     := bin/$(APP_NAME)
MAIN       := ./cmd/server
CONFIG     ?= configs/config.yaml

# golang-migrate DSN (used by migrate-up / migrate-down).
# Override on CLI: make migrate-up MIGRATE_DSN="mysql://root:cooking123@tcp(127.0.0.1:3306)/cooking_platform"
MIGRATE_DSN ?= mysql://root:cooking123@tcp(127.0.0.1:3306)/cooking_platform
MIGRATIONS  := file://migrations

# Docker Compose file (dev by default).
DC_FILE  ?= docker-compose.yml
V        ?= 0   # set V=1 to also remove volumes on docker-down

.PHONY: all build run test lint \
        migrate-up migrate-down migrate-status migrate-force \
        docker-up docker-down docker-logs docker-ps \
        clean deps help

# ── Default ───────────────────────────────────────────────────────────────────
all: build

# ── Build ─────────────────────────────────────────────────────────────────────
## build: compile the server binary to ./bin/cooking-platform
build: deps
	@mkdir -p bin
	go build -v \
		-ldflags "-X main.Version=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev)" \
		-o $(BINARY) $(MAIN)
	@echo "✅  Binary: $(BINARY)"

# ── Run ───────────────────────────────────────────────────────────────────────
## run: run the server from source (no compilation step, fast for dev)
run:
	CONFIG_PATH=$(CONFIG) go run $(MAIN)

# ── Test ──────────────────────────────────────────────────────────────────────
## test: run all tests with race detector
test:
	go test -v -race -count=1 ./...

## test-cover: run tests and open HTML coverage report
test-cover:
	go test -race -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out

# ── Lint ──────────────────────────────────────────────────────────────────────
## lint: run golangci-lint (install: https://golangci-lint.run/usage/install/)
lint:
	golangci-lint run ./...

## lint-fix: run golangci-lint with auto-fix where possible
lint-fix:
	golangci-lint run --fix ./...

# ── Dependencies ──────────────────────────────────────────────────────────────
## deps: tidy and download Go modules
deps:
	go mod tidy
	go mod download

# ── Database Migrations ───────────────────────────────────────────────────────
## migrate-up: apply all pending migrations
migrate-up:
	migrate -path $(MIGRATIONS) -database "$(MIGRATE_DSN)" up

## migrate-down: roll back the last applied migration
migrate-down:
	migrate -path $(MIGRATIONS) -database "$(MIGRATE_DSN)" down 1

## migrate-status: show current migration version
migrate-status:
	migrate -path $(MIGRATIONS) -database "$(MIGRATE_DSN)" version

## migrate-force VERSION=N: force-set migration version (use after manual fix)
migrate-force:
	migrate -path $(MIGRATIONS) -database "$(MIGRATE_DSN)" force $(VERSION)

# ── Docker ────────────────────────────────────────────────────────────────────
## docker-up: start dev infrastructure (MySQL + Redis) in background
docker-up:
	docker compose -f $(DC_FILE) up -d --wait
	@echo ""
	@echo "✅  MySQL  → localhost:3306  (root / cooking123)"
	@echo "✅  Redis  → localhost:6379  (no password)"
	@echo ""
	@echo "Run 'make run' to start the Go server."

## docker-down: stop dev infrastructure (add V=1 to also remove volumes)
docker-down:
ifeq ($(V),1)
	docker compose -f $(DC_FILE) down -v
	@echo "⚠️  Volumes removed — all data wiped."
else
	docker compose -f $(DC_FILE) down
	@echo "ℹ️  Volumes preserved. Use 'make docker-down V=1' to also wipe data."
endif

## docker-logs: tail logs from all dev containers
docker-logs:
	docker compose -f $(DC_FILE) logs -f

## docker-ps: list running dev containers
docker-ps:
	docker compose -f $(DC_FILE) ps

# ── Cleanup ───────────────────────────────────────────────────────────────────
## clean: remove build artefacts
clean:
	rm -rf bin/ coverage.out

# ── Help ─────────────────────────────────────────────────────────────────────
## help: list all available targets with descriptions
help:
	@echo "cooking-platform — available make targets:"
	@echo ""
	@grep -E '^## ' Makefile | sed 's/## /  /'
	@echo ""
