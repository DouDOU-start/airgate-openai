package gateway

import "time"

// quotaInfo 是 OpenAI 插件私有的账号订阅探测结果。
//
// SDK v1 不再定义统一额度模型；不同网关的账号标识、订阅字段和用量窗口差异很大，
// 这里仅服务本插件的 OAuth 开发接口和账号页面。
type quotaInfo struct {
	ExpiresAt string            `json:"expires_at,omitempty"`
	Extra     map[string]string `json:"extra,omitempty"`
}

// accountUsageWindow 是 OpenAI 插件私有的账号用量窗口。
type accountUsageWindow struct {
	Key               string  `json:"key"`
	Label             string  `json:"label"`
	UsedPercent       float64 `json:"used_percent"`
	ResetAt           string  `json:"reset_at,omitempty"`
	ResetAfterSeconds int     `json:"reset_after_seconds,omitempty"`
	UpdatedAt         string  `json:"updated_at,omitempty"`
}

type accountUsageCredits struct {
	Balance   float64 `json:"balance"`
	Unlimited bool    `json:"unlimited"`
}

type accountUsageInfo struct {
	UpdatedAt string               `json:"updated_at"`
	Windows   []accountUsageWindow `json:"windows,omitempty"`
	Credits   *accountUsageCredits `json:"credits,omitempty"`
}

type accountUsageError struct {
	ID      int64  `json:"id"`
	Message string `json:"message"`
}

type accountUsageAccountsResponse struct {
	Accounts map[string]accountUsageInfo `json:"accounts"`
	Errors   []accountUsageError         `json:"errors,omitempty"`
}

func resetAtFromBase(base time.Time, resetAfterSeconds int) *time.Time {
	if resetAfterSeconds <= 0 {
		return nil
	}
	if base.IsZero() {
		base = time.Now()
	}
	resetAt := base.Add(time.Duration(resetAfterSeconds) * time.Second)
	return &resetAt
}

func newAccountUsageWindow(key, label string, usedPercent float64, resetAt *time.Time, now time.Time) accountUsageWindow {
	window := accountUsageWindow{
		Key:         key,
		Label:       label,
		UsedPercent: usedPercent,
		UpdatedAt:   now.UTC().Format(time.RFC3339),
	}
	if resetAt != nil {
		window.ResetAt = resetAt.UTC().Format(time.RFC3339)
		if resetAt.After(now) {
			window.ResetAfterSeconds = int(resetAt.Sub(now).Seconds())
		}
	}
	return window
}
