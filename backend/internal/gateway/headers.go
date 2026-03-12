package gateway

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

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
			dst.Set(key, v)
		}
	}
}

// CodexUsageSnapshot Codex 速率限制用量快照（从响应头中捕获）
type CodexUsageSnapshot struct {
	// 主要限制（短窗口，通常 5h）
	PrimaryUsedPercent       float64 `json:"primary_used_percent"`
	PrimaryResetAfterSeconds int     `json:"primary_reset_after_seconds"`
	PrimaryWindowMinutes     int     `json:"primary_window_minutes"`
	// 次要限制（长窗口，通常 7d）
	SecondaryUsedPercent       float64 `json:"secondary_used_percent"`
	SecondaryResetAfterSeconds int     `json:"secondary_reset_after_seconds"`
	SecondaryWindowMinutes     int     `json:"secondary_window_minutes"`
	// Bengalfox 子限制（模型特定限制）
	BengalfoxPrimaryUsedPercent         float64 `json:"bengalfox_primary_used_percent"`
	BengalfoxPrimaryResetAfterSeconds   int     `json:"bengalfox_primary_reset_after_seconds"`
	BengalfoxPrimaryWindowMinutes       int     `json:"bengalfox_primary_window_minutes"`
	BengalfoxSecondaryUsedPercent       float64 `json:"bengalfox_secondary_used_percent"`
	BengalfoxSecondaryResetAfterSeconds int     `json:"bengalfox_secondary_reset_after_seconds"`
	BengalfoxSecondaryWindowMinutes     int     `json:"bengalfox_secondary_window_minutes"`
	// 元信息
	PlanType    string `json:"plan_type,omitempty"`
	LimitName   string `json:"limit_name,omitempty"`
	ActiveLimit string `json:"active_limit,omitempty"`
	// 积分信息
	CreditsHasCredits bool    `json:"credits_has_credits"`
	CreditsUnlimited  bool    `json:"credits_unlimited"`
	CreditsBalance    float64 `json:"credits_balance"`
	// 快照时间
	CapturedAt time.Time `json:"captured_at"`
}

// parseCodexUsageFromHeaders 从响应头中解析 Codex 用量快照
func parseCodexUsageFromHeaders(h http.Header) *CodexUsageSnapshot {
	primaryStr := h.Get("x-codex-primary-used-percent")
	secondaryStr := h.Get("x-codex-secondary-used-percent")
	if primaryStr == "" && secondaryStr == "" {
		return nil
	}

	parseFloat := func(key string) float64 {
		s := h.Get(key)
		if s == "" {
			return 0
		}
		v, _ := strconv.ParseFloat(s, 64)
		return v
	}
	parseInt := func(key string) int {
		s := h.Get(key)
		if s == "" {
			return 0
		}
		v, _ := strconv.Atoi(s)
		return v
	}

	return &CodexUsageSnapshot{
		PrimaryUsedPercent:                  parseFloat("x-codex-primary-used-percent"),
		PrimaryResetAfterSeconds:            parseInt("x-codex-primary-reset-after-seconds"),
		PrimaryWindowMinutes:                parseInt("x-codex-primary-window-minutes"),
		SecondaryUsedPercent:                parseFloat("x-codex-secondary-used-percent"),
		SecondaryResetAfterSeconds:          parseInt("x-codex-secondary-reset-after-seconds"),
		SecondaryWindowMinutes:              parseInt("x-codex-secondary-window-minutes"),
		BengalfoxPrimaryUsedPercent:         parseFloat("x-codex-bengalfox-primary-used-percent"),
		BengalfoxPrimaryResetAfterSeconds:   parseInt("x-codex-bengalfox-primary-reset-after-seconds"),
		BengalfoxPrimaryWindowMinutes:       parseInt("x-codex-bengalfox-primary-window-minutes"),
		BengalfoxSecondaryUsedPercent:       parseFloat("x-codex-bengalfox-secondary-used-percent"),
		BengalfoxSecondaryResetAfterSeconds: parseInt("x-codex-bengalfox-secondary-reset-after-seconds"),
		BengalfoxSecondaryWindowMinutes:     parseInt("x-codex-bengalfox-secondary-window-minutes"),
		PlanType:                            strings.ToLower(h.Get("x-codex-plan-type")),
		LimitName:                           h.Get("x-codex-bengalfox-limit-name"),
		ActiveLimit:                         h.Get("x-codex-active-limit"),
		CreditsHasCredits:                   strings.EqualFold(h.Get("x-codex-credits-has-credits"), "true"),
		CreditsUnlimited:                    strings.EqualFold(h.Get("x-codex-credits-unlimited"), "true"),
		CreditsBalance:                      parseFloat("x-codex-credits-balance"),
		CapturedAt:                          time.Now(),
	}
}

// parseCodexUsageFromSSEEvent 从 codex.rate_limits SSE 事件中解析用量快照
func parseCodexUsageFromSSEEvent(data []byte) *CodexUsageSnapshot {
	var ev struct {
		RateLimits struct {
			Primary struct {
				UsedPercent       float64 `json:"used_percent"`
				ResetAfterSeconds int     `json:"reset_after_seconds"`
				WindowMinutes     int     `json:"window_minutes"`
			} `json:"primary"`
			Secondary struct {
				UsedPercent       float64 `json:"used_percent"`
				ResetAfterSeconds int     `json:"reset_after_seconds"`
				WindowMinutes     int     `json:"window_minutes"`
			} `json:"secondary"`
		} `json:"rate_limits"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return nil
	}
	rl := ev.RateLimits
	if rl.Primary.UsedPercent == 0 && rl.Secondary.UsedPercent == 0 {
		return nil
	}
	return &CodexUsageSnapshot{
		PrimaryUsedPercent:         rl.Primary.UsedPercent,
		PrimaryResetAfterSeconds:   rl.Primary.ResetAfterSeconds,
		PrimaryWindowMinutes:       rl.Primary.WindowMinutes,
		SecondaryUsedPercent:       rl.Secondary.UsedPercent,
		SecondaryResetAfterSeconds: rl.Secondary.ResetAfterSeconds,
		SecondaryWindowMinutes:     rl.Secondary.WindowMinutes,
		CapturedAt:                 time.Now(),
	}
}

// usageStore 存储每个账号的最新用量快照（accountID → snapshot）
var usageStore sync.Map

// StoreCodexUsage 保存某个账号的用量快照
func StoreCodexUsage(accountID int64, snapshot *CodexUsageSnapshot) {
	if snapshot != nil {
		usageStore.Store(accountID, snapshot)
	}
}

// GetCodexUsage 获取某个账号的最新用量快照
func GetCodexUsage(accountID int64) *CodexUsageSnapshot {
	val, ok := usageStore.Load(accountID)
	if !ok {
		return nil
	}
	return val.(*CodexUsageSnapshot)
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
