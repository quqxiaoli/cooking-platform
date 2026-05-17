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
MIGRATIONS  := migrations

# Docker Compose file (dev by default).
DC_FILE  ?= docker-compose.yml
V        ?= 0   # set V=1 to also remove volumes on docker-down

.PHONY: all build run test test-cover lint lint-fix \
        check-migrate migrate-up migrate-down migrate-status migrate-force migrate-create \
        docker-up docker-down docker-logs docker-ps \
        verify-step7 verify-step11 verify-step12 verify-step13 verify-step14 verify-step15 migrate-phone migrate-phone-dry \
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

# ── Database Migrations (golang-migrate) ──────────────────────────────────────
# Requires `migrate` CLI installed on host:
#   macOS:  brew install golang-migrate
#   Linux:  see https://github.com/golang-migrate/migrate/releases

## check-migrate: verify the migrate CLI is installed before running migrations
check-migrate:
	@which migrate > /dev/null || (echo "ERROR: 'migrate' CLI not found. Install with:" && echo "  macOS:  brew install golang-migrate" && echo "  Linux:  https://github.com/golang-migrate/migrate/releases" && exit 1)

## migrate-up: apply all pending migrations
migrate-up: check-migrate
	migrate -path $(MIGRATIONS) -database "$(MIGRATE_DSN)" up

## migrate-down: roll back the last applied migration
migrate-down: check-migrate
	migrate -path $(MIGRATIONS) -database "$(MIGRATE_DSN)" down 1

## migrate-status: show current migration version
migrate-status: check-migrate
	migrate -path $(MIGRATIONS) -database "$(MIGRATE_DSN)" version

## migrate-force VERSION=N: force-set migration version (use after manual fix)
migrate-force: check-migrate
	migrate -path $(MIGRATIONS) -database "$(MIGRATE_DSN)" force $(VERSION)

## migrate-create: scaffold a new migration pair. Usage: make migrate-create name=add_some_column
migrate-create:
	@if [ -z "$(name)" ]; then echo "ERROR: name required. Usage: make migrate-create name=add_some_column"; exit 1; fi
	migrate create -ext sql -dir migrations -seq $(name)

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

# ── Verification ──────────────────────────────────────────────────────────────
## verify-step7: run the Step 7 search-module end-to-end verification script
# Prerequisite: server running via `go run ./cmd/server 2>&1 | tee logs/dev.log`
# and the dev docker stack (cooking-mysql-dev / cooking-redis-dev) up.
verify-step7:
	bash scripts/verify_step7.sh

.PHONY: verify-step8
verify-step8: ## 运行 Step 8 关注模块端到端验证
	@bash scripts/verify_step8.sh

.PHONY: verify-step9
verify-step9: ## 运行 Step 9 图片上传模块端到端验证
	@bash scripts/verify_step9.sh

.PHONY: verify-step10
verify-step10: ## 运行 Step 10 内容审核 + 阿里云短信端到端验证
	@bash scripts/verify_step10.sh

.PHONY: verify-step11
verify-step11: ## 运行 Step 11 手机号 AES-GCM 加密端到端验证
	@bash scripts/verify_step11.sh

.PHONY: verify-step12
verify-step12: ## 运行 Step 12 日志脱敏 + 错误码收口端到端验证
	@bash scripts/verify_step12.sh

.PHONY: verify-step13
verify-step13: ## 运行 Step 13 EventBus RabbitMQ 生产加固端到端验证
	@bash scripts/verify_step13.sh

.PHONY: verify-step14
verify-step14: ## 运行 Step 14 MySQL 主从复制 + DBResolver 读写分离端到端验证
	@bash scripts/verify_step14.sh

.PHONY: verify-step15
verify-step15: ## 运行 Step 15 Nginx 双实例负载均衡端到端验证
	@bash scripts/verify_step15.sh

## migrate-phone: 一次性迁移 phone_encrypted 为 AES-GCM 密文（需设 APP_ENCRYPTION_PHONE_KEY）
.PHONY: migrate-phone
migrate-phone:
	@go run ./cmd/migrate-phone

## migrate-phone-dry: 预览迁移变更（不写入数据库）
.PHONY: migrate-phone-dry
migrate-phone-dry:
	@go run ./cmd/migrate-phone --dry-run

# ── Step Management (v4 工作流) ───────────────────────────────────────────────
## step-diff N=10: 生成本步代码变更清单脚手架（基于 git diff step-N-1-done..HEAD）
.PHONY: step-diff
step-diff:
	@if [ -z "$(N)" ]; then echo "ERROR: N required. Usage: make step-diff N=10"; exit 1; fi
	@bash scripts/gen_step_diff.sh $(N)

## step-closeout N=10: 收尾自检（敏感信息扫描 + 验证脚本 + commit message 模板）
.PHONY: step-closeout
step-closeout:
	@if [ -z "$(N)" ]; then echo "ERROR: N required. Usage: make step-closeout N=10"; exit 1; fi
	-@bash scripts/step_closeout.sh $(N)
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