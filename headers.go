package main

import (
	"net/http"
	"strings"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

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
	if headers.Get("originator") == "codex_cli_rs" {
		return true
	}
	if headers.Get("x-stainless-timeout") != "" {
		return true
	}
	return false
}
