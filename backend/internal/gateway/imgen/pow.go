package imgen

import (
	"bytes"
	crand "crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/sha3"
)

// GenerateRequirementsToken 生成 requirements token（发送到 chat-requirements 的 "p" 字段）。
// 18 元素配置数组，固定难度 "0fffff"，前缀 "gAAAAAC"，最多 500k 次迭代。
func GenerateRequirementsToken(userAgent string) string {
	seed := randHex(32)
	difficulty := "0fffff"
	diffBytes, _ := hex.DecodeString(difficulty)
	diffLen := len(diffBytes) // 3 bytes
	maxIter := 500_000
	prefix := "gAAAAAC"

	now := time.Now()
	screenVar := rand.Intn(17) + 16 + rand.Intn(3001) + 3000
	dpl := fmt.Sprintf("dpl_%s", randHex(32))
	uid := uuid.New().String()

	for i := 0; i < maxIter; i++ {
		config := []any{
			screenVar,             // [0] screen resolution variant
			"Asia/Shanghai",       // [1] timezone
			nil,                   // [2]
			rand.Float64(),        // [3] random float (iterated)
			userAgent,             // [4] user agent
			nil,                   // [5]
			dpl,                   // [6] dpl cookie
			"en-US",               // [7]
			"en-US,zh-CN",         // [8]
			i,                     // [9] counter (iterated)
			"webdriver",           // [10] navigator key
			"location",            // [11]
			"__cf_chl_opt",        // [12] window key
			rand.Intn(9901) + 100, // [13] performance counter
			uid,                   // [14] random UUID
			"",                    // [15] empty string
			8,                     // [16]
			now.Unix(),            // [17] unix timestamp
		}

		configJSON, _ := json.Marshal(config)
		configB64 := base64.StdEncoding.EncodeToString(configJSON)

		h := sha3.New512()
		h.Write([]byte(seed))
		h.Write([]byte(configB64))
		hash := h.Sum(nil)

		// bytes 级比较
		if bytes.Compare(hash[:diffLen], diffBytes) <= 0 {
			return prefix + configB64
		}
	}

	return prefix + "wQ8Lk5FbGpA2NcR9dShT6gYjU7VxZ4D" + base64.StdEncoding.EncodeToString([]byte(seed))
}

// SolveProofToken 解算 ProofToken（用于 Openai-Sentinel-Proof-Token 请求头）。
// 13 元素配置数组，可变难度，前缀 "gAAAAAB"，最多 100k 次迭代。
func SolveProofToken(seed, difficulty, userAgent string) string {
	if seed == "" || difficulty == "" {
		return ""
	}

	diffLen := len(difficulty)
	maxIter := 100_000
	prefix := "gAAAAAB"

	now := time.Now()
	dateStr := now.Format("Mon Jan 02 2006 15:04:05") + " GMT+0800 (China Standard Time)"

	screens := []string{"4032x2268", "3360x1890", "2560x1440", "1920x1080", "3840x2160"}
	screen := screens[rand.Intn(len(screens))]
	dpl := fmt.Sprintf("dpl_%s", randHex(32))

	for i := 0; i < maxIter; i++ {
		config := []any{
			screen,    // [0] 屏幕分辨率
			dateStr,   // [1] 日期字符串
			nil,       // [2]
			i,         // [3] 迭代计数器
			userAgent, // [4] UA
			"/backend-api/sentinel/chat-requirements", // [5]
			dpl,     // [6] dpl cookie
			"en",    // [7]
			"en-US", // [8]
			nil,     // [9]
			"[]",    // [10] plugins
			"r",     // [11]
			"p",     // [12]
		}

		configJSON, _ := json.Marshal(config)
		configB64 := base64.StdEncoding.EncodeToString(configJSON)

		h := sha3.New512()
		h.Write([]byte(seed))
		h.Write([]byte(configB64))
		hashBytes := h.Sum(nil)
		hashHex := hex.EncodeToString(hashBytes)

		if hashHex[:diffLen] <= difficulty {
			return prefix + configB64
		}
	}

	return prefix + "wQ8Lk5FbGpA2NcR9dShT6gYjU7VxZ4D" + base64.StdEncoding.EncodeToString([]byte(seed))
}

func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = crand.Read(b)
	return hex.EncodeToString(b)[:n]
}
