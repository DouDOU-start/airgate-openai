# AirGate OpenAI 插件 Makefile

GO := GOTOOLCHAIN=local go

.PHONY: help build build-web build-backend lint fmt test clean

help: ## 显示帮助信息
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

# ===================== 构建 =====================

build: build-web build-backend ## 完整构建：前端 → 复制 → 后端

build-web: ## 构建前端
	cd web && npm run build

build-backend: ## 构建后端（自动复制前端产物）
	rm -rf backend/internal/gateway/webdist
	cp -r web/dist backend/internal/gateway/webdist
	cd backend && $(GO) build -o ../bin/gateway-openai .

# ===================== 质量检查 =====================

lint: ## 代码检查
	@cd backend && \
	if command -v golangci-lint > /dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "未安装 golangci-lint，回退到 go vet"; \
		$(GO) vet ./...; \
	fi
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

# ===================== 清理 =====================

clean: ## 清理构建产物
	rm -rf backend/internal/gateway/webdist bin/
