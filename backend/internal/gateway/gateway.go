package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk"

	"github.com/DouDOU-start/airgate-openai/backend/internal/model"
)

// OpenAIGateway OpenAI 网关插件（SimpleGatewayPlugin 实现）
// 核心自动处理调度、计费、限流、并发控制，插件只负责转发
type OpenAIGateway struct {
	logger        *slog.Logger
	ctx           sdk.PluginContext
	snapshotStore *codexUsagePersistenceStore
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
	if ctx != nil && ctx.Config() != nil {
		if dsn := ctx.Config().GetString("db_dsn"); dsn != "" {
			store, err := newCodexUsagePersistenceStore(dsn, PluginID, g.logger)
			if err != nil {
				g.logger.Warn("初始化 Codex 用量快照持久化失败，将退回内存缓存", "error", err)
			} else {
				g.snapshotStore = store
				setCodexUsagePersistenceStore(store)
				if err := store.WarmCache(context.Background()); err != nil {
					g.logger.Warn("预热 Codex 用量快照缓存失败", "error", err)
				}
			}
		}
	}
	g.logger.Info("OpenAI 网关插件初始化")
	return nil
}

func (g *OpenAIGateway) Start(_ context.Context) error {
	g.logger.Info("OpenAI 网关插件启动")
	return nil
}

func (g *OpenAIGateway) Stop(_ context.Context) error {
	if g.snapshotStore != nil {
		setCodexUsagePersistenceStore(nil)
		if err := g.snapshotStore.Close(); err != nil {
			g.logger.Warn("关闭 Codex 用量快照持久化失败", "error", err)
		}
		g.snapshotStore = nil
	}
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
		if err := g.validateAPIKeyViaModels(ctx, account, apiKey); err != nil {
			g.logger.Warn("OpenAI API Key /v1/models 验证失败，尝试回退 /v1/responses", "error", err)
			if fallbackErr := g.validateAPIKeyViaResponses(ctx, account, apiKey); fallbackErr != nil {
				return fmt.Errorf("验证请求失败: models=%v; responses=%v", err, fallbackErr)
			}
		}
		return nil
	}

	// OAuth 模式：access_token 非空即通过
	return nil
}

func (g *OpenAIGateway) validateAPIKeyViaModels(ctx context.Context, account *sdk.Account, apiKey string) error {
	targetURL := buildAPIKeyURL(account, "/v1/models")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return fmt.Errorf("构建 /v1/models 验证请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := g.buildHTTPClient(account)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("API Key 无效")
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("/v1/models 返回 HTTP %d", resp.StatusCode)
	}
	return nil
}

func (g *OpenAIGateway) validateAPIKeyViaResponses(ctx context.Context, account *sdk.Account, apiKey string) error {
	targetURL := buildAPIKeyURL(account, "/v1/responses")
	body, err := json.Marshal(map[string]any{
		"model":             "gpt-5.4",
		"input":             "Reply with just ok.",
		"max_output_tokens": 8,
	})
	if err != nil {
		return fmt.Errorf("构建 /v1/responses 验证请求体失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("构建 /v1/responses 验证请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := g.buildHTTPClient(account)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("API Key 无效")
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return fmt.Errorf("/v1/responses 不可用: HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode >= 500 {
		return fmt.Errorf("/v1/responses 返回 HTTP %d", resp.StatusCode)
	}
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
	if info.Email != "" {
		extra["email"] = info.Email
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
	// 401/403 标记为凭证错误
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("[ProbeUsage] API Key 账号 %d: HTTP %d, body=%s", accountID, resp.StatusCode, truncate(string(body), 200))
		StoreProbeError(accountID, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body)))
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
		// 401/403 标记为凭证错误，存入 probe error 缓存供 HandleRequest 查询
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			StoreProbeError(accountID, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body)))
		}
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

// HandleRequest 处理 Core 透传的自定义请求（实现 sdk.RequestHandler 接口）
func (g *OpenAIGateway) HandleRequest(ctx context.Context, _, path, _ string, _ http.Header, body []byte) (int, http.Header, []byte, error) {
	switch path {
	case "usage/accounts":
		var accounts []struct {
			ID          int64             `json:"id"`
			Credentials map[string]string `json:"credentials"`
		}
		if err := json.Unmarshal(body, &accounts); err != nil {
			return http.StatusBadRequest, nil, jsonError("invalid request body"), nil
		}

		appendNormalizedWindow := func(
			windows []sdk.AccountUsageWindow,
			key string,
			label string,
			usedPercent *float64,
			resetAfterSeconds *int,
			base time.Time,
			now time.Time,
		) []sdk.AccountUsageWindow {
			if usedPercent == nil && resetAfterSeconds == nil {
				return windows
			}
			used := 0.0
			if usedPercent != nil {
				used = *usedPercent
			}
			var resetAt *time.Time
			if resetAfterSeconds != nil {
				resetAt = sdk.ResetAtFromBase(base, *resetAfterSeconds)
			}
			return append(windows, sdk.NewAccountUsageWindow(key, label, used, resetAt, now))
		}

		buildUsageWindows := func(snapshot *CodexUsageSnapshot, limitName string, now time.Time) []sdk.AccountUsageWindow {
			windows := make([]sdk.AccountUsageWindow, 0, 4)
			if normalized := snapshot.Normalize(); normalized != nil {
				windows = appendNormalizedWindow(
					windows, "5h", "5h", normalized.Used5hPercent, normalized.Reset5hSeconds, snapshot.CapturedAt, now,
				)
				windows = appendNormalizedWindow(
					windows, "7d", "7d", normalized.Used7dPercent, normalized.Reset7dSeconds, snapshot.CapturedAt, now,
				)
			}

			hasModelWindows := hasCodexWindowData(
				snapshot.BengalfoxPrimaryUsedPercent,
				snapshot.BengalfoxPrimaryResetAfterSeconds,
				snapshot.BengalfoxPrimaryWindowMinutes,
			) || hasCodexWindowData(
				snapshot.BengalfoxSecondaryUsedPercent,
				snapshot.BengalfoxSecondaryResetAfterSeconds,
				snapshot.BengalfoxSecondaryWindowMinutes,
			)
			if hasModelWindows {
				if limitName == "" {
					limitName = "spark"
				}
				modelSnapshot := &CodexUsageSnapshot{
					PrimaryUsedPercent:         snapshot.BengalfoxPrimaryUsedPercent,
					PrimaryResetAfterSeconds:   snapshot.BengalfoxPrimaryResetAfterSeconds,
					PrimaryWindowMinutes:       snapshot.BengalfoxPrimaryWindowMinutes,
					SecondaryUsedPercent:       snapshot.BengalfoxSecondaryUsedPercent,
					SecondaryResetAfterSeconds: snapshot.BengalfoxSecondaryResetAfterSeconds,
					SecondaryWindowMinutes:     snapshot.BengalfoxSecondaryWindowMinutes,
					CapturedAt:                 snapshot.CapturedAt,
				}
				if normalized := modelSnapshot.Normalize(); normalized != nil {
					windows = appendNormalizedWindow(
						windows,
						"model:5h:"+limitName,
						"5h "+limitName,
						normalized.Used5hPercent,
						normalized.Reset5hSeconds,
						snapshot.CapturedAt,
						now,
					)
					windows = appendNormalizedWindow(
						windows,
						"model:7d:"+limitName,
						"7d "+limitName,
						normalized.Used7dPercent,
						normalized.Reset7dSeconds,
						snapshot.CapturedAt,
						now,
					)
				}
			}

			return windows
		}

		result := make(map[string]sdk.AccountUsageInfo)
		var probeErrors []sdk.AccountUsageError
		now := time.Now()
		for _, a := range accounts {
			// 检查是否有探测时发现的凭证错误
			if errMsg := GetProbeError(a.ID); errMsg != "" {
				probeErrors = append(probeErrors, sdk.AccountUsageError{ID: a.ID, Message: errMsg})
			}
			snapshot := GetCodexUsage(a.ID)
			if snapshot == nil {
				snapshot = g.ProbeUsage(ctx, a.ID, a.Credentials)
			}
			// 再次检查（ProbeUsage 刚产生的错误）
			if errMsg := GetProbeError(a.ID); errMsg != "" {
				probeErrors = append(probeErrors, sdk.AccountUsageError{ID: a.ID, Message: errMsg})
			}
			if snapshot == nil {
				continue
			}

			usage := sdk.AccountUsageInfo{
				UpdatedAt: snapshot.CapturedAt.UTC().Format(time.RFC3339),
				Windows:   buildUsageWindows(snapshot, snapshot.LimitName, now),
			}
			// Credits
			if snapshot.CreditsHasCredits {
				usage.Credits = &sdk.AccountUsageCredits{
					Balance:   snapshot.CreditsBalance,
					Unlimited: snapshot.CreditsUnlimited,
				}
			}

			if len(usage.Windows) > 0 || usage.Credits != nil {
				result[strconv.FormatInt(a.ID, 10)] = usage
			}
		}
		return http.StatusOK, nil, jsonMarshal(sdk.AccountUsageAccountsResponse{
			Accounts: result,
			Errors:   probeErrors,
		}), nil

	case "oauth/start":
		resp, err := g.StartOAuth(context.Background(), &OAuthStartRequest{})
		if err != nil {
			return http.StatusInternalServerError, nil, jsonError(err.Error()), nil
		}
		return http.StatusOK, nil, jsonMarshal(map[string]string{
			"authorize_url": resp.AuthorizeURL,
			"state":         resp.State,
		}), nil

	case "oauth/exchange":
		var raw struct {
			CallbackURL string `json:"callback_url"`
		}
		if err := json.Unmarshal(body, &raw); err != nil || raw.CallbackURL == "" {
			return http.StatusBadRequest, nil, jsonError("缺少 callback_url 参数"), nil
		}
		parsed, err := url.Parse(raw.CallbackURL)
		if err != nil {
			return http.StatusBadRequest, nil, jsonError("callback_url 格式无效"), nil
		}
		code := parsed.Query().Get("code")
		state := parsed.Query().Get("state")
		if code == "" || state == "" {
			return http.StatusBadRequest, nil, jsonError("callback_url 缺少 code 或 state 参数"), nil
		}
		result, err := g.HandleOAuthCallback(context.Background(), &OAuthCallbackRequest{
			Code:  code,
			State: state,
		})
		if err != nil {
			return http.StatusInternalServerError, nil, jsonError(err.Error()), nil
		}
		return http.StatusOK, nil, jsonMarshal(map[string]interface{}{
			"account_type": result.AccountType,
			"credentials":  result.Credentials,
			"account_name": result.AccountName,
		}), nil

	default:
		return http.StatusNotFound, nil, jsonError("未知的操作: " + path), nil
	}
}

func jsonError(msg string) []byte {
	b, _ := json.Marshal(map[string]string{"error": msg})
	return b
}

func jsonMarshal(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
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
