<div align="center">
  <h1>AirGate OpenAI</h1>

  <p><strong>OpenAI / ChatGPT / Anthropic 协议三合一网关插件</strong></p>

  <p>
    <a href="https://github.com/DouDOU-start/airgate-openai/releases"><img src="https://img.shields.io/github/v/release/DouDOU-start/airgate-openai?style=flat-square" alt="release" /></a>
    <a href="https://github.com/DouDOU-start/airgate-openai/actions/workflows/ci.yml"><img src="https://img.shields.io/github/actions/workflow/status/DouDOU-start/airgate-openai/ci.yml?branch=master&style=flat-square&label=CI" alt="ci" /></a>
    <a href="https://github.com/DouDOU-start/airgate-openai/blob/master/LICENSE"><img src="https://img.shields.io/github/license/DouDOU-start/airgate-openai?style=flat-square" alt="license" /></a>
    <img src="https://img.shields.io/badge/Go-1.25-00ADD8?style=flat-square&logo=go" alt="go" />
    <img src="https://img.shields.io/badge/React-19-61DAFB?style=flat-square&logo=react" alt="react" />
  </p>
</div>

---

AirGate OpenAI 不是又一个"OpenAI 转发服务"，而是 [airgate-core](https://github.com/DouDOU-start/airgate-core) 的旗舰网关插件，也是 [airgate-sdk](https://github.com/DouDOU-start/airgate-sdk) 的官方参考实现。它在一个 gRPC 子进程里同时承载：

- **OpenAI Responses / Chat Completions API** 转发（Codex 核心端点）
- **ChatGPT OAuth 浏览器授权账号** 接入（PKCE + WebSocket 桥接）
- **Anthropic Messages API** 协议翻译（Claude → Responses 一步直转）

它解决一个具体问题：**用一套账号池同时服务 OpenAI、Codex CLI、Claude Code 三种客户端**，而不需要在 core 里为每一种协议各塞一份代码。插件可以**独立发版、独立 release、独立装卸、独立热更**，core 不重启、其他插件不受影响。

## ✨ 核心特性

- **🔌 双账号类型** — `apikey`（任何 Responses 兼容服务）与 `oauth`（浏览器登录 ChatGPT，自动刷新 token）同池调度，core 选好账号后插件按类型分发
- **🔄 Anthropic 协议翻译** — Claude 客户端的 `/v1/messages` 一步直转为 Responses API 请求，SSE 流再回译为 Anthropic 事件，工具调用 / 推理 token / stop_reason 全保留
- **🌐 双协议入口** — 同一个 `/v1/responses` 同时支持 HTTP/SSE 与 WebSocket；OAuth 账号走 WebSocket 上行，再以 SSE 写回客户端
- **🎯 模型降级与重试** — Anthropic 转发链路在模型不存在 / 被拒时自动降级到映射表里的下一个候选
- **🧠 上下文裁剪** — 历史消息超窗时按规则截断，避免上游 400
- **🪄 系统提示词预设** — 内置 default / simple / nsfw / cc 四套 Codex 提示词，按账号选择
- **💼 完整账号 Widget** — 自带前端账号表单（OAuth 引导面板、字段提示、状态展示），由 core 自动嵌入管理后台
- **📦 一键发版** — git tag 触发 release workflow，矩阵构建 4 个平台（linux/darwin × amd64/arm64），自动注入版本号、上传 sha256

## 🧩 接入位置

```text
                  ┌──────────────────────────────────────┐
                  │           AirGate Core               │
                  │   (账号 / 调度 / 计费 / 管理后台)     │
                  └────────────┬─────────────────────────┘
                               │ go-plugin (gRPC)
                               ▼
                  ┌──────────────────────────────────────┐
                  │      airgate-openai (本仓库)         │
                  │                                      │
                  │   ┌──────────┐    ┌──────────────┐   │
                  │   │ HTTP/SSE │    │  WebSocket   │   │
                  │   │   入口   │    │     入口     │   │
                  │   └────┬─────┘    └──────┬───────┘   │
                  │        ├─ apikey ────────┤           │
                  │        └─ oauth ─────────┤           │
                  │                          │           │
                  │     ┌────────────────────▼────────┐  │
                  │     │     Anthropic 协议翻译      │  │
                  │     │ (request convert / decode)  │  │
                  │     └──────────────┬──────────────┘  │
                  └────────────────────┼─────────────────┘
                                       ▼
                       OpenAI / ChatGPT / 兼容平台
```

**请求生命周期**：

```text
客户端请求 ──► Core 鉴权 ──► Core 选账号 ──► Plugin.Forward()
                                                  │
                                          ┌───────┴───────┐
                                          ▼               ▼
                                  Anthropic 翻译     原生 Responses
                                   (convert)         (forwardAPIKey
                                          │           / forwardOAuth)
                                          ▼               │
                                     上游 AI API ◄────────┘
                                          │
                                          ▼
                                     ForwardResult
                                  ┌───────┴───────┐
                              token 用量      账号状态反馈
                              Core 计费       Core 更新账号
```

`Forward()` 拿到 core 调度好的账号，识别请求是 Anthropic Messages 还是 OpenAI 原生，再分发到 `forwardAPIKey`（HTTP/SSE 直连）或 `forwardOAuth`（WebSocket 桥接）。

## 🚦 路由

由 `metadata.go` 声明、`Routes()` 返回，core 启动时自动注册到网关：

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/v1/responses` | Responses API（Codex 核心端点）|
| POST | `/v1/chat/completions` | Chat Completions API |
| POST | `/v1/messages` | Anthropic Messages API（协议翻译）|
| POST | `/v1/messages/count_tokens` | Anthropic Count Tokens（兼容回退）|
| GET  | `/v1/models` | 模型列表 |
| WS   | `/v1/responses` | Responses API（WebSocket）|

另外提供不带 `/v1` 前缀的别名路由（`POST /responses`、`POST /chat/completions`、`POST /messages`、`GET /models`、`WS /responses` 等），方便客户端直接填站点根地址。

## 🔑 账号类型

| Key | 标签 | 凭证字段 | 适用场景 |
|---|---|---|---|
| `apikey` | API Key | `api_key` + `base_url`（可选）| 所有提供 Responses 标准接口的服务 |
| `oauth`  | OAuth 登录 | `access_token` / `refresh_token` / `chatgpt_account_id`（授权后自动填充）| 浏览器登录 ChatGPT 个人账号 |

账号字段定义同样从 `metadata.go` 派生，通过 `Info()` 上报给 core，前端表单 Widget 自动渲染。

## 🛠 技术栈

| 层 | 技术 |
|---|---|
| 后端 | Go 1.25 · gRPC · gjson/sjson（零 struct）· nhooyr.io/websocket |
| 前端 | React 19 · Vite · TypeScript（账号表单 Widget） |
| 插件协议 | hashicorp/go-plugin (gRPC) |
| 上游协议 | OpenAI Responses / Chat Completions · ChatGPT WebSocket · Anthropic Messages |
| 发布 | GitHub Actions · 矩阵构建 4 平台二进制 · GitHub Release |

## 🚀 安装与开发

### 方式 1：安装到 core（推荐）

打开 core 管理后台 → **插件管理** → 三种方式任选：

```text
1. 插件市场 → 点击「安装」               （从 GitHub Release 自动拉取，匹配当前架构）
2. 上传安装 → 拖入二进制文件              （适合内部环境 / 自建二进制）
3. GitHub 安装 → 输入 DouDOU-start/airgate-openai
```

market 会**定时从 GitHub API 同步**最新 release（默认 6h，使用 ETag 不消耗 API 配额），新 tag push 后通常几分钟内即可在市场看到。

### 方式 2：源码运行（开发）

需要 Go 1.25+、Node 22+，以及兄弟目录 [`airgate-sdk`](https://github.com/DouDOU-start/airgate-sdk) 与 [`airgate-core`](https://github.com/DouDOU-start/airgate-core)：

```bash
git clone https://github.com/DouDOU-start/airgate-sdk.git
git clone https://github.com/DouDOU-start/airgate-core.git
git clone https://github.com/DouDOU-start/airgate-openai.git
cd airgate-openai

make install        # 装 web 依赖与 Go 模块
make build          # 完整构建：web/dist → backend/webdist → bin/gateway-openai
make manifest       # 重新生成 plugin.yaml
make ci             # 与 CI 完全一致的本地检查（lint + test + vet + build）
```

把本插件以 dev 模式挂到 core，热重载不重启 core：

```yaml
# airgate-core/backend/config.yaml
plugins:
  dev:
    - name: gateway-openai
      path: /absolute/path/to/airgate-openai/backend
```

然后 `cd airgate-core/backend && go run ./cmd/server`，core 会通过 `go run .` 启动本插件，握手 gRPC，依次调用 `Init → Start → RegisterRoutes`。

### 方式 3：不依赖 core 的端到端调试

```bash
cd backend && go run ./cmd/devserver   # 启动本地 devserver（模拟 core）
cd backend && go run ./cmd/chat        # 启动交互式测试客户端（SSE / WS 双协议）
```

更多命令见 `make help`。

## 🔄 Anthropic 协议翻译

```text
Anthropic JSON 请求
  → anthropic_convert.go    一步直转为 Responses API JSON（保留工具、reasoning、system）
  → anthropic_forward.go    转发到上游（含模型降级重试）
  → anthropic_response.go   Responses SSE → Anthropic SSE 回译
  → anthropic_model_map.go  Claude ↔ OpenAI 模型映射表
  → anthropic_util.go       工具名缩短、stop_reason 转换
```

设计上避开"先把 Anthropic 转成 Chat Completions 再转 Responses"的两步走 —— 直接一步转换，零中间结构体，全部用 gjson/sjson 操作 JSON。

## 🏗 项目结构

```text
airgate-openai/
├── backend/                              # Go 后端（插件主体）
│   ├── main.go                           # gRPC 插件入口
│   ├── cmd/
│   │   ├── chat/                         # 交互式测试客户端（SSE / WS 双协议）
│   │   ├── devserver/                    # 开发服务器（模拟 core）
│   │   └── genmanifest/                  # plugin.yaml 生成器
│   └── internal/
│       ├── gateway/                      # 网关核心逻辑
│       │   ├── gateway.go                # GatewayPlugin 接口实现
│       │   ├── metadata.go               # 插件元信息（运行时单源）
│       │   ├── forward.go                # 三模式转发分发 + apikey/oauth 转发
│       │   ├── anthropic_convert.go      # Anthropic → Responses 请求一步直转
│       │   ├── anthropic_response.go     # Responses → Anthropic 响应回译
│       │   ├── anthropic_forward.go      # Anthropic 转发入口、模型降级重试
│       │   ├── anthropic_model_map.go    # Claude ↔ OpenAI 模型映射
│       │   ├── anthropic_count_tokens.go # count_tokens 兼容回退
│       │   ├── anthropic_context_guard.go# Anthropic 历史裁剪
│       │   ├── request.go                # 请求检测、URL 构建、预处理
│       │   ├── stream.go                 # SSE 流式响应处理
│       │   ├── ws.go / ws_handler.go     # WebSocket 连接与事件解析
│       │   ├── transport_pool.go         # HTTP transport 复用池
│       │   ├── headers.go                # 认证头、白名单、Codex 标识
│       │   ├── oauth.go / oauth_handler.go # OAuth 授权流程（PKCE）
│       │   └── assets.go                 # WebAssetsProvider，embed webdist
│       ├── model/registry.go             # 集中模型规格定义（含单价）
│       └── resources/                    # 嵌入资源（系统提示词预设）
├── web/                                  # 前端（账号表单 Widget）
│   └── src/components/AccountForm.tsx
├── .github/workflows/
│   ├── ci.yml                            # push/PR 触发，复用 make ci
│   └── release.yml                       # v* tag 触发，矩阵构建 4 平台二进制
├── plugin.yaml                           # genmanifest 自动生成
└── Makefile
```

## 🔧 设计要点

- **`metadata.go` 是运行时真相**，`plugin.yaml` 仅为分发产物（`make manifest` 重生）。账号表单、路由、模型列表、依赖声明全部从 `metadata.go` 派生
- **零 struct JSON 处理** —— 全程使用 gjson/sjson；上游 schema 经常变，零 struct 让兼容性维护成本最低
- **Anthropic 翻译只做单向**：请求 Anthropic → Responses，响应 Responses → Anthropic，不引入第三种中间格式
- **transport 复用**：HTTP transport 由 `transport_pool` 按 base_url 分桶，避免每次都新建连接
- **版本单一来源**：`PluginVersion` 是 `var`，发版时由 `-ldflags` 在编译期注入 git tag —— git tag = release 版本 = 已安装 tab 显示的版本，永不偏离

## 📦 发版

正式发版**只需要打 git tag，不要手工改版本号字段**：

```bash
git tag v0.2.0
git push origin v0.2.0
```

[release.yml](.github/workflows/release.yml) 会自动：

1. 矩阵构建 4 个平台二进制（linux/darwin × amd64/arm64）
2. 通过 `-ldflags "-X .../gateway.PluginVersion=${version}"` 把 git tag（去掉 `v` 前缀）注入到二进制
3. 上传到 GitHub Release，资产命名 `gateway-openai-{os}-{arch}`，附带 `.sha256`
4. airgate-core 插件市场会通过 GitHub API 自动同步新版本

## 🤝 贡献 / 反馈

- Bug / Feature: [Issues](https://github.com/DouDOU-start/airgate-openai/issues)
- 主仓库: [airgate-core](https://github.com/DouDOU-start/airgate-core)
- 插件 SDK: [airgate-sdk](https://github.com/DouDOU-start/airgate-sdk)

## 📜 License

MIT — 详见 [LICENSE](LICENSE)。
