package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

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
	json.NewEncoder(w).Encode(map[string]string{
		"authorize_url": resp.AuthorizeURL,
		"state":         resp.State,
	})
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
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": account,
	})
}
