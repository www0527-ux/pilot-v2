# Pilot 本地开发、真实依赖验证和与 CI 对齐的质量门禁。
# GNU Make 请在 macOS/Linux 或 Windows 的 Git Bash/WSL 中使用；原生 Windows
# 启动路径由 scripts/start.bat 提供。

PILOT_CONFIG ?= $(CURDIR)/manifest/config/config.yaml
COMPOSE_FILE ?= manifest/docker/docker-compose.yml
COMPOSE_ARM_FILE ?= manifest/docker/docker-compose.arm.yml
# Compose 默认在 compose 文件目录找 .env；显式指向仓库根目录，与 Pilot 配置共用同一份密钥。
COMPOSE_ENV_FILE ?= $(CURDIR)/.env
BINARY ?= bin/pilot
WAIT_RETRIES ?= 30
WAIT_INTERVAL_SECONDS ?= 2
MODULE ?= base
USE_ARM_OVERLAY ?= 1
GOFILES := $(shell find . -type f -name '*.go' -not -path './.git/*' -not -path './vendor/*')

GO_ENV = PILOT_CONFIG="$(PILOT_CONFIG)" $(if $(GOCACHE),GOCACHE="$(GOCACHE)") $(if $(GOTMPDIR),GOTMPDIR="$(GOTMPDIR)")
COMPOSE_FILES = -f $(COMPOSE_FILE)
ifeq ($(USE_ARM_OVERLAY),1)
COMPOSE_FILES += -f $(COMPOSE_ARM_FILE)
endif
COMPOSE = docker compose --env-file "$(COMPOSE_ENV_FILE)" $(COMPOSE_FILES)

.DEFAULT_GOAL := help

.PHONY: help up down ps logs run start build build-check fmt fmt-check vet test test-race web-test tidy-check ci health verify wait-deps e2e seed-logs clean docker-build docker-up docker-down docker-e2e docker-module docker-eval

help:
	@echo "Available targets:"
	@echo "  make up          # 启动 Docker 依赖服务（不含 Pilot 容器）"
	@echo "  make down        # 停止并删除依赖服务"
	@echo "  make ps          # 查看依赖服务状态"
	@echo "  make logs        # 查看依赖服务日志"
	@echo "  make run         # 启动 Pilot 服务（本机 go run）"
	@echo "  make start       # 启动依赖、等待真实连通性后启动 Pilot"
	@echo "  make build       # 构建二进制到 bin/pilot"
	@echo "  make test        # 运行 go test ./..."
	@echo "  make test-race   # 运行 go test -race -count=1 ./..."
	@echo "  make web-test    # 运行前端静态契约与可访问性测试"
	@echo "  make fmt         # 格式化项目中的 Go 文件"
	@echo "  make fmt-check   # 检查格式化，不修改文件"
	@echo "  make vet         # 运行 go vet ./..."
	@echo "  make tidy-check  # 校验 go.mod 和 go.sum 已 tidy"
	@echo "  make ci          # 运行与 GitHub CI 一致的质量门禁"
	@echo "  make health      # HTTP 健康检查"
	@echo "  make verify      # 验证中间件与关键配置连通性"
	@echo "  make e2e         # 执行完整链路检查并验证真实写入（本机）"
	@echo "  make seed-logs   # 导入示例日志到 Elasticsearch"
	@echo "  make docker-build# 构建 ARM Pilot 镜像"
	@echo "  make docker-up   # 启动含 Pilot 的全栈 Compose（profile app）"
	@echo "  make docker-down # 停止全栈 Compose（含 app/e2e profiles）"
	@echo "  make docker-e2e  # 容器内完整 e2echeck"
	@echo "  make docker-module MODULE=base|corr|episodic|eval|hitl|mcp"
	@echo "  make docker-eval # 容器网络内 Agent 评测报告"
	@echo "  make clean       # 清理构建产物和 Go 构建缓存"
	@echo ""
	@echo "Optional variables:"
	@echo "  PILOT_CONFIG=$(CURDIR)/manifest/config/config.yaml"
	@echo "  COMPOSE_ENV_FILE=$(CURDIR)/.env"
	@echo "  USE_ARM_OVERLAY=1"
	@echo "  MODULE=base"
	@echo "  GOCACHE=/tmp/go-build-cache"
	@echo "  GOTMPDIR=/tmp/go-tmp"
	@echo "  WAIT_RETRIES=$(WAIT_RETRIES)"
	@echo "  WAIT_INTERVAL_SECONDS=$(WAIT_INTERVAL_SECONDS)"

up:
	@test -f "$(COMPOSE_ENV_FILE)" || (echo "missing $(COMPOSE_ENV_FILE); copy .env.example to .env and set MYSQL_ROOT_PASSWORD" >&2; exit 1)
	@$(COMPOSE) up -d redis elasticsearch mysql prometheus jaeger grafana

down:
	@$(COMPOSE) --profile app --profile e2e --profile mcp down

docker-build:
	@test -f "$(COMPOSE_ENV_FILE)" || (echo "missing $(COMPOSE_ENV_FILE); copy .env.example to .env and set MYSQL_ROOT_PASSWORD" >&2; exit 1)
	@$(COMPOSE) --profile app build pilot

docker-up: docker-build
	@$(COMPOSE) --profile app up -d

docker-down:
	@$(COMPOSE) --profile app --profile e2e --profile mcp down

docker-e2e: docker-up
	@$(COMPOSE) --profile app --profile e2e run --rm e2e

docker-module:
	@test -n "$(MODULE)" || (echo "MODULE is required" >&2; exit 1)
	@test -f "scripts/verify/$(MODULE).sh" || (echo "missing scripts/verify/$(MODULE).sh" >&2; exit 1)
	@bash "scripts/verify/$(MODULE).sh"

docker-eval:
	@bash scripts/verify/eval.sh

ps:
	@$(COMPOSE) ps

logs:
	@$(COMPOSE) logs -f --tail=100

run:
	@env $(GO_ENV) go run ./cmd/main.go

start:
	@$(MAKE) up
	@$(MAKE) wait-deps
	@$(MAKE) run

wait-deps:
	@attempt=1; \
	while [ $$attempt -le $(WAIT_RETRIES) ]; do \
		if env $(GO_ENV) go run ./cmd/checkdeps/main.go >/dev/null 2>&1; then exit 0; fi; \
		if [ $$attempt -eq $(WAIT_RETRIES) ]; then env $(GO_ENV) go run ./cmd/checkdeps/main.go; exit 1; fi; \
		echo "waiting for dependencies ($$attempt/$(WAIT_RETRIES))"; \
		sleep $(WAIT_INTERVAL_SECONDS); \
		attempt=$$((attempt + 1)); \
	done

build:
	@mkdir -p $(dir $(BINARY))
	@env $(GO_ENV) go build -o $(BINARY) ./cmd/main.go
	@echo "Built: $(BINARY)"

build-check:
	@env $(GO_ENV) go build ./...

fmt:
	@gofmt -w $(GOFILES)

fmt-check:
	@unformatted="$$(gofmt -l $(GOFILES))"; if [ -n "$$unformatted" ]; then echo "Go files need gofmt:"; echo "$$unformatted"; exit 1; fi

vet:
	@env $(GO_ENV) go vet ./...

test:
	@env $(GO_ENV) go test ./...

test-race:
	@env $(GO_ENV) go test -race -count=1 ./...

web-test:
	@node --check web/app.js
	@node --check web/agent-session.js
	@node --check web/agent-chat.js
	@node --check web/progress.js
	@node --test web/app.test.mjs web/agent-session.test.mjs

tidy-check:
	@go mod tidy -diff

ci: fmt-check vet build-check test-race web-test tidy-check

health:
	@env $(GO_ENV) go run ./cmd/healthcheck/main.go

verify:
	@env $(GO_ENV) go run ./cmd/checkdeps/main.go

e2e:
	@env $(GO_ENV) go run ./cmd/e2echeck/main.go

seed-logs:
	@env $(GO_ENV) go run ./cmd/seedlogs/main.go --count 40

clean:
	@go clean -cache
	@rm -rf bin
