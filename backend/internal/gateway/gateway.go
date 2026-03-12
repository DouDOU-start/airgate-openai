package gateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"strings"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk"

	"github.com/DouDOU-start/airgate-openai/backend/internal/model"
)

// OpenAIGateway OpenAI 网关插件（SimpleGatewayPlugin 实现）
// 核心自动处理调度、计费、限流、并发控制，插件只负责转发
type OpenAIGateway struct {
	logger *slog.Logger
	ctx    sdk.PluginContext
}

func (g *OpenAIGateway) Info() sdk.PluginInfo {
	return BuildPluginInfo()
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
	return PluginPlatform
}

func (g *OpenAIGateway) Models() []sdk.ModelInfo {
	return model.AllSpecs()
}

func (g *OpenAIGateway) Routes() []sdk.RouteDefinition {
	return PluginRouteDefinitions()
}

func (g *OpenAIGateway) Forward(ctx context.Context, req *sdk.ForwardRequest) (*sdk.ForwardResult, error) {
	return g.forwardHTTP(ctx, req)
}

// ValidateAccount 验证凭证有效性
func (g *OpenAIGateway) ValidateAccount(ctx context.Context, credentials map[string]string) error {
	apiKey := credentials["api_key"]
	accessToken := credentials["access_token"]

	if apiKey == "" && accessToken == "" {
		return fmt.Errorf("缺少 api_key 或 access_token")
	}

	// API Key 模式：调用 /v1/models 验证
	if apiKey != "" {
		account := &sdk.Account{Credentials: credentials}
		targetURL := buildAPIKeyURL(account, "/v1/models")
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
		if err != nil {
			return fmt.Errorf("构建验证请求失败: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)

		client := g.buildHTTPClient(account)
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("验证请求失败: %w", err)
		}
		defer func() {
			_ = resp.Body.Close()
		}()

		if resp.StatusCode == 401 {
			return fmt.Errorf("API Key 无效")
		}
		if resp.StatusCode >= 400 {
			return fmt.Errorf("API Key 验证失败: HTTP %d", resp.StatusCode)
		}
		return nil
	}

	// OAuth 模式：access_token 非空即通过
	return nil
}

// QueryQuota 查询账号额度
// OAuth 账号：刷新 token 并从 id_token 中提取订阅信息
// API Key 账号：不支持额度查询
func (g *OpenAIGateway) QueryQuota(ctx context.Context, credentials map[string]string) (*sdk.QuotaInfo, error) {
	refreshToken := credentials["refresh_token"]
	if refreshToken == "" {
		return nil, sdk.ErrNotSupported
	}

	// 用 refresh_token 换取新的 token 组，从中获取最新订阅状态
	tokens, err := g.refreshTokens(ctx, refreshToken, credentials["proxy_url"])
	if err != nil {
		return nil, fmt.Errorf("刷新 token 失败: %w", err)
	}

	info := parseIDToken(tokens.IDToken)

	extra := map[string]string{}
	if info.PlanType != "" {
		extra["plan_type"] = info.PlanType
	}
	if info.AccountID != "" {
		extra["chatgpt_account_id"] = info.AccountID
	}
	if info.AccountName != "" {
		extra["account_name"] = info.AccountName
	}
	// 将刷新后的 token 也放入 extra，供调用方更新存储
	if tokens.AccessToken != "" {
		extra["access_token"] = tokens.AccessToken
	}
	if tokens.RefreshToken != "" {
		extra["refresh_token"] = tokens.RefreshToken
	}

	return &sdk.QuotaInfo{
		ExpiresAt: info.SubscriptionActiveUntil,
		Extra:     extra,
	}, nil
}

// ProbeUsage 主动探测账号的 Codex 用量
// OAuth 账号：建立 WebSocket 连接发送最小请求，等待 codex.rate_limits 事件
// API Key 账号：发送 GET /v1/models 捕获响应头
func (g *OpenAIGateway) ProbeUsage(ctx context.Context, accountID int64, credentials map[string]string) *CodexUsageSnapshot {
	if credentials["access_token"] != "" {
		return g.probeOAuthUsage(ctx, accountID, credentials)
	}
	return g.probeAPIKeyUsage(ctx, accountID, credentials)
}

// probeAPIKeyUsage 通过 GET /v1/models 探测 API Key 账号用量
func (g *OpenAIGateway) probeAPIKeyUsage(ctx context.Context, accountID int64, credentials map[string]string) *CodexUsageSnapshot {
	account := &sdk.Account{ID: accountID, Credentials: credentials}
	targetURL := buildAPIKeyURL(account, "/v1/models")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil
	}
	setAuthHeaders(req, account)

	client := g.buildHTTPClient(account)
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	snapshot := parseCodexUsageFromHeaders(resp.Header)
	if snapshot != nil {
		StoreCodexUsage(accountID, snapshot)
	}
	return snapshot
}

// probeOAuthUsage 通过 SSE HTTP POST 到 ChatGPT Codex 端点探测 OAuth 账号用量
// 复用 buildAnthropicUpstreamRequest 相同的请求构建模式（SSE 而非 WebSocket）
func (g *OpenAIGateway) probeOAuthUsage(ctx context.Context, accountID int64, credentials map[string]string) *CodexUsageSnapshot {
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	probeBody := []byte(`{"model":"gpt-5.2","instructions":"reply ok","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"store":false,"stream":true}`)

	// 构建 HTTP POST 请求到 SSE 端点（与 buildAnthropicUpstreamRequest OAuth 模式一致）
	req, err := http.NewRequestWithContext(probeCtx, http.MethodPost, ChatGPTSSEURL, bytes.NewReader(probeBody))
	if err != nil {
		log.Printf("[ProbeUsage] OAuth 账号 %d: 构建请求失败: %v", accountID, err)
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+credentials["access_token"])
	req.Header.Set("OpenAI-Beta", SSEBetaHeader)
	req.Header.Set("Originator", "codex_cli_rs")
	if aid := credentials["chatgpt_account_id"]; aid != "" {
		req.Header.Set("ChatGPT-Account-ID", aid)
	}

	account := &sdk.Account{ID: accountID, Credentials: credentials, ProxyURL: credentials["proxy_url"]}
	client := g.buildHTTPClient(account)
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[ProbeUsage] OAuth 账号 %d: 请求失败: %v", accountID, err)
		return nil
	}
	defer func() { _ = resp.Body.Close() }()

	if snapshot := parseCodexUsageFromHeaders(resp.Header); snapshot != nil {
		StoreCodexUsage(accountID, snapshot)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("[ProbeUsage] OAuth 账号 %d: HTTP %d, body=%s", accountID, resp.StatusCode, truncate(string(body), 200))
		return GetCodexUsage(accountID)
	}

	// 读取 SSE 流，从 codex.rate_limits 事件中捕获用量
	buf := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			for _, line := range splitSSELines(string(buf[:n])) {
				if snapshot := parseCodexUsageFromSSEEvent([]byte(line)); snapshot != nil {
					StoreCodexUsage(accountID, snapshot)
				}
			}
		}
		if readErr != nil {
			break
		}
	}

	return GetCodexUsage(accountID)
}

// splitSSELines 从 SSE chunk 中提取 data: 行的内容
func splitSSELines(chunk string) []string {
	var results []string
	for _, line := range strings.Split(chunk, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data: ") {
			results = append(results, strings.TrimPrefix(line, "data: "))
		}
	}
	return results
}
