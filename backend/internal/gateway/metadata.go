package gateway

import sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"

//go:generate go run ../../cmd/genmanifest

const (
	PluginID             = "gateway-openai"
	PluginDisplayName    = "OpenAI 网关"
	PluginDescription    = "OpenAI Responses API / Chat Completions 转发"
	PluginAuthor         = "airgate"
	PluginPlatform       = "openai"
	PluginMode           = "simple"
	PluginMinCoreVersion = "1.0.0"
)

// PluginVersion 插件版本号。
//
// 默认值是开发态版本，正式 release 构建时由 GitHub Actions 通过 ldflags 注入：
//
//	go build -ldflags "-X 'github.com/DouDOU-start/airgate-openai/backend/internal/gateway.PluginVersion=0.1.42'"
//
// 这样 git tag 即唯一发版来源，无需手动维护 plugin.yaml / metadata.go 里的版本字段。
var PluginVersion = "dev"

func PluginDependencies() []string {
	return []string{}
}

func BuildPluginInfo() sdk.PluginInfo {
	return sdk.PluginInfo{
		ID:          PluginID,
		Name:        PluginDisplayName,
		Version:     PluginVersion,
		SDKVersion:  sdk.SDKVersion,
		Description: PluginDescription,
		Author:      PluginAuthor,
		Type:        sdk.PluginTypeGateway,
		Capabilities: []sdk.Capability{
			sdk.CapabilityHostInvoke,
			sdk.CapabilityForHostMethod(hostMethodTasksCreate),
			sdk.CapabilityForHostMethod(hostMethodTasksUpdate),
			sdk.CapabilityForHostMethod(hostMethodTasksGet),
			sdk.CapabilityForHostMethod(hostMethodTasksList),
			sdk.CapabilityForHostMethod(hostMethodGatewayForward),
			sdk.CapabilityForHostMethod(hostMethodAssetsStore),
			sdk.CapabilityForHostMethod(hostMethodAssetsStoreURL),
		},
		Metadata: map[string]string{
			"account.oauth_plans": `[
				{"key":"free","label":"Free","credential_key":"plan_type","matches":["free"]},
				{"key":"plus","label":"Plus","credential_key":"plan_type","matches":["plus"]},
				{"key":"pro","label":"Pro","credential_key":"plan_type","matches":["pro"]}
			]`,
		},
		AccountTypes: []sdk.AccountType{
			{
				Key:         "apikey",
				Label:       "API Key",
				Description: "支持所有提供 Responses 标准接口的服务",
				Fields: []sdk.CredentialField{
					{Key: "api_key", Label: "API Key", Type: "password", Required: true, Placeholder: "sk-..."},
					{Key: "base_url", Label: "API 地址", Type: "text", Required: false, Placeholder: "https://api.openai.com"},
				},
			},
			{
				Key:         "oauth",
				Label:       "OAuth 登录",
				Description: "通过浏览器授权登录 ChatGPT 账号",
				Fields: []sdk.CredentialField{
					{Key: "access_token", Label: "Access Token", Type: "password", Required: false, Placeholder: "授权后自动填充", EditDisabled: true},
					{Key: "refresh_token", Label: "Refresh Token", Type: "password", Required: false, Placeholder: "授权后自动填充"},
					{Key: "session_token", Label: "Session Token (JWE)", Type: "password", Required: false, Placeholder: "Session 导入后自动填充"},
					{Key: "chatgpt_account_id", Label: "ChatGPT Account ID", Type: "text", Required: false, Placeholder: "授权后自动填充", EditDisabled: true},
				},
			},
		},
		FrontendWidgets: []sdk.FrontendWidget{
			{Slot: sdk.SlotAccountIdentity, EntryFile: "index.js", Title: "OpenAI 账号身份"},
			{Slot: sdk.SlotAccountCreate, EntryFile: "index.js", Title: "创建 OpenAI 账号"},
			{Slot: sdk.SlotAccountEdit, EntryFile: "index.js", Title: "编辑 OpenAI 账号"},
			{Slot: sdk.SlotAccountUsageWindow, EntryFile: "index.js", Title: "账号用量窗口"},
			{Slot: sdk.SlotUsageMetricDetail, EntryFile: "index.js", Title: "OpenAI 计量明细"},
			{Slot: sdk.SlotUsageCostDetail, EntryFile: "index.js", Title: "OpenAI 费用明细"},
		},
		InstructionPresets: []string{"default", "simple", "nsfw", "cc"},
	}
}

func PluginRouteDefinitions() []sdk.RouteDefinition {
	return []sdk.RouteDefinition{
		{Method: "POST", Path: "/v1/responses", Description: "Responses API（Codex 核心端点）"},
		{Method: "POST", Path: "/v1/chat/completions", Description: "Chat Completions API"},
		{Method: "POST", Path: "/v1/messages", Description: "Anthropic Messages API（协议翻译）", Metadata: anthropicRouteMetadata()},
		{Method: "POST", Path: "/v1/messages/count_tokens", Description: "Anthropic Count Tokens（兼容回退）", Metadata: anthropicRouteMetadata()},
		{Method: "GET", Path: "/v1/models", Description: "模型列表", Metadata: map[string]string{"metadata_only": "true"}},
		{Method: "POST", Path: "/v1/images/generations", Description: "Images API（文生图）"},
		{Method: "POST", Path: "/v1/images/edits", Description: "Images API（图生图 / 编辑）"},
		{Method: "GET", Path: "/v1/images/tasks", Description: "Images Task 状态查询", Metadata: map[string]string{"metadata_only": "true"}},
		{Method: "GET", Path: "/v1/images/tasks/list", Description: "Images Task 历史列表", Metadata: map[string]string{"metadata_only": "true"}},
		{Method: "WS", Path: "/v1/responses", Description: "Responses API（WebSocket）"},
		// 不带 /v1 前缀的别名路由，方便用户配置时直接使用站点根地址
		{Method: "POST", Path: "/responses", Description: "Responses API（无 /v1 前缀）"},
		{Method: "POST", Path: "/chat/completions", Description: "Chat Completions API（无 /v1 前缀）"},
		{Method: "POST", Path: "/messages", Description: "Anthropic Messages API（无 /v1 前缀）", Metadata: anthropicRouteMetadata()},
		{Method: "POST", Path: "/messages/count_tokens", Description: "Anthropic Count Tokens（无 /v1 前缀）", Metadata: anthropicRouteMetadata()},
		{Method: "GET", Path: "/models", Description: "模型列表（无 /v1 前缀）", Metadata: map[string]string{"metadata_only": "true"}},
		{Method: "POST", Path: "/images/generations", Description: "Images API（文生图，无 /v1 前缀）"},
		{Method: "POST", Path: "/images/edits", Description: "Images API（图生图，无 /v1 前缀）"},
		{Method: "GET", Path: "/images/tasks", Description: "Images Task 状态查询（无 /v1 前缀）", Metadata: map[string]string{"metadata_only": "true"}},
		{Method: "GET", Path: "/images/tasks/list", Description: "Images Task 历史列表（无 /v1 前缀）", Metadata: map[string]string{"metadata_only": "true"}},
		{Method: "WS", Path: "/responses", Description: "Responses API WebSocket（无 /v1 前缀）"},
	}
}
