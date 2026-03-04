package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// DevAccount 开发用账号（复用 sdk.Account 字段 + 额外元数据）
type DevAccount struct {
	ID          int64             `json:"id"`
	Name        string            `json:"name"`
	AccountType string            `json:"account_type"` // "apikey" 或 "oauth"
	Credentials map[string]string `json:"credentials"`
	ProxyURL    string            `json:"proxy_url,omitempty"`
}

// AccountStore JSON 文件存储
type AccountStore struct {
	mu       sync.RWMutex
	filePath string
	accounts []DevAccount
	nextID   int64
}

func NewAccountStore(filePath string) *AccountStore {
	s := &AccountStore{filePath: filePath, nextID: 1}
	s.load()
	return s
}

func (s *AccountStore) load() {
	data, err := os.ReadFile(s.filePath)
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, &s.accounts)
	for _, a := range s.accounts {
		if a.ID >= s.nextID {
			s.nextID = a.ID + 1
		}
	}
}

func (s *AccountStore) save() {
	// 确保目录存在
	_ = os.MkdirAll(filepath.Dir(s.filePath), 0o755)
	data, _ := json.MarshalIndent(s.accounts, "", "  ")
	_ = os.WriteFile(s.filePath, data, 0o644)
}

func (s *AccountStore) List() []DevAccount {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]DevAccount, len(s.accounts))
	copy(result, s.accounts)
	return result
}

func (s *AccountStore) Get(id int64) *DevAccount {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.accounts {
		if a.ID == id {
			cp := a
			return &cp
		}
	}
	return nil
}

// First 返回第一个账号（代理时简单选取）
func (s *AccountStore) First() *DevAccount {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.accounts) == 0 {
		return nil
	}
	cp := s.accounts[0]
	return &cp
}

func (s *AccountStore) Create(a DevAccount) DevAccount {
	s.mu.Lock()
	defer s.mu.Unlock()
	a.ID = s.nextID
	s.nextID++
	s.accounts = append(s.accounts, a)
	s.save()
	return a
}

func (s *AccountStore) Update(id int64, a DevAccount) *DevAccount {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.accounts {
		if s.accounts[i].ID == id {
			a.ID = id
			s.accounts[i] = a
			s.save()
			return &a
		}
	}
	return nil
}

func (s *AccountStore) Delete(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.accounts {
		if s.accounts[i].ID == id {
			s.accounts = append(s.accounts[:i], s.accounts[i+1:]...)
			s.save()
			return true
		}
	}
	return false
}

// AccountHandler 处理 /api/accounts 路由
type AccountHandler struct {
	store *AccountStore
}

func (h *AccountHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// 解析 ID：/api/accounts/{id}
	idStr := strings.TrimPrefix(r.URL.Path, "/api/accounts/")
	idStr = strings.TrimPrefix(idStr, "/api/accounts")
	idStr = strings.TrimPrefix(idStr, "/")

	switch r.Method {
	case http.MethodGet:
		if idStr == "" {
			json.NewEncoder(w).Encode(h.store.List())
		} else {
			id, _ := strconv.ParseInt(idStr, 10, 64)
			a := h.store.Get(id)
			if a == nil {
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(a)
		}

	case http.MethodPost:
		var a DevAccount
		if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		created := h.store.Create(a)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(created)

	case http.MethodPut:
		id, _ := strconv.ParseInt(idStr, 10, 64)
		var a DevAccount
		if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		updated := h.store.Update(id, a)
		if updated == nil {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(updated)

	case http.MethodDelete:
		id, _ := strconv.ParseInt(idStr, 10, 64)
		if !h.store.Delete(id) {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}
