GO ?= go
BIN_DIR := dist

# ---- 构建信息 -------------------------------------------------------
# 版本单一来源：VERSION 文件；在 git 环境里用 git describe 覆盖。
# 如此 tag 是天然真源，而无仓库时 VERSION 仍能让产物不报 dev。
VERSION_FILE := $(shell cat VERSION 2>/dev/null)
GIT_DESC     := $(shell git describe --tags --always --dirty 2>/dev/null)
VERSION      := $(if $(GIT_DESC),$(GIT_DESC),$(VERSION_FILE))
VERSION      := $(if $(VERSION),$(VERSION),dev)
COMMIT       := $(shell git rev-parse --short HEAD 2>/dev/null)
COMMIT       := $(if $(COMMIT),$(COMMIT),none)
BUILD_DATE   := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

BINFO   := github.com/example/benchmark-prompts/internal/buildinfo
XFLAGS  := -X $(BINFO).Version=$(VERSION) -X $(BINFO).Commit=$(COMMIT) -X $(BINFO).Date=$(BUILD_DATE)
LDFLAGS := -s -w $(XFLAGS)

# Windows 下 go build 产物必须带 .exe，否则无法直接执行
GOEXE := $(shell $(GO) env GOEXE 2>/dev/null)

.PHONY: help tidy fmt vet static build build-cli test test-ts lint-ts race cover run dev \
        build-all release release-verify version check smoke smoke-cli smoke-pi smoke-dsh typecheck-dsh contract backup clean \
        web-install web-build web-preview web-check web-size smoke-web

help: ## 显示可用目标
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-12s %s\n", $$1, $$2}'

tidy: ## 整理依赖
	$(GO) mod tidy

fmt: ## 格式化
	$(GO) fmt ./...

vet: ## 静态检查
	$(GO) vet ./...

static: ## staticcheck 深度静态检查（未安装则明说 SKIP，不假通过）
	@if command -v staticcheck >/dev/null 2>&1; then \
		staticcheck ./...; \
	else \
		echo "SKIP static：未安装 staticcheck（不是通过；完整门禁请在装有它的机器上跑）"; \
	fi

build: ## 编译 server
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/bench-server$(GOEXE) ./cmd/server

build-cli: ## 编译 bench CLI（DSH/Pi 插件调用的入物）
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/bench$(GOEXE) ./cmd/cli

test: ## 运行测试
	$(GO) test ./...

test-ts: build-cli ## 运行扩展的 TS 单测（pi 侧 + dsh 侧纯逻辑；有 DSH 安装树再跑插件层）
	@if command -v node >/dev/null 2>&1; then \
		BENCH_BIN=$(BIN_DIR)/bench$(GOEXE) node --test plugins/pi/extension/bench-core.test.ts plugins/dsh/bench-core.test.ts; \
		if [ -d "$$HOME/.dsh/profiles/node_modules/@deepseek-ai/dsh-tools" ]; then \
			node --import ./scripts/dsh-module-hook.mjs --test plugins/dsh/plugin.test.ts; \
		else \
			echo "SKIP plugin.test.ts：无 DSH 安装树（不是通过）"; \
		fi; \
	else \
		echo "SKIP test-ts：未找到 node（不是假通过，请装有 node 的机器上补跑）"; \
	fi

lint-ts: ## biome 检查并格式化 TS（Pi/DSH 扩展 + 前端 ts/tsx）
	@if command -v biome >/dev/null 2>&1; then \
		biome check --write plugins/pi/extension/ plugins/dsh/ web/src/; \
	else \
		echo "SKIP lint-ts：未找到 biome"; \
	fi

race: ## 带竞态检测运行测试
	$(GO) test -race ./...

cover: ## 覆盖率摘要
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -20

dev: ## 本地明文 HTTP 启动（无需证书与主密钥）
	$(GO) run ./cmd/server -dev -config config.example.yaml -log-level debug

run: build ## 用示例配置启动
	$(BIN_DIR)/bench-server -config config.example.yaml

backup: ## 一次性一致性备份
	$(GO) run ./cmd/server -config config.example.yaml -backup ./backup-manual.db

smoke: build ## 服务端端到端冒烟（真实 HTTP + 真实 SQLite）
	bash scripts/smoke.sh

smoke-cli: build build-cli ## CLI 二进制端到端冒烟（验证插件实际调用的东西）
	bash scripts/smoke-cli.sh

smoke-pi: build build-cli ## Pi 扩展真实验证（需本机有 pi 与模型凭据，否则自动 SKIP）
	bash scripts/smoke-pi.sh

smoke-dsh: build build-cli ## DSH 插件真实验证（需本机 DSH 安装树与模型凭据，否则自动 SKIP）
	bash scripts/smoke-dsh.sh

typecheck-dsh: ## DSH 插件 tsc 类型检查（DSH 安装树缺件时 SKIP 并明说"不是通过"）
	bash scripts/dsh-typecheck.sh

##@ 前端（web/，Astro 纯静态 + Preact 岛）

web-install: ## 安装前端依赖（pnpm；版本锁定见 web/pnpm-lock.yaml）
	cd web && pnpm install

web-build: ## 构建前端站点 → web/dist/（整目录可上 CDN，源站零前端流量）
	cd web && pnpm exec astro sync
	cd web && pnpm exec astro build
	@bash scripts/web-size.sh

web-preview: ## 本地预览已构建产物（默认 http://localhost:4321）
	cd web && pnpm exec astro preview

web-check: ## 前端类型与静态检查（缺 pnpm/依赖或未构建时明说 SKIP，不假通过）
	@if command -v pnpm >/dev/null 2>&1 && [ -d web/node_modules ]; then \
		cd web && pnpm exec astro check && bash ../scripts/web-size.sh; \
	else \
		echo "SKIP web-check：缺 pnpm 或未跑 make web-install（不是通过）"; \
	fi

web-size: ## 只报告前端产物体积（gzip -9）
	@bash scripts/web-size.sh

smoke-web: build build-cli ## 前端端到端冒烟（真源站 + 真产物 + 浏览器那份 api.ts；缺依赖自动 SKIP）
	bash scripts/smoke-web.sh

contract: ## 采集 bench 的真实 --json 输出与退出码（框架适配的契约地面数据）
	bash scripts/capture-contract.sh

check: fmt vet static build build-cli race lint-ts test-ts web-check ## 完整质量门禁（Go + TS + 前端）

build-all: ## 交叉编译全发布矩阵（bench 5 平台 / server 3 平台，纯 Go 无需 CGO）
	@mkdir -p $(BIN_DIR)
	@echo "构建版本：$(VERSION) (commit $(COMMIT), $(BUILD_DATE))"
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/bench-linux-amd64           ./cmd/cli
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/bench-linux-arm64           ./cmd/cli
	CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/bench-darwin-amd64          ./cmd/cli
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/bench-darwin-arm64          ./cmd/cli
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/bench-windows-amd64.exe     ./cmd/cli
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/bench-server-linux-amd64    ./cmd/server
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/bench-server-linux-arm64    ./cmd/server
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/bench-server-darwin-arm64   ./cmd/server
	@echo "产物在 $(BIN_DIR)/（纯 Go 驱动，无需 CGO/交叉工具链）"

release: ## 全平台编译 + 打包 tar.gz + sha256sums.txt + RELEASE-INFO
	VERSION="$(VERSION)" COMMIT="$(COMMIT)" bash scripts/release.sh

release-verify: ## 验证发布产物（读 dist/RELEASE-INFO 对比字节级注入证据）
	bash scripts/verify-release.sh

version: ## 打印将要注入的构建信息（不构建）
	@echo "VERSION=$(VERSION)  COMMIT=$(COMMIT)  DATE=$(BUILD_DATE)"
	@echo "来源：$(if $(GIT_DESC),git describe,VERSION 文件)"

clean: ## 清理产物
	rm -rf $(BIN_DIR) coverage.out .smoke