# airgate-openai

AirGate 的 OpenAI 网关插件。

运行时元信息以 `backend/internal/gateway/metadata.go` 为单源，根目录 `plugin.yaml` 由 genmanifest 自动生成，不要手工修改。

## 功能

- OpenAI Responses API / Chat Completions 转发
- ChatGPT OAuth 账号接入
- Anthropic `/v1/messages` 协议翻译（Claude → OpenAI）
- WebSocket 双向桥接

## 路由

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/v1/responses` | Responses API（Codex 核心端点）|
| POST | `/v1/chat/completions` | Chat Completions API |
| POST | `/v1/messages` | Anthropic Messages API（协议翻译）|
| POST | `/v1/messages/count_tokens` | Anthropic Count Tokens（兼容回退） |
| GET | `/v1/models` | 模型列表 |
| WS | `/v1/responses` | Responses API（WebSocket）|

另外还提供不带 `/v1` 前缀的别名路由：

- `POST /responses`
- `POST /chat/completions`
- `POST /messages`
- `POST /messages/count_tokens`
- `GET /models`
- `WS /responses`

## 账号类型

| 类型 | 说明 |
|------|------|
| `apikey` | 支持所有提供 Responses 标准接口的服务 |
| `oauth` | 浏览器授权登录 ChatGPT |

## 转发流程

```
请求进入
  │
  ├─ Anthropic Messages API？
  │    └→ anthropic_forward.go → 协议翻译 → Responses API → 上游
  │
  ├─ API Key 账号？
  │    └→ forward.go:forwardAPIKey → 直连上游（HTTP/SSE）
  │
  └─ OAuth 账号？
       └→ forward.go:forwardOAuth → WebSocket 连上游，SSE 写回客户端
```

### Anthropic 协议翻译

```
Anthropic JSON 请求
  → anthropic_convert.go    一步直转为 Responses API JSON
  → anthropic_forward.go    转发到上游（含模型降级重试）
  → anthropic_response.go   Responses SSE → Anthropic SSE 回译
```

## 目录结构

```
├── backend/                        Go 后端（插件主体）
│   ├── main.go                      gRPC 插件入口
│   ├── cmd/
│   │   ├── chat/                    交互式测试客户端（SSE/WS 双协议）
│   │   ├── devserver/               开发服务器（模拟 AirGate Core）
│   │   └── genmanifest/             plugin.yaml 生成器
│   ├── internal/
│   │   ├── gateway/                 网关核心逻辑
│   │   │   ├── gateway.go            插件接口实现（GatewayPlugin）
│   │   │   ├── metadata.go           插件元信息（单源）
│   │   │   ├── forward.go            三模式转发分发 + API Key/OAuth 转发
│   │   │   ├── anthropic_forward.go  Anthropic 转发入口、模型降级重试
│   │   │   ├── anthropic_convert.go  Anthropic → Responses 请求一步直转
│   │   │   ├── anthropic_response.go Responses → Anthropic 响应回译
│   │   │   ├── anthropic_model_map.go Claude ↔ OpenAI 模型映射
│   │   │   ├── anthropic_util.go     工具名缩短、stop_reason 转换
│   │   │   ├── request.go            请求检测、URL 构建、预处理
│   │   │   ├── request_convert.go    Chat Completions → Responses 转换
│   │   │   ├── context_guard.go      上下文裁剪（历史消息截断）
│   │   │   ├── stream.go             SSE 流式响应处理
│   │   │   ├── ws.go                 WebSocket 连接与事件解析
│   │   │   ├── ws_handler.go         WebSocket 入站连接处理
│   │   │   ├── headers.go            认证头、白名单、Codex 标识
│   │   │   ├── errors.go             统一错误处理
│   │   │   ├── oauth.go              OAuth 授权流程（PKCE）
│   │   │   └── assets.go             前端资源嵌入
│   │   ├── model/                   模型注册表
│   │   │   └── registry.go           集中模型规格定义
│   ├── resources/                   嵌入资源（系统提示词）
│   └── devdata/                     开发服务器运行时数据（gitignore）
├── web/                            前端（插件自定义账号表单）
│   └── src/components/AccountForm.tsx
├── plugin.yaml                     插件描述文件（生成产物）
└── Makefile                        构建脚本
```

## 规则

- `metadata.go` 是运行时真相
- `plugin.yaml` 是生成产物（`go run ./cmd/genmanifest`）
- 账号表单、路由、模型列表都应该和 `metadata.go` 保持一致
- 使用 gjson/sjson 处理 JSON（零 struct 哲学）

## 构建

```bash
make build          # 完整构建（前端 → 后端）
make test           # 运行测试
make lint           # 代码检查

# 单独操作
cd backend && go run ./cmd/genmanifest   # 重新生成 plugin.yaml
cd backend && go run ./cmd/devserver     # 启动开发服务器
cd backend && go run ./cmd/chat          # 启动交互式测试
```

## 发版

`metadata.go` 中的 `PluginVersion` 是 `var`，默认值用于本地开发。**正式发版只需要打 git tag，不要手工改版本号字段**：

```bash
git tag v0.2.0
git push origin v0.2.0
```

GitHub Actions 的 `release.yml` 工作流会自动：

1. 矩阵构建 4 个平台二进制（linux/darwin × amd64/arm64）
2. 通过 `-ldflags "-X .../gateway.PluginVersion=${version}"` 把 git tag（去掉 `v` 前缀）注入到二进制
3. 上传到 GitHub Release，资产命名 `gateway-openai-{os}-{arch}`
4. airgate-core 插件市场会通过 GitHub API 自动同步新版本（默认 6h，或在控制台市场页点刷新按钮立即同步）

这样 git tag = release 版本 = 已安装 tab 显示的版本，**单一来源、永不偏离**。
