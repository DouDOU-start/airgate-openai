package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

// OAuth 请求/响应类型（插件内部定义，不依赖 SDK）
// OAuth 仅在 devserver 中使用，不走 gRPC

// OAuthStartRequest OAuth 授权发起请求
type OAuthStartRequest struct{}

// OAuthStartResponse OAuth 授权发起响应
type OAuthStartResponse struct {
	AuthorizeURL string
	State        string
}

// OAuthCallbackRequest OAuth 回调请求
type OAuthCallbackRequest struct {
	Code     string
	State    string
	ProxyURL string
}

// OAuthResult OAuth 授权结果
type OAuthResult struct {
	AccountType string
	Credentials map[string]string
	AccountName string
}

// OAuth 常量（与 codex 项目完全一致）
const (
	oauthClientID     = "app_EMoamEEZ73f0CkXaXp7hrann"
	oauthScope        = "openid profile email offline_access"
	oauthAuthEndpoint = "https://auth.openai.com/oauth/authorize"
	oauthTokenURL     = "https://auth.openai.com/oauth/token"

	// OAuthCallbackPort codex 注册的固定回调端口，不可更改
	OAuthCallbackPort = 1455
	// OAuthCallbackPath 回调路径
	OAuthCallbackPath = "/auth/callback"
)

// OAuthCallbackURL 返回固定的 OAuth 回调地址
func OAuthCallbackURL() string {
	return fmt.Sprintf("http://localhost:%d%s", OAuthCallbackPort, OAuthCallbackPath)
}

// pkceSession 保存 PKCE 会话信息
type pkceSession struct {
	verifier    string
	callbackURL string
	createdAt   time.Time
}

// oauthSessions 存储进行中的 OAuth 会话（state → pkceSession）
var oauthSessions sync.Map

// StartOAuth 发起 OAuth 授权
func (g *OpenAIGateway) StartOAuth(ctx context.Context, req *OAuthStartRequest) (*OAuthStartResponse, error) {
	cleanExpiredSessions()

	// 生成 PKCE
	verifier, challenge, err := generatePKCE()
	if err != nil {
		return nil, fmt.Errorf("生成 PKCE 失败: %w", err)
	}

	// 生成随机 state
	state, err := randomBase64URL(32)
	if err != nil {
		return nil, fmt.Errorf("生成 state 失败: %w", err)
	}

	// 回调地址固定为 codex 注册的 localhost:1455
	callbackURL := OAuthCallbackURL()

	// 保存会话
	oauthSessions.Store(state, &pkceSession{
		verifier:    verifier,
		callbackURL: callbackURL,
		createdAt:   time.Now(),
	})

	// 构建授权 URL（参数与 codex 完全一致）
	q := url.Values{}
	q.Set("client_id", oauthClientID)
	q.Set("scope", oauthScope)
	q.Set("response_type", "code")
	q.Set("redirect_uri", callbackURL)
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("id_token_add_organizations", "true")
	q.Set("codex_cli_simplified_flow", "true")
	authorizeURL := oauthAuthEndpoint + "?" + q.Encode()

	g.logger.Info("oauth_authorize_initiated", "authorize_url", authorizeURL)

	return &OAuthStartResponse{
		AuthorizeURL: authorizeURL,
		State:        state,
	}, nil
}

// HandleOAuthCallback 处理 OAuth 回调，完成 token 交换
func (g *OpenAIGateway) HandleOAuthCallback(ctx context.Context, req *OAuthCallbackRequest) (*OAuthResult, error) {
	val, ok := oauthSessions.LoadAndDelete(req.State)
	if !ok {
		return nil, fmt.Errorf("无效或已过期的 state")
	}
	session := val.(*pkceSession)

	if time.Since(session.createdAt) > 10*time.Minute {
		return nil, fmt.Errorf("OAuth 会话已过期")
	}

	// Token 交换
	tokens, err := g.exchangeCodeForTokens(ctx, session.callbackURL, session.verifier, req.Code, req.ProxyURL)
	if err != nil {
		return nil, fmt.Errorf("token 交换失败: %w", err)
	}

	// 解析 id_token JWT payload 提取用户信息和订阅状态
	info := parseIDToken(tokens.IDToken)

	credentials := map[string]string{
		"access_token":  tokens.AccessToken,
		"refresh_token": tokens.RefreshToken,
	}
	if info.AccountID != "" {
		credentials["chatgpt_account_id"] = info.AccountID
	}
	if info.Email != "" {
		credentials["email"] = info.Email
	}
	if info.PlanType != "" {
		credentials["plan_type"] = info.PlanType
	}
	if info.SubscriptionActiveUntil != "" {
		credentials["subscription_active_until"] = info.SubscriptionActiveUntil
	}

	g.logger.Info("oauth_authorize_completed",
		"account_name", info.AccountName,
		sdk.LogFieldAccountID, info.AccountID,
		"plan_type", info.PlanType,
	)

	return &OAuthResult{
		AccountType: "oauth",
		Credentials: credentials,
		AccountName: info.AccountName,
	}, nil
}

// ImportFromRefreshToken 使用已有的 refresh_token 重新申请一次 token，
// 从 id_token 解析出 chatgpt_account_id / email / plan_type / 订阅到期等字段，
// 返回 OAuthResult（结构与 HandleOAuthCallback 对齐）。
//
// 用于后台管理员粘贴 refresh_token 批量/单条导入 OAuth 账号的场景。
func (g *OpenAIGateway) ImportFromRefreshToken(ctx context.Context, refreshToken, proxyURL string) (*OAuthResult, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return nil, fmt.Errorf("refresh_token 不能为空")
	}

	tokens, err := g.refreshTokens(ctx, refreshToken, proxyURL)
	if err != nil {
		return nil, fmt.Errorf("刷新 token 失败: %w", err)
	}
	if tokens.AccessToken == "" {
		return nil, fmt.Errorf("刷新响应缺少 access_token")
	}

	info := parseIDToken(tokens.IDToken)

	// 部分上游在 refresh_token 模式下不轮换 refresh_token（返回空串），此时沿用原值。
	nextRefresh := tokens.RefreshToken
	if nextRefresh == "" {
		nextRefresh = refreshToken
	}

	credentials := map[string]string{
		"access_token":  tokens.AccessToken,
		"refresh_token": nextRefresh,
	}
	if info.AccountID != "" {
		credentials["chatgpt_account_id"] = info.AccountID
	}
	if info.Email != "" {
		credentials["email"] = info.Email
	}
	if info.PlanType != "" {
		credentials["plan_type"] = info.PlanType
	}
	if info.SubscriptionActiveUntil != "" {
		credentials["subscription_active_until"] = info.SubscriptionActiveUntil
	}

	g.logger.Info("oauth_refresh_token_imported",
		"account_name", info.AccountName,
		sdk.LogFieldAccountID, info.AccountID,
		"plan_type", info.PlanType,
	)

	return &OAuthResult{
		AccountType: "oauth",
		Credentials: credentials,
		AccountName: info.AccountName,
	}, nil
}

// tokenResponse token 交换响应
// 注意：上游失败时 error 字段可能是 string（"invalid_grant"）也可能是 object
// （{code, message, ...}），因此用 json.RawMessage 兼容，再用 errorMessage() 提取文本。
type tokenResponse struct {
	IDToken      string          `json:"id_token"`
	AccessToken  string          `json:"access_token"`
	RefreshToken string          `json:"refresh_token"`
	ExpiresIn    int             `json:"expires_in"`
	Error        json.RawMessage `json:"error"`
	Description  string          `json:"error_description"`
}

// errorMessage 从 Error 字段中提取可读文本，兼容 string / {message} / {code} / 任意对象。
func (t *tokenResponse) errorMessage() string {
	if len(t.Error) == 0 {
		return ""
	}
	// 情况 1：字符串
	var s string
	if err := json.Unmarshal(t.Error, &s); err == nil {
		return s
	}
	// 情况 2：对象，尝试常见字段
	var obj map[string]any
	if err := json.Unmarshal(t.Error, &obj); err == nil {
		for _, key := range []string{"message", "error_description", "description", "detail", "code", "type"} {
			if v, ok := obj[key]; ok {
				if str, ok := v.(string); ok && str != "" {
					return str
				}
			}
		}
		// 退化为整体 JSON
		return string(t.Error)
	}
	// 既不是 string 也不是 object，原样返回
	return string(t.Error)
}

// exchangeCodeForTokens 使用授权码交换 token（参考 chatgpt-register）
func (g *OpenAIGateway) exchangeCodeForTokens(ctx context.Context, callbackURL, verifier, code, proxyURL string) (*tokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", callbackURL)
	form.Set("client_id", oauthClientID)
	form.Set("code_verifier", verifier)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		oauthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := g.buildHTTPClient(&sdk.Account{ProxyURL: proxyURL})
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("请求 token 端点失败: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	var tokens tokenResponse
	if err := json.Unmarshal(body, &tokens); err != nil {
		return nil, fmt.Errorf("解析 token 响应失败: %w", err)
	}

	if resp.StatusCode >= 400 {
		msg := tokens.Description
		if msg == "" {
			msg = tokens.errorMessage()
		}
		if msg == "" {
			msg = fmt.Sprintf("token 请求失败: %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("%s", msg)
	}

	if tokens.AccessToken == "" {
		return nil, fmt.Errorf("token 响应缺少 access_token")
	}

	return &tokens, nil
}

// idTokenInfo 从 id_token 中解析出的用户和订阅信息
type idTokenInfo struct {
	AccountID               string
	AccountName             string
	Email                   string
	PlanType                string // free / plus / pro / team
	SubscriptionActiveUntil string // ISO 8601 格式
}

// parseIDToken 解码 JWT payload（不验签），提取账号信息和订阅状态
func parseIDToken(idToken string) *idTokenInfo {
	info := &idTokenInfo{}
	if idToken == "" {
		return info
	}

	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return info
	}

	// 解码 payload（base64url，可能缺 padding）
	payload := parts[1]
	if m := len(payload) % 4; m != 0 {
		payload += strings.Repeat("=", 4-m)
	}
	data, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return info
	}

	var claims map[string]interface{}
	if err := json.Unmarshal(data, &claims); err != nil {
		return info
	}

	// 直接取 chatgpt_account_id
	if id, ok := claims["chatgpt_account_id"].(string); ok {
		info.AccountID = id
	}

	// 尝试从嵌套的 auth claims 中取
	if authClaims, ok := claims["https://api.openai.com/auth"].(map[string]interface{}); ok {
		if id, ok := authClaims["chatgpt_account_id"].(string); ok && info.AccountID == "" {
			info.AccountID = id
		}
		if pt, ok := authClaims["chatgpt_plan_type"].(string); ok {
			info.PlanType = pt
		}
		if until := authClaims["chatgpt_subscription_active_until"]; until != nil {
			info.SubscriptionActiveUntil = fmt.Sprintf("%v", until)
		}
	}

	// 邮箱
	if email, ok := claims["email"].(string); ok && email != "" {
		info.Email = email
	}

	// 用户名：优先 name，其次 email
	if name, ok := claims["name"].(string); ok && name != "" {
		info.AccountName = name
	} else if info.Email != "" {
		info.AccountName = info.Email
	}

	return info
}

// refreshTokens 使用 refresh_token 刷新获取新的 token 组
func (g *OpenAIGateway) refreshTokens(ctx context.Context, refreshToken, proxyURL string) (*tokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", oauthClientID)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		oauthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := g.buildHTTPClient(&sdk.Account{ProxyURL: proxyURL})
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("请求 token 端点失败: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	var tokens tokenResponse
	if err := json.Unmarshal(body, &tokens); err != nil {
		return nil, fmt.Errorf("解析 token 响应失败: %w", err)
	}

	if resp.StatusCode >= 400 {
		msg := tokens.Description
		if msg == "" {
			msg = tokens.errorMessage()
		}
		if msg == "" {
			msg = fmt.Sprintf("刷新 token 失败: %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("%s", msg)
	}

	return &tokens, nil
}

// generatePKCE 生成 PKCE code_verifier 和 code_challenge (S256)
func generatePKCE() (verifier, challenge string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// randomBase64URL 生成指定字节数的随机 base64url 字符串
func randomBase64URL(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// cleanExpiredSessions 清理超过 10 分钟的过期会话
func cleanExpiredSessions() {
	oauthSessions.Range(func(key, value any) bool {
		session := value.(*pkceSession)
		if time.Since(session.createdAt) > 10*time.Minute {
			oauthSessions.Delete(key)
		}
		return true
	})
}
