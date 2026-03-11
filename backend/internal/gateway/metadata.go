package gateway

import sdk "github.com/DouDOU-start/airgate-sdk"

//go:generate go run ../../cmd/genmanifest

const (
	PluginID             = "gateway-openai"
	PluginDisplayName    = "OpenAI 网关"
	PluginVersion        = "1.0.0"
	PluginDescription    = "OpenAI Responses API / Chat Completions 转发"
	PluginAuthor         = "airgate"
	PluginPlatform       = "openai"
	PluginMode           = "simple"
	PluginMinCoreVersion = "1.0.0"
)

func PluginDependencies() []string {
	return []string{}
}

func BuildPluginInfo() sdk.PluginInfo {
	return sdk.PluginInfo{
		ID:          PluginID,
		Name:        PluginDisplayName,
		Version:     PluginVersion,
		Description: PluginDescription,
		Author:      PluginAuthor,
		Type:        sdk.PluginTypeGateway,
		AccountTypes: []sdk.AccountType{
			{
				Key:         "apikey",
				Label:       "API Key",
				Description: "使用 OpenAI API Key 直连",
				Fields: []sdk.CredentialField{
					{Key: "api_key", Label: "API Key", Type: "password", Required: true, Placeholder: "sk-..."},
					{Key: "base_url", Label: "API 地址", Type: "text", Required: false, Placeholder: "https://api.openai.com"},
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
	}
}

func PluginRouteDefinitions() []sdk.RouteDefinition {
	return []sdk.RouteDefinition{
		{Method: "POST", Path: "/v1/responses", Description: "Responses API（Codex 核心端点）"},
		{Method: "POST", Path: "/v1/chat/completions", Description: "Chat Completions API"},
		{Method: "POST", Path: "/v1/messages", Description: "Anthropic Messages API（协议翻译）"},
		{Method: "GET", Path: "/v1/models", Description: "模型列表"},
		{Method: "WS", Path: "/v1/responses", Description: "Responses API（WebSocket）"},
	}
}
