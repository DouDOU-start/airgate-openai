package gateway

import (
	"encoding/json"
	"fmt"
)

// ──────────────────────────────────────────────────────
// Anthropic Messages API 数据模型
// 参考 AxonHub llm/transformer/anthropic/model.go
// ──────────────────────────────────────────────────────

// AnthropicMessageRequest Anthropic Messages API 请求格式
type AnthropicMessageRequest struct {
	MaxTokens int64                  `json:"max_tokens"`
	Messages  []AnthropicMsgParam    `json:"messages"`
	Model     string                 `json:"model,omitempty"`

	AnthropicVersion string   `json:"anthropic_version,omitempty"`
	AnthropicBeta    []string `json:"anthropic_beta,omitempty"`

	Temperature   *float64 `json:"temperature,omitempty"`
	TopK          *int64   `json:"top_k,omitempty"`
	TopP          *float64 `json:"top_p,omitempty"`

	Metadata      *AnthropicMetadata `json:"metadata,omitempty"`
	ServiceTier   string             `json:"service_tier,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`

	System       *AnthropicSystemPrompt `json:"system,omitempty"`
	Thinking     *AnthropicThinking     `json:"thinking,omitempty"`
	OutputConfig *AnthropicOutputConfig `json:"output_config,omitempty"`

	Tools      []AnthropicTool        `json:"tools,omitempty"`
	ToolChoice *AnthropicToolChoice   `json:"tool_choice,omitempty"`

	Stream *bool `json:"stream,omitempty"`
}

type AnthropicMetadata struct {
	UserID string `json:"user_id,omitempty"`
}

// ──────────────────────────────────────────────────────
// System Prompt（支持 string/array 双格式）
// ──────────────────────────────────────────────────────

type AnthropicSystemPrompt struct {
	Prompt          *string                     `json:"-"`
	MultiplePrompts []AnthropicSystemPromptPart `json:"-"`
}

func (s *AnthropicSystemPrompt) MarshalJSON() ([]byte, error) {
	if s.Prompt != nil {
		return json.Marshal(s.Prompt)
	}
	if len(s.MultiplePrompts) > 0 {
		return json.Marshal(s.MultiplePrompts)
	}
	return []byte("null"), nil
}

func (s *AnthropicSystemPrompt) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		s.Prompt = &str
		return nil
	}
	var parts []AnthropicSystemPromptPart
	if err := json.Unmarshal(data, &parts); err == nil {
		s.MultiplePrompts = parts
		return nil
	}
	return fmt.Errorf("invalid system prompt format")
}

type AnthropicSystemPromptPart struct {
	Type         string                `json:"type"`
	Text         string                `json:"text"`
	CacheControl *AnthropicCacheControl `json:"cache_control,omitempty"`
}

// ──────────────────────────────────────────────────────
// Thinking / OutputConfig
// ──────────────────────────────────────────────────────

type AnthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int64  `json:"budget_tokens,omitempty"`
}

type AnthropicOutputConfig struct {
	Effort string `json:"effort,omitempty"`
}

// ──────────────────────────────────────────────────────
// Tool 定义
// ──────────────────────────────────────────────────────

type AnthropicToolChoice struct {
	Type                   string  `json:"type"`
	DisableParallelToolUse *bool   `json:"disable_parallel_tool_use,omitempty"`
	Name                   *string `json:"name,omitempty"`
}

type AnthropicTool struct {
	Type         string          `json:"type,omitempty"`
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema,omitempty"`
	CacheControl *AnthropicCacheControl `json:"cache_control,omitempty"`

	// web_search_20250305 参数
	MaxUses        *int64                          `json:"max_uses,omitempty"`
	Strict         *bool                           `json:"strict,omitempty"`
	AllowedDomains []string                        `json:"allowed_domains,omitempty"`
	BlockedDomains []string                        `json:"blocked_domains,omitempty"`
	UserLocation   AnthropicWebSearchUserLocation  `json:"user_location,omitempty"`
}

type AnthropicWebSearchUserLocation struct {
	City     string `json:"city,omitempty"`
	Country  string `json:"country,omitempty"`
	Region   string `json:"region,omitempty"`
	Timezone string `json:"timezone,omitempty"`
	Type     string `json:"type,omitempty"`
}

// ──────────────────────────────────────────────────────
// CacheControl
// ──────────────────────────────────────────────────────

type AnthropicCacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

// ──────────────────────────────────────────────────────
// Message（消息，支持 string/array 双格式）
// ──────────────────────────────────────────────────────

type AnthropicMsgParam struct {
	Role    string                  `json:"role"`
	Content AnthropicMessageContent `json:"content"`
}

type AnthropicMessageContent struct {
	Content         *string                      `json:"-"`
	MultipleContent []AnthropicMessageContentBlock `json:"-"`
}

func (c AnthropicMessageContent) MarshalJSON() ([]byte, error) {
	if c.Content != nil {
		return json.Marshal(c.Content)
	}
	return json.Marshal(c.MultipleContent)
}

func (c *AnthropicMessageContent) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return fmt.Errorf("content cannot be null")
	}
	var blocks []AnthropicMessageContentBlock
	if err := json.Unmarshal(data, &blocks); err == nil {
		c.MultipleContent = blocks
		return nil
	}
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		c.Content = &str
		return nil
	}
	return fmt.Errorf("invalid content type")
}

// AnthropicMessageContentBlock 内容块
type AnthropicMessageContentBlock struct {
	// text, image, thinking, redacted_thinking, tool_use, server_tool_use, tool_result
	Type string `json:"type"`

	// text
	Text *string `json:"text,omitempty"`

	// thinking
	Thinking  *string `json:"thinking,omitempty"`
	Signature *string `json:"signature,omitempty"`

	// redacted_thinking
	Data string `json:"data,omitempty"`

	// image
	Source *AnthropicImageSource `json:"source,omitempty"`

	// tool_use / server_tool_use
	ID           string          `json:"id,omitempty"`
	Name         *string         `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	CacheControl *AnthropicCacheControl `json:"cache_control,omitempty"`

	// tool_result
	ToolUseID *string                  `json:"tool_use_id,omitempty"`
	Content   *AnthropicMessageContent `json:"content,omitempty"`
	IsError   *bool                    `json:"is_error,omitempty"`
}

// AnthropicImageSource 图片来源
type AnthropicImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// ──────────────────────────────────────────────────────
// 响应模型
// ──────────────────────────────────────────────────────

// AnthropicMessage 非流式响应
type AnthropicMessage struct {
	ID           string                         `json:"id"`
	Type         string                         `json:"type"`
	Role         string                         `json:"role"`
	Content      []AnthropicMessageContentBlock `json:"content"`
	Model        string                         `json:"model"`
	StopReason   *string                        `json:"stop_reason,omitempty"`
	StopSequence *string                        `json:"stop_sequence,omitempty"`
	Usage        *AnthropicUsage                `json:"usage,omitempty"`
}

// ──────────────────────────────────────────────────────
// 流式事件模型
// ──────────────────────────────────────────────────────

// AnthropicStreamEvent SSE 事件
type AnthropicStreamEvent struct {
	Type         string                        `json:"type"`
	Message      *AnthropicStreamMessage       `json:"message,omitempty"`
	Index        *int64                        `json:"index,omitempty"`
	ContentBlock *AnthropicMessageContentBlock `json:"content_block,omitempty"`
	Delta        *AnthropicStreamDelta         `json:"delta,omitempty"`
	Usage        *AnthropicUsage               `json:"usage,omitempty"`
}

type AnthropicStreamDelta struct {
	Type         *string `json:"type,omitempty"`
	Text         *string `json:"text,omitempty"`
	PartialJSON  *string `json:"partial_json,omitempty"`
	Thinking     *string `json:"thinking,omitempty"`
	Signature    *string `json:"signature,omitempty"`
	StopReason   *string `json:"stop_reason,omitempty"`
	StopSequence *string `json:"stop_sequence,omitempty"`
}

type AnthropicStreamMessage struct {
	ID      string                         `json:"id"`
	Type    string                         `json:"type"`
	Role    string                         `json:"role"`
	Content []AnthropicMessageContentBlock `json:"content"`
	Model   string                         `json:"model"`
	Usage   *AnthropicUsage                `json:"usage,omitempty"`
}

// ──────────────────────────────────────────────────────
// Usage
// ──────────────────────────────────────────────────────

type AnthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

// ──────────────────────────────────────────────────────
// 错误模型
// ──────────────────────────────────────────────────────

type AnthropicErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type AnthropicErrorResponse struct {
	Type  string               `json:"type"`
	Error AnthropicErrorDetail `json:"error"`
}
