package session

import (
	"crypto/rand"
	"fmt"
	"sync"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk"
	"github.com/tidwall/gjson"
)

// ──────────────────────────────────────────────────────
// Session 缓存管理（为 sub2api 提供 sticky session 路由）
// ──────────────────────────────────────────────────────

type entry struct {
	ID     string
	Expire time.Time
}

var (
	sessionMap  = make(map[string]entry)
	sessionMu   sync.RWMutex
	sessionTTL  = 1 * time.Hour
	cleanupOnce sync.Once
)

// GetOrCreate 查找或创建 session 缓存条目
// cacheKey 格式: "model-userID" 或 "model-accountHash"
func GetOrCreate(cacheKey string) string {
	cleanupOnce.Do(startCleanup)

	// 快路径：读锁查找
	sessionMu.RLock()
	if e, ok := sessionMap[cacheKey]; ok && time.Now().Before(e.Expire) {
		sessionMu.RUnlock()
		return e.ID
	}
	sessionMu.RUnlock()

	// 慢路径：写锁创建
	sessionMu.Lock()
	defer sessionMu.Unlock()

	// double-check
	if e, ok := sessionMap[cacheKey]; ok && time.Now().Before(e.Expire) {
		return e.ID
	}

	e := entry{
		ID:     newUUID(),
		Expire: time.Now().Add(sessionTTL),
	}
	sessionMap[cacheKey] = e
	return e.ID
}

// startCleanup 启动后台清理协程（每 15 分钟清理过期条目）
func startCleanup() {
	go func() {
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			sessionMu.Lock()
			now := time.Now()
			for k, v := range sessionMap {
				if now.After(v.Expire) {
					delete(sessionMap, k)
				}
			}
			sessionMu.Unlock()
		}
	}()
}

// DeriveID 从请求上下文派生稳定的 session ID
// 优先使用 Anthropic metadata.user_id，回退到 account 凭证 hash
func DeriveID(originalBody []byte, account *sdk.Account, modelName string) string {
	// 优先从 Anthropic 请求的 metadata.user_id 提取
	userID := gjson.GetBytes(originalBody, "metadata.user_id").String()

	var cacheKey string
	if userID != "" {
		cacheKey = fmt.Sprintf("%s-%s", modelName, userID)
	} else {
		// 回退：使用 account api_key 前 16 字符 + model 构造稳定 key
		apiKey := account.Credentials["api_key"]
		keyPrefix := apiKey
		if len(keyPrefix) > 16 {
			keyPrefix = keyPrefix[:16]
		}
		cacheKey = fmt.Sprintf("%s-%s", modelName, keyPrefix)
	}

	return GetOrCreate(cacheKey)
}

// newUUID 生成 UUID v4（不依赖外部库）
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
