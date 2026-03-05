package gateway

import (
	"fmt"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ──────────────────────────────────────────────────────
// Cache Control 优化（纯 gjson/sjson，零 struct）
// ──────────────────────────────────────────────────────

const (
	// maxCacheBreakpoints Anthropic API 允许的最大 cache_control 断点数
	maxCacheBreakpoints = 4
	// cacheBlockWindow 自适应窗口大小
	cacheBlockWindow = 20
)

// optimizeCacheControlJSON 统一自动优化 cache_control 断点位置
// 操作原始 []byte，返回优化后的 []byte
func optimizeCacheControlJSON(body []byte) []byte {
	body = normalizeStringContents(body)
	body = clearAllCacheControls(body)

	structural := 0

	// ─── 结构锚点：tools 末尾 ───
	if toolsCount := gjson.GetBytes(body, "tools.#").Int(); toolsCount > 0 {
		path := fmt.Sprintf("tools.%d.cache_control", toolsCount-1)
		body, _ = sjson.SetRawBytes(body, path, []byte(`{"type":"ephemeral"}`))
		structural++
	}

	// ─── 结构锚点：system 末尾 ───
	sysResult := gjson.GetBytes(body, "system")
	if sysResult.IsArray() {
		sysCount := sysResult.Get("#").Int()
		if sysCount > 0 {
			path := fmt.Sprintf("system.%d.cache_control", sysCount-1)
			body, _ = sjson.SetRawBytes(body, path, []byte(`{"type":"ephemeral"}`))
			structural++
		}
	} else if sysResult.Type == gjson.String && sysResult.String() != "" {
		// string 形式 system → 归一化为数组再打点
		text := sysResult.String()
		sysArray := fmt.Sprintf(`[{"type":"text","text":%q,"cache_control":{"type":"ephemeral"}}]`, text)
		body, _ = sjson.SetRawBytes(body, "system", []byte(sysArray))
		structural++
	}

	// ─── 消息锚点 ───
	remaining := maxCacheBreakpoints - structural
	if remaining <= 0 {
		return body
	}

	// 收集可缓存消息块路径
	type blockRef struct {
		msgIdx   int
		blockIdx int
	}
	var refs []blockRef

	messages := gjson.GetBytes(body, "messages")
	if messages.IsArray() {
		for mi, msg := range messages.Array() {
			content := msg.Get("content")
			if content.IsArray() {
				for bi, block := range content.Array() {
					if isCacheableBlock(block) {
						refs = append(refs, blockRef{mi, bi})
					}
				}
			}
		}
	}

	// 确定消息锚点数量
	messageAnchors := 0
	if len(refs) > 0 {
		messageAnchors = 1
		if len(refs) >= cacheBlockWindow {
			messageAnchors = 2
		}
	}
	if messageAnchors > remaining {
		messageAnchors = remaining
	}

	if messageAnchors <= 0 || len(refs) == 0 {
		return body
	}

	// 第一锚点：最后一个可缓存块
	last := refs[len(refs)-1]
	path := fmt.Sprintf("messages.%d.content.%d.cache_control", last.msgIdx, last.blockIdx)
	body, _ = sjson.SetRawBytes(body, path, []byte(`{"type":"ephemeral"}`))

	// 第二锚点：末尾前 cacheBlockWindow 个位置
	if messageAnchors > 1 {
		target := len(refs) - 1 - cacheBlockWindow
		if target < 0 {
			target = 0
		}
		// 避免与第一锚点重复
		if target != len(refs)-1 {
			r := refs[target]
			path = fmt.Sprintf("messages.%d.content.%d.cache_control", r.msgIdx, r.blockIdx)
			body, _ = sjson.SetRawBytes(body, path, []byte(`{"type":"ephemeral"}`))
		}
	}

	// ─── 清理不允许缓存的块（thinking/empty text）───
	body = sanitizeUnsupportedCacheControlsJSON(body)

	return body
}

// normalizeStringContents 将 messages 中纯字符串 content 归一化为数组格式
func normalizeStringContents(body []byte) []byte {
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return body
	}
	for i, msg := range messages.Array() {
		content := msg.Get("content")
		if content.Type == gjson.String && content.String() != "" {
			text := content.String()
			arr := `[{"type":"text"}]`
			arr, _ = sjson.Set(arr, "0.text", text)
			body, _ = sjson.SetRawBytes(body, fmt.Sprintf("messages.%d.content", i), []byte(arr))
		}
	}
	return body
}

// clearAllCacheControls 清除所有 cache_control 断点
func clearAllCacheControls(body []byte) []byte {
	// 清理 tools
	if tools := gjson.GetBytes(body, "tools"); tools.IsArray() {
		for i := range tools.Array() {
			body, _ = sjson.DeleteBytes(body, fmt.Sprintf("tools.%d.cache_control", i))
		}
	}

	// 清理 system
	if sys := gjson.GetBytes(body, "system"); sys.IsArray() {
		for i := range sys.Array() {
			body, _ = sjson.DeleteBytes(body, fmt.Sprintf("system.%d.cache_control", i))
		}
	}

	// 清理 messages
	if msgs := gjson.GetBytes(body, "messages"); msgs.IsArray() {
		for mi, msg := range msgs.Array() {
			if content := msg.Get("content"); content.IsArray() {
				for bi := range content.Array() {
					body, _ = sjson.DeleteBytes(body, fmt.Sprintf("messages.%d.content.%d.cache_control", mi, bi))
				}
			}
		}
	}

	return body
}

// sanitizeUnsupportedCacheControlsJSON 清理不允许设置 cache_control 的内容块
func sanitizeUnsupportedCacheControlsJSON(body []byte) []byte {
	if msgs := gjson.GetBytes(body, "messages"); msgs.IsArray() {
		for mi, msg := range msgs.Array() {
			if content := msg.Get("content"); content.IsArray() {
				for bi, block := range content.Array() {
					if !isCacheableBlock(block) && block.Get("cache_control").Exists() {
						body, _ = sjson.DeleteBytes(body, fmt.Sprintf("messages.%d.content.%d.cache_control", mi, bi))
					}
				}
			}
		}
	}
	return body
}

// isCacheableBlock 判断内容块是否可缓存
func isCacheableBlock(block gjson.Result) bool {
	switch block.Get("type").String() {
	case "thinking", "redacted_thinking":
		return false
	case "text":
		return block.Get("text").String() != ""
	default:
		return true
	}
}
