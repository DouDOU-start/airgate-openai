package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	openaiDefaultBaseURL = "https://api.openai.com"

	// OAuth 账号走 ChatGPT 内部 API
	chatgptCodexURL = "https://chatgpt.com/backend-api/codex/responses"
)

// ──────────────────────────────────────────────────────
// 主入口
// ──────────────────────────────────────────────────────

// forwardHTTP 接收核心已调度好的账号，负责将请求转发到 OpenAI 上游
func (g *OpenAIGateway) forwardHTTP(ctx context.Context, req *sdk.ForwardRequest) (*sdk.ForwardResult, error) {
	start := time.Now()
	account := req.Account

	// 解析上游请求方法与路径
	upstreamMethod, upstreamPath := resolveUpstreamRoute(req, account)
	targetURL := buildUpstreamURL(account, upstreamPath)

	// 仅对带请求体的方法执行请求预处理
	body := req.Body
	if methodAllowsBody(upstreamMethod) {
		body = preprocessRequestBody(req.Body, req.Model, account)
	}

	var bodyReader io.Reader
	if methodAllowsBody(upstreamMethod) && len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}

	// 构建 HTTP 请求
	upstreamReq, err := http.NewRequestWithContext(ctx, upstreamMethod, targetURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("构建上游请求失败: %w", err)
	}

	// 设置认证头
	setAuthHeaders(upstreamReq, account)

	// 仅在有请求体时设置 JSON Content-Type
	if methodAllowsBody(upstreamMethod) {
		upstreamReq.Header.Set("Content-Type", "application/json")
	}

	// OAuth 账号特殊处理
	if account.Credentials["access_token"] != "" {
		upstreamReq.Host = "chatgpt.com"
		upstreamReq.Header.Set("Accept", "text/event-stream")
		upstreamReq.Header.Set("OpenAI-Beta", "responses=experimental")

		// Codex CLI 识别
		if isCodexCLI(req.Headers) {
			upstreamReq.Header.Set("originator", "codex_cli_rs")
		}

		// ChatGPT Account ID
		if chatgptAccountID := account.Credentials["chatgpt_account_id"]; chatgptAccountID != "" {
			upstreamReq.Header.Set("chatgpt-account-id", chatgptAccountID)
		}
	}

	// 透传白名单头
	passHeaders(req.Headers, upstreamReq.Header)

	// 构建 HTTP 客户端（含代理）
	client := g.buildHTTPClient(account)

	// 发送请求
	resp, err := client.Do(upstreamReq)
	if err != nil {
		return nil, fmt.Errorf("请求上游失败: %w", err)
	}
	defer resp.Body.Close()

	// 上游返回错误时，返回 error 让核心 SimpleForwarder 决定是否 failover
	if resp.StatusCode >= 500 || resp.StatusCode == 429 {
		respBody, _ := io.ReadAll(resp.Body)
		return &sdk.ForwardResult{
			StatusCode: resp.StatusCode,
			Duration:   time.Since(start),
		}, fmt.Errorf("上游返回 %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}

	// 流式响应
	if req.Stream && req.Writer != nil {
		return handleStreamResponse(resp, req.Writer, start)
	}

	// 非流式响应
	return handleNonStreamResponse(resp, req.Writer, start)
}

// ──────────────────────────────────────────────────────
// URL 构建
// ──────────────────────────────────────────────────────

// resolveUpstreamRoute 解析上游请求方法与路径
// 优先读取核心透传头；若缺失则按请求体特征兜底推断
func resolveUpstreamRoute(req *sdk.ForwardRequest, account *sdk.Account) (string, string) {
	// OAuth 账号固定走 ChatGPT Codex Responses
	if account.Credentials["access_token"] != "" {
		return http.MethodPost, "/backend-api/codex/responses"
	}

	reqPath := extractForwardedPath(req.Headers)
	reqMethod := strings.ToUpper(strings.TrimSpace(req.Headers.Get("X-Forwarded-Method")))

	// 当核心尚未透传 path/method 时，按已声明路由做兜底推断
	if reqPath == "" {
		trimmed := bytes.TrimSpace(req.Body)
		switch {
		case len(trimmed) == 0 && !req.Stream:
			reqPath = "/v1/models"
		case gjson.GetBytes(trimmed, "messages").Exists() && !gjson.GetBytes(trimmed, "input").Exists():
			reqPath = "/v1/chat/completions"
		default:
			reqPath = "/v1/responses"
		}
	}

	if reqMethod == "" {
		if reqPath == "/v1/models" {
			reqMethod = http.MethodGet
		} else {
			reqMethod = http.MethodPost
		}
	}

	switch reqMethod {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead:
	default:
		reqMethod = http.MethodPost
	}

	if !strings.HasPrefix(reqPath, "/") {
		reqPath = "/" + reqPath
	}
	return reqMethod, reqPath
}

// extractForwardedPath 从透传头中提取原始请求路径
func extractForwardedPath(headers http.Header) string {
	if headers == nil {
		return ""
	}

	candidates := []string{
		"X-Forwarded-Path",
		"X-Request-Path",
		"X-Original-URI",
		"X-Rewrite-URL",
	}
	for _, key := range candidates {
		raw := strings.TrimSpace(headers.Get(key))
		if raw == "" {
			continue
		}

		// 可能是完整 URL
		if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
			if u, err := url.Parse(raw); err == nil {
				path := strings.TrimSpace(u.EscapedPath())
				if path != "" {
					if u.RawQuery != "" {
						return path + "?" + u.RawQuery
					}
					return path
				}
			}
		}

		// 可能是 "/path?query"
		if strings.HasPrefix(raw, "/") {
			return raw
		}
	}
	return ""
}

// buildUpstreamURL 根据账号类型决定上游 URL
func buildUpstreamURL(account *sdk.Account, reqPath string) string {
	// OAuth 账号使用 ChatGPT 内部端点
	if account.Credentials["access_token"] != "" {
		return chatgptCodexURL
	}

	// API Key 账号使用标准端点或自定义 base_url
	baseURL := openaiDefaultBaseURL
	if u := account.Credentials["base_url"]; u != "" {
		baseURL = strings.TrimRight(u, "/")
	}

	if reqPath == "" {
		reqPath = "/v1/responses"
	}

	// 确保 URL 格式正确
	if strings.HasSuffix(baseURL, "/v1") {
		return baseURL + strings.TrimPrefix(reqPath, "/v1")
	}
	return baseURL + reqPath
}

// ──────────────────────────────────────────────────────
// 请求预处理
// ──────────────────────────────────────────────────────

// preprocessRequestBody 预处理请求体
func preprocessRequestBody(body []byte, model string, account *sdk.Account) []byte {
	if len(body) == 0 {
		return body
	}

	result := body

	// 同步 model 字段（核心传入的 model 可能经过映射）
	if model != "" {
		bodyModel := gjson.GetBytes(result, "model").String()
		if bodyModel != model {
			if modified, err := sjson.SetBytes(result, "model", model); err == nil {
				result = modified
			}
		}
	}

	// OAuth 账号强制 store=false, stream=true
	if account.Credentials["access_token"] != "" {
		if modified, err := sjson.SetBytes(result, "store", false); err == nil {
			result = modified
		}
		if modified, err := sjson.SetBytes(result, "stream", true); err == nil {
			result = modified
		}
	}

	return result
}

// ──────────────────────────────────────────────────────
// HTTP 客户端
// ──────────────────────────────────────────────────────

func methodAllowsBody(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		return true
	default:
		return false
	}
}

// requestTimeout 获取插件默认请求超时配置
func (g *OpenAIGateway) requestTimeout() time.Duration {
	// 与 plugin.yaml / plugin.go 的默认值保持一致
	const fallback = 300 * time.Second
	if g == nil || g.ctx == nil || g.ctx.Config() == nil {
		return fallback
	}
	timeout := g.ctx.Config().GetDuration("default_timeout")
	if timeout <= 0 {
		return fallback
	}
	return timeout
}

// buildHTTPClient 构建带代理支持的 HTTP 客户端
func (g *OpenAIGateway) buildHTTPClient(account *sdk.Account) *http.Client {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}

	// 代理设置
	if account.ProxyURL != "" {
		if proxyURL, err := url.Parse(account.ProxyURL); err == nil {
			transport.Proxy = http.ProxyURL(proxyURL)
		}
	}

	return &http.Client{
		Transport: transport,
		Timeout:   g.requestTimeout(),
	}
}

// ──────────────────────────────────────────────────────
// 流式响应处理
// ──────────────────────────────────────────────────────

// handleStreamResponse 处理 SSE 流式响应
func handleStreamResponse(resp *http.Response, w http.ResponseWriter, start time.Time) (*sdk.ForwardResult, error) {
	// 设置 SSE 响应头
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// 透传上游 Codex 速率限制头
	passCodexRateLimitHeaders(resp.Header, w.Header())

	w.WriteHeader(resp.StatusCode)

	result := &sdk.ForwardResult{
		StatusCode: resp.StatusCode,
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var streamErr error

	for scanner.Scan() {
		line := scanner.Text()

		// 写入到客户端
		if _, err := fmt.Fprintf(w, "%s\n", line); err != nil {
			break
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}

		// 提取 SSE data 行
		data, ok := extractSSEData(line)
		if !ok || len(data) == 0 || data == "[DONE]" {
			continue
		}

		// 解析 usage（仅在 response.completed 事件中）
		parseSSEUsage([]byte(data), result)

		// 捕获上游 SSE 失败事件
		if streamErr == nil {
			streamErr = parseSSEFailureEvent([]byte(data))
		}
	}
	if err := scanner.Err(); err != nil && streamErr == nil {
		streamErr = fmt.Errorf("读取上游 SSE 失败: %w", err)
	}

	result.Duration = time.Since(start)
	if streamErr != nil {
		// SSE 失败事件通常发生在 HTTP 200 下，返回错误让核心走失败统计/重试策略
		if result.StatusCode < http.StatusBadRequest {
			result.StatusCode = http.StatusBadGateway
		}
		return result, streamErr
	}
	return result, nil
}

// extractSSEData 从 SSE 行中提取 data 内容
// 兼容 "data: xxx" 和 "data:xxx" 两种格式
func extractSSEData(line string) (string, bool) {
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	s := line[5:]
	// 跳过前导空白
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	return s, true
}

// parseSSEUsage 从 SSE 数据中提取 usage 信息
// 支持 Responses API（response.completed）和 Chat Completions 格式
func parseSSEUsage(data []byte, result *sdk.ForwardResult) {
	eventType := gjson.GetBytes(data, "type").String()

	switch eventType {
	case "response.completed", "response.done":
		// Responses API 格式：usage 在 response.usage 中
		resp := gjson.GetBytes(data, "response")
		if !resp.Exists() {
			return
		}
		result.Model = resp.Get("model").String()
		usage := resp.Get("usage")
		if usage.Exists() {
			result.InputTokens = int(usage.Get("input_tokens").Int())
			result.OutputTokens = int(usage.Get("output_tokens").Int())
			// 缓存 tokens
			cacheRead := int(usage.Get("input_tokens_details.cached_tokens").Int())
			result.CacheTokens = cacheRead
		}

	default:
		// Chat Completions 格式：最终 chunk 含 usage 字段
		usage := gjson.GetBytes(data, "usage")
		if !usage.Exists() {
			return
		}
		result.InputTokens = int(usage.Get("prompt_tokens").Int())
		result.OutputTokens = int(usage.Get("completion_tokens").Int())
		result.Model = gjson.GetBytes(data, "model").String()
		// 缓存 tokens（Chat Completions 格式）
		cacheRead := int(usage.Get("prompt_tokens_details.cached_tokens").Int())
		result.CacheTokens = cacheRead
	}
}

// parseSSEFailureEvent 解析 Responses API 的失败事件并映射为错误
func parseSSEFailureEvent(data []byte) error {
	eventType := gjson.GetBytes(data, "type").String()
	switch eventType {
	case "response.failed":
		errNode := gjson.GetBytes(data, "response.error")
		msg := strings.TrimSpace(errNode.Get("message").String())
		if msg == "" {
			msg = "上游返回 response.failed"
		}
		errType := strings.ToLower(errNode.Get("type").String())
		errCode := strings.ToLower(errNode.Get("code").String())

		switch {
		case containsAny(errType, errCode, msg, "context_length", "context window", "max_tokens"):
			return fmt.Errorf("上游上下文窗口超限: %s", msg)
		case containsAny(errType, errCode, msg, "quota", "insufficient_quota"):
			return fmt.Errorf("上游配额不足: %s", msg)
		case containsAny(errType, errCode, msg, "usage_not_included"):
			return fmt.Errorf("上游使用权不包含: %s", msg)
		case containsAny(errType, errCode, msg, "invalid_prompt", "invalid_request"):
			return fmt.Errorf("上游请求无效: %s", msg)
		case containsAny(errType, errCode, msg, "server_overloaded", "overloaded", "slow_down"):
			return fmt.Errorf("上游服务繁忙: %s", msg)
		case containsAny(errType, errCode, msg, "rate_limit"):
			delay := parseRetryDelay(msg)
			if delay > 0 {
				return fmt.Errorf("上游速率限制(建议 %s 后重试): %s", delay, msg)
			}
			return fmt.Errorf("上游速率限制: %s", msg)
		default:
			return fmt.Errorf("上游流式失败(type=%s, code=%s): %s", errType, errCode, msg)
		}

	case "response.incomplete":
		reason := gjson.GetBytes(data, "response.incomplete_details.reason").String()
		if reason == "" {
			reason = "unknown"
		}
		return fmt.Errorf("上游返回不完整响应: %s", reason)
	}
	return nil
}

// ──────────────────────────────────────────────────────
// 非流式响应处理
// ──────────────────────────────────────────────────────

// handleNonStreamResponse 处理非流式响应
func handleNonStreamResponse(resp *http.Response, w http.ResponseWriter, start time.Time) (*sdk.ForwardResult, error) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取上游响应失败: %w", err)
	}

	// 解析 usage
	usage := parseUsage(body)

	// 写入响应到客户端
	if w != nil {
		// 透传响应头
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		passCodexRateLimitHeaders(resp.Header, w.Header())
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
	}

	return &sdk.ForwardResult{
		StatusCode:   resp.StatusCode,
		InputTokens:  usage.inputTokens,
		OutputTokens: usage.outputTokens,
		CacheTokens:  usage.cacheTokens,
		Model:        gjson.GetBytes(body, "model").String(),
		Duration:     time.Since(start),
	}, nil
}

// ──────────────────────────────────────────────────────
// 认证 & 头部处理
// ──────────────────────────────────────────────────────

// setAuthHeaders 设置认证头
func setAuthHeaders(req *http.Request, account *sdk.Account) {
	// 优先 API Key
	if apiKey := account.Credentials["api_key"]; apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
		return
	}
	// 其次 Access Token（OAuth）
	if token := account.Credentials["access_token"]; token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// passHeaders 透传白名单中的客户端头
func passHeaders(src, dst http.Header) {
	for key, values := range src {
		lowerKey := strings.ToLower(key)
		if openaiAllowedHeaders[lowerKey] {
			for _, v := range values {
				dst.Add(key, v)
			}
		}
	}
}

// openaiAllowedHeaders 允许透传的请求头白名单
var openaiAllowedHeaders = map[string]bool{
	// 标准头
	"accept-language": true,
	"user-agent":      true,
	// OpenAI 特定头
	"openai-beta":         true,
	"openai-organization": true,
	"x-request-id":        true,
	// Codex 特定头
	"x-codex-turn-state":    true,
	"x-codex-turn-metadata": true,
	"conversation_id":       true,
	"session_id":            true,
	"originator":            true,
	// Stainless 超时头（Codex CLI 使用）
	"x-stainless-timeout":         true,
	"x-stainless-read-timeout":    true,
	"x-stainless-connect-timeout": true,
}

// passCodexRateLimitHeaders 透传上游 Codex 速率限制响应头
func passCodexRateLimitHeaders(src, dst http.Header) {
	codexHeaders := []string{
		// Codex 主要限制
		"x-codex-primary-used-percent",
		"x-codex-primary-reset-after-seconds",
		"x-codex-primary-reset-at",
		"x-codex-primary-window-minutes",
		// Codex 次要限制
		"x-codex-secondary-used-percent",
		"x-codex-secondary-reset-after-seconds",
		"x-codex-secondary-reset-at",
		"x-codex-secondary-window-minutes",
		"x-codex-primary-over-secondary-limit-percent",
		// Codex 积分
		"x-codex-credits-has-credits",
		"x-codex-credits-unlimited",
		"x-codex-credits-balance",
		"x-codex-limit-name",
		// 粘性路由与模型信息
		"x-codex-turn-state",
		"openai-model",
		"x-models-etag",
		"x-reasoning-included",
		// 标准 OpenAI 速率限制头
		"x-ratelimit-limit-requests",
		"x-ratelimit-limit-tokens",
		"x-ratelimit-remaining-requests",
		"x-ratelimit-remaining-tokens",
		"x-ratelimit-reset-requests",
		"x-ratelimit-reset-tokens",
	}
	for _, key := range codexHeaders {
		if v := src.Get(key); v != "" {
			dst.Set(http.CanonicalHeaderKey(key), v)
		}
	}
}

// isCodexCLI 检测请求是否来自 Codex CLI
func isCodexCLI(headers http.Header) bool {
	ua := strings.ToLower(headers.Get("User-Agent"))
	if strings.Contains(ua, "codex") {
		return true
	}
	// Codex CLI 会发送特定的 originator 头
	if headers.Get("originator") == "codex_cli_rs" {
		return true
	}
	// Stainless 超时头通常由 Codex CLI SDK 发送
	if headers.Get("x-stainless-timeout") != "" {
		return true
	}
	return false
}

// ──────────────────────────────────────────────────────
// Usage 解析
// ──────────────────────────────────────────────────────

type openaiUsage struct {
	inputTokens  int
	outputTokens int
	cacheTokens  int
}

// parseUsage 从完整响应体解析 usage
// 兼容 Responses API 和 Chat Completions 两种格式
func parseUsage(body []byte) openaiUsage {
	usage := openaiUsage{}
	usageNode := gjson.GetBytes(body, "usage")
	if !usageNode.Exists() {
		return usage
	}

	// Responses API 格式：input_tokens / output_tokens
	usage.inputTokens = int(usageNode.Get("input_tokens").Int())
	usage.outputTokens = int(usageNode.Get("output_tokens").Int())

	// Chat Completions 格式兼容：prompt_tokens / completion_tokens
	if usage.inputTokens == 0 {
		usage.inputTokens = int(usageNode.Get("prompt_tokens").Int())
	}
	if usage.outputTokens == 0 {
		usage.outputTokens = int(usageNode.Get("completion_tokens").Int())
	}

	// 缓存 tokens（两种格式都支持）
	cacheCreation := int(usageNode.Get("cache_creation_input_tokens").Int())
	cacheRead := int(usageNode.Get("cache_read_input_tokens").Int())
	if cacheRead == 0 {
		cacheRead = int(usageNode.Get("input_tokens_details.cached_tokens").Int())
	}
	if cacheRead == 0 {
		cacheRead = int(usageNode.Get("prompt_tokens_details.cached_tokens").Int())
	}
	usage.cacheTokens = cacheCreation + cacheRead

	return usage
}

// ──────────────────────────────────────────────────────
// 工具函数
// ──────────────────────────────────────────────────────

// truncate 截断字符串
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// retryDelayPattern 匹配 "try again in Ns" / "try again in Nms" 格式
var retryDelayPattern = regexp.MustCompile(`(?i)try again in\s*(\d+(?:\.\d+)?)\s*(s|ms|seconds?)`)

// parseRetryDelay 从错误消息中提取建议重试延迟
func parseRetryDelay(msg string) time.Duration {
	matches := retryDelayPattern.FindStringSubmatch(msg)
	if len(matches) < 3 {
		return 0
	}
	val, err := strconv.ParseFloat(matches[1], 64)
	if err != nil {
		return 0
	}
	unit := strings.ToLower(matches[2])
	if unit == "ms" {
		return time.Duration(val * float64(time.Millisecond))
	}
	return time.Duration(val * float64(time.Second))
}

func containsAny(values ...string) bool {
	if len(values) < 4 {
		return false
	}
	haystacks := []string{
		strings.ToLower(values[0]),
		strings.ToLower(values[1]),
		strings.ToLower(values[2]),
	}
	for i := 3; i < len(values); i++ {
		kw := strings.ToLower(values[i])
		if kw == "" {
			continue
		}
		for _, h := range haystacks {
			if strings.Contains(h, kw) {
				return true
			}
		}
	}
	return false
}
