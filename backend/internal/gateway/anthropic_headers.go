package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"
)

// setAnthropicStyleResponseHeaders 给 Anthropic 协议响应补齐 Claude 官方 API 常见的响应头。
//
// 目的：让"结构完整性 / 签名校验"类检测能识别为合法的 Anthropic 协议响应。
// 注意：这些头部仅是协议结构补全，不涉及也不能绕过模型指纹 / 行为 / 签名真实性检测 ——
// 那些检测在原理上就无法通过 OpenAI 后端。
//
// 必须在 w.WriteHeader(...) 之前调用。
func setAnthropicStyleResponseHeaders(w http.ResponseWriter) {
	if w == nil {
		return
	}
	h := w.Header()

	// 请求追踪
	reqID := generateAnthropicRequestID()
	h.Set("request-id", reqID)
	h.Set("x-request-id", reqID)

	// 组织标识（固定仿真值，不暴露真实账号数）
	h.Set("anthropic-organization-id", "00000000-0000-4000-8000-000000000001")

	// 速率限制 — 使用合理静态值，表示配额充裕
	resetAt := time.Now().Add(60 * time.Second).UTC().Format(time.RFC3339)

	h.Set("anthropic-ratelimit-requests-limit", "4000")
	h.Set("anthropic-ratelimit-requests-remaining", "3999")
	h.Set("anthropic-ratelimit-requests-reset", resetAt)

	h.Set("anthropic-ratelimit-tokens-limit", "400000")
	h.Set("anthropic-ratelimit-tokens-remaining", "399000")
	h.Set("anthropic-ratelimit-tokens-reset", resetAt)

	h.Set("anthropic-ratelimit-input-tokens-limit", "400000")
	h.Set("anthropic-ratelimit-input-tokens-remaining", "399500")
	h.Set("anthropic-ratelimit-input-tokens-reset", resetAt)

	h.Set("anthropic-ratelimit-output-tokens-limit", "80000")
	h.Set("anthropic-ratelimit-output-tokens-remaining", "79500")
	h.Set("anthropic-ratelimit-output-tokens-reset", resetAt)

	// Cloudflare 边缘头 —— Anthropic 官方 API 实际部署在 Cloudflare 后面，
	// 客户端常检测这些字段作为"是否经过 Anthropic 公共入口"的旁证
	h.Set("cf-cache-status", "DYNAMIC")
	h.Set("cf-ray", generateCloudflareRay())
	h.Set("server", "cloudflare")
	h.Set("via", "1.1 google")
	h.Set("strict-transport-security", "max-age=31536000; includeSubDomains; preload")
}

// generateAnthropicRequestID 生成 Anthropic 风格的请求 ID。
// 格式：`req_01` + 32 字符十六进制。真实 Anthropic 的 request-id 形如 `req_01ABCdef...`。
func generateAnthropicRequestID() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return "req_01" + hex.EncodeToString(buf)
}

// generateCloudflareRay 生成 Cloudflare cf-ray 风格的值。
// 格式：16 位十六进制 + `-` + 3 字母数据中心代码。
func generateCloudflareRay() string {
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf) + "-SJC"
}
