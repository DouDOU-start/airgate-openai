package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/DouDOU-start/airgate-openai/backend/internal/gateway"
)

// OAuthDevHandler 处理 devserver 的 OAuth 路由
type OAuthDevHandler struct {
	gateway *gateway.OpenAIGateway
	store   *AccountStore
}

// HandleStart 处理 POST /api/oauth/start，返回授权链接
func (h *OAuthDevHandler) HandleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	resp, err := h.gateway.StartOAuth(context.Background(), &gateway.OAuthStartRequest{})
	if err != nil {
		log.Printf("StartOAuth 失败: %v", err)
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"authorize_url": resp.AuthorizeURL,
		"state":         resp.State,
	})
}

// HandleCallback 处理 POST /api/oauth/callback
// 用户授权后浏览器会跳转到 localhost:1455/auth/callback?code=xxx&state=yyy
// 由于没有监听 1455，页面会报错，但用户可以从地址栏复制完整 URL
// 前端解析 URL 中的 code 和 state 提交到此接口完成 token 交换
func (h *OAuthDevHandler) HandleCallback(w http.ResponseWriter, r *http.Request) {
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

	result, err := h.gateway.HandleOAuthCallback(context.Background(), &gateway.OAuthCallbackRequest{
		Code:  body.Code,
		State: body.State,
	})
	if err != nil {
		log.Printf("HandleOAuthCallback 失败: %v", err)
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	// 自动创建账号
	name := result.AccountName
	if name == "" {
		name = "OAuth 账号"
	}
	account := h.store.Create(DevAccount{
		Name:        name,
		AccountType: result.AccountType,
		Credentials: result.Credentials,
	})

	log.Printf("OAuth 授权成功，账号已创建: id=%d name=%s", account.ID, account.Name)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": account,
	})
}
