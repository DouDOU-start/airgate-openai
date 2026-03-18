# AirGate OpenAI 插件 Makefile

GO := GOTOOLCHAIN=local go

.PHONY: help install build build-web build-backend release dev ensure-webdist ci pre-commit lint fmt test vet clean setup-hooks

help: ## 显示帮助信息
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

# ===================== 构建 =====================

install: ## 安装前后端依赖
	cd web && npm install
	cd backend && $(GO) mod download
	@echo "依赖安装完成"

build: build-web build-backend ## 完整构建：前端 → 复制 → 后端

build-web: ## 构建前端
	cd web && npm run build

build-backend: ## 构建后端（自动复制前端产物）
	rm -rf backend/internal/gateway/webdist
	cp -r web/dist backend/internal/gateway/webdist
	cd backend && $(GO) build -o ../bin/gateway-openai .

release: build-web ## 编译 Linux 版本（用于上传到 Docker 部署）
	rm -rf backend/internal/gateway/webdist
	cp -r web/dist backend/internal/gateway/webdist
	cd backend && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -buildvcs=false -trimpath -o ../bin/gateway-openai-linux-amd64 .
	@echo "构建完成: bin/gateway-openai-linux-amd64"
	@echo "通过 AirGate 管理界面 → 插件管理 → 安装插件 → 上传安装"

# ===================== 开发 =====================

dev: ## 启动开发服务器（自动安装依赖、构建前端）
	@if [ ! -d web/node_modules ]; then \
		echo "检测到前端依赖未安装，正在安装..."; \
		cd web && npm install; \
	fi
	@if [ ! -d web/dist ]; then \
		echo "检测到前端未构建，正在构建..."; \
		cd web && npm run build; \
	fi
	cd backend && $(GO) run ./cmd/devserver

# ===================== 质量检查 =====================

ensure-webdist: ## 确保 webdist 非空（go:embed 要求至少一个文件）
	@if [ -d web/dist ] && [ "$$(ls -A web/dist 2>/dev/null)" ]; then \
		rm -rf backend/internal/gateway/webdist; \
		cp -r web/dist backend/internal/gateway/webdist; \
	elif [ ! "$$(ls -A backend/internal/gateway/webdist 2>/dev/null)" ]; then \
		mkdir -p backend/internal/gateway/webdist; \
		echo "placeholder" > backend/internal/gateway/webdist/.gitkeep; \
	fi

ci: ensure-webdist lint test vet build-backend ## 本地运行与 CI 完全一致的检查

pre-commit: ensure-webdist lint vet ## pre-commit hook 调用（跳过耗时的测试和构建）

lint: ## 代码检查（需要安装 golangci-lint）
	@if ! command -v golangci-lint > /dev/null 2>&1; then \
		echo "错误: 未安装 golangci-lint，请执行: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest"; \
		exit 1; \
	fi
	@cd backend && golangci-lint run ./...
	@echo "代码检查通过"

fmt: ## 格式化代码
	@cd backend && \
	if command -v goimports > /dev/null 2>&1; then \
		goimports -w -local github.com/DouDOU-start .; \
	else \
		$(GO) fmt ./...; \
	fi
	@echo "代码格式化完成"

test: ## 运行测试
	@cd backend && $(GO) test ./...
	@echo "测试完成"

vet: ## 静态分析
	@cd backend && $(GO) vet ./...

# ===================== Git Hooks =====================

setup-hooks: ## 安装 Git pre-commit hook
	@echo '#!/bin/sh' > .git/hooks/pre-commit
	@echo 'make pre-commit' >> .git/hooks/pre-commit
	@chmod +x .git/hooks/pre-commit
	@echo "pre-commit hook 已安装"

# ===================== 清理 =====================

clean: ## 清理构建产物
	rm -rf backend/internal/gateway/webdist bin/
