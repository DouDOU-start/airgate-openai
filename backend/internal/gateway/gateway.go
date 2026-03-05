package gateway

import (
	"context"
	"log/slog"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

// OpenAIGateway OpenAI 网关插件（SimpleGatewayPlugin 实现）
// 核心自动处理调度、计费、限流、并发控制，插件只负责转发
type OpenAIGateway struct {
	logger *slog.Logger
	ctx    sdk.PluginContext
}

func (g *OpenAIGateway) Info() sdk.PluginInfo {
	return sdk.PluginInfo{
		ID:          "gateway-openai",
		Name:        "OpenAI 网关",
		Version:     "1.0.0",
		Description: "OpenAI Responses API / Chat Completions 转发",
		Author:      "airgate",
		Type:        sdk.PluginTypeGateway,
		AccountTypes: []sdk.AccountType{
			{
				Key:         "apikey",
				Label:       "API Key",
				Description: "使用 OpenAI API Key 直连",
				Fields: []sdk.CredentialField{
					{Key: "api_key", Label: "API Key", Type: "password", Required: true, Placeholder: "sk-..."},
				},
			},
			{
				Key:         "sub2api",
				Label:       "Sub2API",
				Description: "通过 sub2api API Key 转发（仅 Responses 协议）",
				Fields: []sdk.CredentialField{
					{Key: "api_key", Label: "API Key", Type: "password", Required: true, Placeholder: "sk-..."},
					{Key: "base_url", Label: "API 地址", Type: "text", Required: false, Placeholder: "https://sub2api.xxxx.com"},
					{Key: "provider", Label: "Provider", Type: "text", Required: false, Placeholder: "sub2api"},
				},
			},
			{
				Key:         "oauth",
				Label:       "OAuth 登录",
				Description: "通过浏览器授权登录 ChatGPT 账号",
				Fields: []sdk.CredentialField{
					{Key: "access_token", Label: "Access Token", Type: "password", Required: false, Placeholder: "授权后自动填充"},
					{Key: "refresh_token", Label: "Refresh Token", Type: "password", Required: false, Placeholder: "授权后自动填充"},
					{Key: "chatgpt_account_id", Label: "ChatGPT Account ID", Type: "text", Required: false, Placeholder: "授权后自动填充"},
				},
			},
		},
		ConfigFields: []sdk.ConfigField{
			{Key: "default_timeout", Type: "duration", Default: "300s", Description: "默认请求超时"},
		},
	}
}

func (g *OpenAIGateway) Init(ctx sdk.PluginContext) error {
	g.ctx = ctx
	if ctx != nil {
		g.logger = ctx.Logger()
	}
	if g.logger == nil {
		g.logger = slog.Default()
	}
	g.logger.Info("OpenAI 网关插件初始化")
	return nil
}

func (g *OpenAIGateway) Start(_ context.Context) error {
	g.logger.Info("OpenAI 网关插件启动")
	return nil
}

func (g *OpenAIGateway) Stop(_ context.Context) error {
	g.logger.Info("OpenAI 网关插件停止")
	return nil
}

func (g *OpenAIGateway) Platform() string {
	return "openai"
}

func (g *OpenAIGateway) Models() []sdk.ModelInfo {
	return []sdk.ModelInfo{
		// Codex 系列
		{ID: "codex-mini-latest", Name: "Codex Mini", MaxTokens: 128000, InputPrice: 1.5, OutputPrice: 6.0},
		// GPT 系列
		{ID: "gpt-4.1", Name: "GPT-4.1", MaxTokens: 1047576, InputPrice: 2.0, OutputPrice: 8.0},
		{ID: "gpt-4.1-mini", Name: "GPT-4.1 Mini", MaxTokens: 1047576, InputPrice: 0.4, OutputPrice: 1.6},
		{ID: "gpt-4.1-nano", Name: "GPT-4.1 Nano", MaxTokens: 1047576, InputPrice: 0.1, OutputPrice: 0.4},
		{ID: "gpt-4o", Name: "GPT-4o", MaxTokens: 128000, InputPrice: 2.5, OutputPrice: 10.0},
		{ID: "gpt-4o-mini", Name: "GPT-4o Mini", MaxTokens: 128000, InputPrice: 0.15, OutputPrice: 0.6},
		// o 系列推理模型
		{ID: "o3", Name: "o3", MaxTokens: 200000, InputPrice: 10.0, OutputPrice: 40.0},
		{ID: "o3-pro", Name: "o3 Pro", MaxTokens: 200000, InputPrice: 20.0, OutputPrice: 80.0},
		{ID: "o4-mini", Name: "o4-mini", MaxTokens: 200000, InputPrice: 1.1, OutputPrice: 4.4},
		{ID: "o3-mini", Name: "o3-mini", MaxTokens: 200000, InputPrice: 1.1, OutputPrice: 4.4},
		// 图像模型
		{ID: "gpt-image-1", Name: "GPT Image 1", MaxTokens: 0, InputPrice: 5.0, OutputPrice: 40.0},
	}
}

func (g *OpenAIGateway) Routes() []sdk.RouteDefinition {
	return []sdk.RouteDefinition{
		{Method: "POST", Path: "/v1/responses", Description: "Responses API（Codex 核心端点）"},
		{Method: "POST", Path: "/v1/chat/completions", Description: "Chat Completions API"},
		{Method: "POST", Path: "/v1/messages", Description: "Anthropic Messages API（协议翻译）"},
		{Method: "GET", Path: "/v1/models", Description: "模型列表"},
		{Method: "WS", Path: "/v1/responses", Description: "Responses API（WebSocket）"},
	}
}

func (g *OpenAIGateway) Forward(ctx context.Context, req *sdk.ForwardRequest) (*sdk.ForwardResult, error) {
	return g.forwardHTTP(ctx, req)
}
