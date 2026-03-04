package gateway

// ──────────────────────────────────────────────────────
// Cache Control 优化
// 参考 AxonHub llm/transformer/anthropic/ensure_cache_control.go
// ──────────────────────────────────────────────────────

const (
	// maxCacheControlBreakpoints Anthropic API 允许的最大 cache_control 断点数
	maxCacheControlBreakpoints = 4
	// adaptiveCacheControlBlockWindow 自适应窗口大小
	adaptiveCacheControlBlockWindow = 20
)

// optimizeCacheControl 统一自动优化 cache_control 断点位置：
//   - 先清空全部断点，再按固定规划重建
//   - 结构锚点：tools(last) + system(last)
//   - 消息锚点：短内容 1 个、长内容 2 个（受 4 个上限约束）
//   - thinking 与空 text 不允许打点
func optimizeCacheControl(req *AnthropicMessageRequest) {
	normalizeMessageContents(req)
	clearCacheControls(req)

	structural := ensureStructuralCacheControls(req)

	remaining := maxCacheControlBreakpoints - structural
	if remaining <= 0 {
		return
	}

	refs := collectMessageBlockRefs(req)
	messageAnchors := min(desiredMessageCacheAnchors(len(refs)), remaining)
	injectPlannedMessageCacheControls(refs, messageAnchors)

	sanitizeUnsupportedCacheControls(req)
}

// normalizeMessageContents 将 Messages 中的纯字符串 Content 统一归一化为 MultipleContent 数组格式
func normalizeMessageContents(req *AnthropicMessageRequest) {
	for i := range req.Messages {
		msg := &req.Messages[i]
		if len(msg.Content.MultipleContent) == 0 && msg.Content.Content != nil && *msg.Content.Content != "" {
			text := *msg.Content.Content
			msg.Content.Content = nil
			msg.Content.MultipleContent = []AnthropicMessageContentBlock{{
				Type: "text",
				Text: &text,
			}}
		}
	}
}

// ensureStructuralCacheControls 确保 tools 和 system 的最后一个元素有 cache_control
func ensureStructuralCacheControls(req *AnthropicMessageRequest) int {
	count := 0

	if len(req.Tools) > 0 {
		req.Tools[len(req.Tools)-1].CacheControl = &AnthropicCacheControl{Type: "ephemeral"}
		count++
	}

	if req.System == nil {
		return count
	}

	if len(req.System.MultiplePrompts) > 0 {
		last := len(req.System.MultiplePrompts) - 1
		req.System.MultiplePrompts[last].CacheControl = &AnthropicCacheControl{Type: "ephemeral"}
		count++
		return count
	}

	// system 是字符串形式时归一化为 MultiplePrompts 数组格式
	if req.System.Prompt != nil && *req.System.Prompt != "" {
		text := *req.System.Prompt
		req.System.Prompt = nil
		req.System.MultiplePrompts = []AnthropicSystemPromptPart{{
			Type:         "text",
			Text:         text,
			CacheControl: &AnthropicCacheControl{Type: "ephemeral"},
		}}
		count++
	}

	return count
}

// sanitizeUnsupportedCacheControls 清理不允许设置 cache_control 的内容块
func sanitizeUnsupportedCacheControls(req *AnthropicMessageRequest) {
	for i := range req.Messages {
		for j := range req.Messages[i].Content.MultipleContent {
			block := &req.Messages[i].Content.MultipleContent[j]
			if !isCacheableMessageBlock(*block) && block.CacheControl != nil {
				block.CacheControl = nil
			}
		}
	}
}

func desiredMessageCacheAnchors(cacheableBlocks int) int {
	if cacheableBlocks == 0 {
		return 0
	}
	if cacheableBlocks >= adaptiveCacheControlBlockWindow {
		return 2
	}
	return 1
}

func injectPlannedMessageCacheControls(refs []**AnthropicCacheControl, target int) {
	if target <= 0 || len(refs) == 0 {
		return
	}

	// 第一优先级：最后一个可缓存块
	*refs[len(refs)-1] = &AnthropicCacheControl{Type: "ephemeral"}

	// 第二优先级：末尾前 20 blocks 的窗口边界
	if target > 1 {
		idx := pickWindowAnchorIndex(refs, adaptiveCacheControlBlockWindow)
		if idx >= 0 {
			*refs[idx] = &AnthropicCacheControl{Type: "ephemeral"}
		}
	}
}

// pickWindowAnchorIndex 选择距离末尾 window 个位置的未标记锚点
func pickWindowAnchorIndex(refs []**AnthropicCacheControl, window int) int {
	if len(refs) == 0 {
		return -1
	}
	if window < 0 {
		window = 0
	}

	target := max(len(refs)-1-window, 0)

	// 优先选择目标窗口左侧
	for i := target; i >= 0; i-- {
		if *refs[i] != nil {
			continue
		}
		return i
	}

	for i := target + 1; i < len(refs); i++ {
		if *refs[i] != nil {
			continue
		}
		return i
	}

	return -1
}

// collectMessageBlockRefs 收集所有消息中可缓存块的 CacheControl 指针引用
func collectMessageBlockRefs(req *AnthropicMessageRequest) []**AnthropicCacheControl {
	var refs []**AnthropicCacheControl
	for i := range req.Messages {
		for j := range req.Messages[i].Content.MultipleContent {
			if !isCacheableMessageBlock(req.Messages[i].Content.MultipleContent[j]) {
				continue
			}
			refs = append(refs, &req.Messages[i].Content.MultipleContent[j].CacheControl)
		}
	}
	return refs
}

// clearCacheControls 清除所有 cache_control 断点
func clearCacheControls(req *AnthropicMessageRequest) {
	for i := range req.Tools {
		req.Tools[i].CacheControl = nil
	}
	if req.System != nil {
		for i := range req.System.MultiplePrompts {
			req.System.MultiplePrompts[i].CacheControl = nil
		}
	}
	for i := range req.Messages {
		msg := &req.Messages[i]
		for j := range msg.Content.MultipleContent {
			msg.Content.MultipleContent[j].CacheControl = nil
		}
	}
}

// isCacheableMessageBlock 判断内容块是否可缓存
func isCacheableMessageBlock(block AnthropicMessageContentBlock) bool {
	switch block.Type {
	case "thinking", "redacted_thinking":
		return false
	case "text":
		return block.Text != nil && *block.Text != ""
	default:
		return true
	}
}
