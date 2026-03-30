package gateway

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

const cacheDebugLogPath = "/tmp/gateway-openai-cache-debug.log"

var cacheDebugMu sync.Mutex
var cacheDebugCleanupOnce sync.Once

func cacheDebugEnabledNow() bool {
	// Temporary diagnostics window for cache analysis.
	// Expire after 2026-04-03 local time.
	expireAt := time.Date(2026, 4, 4, 0, 0, 0, 0, time.Local)
	return time.Now().Before(expireAt)
}

func cleanupExpiredCacheDebugLog() {
	cacheDebugCleanupOnce.Do(func() {
		_ = os.Remove(cacheDebugLogPath)
	})
}

func appendCacheDebugLog(label string, kv ...any) {
	if !cacheDebugEnabledNow() {
		cleanupExpiredCacheDebugLog()
		return
	}

	cacheDebugMu.Lock()
	defer cacheDebugMu.Unlock()

	f, err := os.OpenFile(cacheDebugLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	ts := time.Now().Format(time.RFC3339Nano)
	_, _ = fmt.Fprintf(f, "ts=%s label=%s", ts, label)
	for i := 0; i+1 < len(kv); i += 2 {
		_, _ = fmt.Fprintf(f, " %v=%q", kv[i], fmt.Sprint(kv[i+1]))
	}
	_, _ = fmt.Fprintln(f)
}

func summarizeResponsesInputPrefix(body []byte, limit int) string {
	if limit <= 0 {
		return ""
	}
	items := gjson.GetBytes(body, "input").Array()
	if len(items) == 0 {
		return ""
	}
	if len(items) > limit {
		items = items[:limit]
	}
	out := ""
	for i, item := range items {
		if i > 0 {
			out += " | "
		}
		itemType := item.Get("type").String()
		role := item.Get("role").String()
		callID := item.Get("call_id").String()
		name := item.Get("name").String()
		sig := shortHashBytes([]byte(item.Raw))
		out += fmt.Sprintf("%d:%s:%s:%s:%s:%s", i, itemType, role, callID, name, sig)
	}
	return out
}
