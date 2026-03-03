package main

import (
	"bytes"
	"context"
	"crypto/tls"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

//go:embed instructions.md
var defaultInstructions string

// ──────────────────────────────────────────────────────
// 转发入口（三模式分发）
// ──────────────────────────────────────────────────────

// forwardHTTP 根据账号凭证类型分发到不同转发模式
func (g *OpenAIGateway) forwardHTTP(ctx context.Context, req *sdk.ForwardRequest) (*sdk.ForwardResult, error) {
	account := req.Account

	if account.Credentials["api_key"] != "" {
		return g.forwardAPIKey(ctx, req)
	}
	if account.Credentials["access_token"] != "" {
		return g.forwardOAuth(ctx, req)
	}
	return nil, fmt.Errorf("账号缺少 api_key 或 access_token")
}

// ──────────────────────────────────────────────────────
// API Key 模式：HTTP/SSE 直连上游
// ──────────────────────────────────────────────────────

func (g *OpenAIGateway) forwardAPIKey(ctx context.Context, req *sdk.ForwardRequest) (*sdk.ForwardResult, error) {
	start := time.Now()
	account := req.Account

	// 解析上游请求方法与路径
	reqMethod, reqPath := resolveAPIKeyRoute(req)
	targetURL := buildAPIKeyURL(account, reqPath)

	// 预处理请求体
	body := req.Body
	if methodAllowsBody(reqMethod) {
		body = preprocessRequestBody(body, req.Model)
	}

	var bodyReader io.Reader
	if methodAllowsBody(reqMethod) && len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}

	// 构建 HTTP 请求
	upstreamReq, err := http.NewRequestWithContext(ctx, reqMethod, targetURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("构建上游请求失败: %w", err)
	}

	// 设置认证头
	setAuthHeaders(upstreamReq, account)
	if methodAllowsBody(reqMethod) {
		upstreamReq.Header.Set("Content-Type", "application/json")
	}

	// 透传白名单头
	passHeaders(req.Headers, upstreamReq.Header)

	// 发送请求
	client := g.buildHTTPClient(account)
	resp, err := client.Do(upstreamReq)
	if err != nil {
		return nil, fmt.Errorf("请求上游失败: %w", err)
	}
	defer resp.Body.Close()

	// 上游返回错误时，返回 error 让核心决定是否 failover
	if resp.StatusCode >= 500 || resp.StatusCode == 429 {
		respBody, _ := io.ReadAll(resp.Body)
		return &sdk.ForwardResult{
			StatusCode: resp.StatusCode,
			Duration:   time.Since(start),
		}, fmt.Errorf("上游返回 %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}

	// 流式 / 非流式响应处理
	if req.Stream && req.Writer != nil {
		return handleStreamResponse(resp, req.Writer, start)
	}
	return handleNonStreamResponse(resp, req.Writer, start)
}

// ──────────────────────────────────────────────────────
// URL 构建
// ──────────────────────────────────────────────────────

// resolveAPIKeyRoute 解析 API Key 模式的上游请求方法与路径
func resolveAPIKeyRoute(req *sdk.ForwardRequest) (string, string) {
	reqPath := extractForwardedPath(req.Headers)
	reqMethod := strings.ToUpper(strings.TrimSpace(req.Headers.Get("X-Forwarded-Method")))

	// 兜底推断
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
		if strings.HasPrefix(raw, "/") {
			return raw
		}
	}
	return ""
}

// buildAPIKeyURL 根据账号 base_url 和请求路径构建上游 URL
func buildAPIKeyURL(account *sdk.Account, reqPath string) string {
	baseURL := strings.TrimRight(account.Credentials["base_url"], "/")
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}

	if reqPath == "" {
		reqPath = "/v1/responses"
	}

	if strings.HasSuffix(baseURL, "/v1") {
		return baseURL + strings.TrimPrefix(reqPath, "/v1")
	}
	return baseURL + reqPath
}

// ──────────────────────────────────────────────────────
// 请求预处理
// ──────────────────────────────────────────────────────

// preprocessRequestBody 预处理请求体（同步 model 字段）
func preprocessRequestBody(body []byte, model string) []byte {
	if len(body) == 0 || model == "" {
		return body
	}

	bodyModel := gjson.GetBytes(body, "model").String()
	if bodyModel != model {
		if modified, err := sjson.SetBytes(body, "model", model); err == nil {
			return modified
		}
	}
	return body
}

// wrapAsResponsesAPI 将请求包装为 Responses API 格式（模拟客户端模式）
func wrapAsResponsesAPI(body []byte, model string) ([]byte, error) {
	// 已是 Responses 格式（有 input 字段），只注入 instructions
	if gjson.GetBytes(body, "input").Exists() {
		result := body
		if !gjson.GetBytes(body, "instructions").Exists() {
			if modified, err := sjson.SetBytes(result, "instructions", defaultInstructions); err == nil {
				result = modified
			}
		}
		return result, nil
	}

	// Chat Completions 格式（有 messages 字段）→ 转换为 Responses API input
	if gjson.GetBytes(body, "messages").Exists() {
		var input []any
		messages := gjson.GetBytes(body, "messages").Array()
		for _, msg := range messages {
			role := msg.Get("role").String()
			content := msg.Get("content").String()
			if role == "" || content == "" {
				continue
			}

			// 映射 role：assistant → assistant，其他 → user
			apiRole := "user"
			if role == "assistant" {
				apiRole = "assistant"
			}

			// 映射 content type
			contentType := "input_text"
			if apiRole == "assistant" {
				contentType = "output_text"
			}

			input = append(input, map[string]any{
				"type": "message",
				"role": apiRole,
				"content": []map[string]string{
					{"type": contentType, "text": content},
				},
			})
		}

		wrapped := map[string]any{
			"model":        model,
			"input":        input,
			"instructions": defaultInstructions,
			"stream":       true,
			"store":        false,
		}

		return json.Marshal(wrapped)
	}

	// 无法识别的格式，原样返回
	return body, nil
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
// 工具函数
// ──────────────────────────────────────────────────────

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
