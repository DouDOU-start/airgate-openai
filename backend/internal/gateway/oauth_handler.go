package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/DouDOU-start/airgate-sdk/devserver"
)

// OAuthDevHandler devserver 的 OAuth HTTP handler
type OAuthDevHandler struct {
	Gateway *OpenAIGateway
	Store   *devserver.AccountStore
}

// RegisterRoutes 注册 OAuth 路由到 mux
func (h *OAuthDevHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/oauth/start", h.handleStart)
	mux.HandleFunc("/api/oauth/callback", h.handleCallback)
	mux.HandleFunc("/api/accounts/quota/", h.handleQuota)
	mux.HandleFunc("/api/accounts/usage/", h.handleUsage)
}

// handleStart 处理 POST /api/oauth/start，返回授权链接
func (h *OAuthDevHandler) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	resp, err := h.Gateway.StartOAuth(context.Background(), &OAuthStartRequest{})
	if err != nil {
		log.Printf("StartOAuth 失败: %v", err)
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]string{
		"authorize_url": resp.AuthorizeURL,
		"state":         resp.State,
	}); err != nil {
		log.Printf("编码 OAuth start 响应失败: %v", err)
	}
}

// handleCallback 处理 POST /api/oauth/callback
func (h *OAuthDevHandler) handleCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		Code  string `json:"code"`
		State string `json:"state"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Code == "" || body.State == "" {
		http.Error(w, `{"error":"缺少 code 或 state 参数"}`, http.StatusBadRequest)
		return
	}

	result, err := h.Gateway.HandleOAuthCallback(context.Background(), &OAuthCallbackRequest{
		Code:  body.Code,
		State: body.State,
	})
	if err != nil {
		log.Printf("HandleOAuthCallback 失败: %v", err)
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	name := result.AccountName
	if name == "" {
		name = "OAuth 账号"
	}
	account := h.Store.Create(devserver.DevAccount{
		Name:        name,
		AccountType: result.AccountType,
		Credentials: result.Credentials,
	})

	log.Printf("OAuth 授权成功，账号已创建: id=%d name=%s", account.ID, account.Name)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": account,
	}); err != nil {
		log.Printf("编码 OAuth callback 响应失败: %v", err)
	}
}

// handleQuota 处理 GET /api/accounts/quota/{id}，查询账号订阅信息
func (h *OAuthDevHandler) handleQuota(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	// 从路径提取账号 ID
	idStr := strings.TrimPrefix(r.URL.Path, "/api/accounts/quota/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, `{"error":"invalid account id"}`, http.StatusBadRequest)
		return
	}

	account := h.Store.Get(id)
	if account == nil {
		http.Error(w, `{"error":"account not found"}`, http.StatusNotFound)
		return
	}

	// 优先尝试实时查询（通过刷新 token）
	quota, queryErr := h.Gateway.QueryQuota(context.Background(), account.Credentials)
	if queryErr == nil && quota != nil {
		// 查询成功，更新存储中的凭证（token 可能已刷新）
		updated := false
		if newAT := quota.Extra["access_token"]; newAT != "" && newAT != account.Credentials["access_token"] {
			account.Credentials["access_token"] = newAT
			updated = true
		}
		if newRT := quota.Extra["refresh_token"]; newRT != "" && newRT != account.Credentials["refresh_token"] {
			account.Credentials["refresh_token"] = newRT
			updated = true
		}
		if pt := quota.Extra["plan_type"]; pt != "" {
			account.Credentials["plan_type"] = pt
			updated = true
		}
		if quota.ExpiresAt != "" {
			account.Credentials["subscription_active_until"] = quota.ExpiresAt
			updated = true
		}
		if updated {
			h.Store.Update(id, *account)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"plan_type":                 quota.Extra["plan_type"],
			"subscription_active_until": quota.ExpiresAt,
			"source":                    "realtime",
		})
		return
	}

	// 实时查询失败，回退到 credentials 中的缓存值
	log.Printf("实时查询额度失败 (id=%d): %v，使用缓存值", id, queryErr)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"plan_type":                 account.Credentials["plan_type"],
		"subscription_active_until": account.Credentials["subscription_active_until"],
		"source":                    "cached",
	})
}

// handleUsage 处理 GET /api/accounts/usage/{id}，返回 Codex 用量快照
// 如果内存中没有缓存，主动发一个轻量探测请求来获取用量头
func (h *OAuthDevHandler) handleUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	idStr := strings.TrimPrefix(r.URL.Path, "/api/accounts/usage/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, `{"error":"invalid account id"}`, http.StatusBadRequest)
		return
	}

	account := h.Store.Get(id)
	if account == nil {
		http.Error(w, `{"error":"account not found"}`, http.StatusNotFound)
		return
	}

	// 先检查缓存，无缓存时主动探测
	snapshot := GetCodexUsage(id)
	if snapshot == nil {
		snapshot = h.Gateway.ProbeUsage(r.Context(), id, account.Credentials)
	}

	w.Header().Set("Content-Type", "application/json")
	if snapshot == nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"available": false,
		})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"available": true,
		"usage":     snapshot,
	})
}
