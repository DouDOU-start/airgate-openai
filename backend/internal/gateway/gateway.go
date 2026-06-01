package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"

	"github.com/DouDOU-start/airgate-openai/backend/internal/model"
)

// OpenAIGateway OpenAI 网关插件（SimpleGatewayPlugin 实现）
// 核心处理鉴权、账号选择、计费、限流和并发控制，插件只负责协议适配与上游转发。
type OpenAIGateway struct {
	logger        *slog.Logger
	ctx           sdk.PluginContext
	host          sdk.Host
	snapshotStore *codexUsagePersistenceStore
	transportPool *TransportPool
	tasks         *TaskRegistry
}

func (g *OpenAIGateway) Info() sdk.PluginInfo {
	return BuildPluginInfo()
}

func (g *OpenAIGateway) Init(ctx sdk.PluginContext) error {
	g.ctx = ctx
	g.transportPool = NewTransportPool()
	g.tasks = NewTaskRegistry()
	g.tasks.Register(imageGenerateHandler{})
	g.tasks.Register(imageEditHandler{})
	if ctx != nil {
		g.logger = ctx.Logger()
	}
	if g.logger == nil {
		g.logger = slog.Default()
	}
	if hostAware, ok := ctx.(sdk.HostAware); ok {
		g.host = hostAware.Host()
	}
	if dsn := sdk.GetPluginDSN(ctx); dsn != "" {
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
	g.logger.Info("OpenAI 网关插件初始化")
	return nil
}

func (g *OpenAIGateway) Start(_ context.Context) error {
	g.logger.Info("OpenAI 网关插件启动")
	return nil
}

func (g *OpenAIGateway) Stop(_ context.Context) error {
	if g.transportPool != nil {
		g.transportPool.CloseIdle()
	}
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
	return model.AllModels()
}

func (g *OpenAIGateway) Routes() []sdk.RouteDefinition {
	return PluginRouteDefinitions()
}

func (g *OpenAIGateway) Forward(ctx context.Context, req *sdk.ForwardRequest) (sdk.ForwardOutcome, error) {
	// 抽取/生成 request_id 并派生请求级 logger，注入 ctx 供下游使用
	rid := sdk.ExtractOrGenerateRequestID(req.Headers)
	logger := sdk.LoggerFromContext(ctx).With(sdk.LogFieldRequestID, rid)
	if logger == nil {
		logger = g.logger.With(sdk.LogFieldRequestID, rid)
	}
	ctx = sdk.WithLogger(ctx, logger)
	ctx = sdk.WithRequestID(ctx, rid)

	method, path := resolveAPIKeyRoute(req)
	logger.Debug("plugin_request_received",
		sdk.LogFieldMethod, method,
		sdk.LogFieldPath, path,
		sdk.LogFieldModel, req.Model,
		"stream", req.Stream,
	)

	// 诊断：Info 级别打印代理状态，让运维 / 开发者直观确认绑定代理是否到了插件这一层。
	// proxy_target 已 redact 掉 user:pass，仅保留 protocol://host:port。
	accountID := int64(0)
	if req.Account != nil {
		accountID = req.Account.ID
	}
	proxyURL := ""
	if req.Account != nil {
		proxyURL = req.Account.ProxyURL
	}
	logger.Info("forward_proxy_resolved",
		sdk.LogFieldAccountID, accountID,
		sdk.LogFieldModel, req.Model,
		"via_proxy", proxyURL != "",
		"proxy_target", redactProxyURL(proxyURL),
	)

	return g.forwardHTTP(ctx, req)
}

// redactProxyURL 去掉 proxy URL 中的 user:pass，避免日志泄露代理凭证。
// 解析失败时返回 "<invalid>"，空串返回空串。
func redactProxyURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid>"
	}
	u.User = nil
	return u.String()
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

// ReauthRequiredPrefix 是 QueryQuota 在 refresh_token 失效且无法本地降级时返回的错误前缀。
// Core 侧按前缀匹配以映射为"需要重新授权"的领域错误（gRPC 会保留 Go error 的字符串）。
const ReauthRequiredPrefix = "reauth_required: "

// QueryQuota 查询账号额度
// OAuth 账号：优先用 refresh_token 刷新；refresh_token 为空但 session_token 存在
// 时走 chatgpt.com session 端点刷新；两者都失败则降级解析存储的 access_token。
// API Key 账号：不支持额度查询
func (g *OpenAIGateway) QueryQuota(ctx context.Context, credentials map[string]string) (*quotaInfo, error) {
	refreshToken := credentials["refresh_token"]
	sessionToken := credentials["session_token"]
	proxyURL := credentials["proxy_url"]

	if refreshToken == "" {
		// 没有 RT：优先尝试 session 端点刷新；失败再降级到存储的 access_token。
		if sessionToken != "" {
			if info, err := g.quotaInfoViaSession(ctx, sessionToken, credentials, proxyURL); err == nil {
				return info, nil
			} else if access := credentials["access_token"]; access != "" {
				return g.quotaInfoFromAccessToken(ctx, access, credentials, "session_refresh_failed: "+err.Error())
			} else {
				return nil, fmt.Errorf("%ssession_token 刷新失败且无 access_token 可用 (原因: %s)", ReauthRequiredPrefix, err.Error())
			}
		}
		if access := credentials["access_token"]; access != "" {
			return g.quotaInfoFromAccessToken(ctx, access, credentials, "")
		}
		return nil, sdk.ErrNotSupported
	}

	// 用 refresh_token 换取新的 token 组，从中获取最新订阅状态
	clientID := credentials["client_id"]
	tokens, err := g.refreshTokens(ctx, refreshToken, proxyURL, clientID)
	if err != nil {
		// refresh_token 失效，按优先级依次降级：
		//   1. session_token 还在 → 走 chatgpt.com session 端点
		//   2. access_token 还在 → 不验签解析 JWT claims（数据可能陈旧）
		//   3. 都没有 → 视为彻底失效
		// 历史上这里要求 claims 里必须有 plan_type 才降级，导致 JWT claims 被
		// 精简（只含 sub / exp 等）的账号直接触发 ErrReauthRequired，前端弹出
		// "需要重新授权"，其实账号还能正常服务请求。现在放宽：有 access_token
		// 就成功返回，带上 refresh_warning 标记数据陈旧即可。
		if sessionToken != "" {
			if info, sessErr := g.quotaInfoViaSession(ctx, sessionToken, credentials, proxyURL); sessErr == nil {
				return info, nil
			}
		}
		if access := credentials["access_token"]; access != "" {
			return g.quotaInfoFromAccessToken(ctx, access, credentials, "refresh_token_invalid: "+err.Error())
		}
		return nil, fmt.Errorf("%srefresh_token 已失效，请重新授权 OAuth (原因: %s)", ReauthRequiredPrefix, err.Error())
	}

	info := g.enrichTokenInfo(ctx, parseTokenInfo(tokens.IDToken, tokens.AccessToken), tokens.AccessToken, credentials["proxy_url"])

	extra := map[string]string{
		"plan_type":                 info.PlanType,
		"subscription_active_until": info.SubscriptionActiveUntil,
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

	return &quotaInfo{
		ExpiresAt: info.SubscriptionActiveUntil,
		Extra:     extra,
	}, nil
}

// quotaInfoViaSession 走 chatgpt.com /api/auth/session 拉一份新 session，
// 把 accessToken / session_token / 元信息回写到 extra，供调用方持久化。
func (g *OpenAIGateway) quotaInfoViaSession(ctx context.Context, sessionToken string, credentials map[string]string, proxyURL string) (*quotaInfo, error) {
	sess, err := g.refreshViaSession(ctx, sessionToken, proxyURL)
	if err != nil {
		return nil, err
	}
	// 在 session 拿到的 AT 之上再做一次 enrichTokenInfo，把订阅有效期补全
	// （session.expires 字段是 session 过期时间，不是订阅到期时间）。
	info := g.enrichTokenInfo(ctx, parseTokenInfo("", sess.AccessToken), sess.AccessToken, proxyURL)
	if info.Email == "" {
		info.Email = sess.User.Email
	}
	if info.AccountID == "" {
		info.AccountID = sess.Account.ID
	}
	if info.PlanType == "" {
		info.PlanType = sess.Account.PlanType
	}
	if info.AccountName == "" {
		if sess.User.Name != "" {
			info.AccountName = sess.User.Name
		} else {
			info.AccountName = info.Email
		}
	}

	extra := map[string]string{
		"plan_type":                 info.PlanType,
		"subscription_active_until": info.SubscriptionActiveUntil,
		"access_token":              sess.AccessToken,
		"session_token":             sess.SessionToken,
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
	return &quotaInfo{
		ExpiresAt: info.SubscriptionActiveUntil,
		Extra:     extra,
	}, nil
}

func (g *OpenAIGateway) quotaInfoFromAccessToken(ctx context.Context, access string, credentials map[string]string, warning string) (*quotaInfo, error) {
	info := g.enrichTokenInfo(ctx, parseIDToken(access), access, credentials["proxy_url"])
	extra := map[string]string{
		"plan_type":                 info.PlanType,
		"subscription_active_until": info.SubscriptionActiveUntil,
	}
	if warning != "" {
		extra["refresh_warning"] = warning
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
	return &quotaInfo{
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
		g.logger.Warn("probe_apikey_credential_error",
			sdk.LogFieldAccountID, accountID,
			sdk.LogFieldStatus, resp.StatusCode,
			sdk.LogFieldReason, truncate(string(body), 200),
		)
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
		g.logger.Warn("probe_oauth_build_request_failed",
			sdk.LogFieldAccountID, accountID,
			sdk.LogFieldError, err,
		)
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
		g.logger.Warn("probe_oauth_request_failed",
			sdk.LogFieldAccountID, accountID,
			sdk.LogFieldError, err,
		)
		return nil
	}
	defer func() { _ = resp.Body.Close() }()

	if snapshot := parseCodexUsageFromHeaders(resp.Header); snapshot != nil {
		StoreCodexUsage(accountID, snapshot)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		g.logger.Warn("probe_oauth_non_2xx",
			sdk.LogFieldAccountID, accountID,
			sdk.LogFieldStatus, resp.StatusCode,
			sdk.LogFieldReason, truncate(string(body), 200),
		)
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

func appendCodexUsageWindow(
	windows []accountUsageWindow,
	key string,
	label string,
	usedPercent *float64,
	resetAfterSeconds *int,
	base time.Time,
	now time.Time,
	force bool,
) []accountUsageWindow {
	if !force && usedPercent == nil && resetAfterSeconds == nil {
		return windows
	}
	used := 0.0
	if usedPercent != nil {
		used = *usedPercent
	}
	var resetAt *time.Time
	if resetAfterSeconds != nil {
		resetAt = resetAtFromBase(base, *resetAfterSeconds)
	}
	return append(windows, newAccountUsageWindow(key, label, used, resetAt, now))
}

func buildCodexUsageWindows(snapshot *CodexUsageSnapshot, limitName string, now time.Time) []accountUsageWindow {
	windows := make([]accountUsageWindow, 0, 4)
	if snapshot == nil {
		return windows
	}
	if normalized := snapshot.Normalize(); normalized != nil {
		windows = appendCodexUsageWindow(
			windows, "5h", "5h", normalized.Used5hPercent, normalized.Reset5hSeconds, snapshot.CapturedAt, now, false,
		)
		windows = appendCodexUsageWindow(
			windows, "7d", "7d", normalized.Used7dPercent, normalized.Reset7dSeconds, snapshot.CapturedAt, now, false,
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
	rawLimitName := strings.TrimSpace(limitName)
	if rawLimitName == "" {
		rawLimitName = strings.TrimSpace(snapshot.LimitName)
	}
	if !hasModelWindows && rawLimitName == "" {
		return windows
	}

	displayLimitName := rawLimitName
	if displayLimitName == "" {
		displayLimitName = "spark"
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
	var normalized *NormalizedCodexLimits
	if hasModelWindows {
		normalized = modelSnapshot.Normalize()
	}
	forceModelWindows := rawLimitName != ""
	var modelUsed5h *float64
	var modelReset5h *int
	var modelUsed7d *float64
	var modelReset7d *int
	if normalized != nil {
		modelUsed5h = normalized.Used5hPercent
		modelReset5h = normalized.Reset5hSeconds
		modelUsed7d = normalized.Used7dPercent
		modelReset7d = normalized.Reset7dSeconds
	}
	windows = appendCodexUsageWindow(
		windows,
		"model:5h:"+displayLimitName,
		"5h "+displayLimitName,
		modelUsed5h,
		modelReset5h,
		snapshot.CapturedAt,
		now,
		forceModelWindows,
	)
	windows = appendCodexUsageWindow(
		windows,
		"model:7d:"+displayLimitName,
		"7d "+displayLimitName,
		modelUsed7d,
		modelReset7d,
		snapshot.CapturedAt,
		now,
		forceModelWindows,
	)

	return windows
}

// HandleRequest 处理 Core 透传的自定义请求（实现 sdk.RequestHandler 接口）
func (g *OpenAIGateway) HandleRequest(ctx context.Context, _, path, _ string, _ http.Header, body []byte) (int, http.Header, []byte, error) {
	switch path {
	case "accounts/quota":
		var req struct {
			ID          int64             `json:"id"`
			Credentials map[string]string `json:"credentials"`
		}
		if err := json.Unmarshal(body, &req); err != nil || req.ID == 0 {
			return http.StatusBadRequest, nil, jsonError("invalid request body"), nil
		}
		quota, err := g.QueryQuota(ctx, req.Credentials)
		if err != nil {
			if errors.Is(err, sdk.ErrNotSupported) {
				return http.StatusNotFound, nil, jsonError("quota refresh unsupported"), nil
			}
			if strings.HasPrefix(err.Error(), ReauthRequiredPrefix) {
				return http.StatusUnauthorized, nil, jsonMarshal(map[string]string{
					"error_code":    "reauth_required",
					"error_message": strings.TrimPrefix(err.Error(), ReauthRequiredPrefix),
				}), nil
			}
			return http.StatusInternalServerError, nil, jsonError(err.Error()), nil
		}
		if quota == nil {
			return http.StatusNotFound, nil, jsonError("quota refresh unsupported"), nil
		}
		resp := map[string]any{
			"expires_at": quota.ExpiresAt,
			"extra":      quota.Extra,
		}
		if quota.Extra != nil {
			if warning := quota.Extra["refresh_warning"]; warning != "" {
				resp["reauth_warning"] = warning
			}
		}
		return http.StatusOK, nil, jsonMarshal(resp), nil

	case "usage/accounts":
		var accounts []struct {
			ID          int64             `json:"id"`
			Credentials map[string]string `json:"credentials"`
		}
		if err := json.Unmarshal(body, &accounts); err != nil {
			return http.StatusBadRequest, nil, jsonError("invalid request body"), nil
		}

		result := make(map[string]accountUsageInfo)
		var probeErrors []accountUsageError
		now := time.Now()
		for _, a := range accounts {
			// 检查是否有探测时发现的凭证错误
			if errMsg := GetProbeError(a.ID); errMsg != "" {
				probeErrors = append(probeErrors, accountUsageError{ID: a.ID, Message: errMsg})
			}
			snapshot := GetCodexUsage(a.ID)
			if snapshot == nil {
				snapshot = g.ProbeUsage(ctx, a.ID, a.Credentials)
			}
			// 再次检查（ProbeUsage 刚产生的错误）
			if errMsg := GetProbeError(a.ID); errMsg != "" {
				probeErrors = append(probeErrors, accountUsageError{ID: a.ID, Message: errMsg})
			}
			if snapshot == nil {
				continue
			}

			usage := accountUsageInfo{
				UpdatedAt: snapshot.CapturedAt.UTC().Format(time.RFC3339),
				Windows:   buildCodexUsageWindows(snapshot, snapshot.LimitName, now),
			}
			// Credits
			if snapshot.CreditsHasCredits {
				usage.Credits = &accountUsageCredits{
					Balance:   snapshot.CreditsBalance,
					Unlimited: snapshot.CreditsUnlimited,
				}
			}

			if len(usage.Windows) > 0 || usage.Credits != nil {
				result[strconv.FormatInt(a.ID, 10)] = usage
			}
		}
		return http.StatusOK, nil, jsonMarshal(accountUsageAccountsResponse{
			Accounts: result,
			Errors:   probeErrors,
		}), nil

	case "usage/probe":
		// 强制重探测单账号：不走 GetCodexUsage 缓存，直接 ProbeUsage 并回写。
		// 用户点"刷新用量"时调用，解决"从未成功探测 → 用量窗口一直为空"的问题。
		var req struct {
			ID          int64             `json:"id"`
			Credentials map[string]string `json:"credentials"`
		}
		if err := json.Unmarshal(body, &req); err != nil || req.ID == 0 {
			return http.StatusBadRequest, nil, jsonError("invalid request body"), nil
		}
		// ProbeUsage 内部会把结果写入 codex usage 缓存（见 probe*Usage 实现），
		// 后续 usage/accounts 会直接读到新鲜数据；这里也顺手构造响应体，让调用方
		// 可以立即拿到探测结果，而不需要再轮询一次 usage/accounts。
		snapshot := g.ProbeUsage(ctx, req.ID, req.Credentials)
		probeErr := GetProbeError(req.ID)
		resp := map[string]any{}
		if snapshot != nil {
			now := time.Now()
			resp["windows"] = buildCodexUsageWindows(snapshot, snapshot.LimitName, now)
			resp["updated_at"] = snapshot.CapturedAt.UTC().Format(time.RFC3339)
		}
		if probeErr != "" {
			resp["error"] = probeErr
		}
		if snapshot == nil && probeErr == "" {
			// 探测失败但没捕获具体错误（ProbeUsage 内部日志有，但这里给调用方一个兜底提示）。
			resp["error"] = "探测失败：上游未返回 rate_limits 事件"
		}
		return http.StatusOK, nil, jsonMarshal(resp), nil

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

	case "oauth/import-refresh":
		var raw struct {
			RefreshToken string `json:"refresh_token"`
			ProxyURL     string `json:"proxy_url"`
			ClientID     string `json:"client_id"`
		}
		if err := json.Unmarshal(body, &raw); err != nil || strings.TrimSpace(raw.RefreshToken) == "" {
			return http.StatusBadRequest, nil, jsonError("缺少 refresh_token 参数"), nil
		}
		result, err := g.ImportFromRefreshToken(context.Background(), raw.RefreshToken, raw.ProxyURL, raw.ClientID)
		if err != nil {
			return http.StatusInternalServerError, nil, jsonError(err.Error()), nil
		}
		return http.StatusOK, nil, jsonMarshal(map[string]any{
			"account_type": result.AccountType,
			"credentials":  result.Credentials,
			"account_name": result.AccountName,
		}), nil

	case "oauth/batch-import-refresh":
		var raw struct {
			RefreshTokens []string `json:"refresh_tokens"`
			ProxyURL      string   `json:"proxy_url"`
			ClientID      string   `json:"client_id"`
		}
		if err := json.Unmarshal(body, &raw); err != nil || len(raw.RefreshTokens) == 0 {
			return http.StatusBadRequest, nil, jsonError("缺少 refresh_tokens 参数"), nil
		}

		type batchResult struct {
			AccountType string            `json:"account_type,omitempty"`
			AccountName string            `json:"account_name,omitempty"`
			Credentials map[string]string `json:"credentials,omitempty"`
			Status      string            `json:"status"`
			Error       string            `json:"error,omitempty"`
		}

		results := make([]batchResult, 0, len(raw.RefreshTokens))
		for _, rt := range raw.RefreshTokens {
			if strings.TrimSpace(rt) == "" {
				continue
			}
			imported, err := g.ImportFromRefreshToken(context.Background(), rt, raw.ProxyURL, raw.ClientID)
			if err != nil {
				results = append(results, batchResult{Status: "failed", Error: err.Error()})
				continue
			}
			results = append(results, batchResult{
				Status:      "ok",
				AccountType: imported.AccountType,
				AccountName: imported.AccountName,
				Credentials: imported.Credentials,
			})
		}
		return http.StatusOK, nil, jsonMarshal(map[string]any{"results": results}), nil

	case "oauth/import-session":
		var raw struct {
			Session  string `json:"session"`
			ProxyURL string `json:"proxy_url"`
		}
		if err := json.Unmarshal(body, &raw); err != nil || strings.TrimSpace(raw.Session) == "" {
			return http.StatusBadRequest, nil, jsonError("缺少 session 参数"), nil
		}
		result, err := g.ImportFromSessionJSON(context.Background(), raw.Session, raw.ProxyURL)
		if err != nil {
			return http.StatusInternalServerError, nil, jsonError(err.Error()), nil
		}
		return http.StatusOK, nil, jsonMarshal(map[string]any{
			"account_type": result.AccountType,
			"credentials":  result.Credentials,
			"account_name": result.AccountName,
		}), nil

	case "oauth/batch-import-session":
		var raw struct {
			Sessions []string `json:"sessions"`
			ProxyURL string   `json:"proxy_url"`
		}
		if err := json.Unmarshal(body, &raw); err != nil || len(raw.Sessions) == 0 {
			return http.StatusBadRequest, nil, jsonError("缺少 sessions 参数"), nil
		}

		type batchResult struct {
			AccountType string            `json:"account_type,omitempty"`
			AccountName string            `json:"account_name,omitempty"`
			Credentials map[string]string `json:"credentials,omitempty"`
			Status      string            `json:"status"`
			Error       string            `json:"error,omitempty"`
		}

		results := make([]batchResult, 0, len(raw.Sessions))
		for _, s := range raw.Sessions {
			if strings.TrimSpace(s) == "" {
				continue
			}
			imported, err := g.ImportFromSessionJSON(context.Background(), s, raw.ProxyURL)
			if err != nil {
				results = append(results, batchResult{Status: "failed", Error: err.Error()})
				continue
			}
			results = append(results, batchResult{
				Status:      "ok",
				AccountType: imported.AccountType,
				AccountName: imported.AccountName,
				Credentials: imported.Credentials,
			})
		}
		return http.StatusOK, nil, jsonMarshal(map[string]any{"results": results}), nil

	default:
		return http.StatusNotFound, nil, jsonError("未知的操作: " + path), nil
	}
}

// imageToolCostModel 是 Responses API 的 image_generation 内置工具使用的模型。
// 上游文档与 Codex `$imagegen` 技能均使用 gpt-image-1.5 作为实际图像生成模型。
// 默认计费模型选择见 outcome.go 的 imageTokenBillingModel。
const imageToolCostModel = "gpt-image-1.5"

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
