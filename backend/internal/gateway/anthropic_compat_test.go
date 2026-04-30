package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

func TestEstimateAnthropicInputTokensCountsSystemMessagesAndTools(t *testing.T) {
	body := []byte(`{"model":"claude-haiku-4-5","system":"You are concise.","messages":[{"role":"user","content":[{"type":"text","text":"hello world"},{"type":"tool_result","tool_use_id":"toolu_1","content":"搜索结果"}]}],"tools":[{"name":"grep","description":"Search files","input_schema":{"type":"object","properties":{"query":{"type":"string"}}}}],"max_tokens":64}`)

	got := estimateAnthropicInputTokens(body)
	if got <= 40 {
		t.Fatalf("estimateAnthropicInputTokens() = %d, want a non-trivial estimate", got)
	}
}

func TestIsModelFallbackErrorIncludesContextWindowErrors(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		body       []byte
	}{
		{
			name:       "context length code",
			statusCode: http.StatusBadRequest,
			body:       []byte(`{"error":{"code":"context_length_exceeded","message":"Your input exceeds the context window of this model."}}`),
		},
		{
			name:       "input too long",
			statusCode: http.StatusBadRequest,
			body:       []byte(`{"error":{"code":"input_too_long","message":"input is too long"}}`),
		},
		{
			name:       "model unavailable",
			statusCode: http.StatusBadRequest,
			body:       []byte(`{"error":{"message":"The model gpt-x is not supported."}}`),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !isModelFallbackError(tc.statusCode, tc.body) {
				t.Fatalf("isModelFallbackError(%d, %s) = false, want true", tc.statusCode, tc.body)
			}
		})
	}
}

func TestIsModelFallbackErrorRejectsOrdinaryClientErrors(t *testing.T) {
	body := []byte(`{"error":{"message":"Missing required parameter: messages."}}`)
	if isModelFallbackError(http.StatusBadRequest, body) {
		t.Fatalf("ordinary invalid request should not trigger model fallback")
	}
}

func TestForwardAnthropicMessageFallsBackOnContextWindowError(t *testing.T) {
	var models []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		model := gjson.GetBytes(body, "model").String()
		models = append(models, model)
		if len(models) == 1 {
			if model != sparkTargetModel {
				t.Fatalf("first request model = %q, want Spark %q", model, sparkTargetModel)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, `data: {"type":"response.failed","response":{"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Your input exceeds the context window of this model."}}}`+"\n\n")
			return
		}
		if model != sonnetTargetModel {
			t.Fatalf("fallback request model = %q, want %q", model, sonnetTargetModel)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, `data: {"type":"response.output_text.delta","delta":"ok"}`+"\n")
		_, _ = fmt.Fprint(w, `data: {"type":"response.completed","response":{"id":"resp_fallback","model":"`+sonnetTargetModel+`","usage":{"input_tokens":3,"output_tokens":1}}}`+"\n\n")
	}))
	defer ts.Close()

	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":128,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Grep","input":{"pattern":"foo"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"foo.go:1"}]}]}`)
	w := httptest.NewRecorder()
	req := &sdk.ForwardRequest{
		Account: &sdk.Account{ID: time.Now().UnixNano(), Credentials: map[string]string{
			"api_key":  "test-key",
			"base_url": ts.URL,
		}},
		Writer: w,
		Body:   body,
	}
	gateway := &OpenAIGateway{transportPool: NewTransportPool()}
	outcome, err := gateway.forwardAnthropicMessage(context.Background(), req)
	if err != nil {
		t.Fatalf("forwardAnthropicMessage err: %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("outcome kind = %v, want success; reason=%s", outcome.Kind, outcome.Reason)
	}
	if len(models) != 2 {
		t.Fatalf("upstream request count = %d, want 2; models=%v", len(models), models)
	}
	if got := gjson.Get(w.Body.String(), "content.0.text").String(); got != "ok" {
		t.Fatalf("response text = %q, want ok; body=%s", got, w.Body.String())
	}
}

func TestForwardAnthropicCountTokensReturnsEstimate(t *testing.T) {
	w := httptest.NewRecorder()
	req := &sdk.ForwardRequest{
		Writer:  w,
		Headers: http.Header{"X-Forwarded-Path": []string{"/v1/messages/count_tokens"}},
		Body:    []byte(`{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"hello world"}]}`),
	}
	outcome, err := (&OpenAIGateway{}).forwardAnthropicCountTokens(context.Background(), req)
	if err != nil {
		t.Fatalf("forwardAnthropicCountTokens err: %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("outcome kind = %v, want success", outcome.Kind)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := gjson.Get(w.Body.String(), "input_tokens").Int(); got <= 0 {
		t.Fatalf("input_tokens = %d, want > 0; body=%s", got, w.Body.String())
	}
}

func TestNormalizeAnthropicStopReason(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty default", in: "", want: "end_turn"},
		{name: "stop to end_turn", in: "stop", want: "end_turn"},
		{name: "length to max_tokens", in: "length", want: "max_tokens"},
		{name: "tool_calls to tool_use", in: "tool_calls", want: "tool_use"},
		{name: "max_output_tokens to max_tokens", in: "max_output_tokens", want: "max_tokens"},
		{name: "content_filter to refusal", in: "content_filter", want: "refusal"},
		{name: "preserve unknown normalized", in: "  CUSTOM_REASON  ", want: "custom_reason"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeAnthropicStopReason(tc.in)
			if got != tc.want {
				t.Fatalf("normalizeAnthropicStopReason(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseSSEStream_AggregatesReasoningFunctionToolUseAndStopReason(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1"}}`,
		`data: {"type":"response.reasoning_summary_text.delta","delta":"think-"}`,
		`data: {"type":"response.reasoning_summary_text.delta","delta":"step"}`,
		`data: {"type":"response.output_text.delta","delta":"hello"}`,
		`data: {"type":"response.output_item.done","item":{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Wuhan\"}"}}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.4","stop_reason":"tool_calls","usage":{"input_tokens":12,"output_tokens":34,"input_tokens_details":{"cached_tokens":5}}}}`,
		"",
	}, "\n")

	result := ParseSSEStream(strings.NewReader(sse), nil)
	if result.Err != nil {
		t.Fatalf("ParseSSEStream returned err: %v", result.Err)
	}
	if result.ResponseID != "resp_1" {
		t.Fatalf("ResponseID = %q, want %q", result.ResponseID, "resp_1")
	}
	if result.Model != "gpt-5.4" {
		t.Fatalf("Model = %q, want %q", result.Model, "gpt-5.4")
	}
	if result.Text != "hello" {
		t.Fatalf("Text = %q, want %q", result.Text, "hello")
	}
	if result.Reasoning != "think-step" {
		t.Fatalf("Reasoning = %q, want %q", result.Reasoning, "think-step")
	}
	if result.StopReason != "tool_calls" {
		t.Fatalf("StopReason = %q, want %q", result.StopReason, "tool_calls")
	}
	if result.InputTokens != 7 || result.OutputTokens != 34 || result.CachedInputTokens != 5 {
		t.Fatalf("usage = (%d,%d,%d), want (7,34,5)", result.InputTokens, result.OutputTokens, result.CachedInputTokens)
	}
	if len(result.ToolUses) != 1 {
		t.Fatalf("ToolUses len = %d, want 1", len(result.ToolUses))
	}
	if result.ToolUses[0].Type != "tool_use" || result.ToolUses[0].ID != "call_1" {
		t.Fatalf("unexpected tool_use block: %+v", result.ToolUses[0])
	}
	if result.ToolUses[0].Name == nil || *result.ToolUses[0].Name != "get_weather" {
		t.Fatalf("tool_use name = %v, want get_weather", result.ToolUses[0].Name)
	}
	if string(result.ToolUses[0].Input) != `{"city":"Wuhan"}` {
		t.Fatalf("tool_use input = %s, want %s", string(result.ToolUses[0].Input), `{"city":"Wuhan"}`)
	}
}

func TestParseSSEStream_AggregatesWebSearchToolUse(t *testing.T) {
	itemID := fmt.Sprintf("ws_%d", time.Now().UnixNano())
	query := fmt.Sprintf("weather-%d", time.Now().UnixNano())

	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_2"}}`,
		fmt.Sprintf(`data: {"type":"response.output_item.added","item":{"type":"web_search_call","id":%q}}`, itemID),
		fmt.Sprintf(`data: {"type":"response.output_item.done","item":{"type":"web_search_call","id":%q,"action":{"query":%q}}}`, itemID, query),
		`data: {"type":"response.completed","response":{"id":"resp_2","model":"gpt-5.4","stop_reason":"stop","usage":{"input_tokens":2,"output_tokens":3}}}`,
		"",
	}, "\n")

	result := ParseSSEStream(strings.NewReader(sse), nil)
	if result.Err != nil {
		t.Fatalf("ParseSSEStream returned err: %v", result.Err)
	}
	if len(result.ToolUses) != 1 {
		t.Fatalf("ToolUses len = %d, want 1", len(result.ToolUses))
	}
	tool := result.ToolUses[0]
	if tool.Name == nil || *tool.Name != "web_search" {
		t.Fatalf("websearch tool name = %v, want web_search", tool.Name)
	}
	if tool.ID != itemID {
		t.Fatalf("websearch tool id = %q, want %q", tool.ID, itemID)
	}
	wantInput := fmt.Sprintf(`{"query":%q}`, query)
	if string(tool.Input) != wantInput {
		t.Fatalf("websearch input = %s, want %s", string(tool.Input), wantInput)
	}
}

// 上游只通过 delta 下发文本、response.completed 里 output 为空的场景（真实 ChatGPT WebSocket 会话行为）
// 回退必须把 wsResult.Text 补到非流式响应的 content 数组里，否则客户端拿到空 content。
func TestConvertResponsesCompletedToAnthropicJSON_FallbackFromDeltas(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_abc123"}}`,
		`data: {"type":"response.reasoning_summary_text.delta","delta":"think-"}`,
		`data: {"type":"response.reasoning_summary_text.delta","delta":"step"}`,
		`data: {"type":"response.output_text.delta","delta":"你好"}`,
		`data: {"type":"response.output_text.delta","delta":"，世界"}`,
		`data: {"type":"response.completed","response":{"id":"resp_abc123","model":"gpt-5.4","usage":{"input_tokens":7,"output_tokens":13}}}`,
		"",
	}, "\n")

	result := ParseSSEStream(strings.NewReader(sse), nil)
	if result.Err != nil {
		t.Fatalf("ParseSSEStream err: %v", result.Err)
	}
	if result.Text != "你好，世界" {
		t.Fatalf("aggregated text = %q, want %q", result.Text, "你好，世界")
	}

	jsonOut := convertResponsesCompletedToAnthropicJSON(
		result.CompletedEventRaw,
		nil,
		"claude-sonnet-4-6",
		&result,
	)
	if jsonOut == "" {
		t.Fatalf("convertResponsesCompletedToAnthropicJSON returned empty")
	}

	// id 前缀必须从 resp_ 规范化为 msg_
	if id := gjson.Get(jsonOut, "id").String(); id != "msg_abc123" {
		t.Fatalf("id = %q, want msg_abc123", id)
	}
	if model := gjson.Get(jsonOut, "model").String(); model != "claude-sonnet-4-6" {
		t.Fatalf("model = %q, want claude-sonnet-4-6", model)
	}
	if sr := gjson.Get(jsonOut, "stop_reason").String(); sr != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn", sr)
	}

	// content 必须非空，并且应包含 thinking + text 两个块
	contentLen := gjson.Get(jsonOut, "content.#").Int()
	if contentLen < 2 {
		t.Fatalf("content length = %d, want >= 2, full=%s", contentLen, jsonOut)
	}
	if got := gjson.Get(jsonOut, "content.0.type").String(); got != "thinking" {
		t.Fatalf("content[0].type = %q, want thinking", got)
	}
	if got := gjson.Get(jsonOut, "content.0.thinking").String(); got != "think-step" {
		t.Fatalf("content[0].thinking = %q, want think-step", got)
	}
	if got := gjson.Get(jsonOut, "content.1.type").String(); got != "text" {
		t.Fatalf("content[1].type = %q, want text", got)
	}
	if got := gjson.Get(jsonOut, "content.1.text").String(); got != "你好，世界" {
		t.Fatalf("content[1].text = %q, want %q", got, "你好，世界")
	}

	// usage 字段要带齐 4 个 token 字段
	if inp := gjson.Get(jsonOut, "usage.input_tokens").Int(); inp != 7 {
		t.Fatalf("usage.input_tokens = %d, want 7", inp)
	}
	if out := gjson.Get(jsonOut, "usage.output_tokens").Int(); out != 13 {
		t.Fatalf("usage.output_tokens = %d, want 13", out)
	}
}

func TestEnsureAnthropicStopReason(t *testing.T) {
	cases := map[string]string{
		"end_turn":      "end_turn",
		"max_tokens":    "max_tokens",
		"stop_sequence": "stop_sequence",
		"tool_use":      "tool_use",
		"refusal":       "refusal",
		"pause_turn":    "pause_turn",
		// 非法值统一降级为 end_turn
		"":               "end_turn",
		"some_garbage":   "end_turn",
		"content_filter": "end_turn", // 未经 normalize 的原始值不在白名单里
	}
	for in, want := range cases {
		if got := ensureAnthropicStopReason(in); got != want {
			t.Errorf("ensureAnthropicStopReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGenerateAnthropicRequestID_Format(t *testing.T) {
	for i := 0; i < 5; i++ {
		id := generateAnthropicRequestID()
		if !strings.HasPrefix(id, "req_01") {
			t.Fatalf("request id %q missing req_01 prefix", id)
		}
		// req_01 + 32 字符 hex = 38 字符
		if len(id) != 38 {
			t.Fatalf("request id %q length = %d, want 38", id, len(id))
		}
	}
}

func TestGenerateCloudflareRay_Format(t *testing.T) {
	for i := 0; i < 5; i++ {
		ray := generateCloudflareRay()
		if !strings.HasSuffix(ray, "-SJC") {
			t.Fatalf("cf-ray %q missing -SJC suffix", ray)
		}
		// 16 字符 hex + "-SJC" = 20 字符
		if len(ray) != 20 {
			t.Fatalf("cf-ray %q length = %d, want 20", ray, len(ray))
		}
	}
}

// 验证流式 message_start 事件后紧跟 ping 事件，对齐 Claude 官方行为
func TestConvertResponsesEventToAnthropic_MessageStartEmitsPing(t *testing.T) {
	state := &anthropicStreamState{}
	line := []byte(`data: {"type":"response.created","response":{"id":"resp_xyz","model":"gpt-5.4"}}`)
	out := convertResponsesEventToAnthropic(line, nil, state, "claude-sonnet-4-6")

	if !strings.Contains(out, "event: message_start") {
		t.Fatalf("missing message_start event, got: %s", out)
	}
	if !strings.Contains(out, "event: ping") {
		t.Fatalf("missing ping event, got: %s", out)
	}
	if !strings.Contains(out, `"type":"ping"`) {
		t.Fatalf("ping event payload wrong, got: %s", out)
	}
	// message_start 必须在 ping 前面
	msi := strings.Index(out, "message_start")
	pi := strings.Index(out, "ping")
	if msi < 0 || pi < 0 || msi > pi {
		t.Fatalf("message_start must come before ping, got: %s", out)
	}
	// id 前缀规范化
	if !strings.Contains(out, `"id":"msg_xyz"`) {
		t.Fatalf("message id not normalized to msg_ prefix, got: %s", out)
	}
	// usage 必须包含 Claude Code usage 累加器要求的完整字段：
	// - service_tier 字段（原生 Anthropic 下发）
	// - cache_creation 嵌套对象（Mo$ 合并函数直接访问 .ephemeral_1h_input_tokens，缺失会崩）
	if !strings.Contains(out, `"service_tier":"standard"`) {
		t.Fatalf("message_start usage missing service_tier, got: %s", out)
	}
	if !strings.Contains(out, `"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0}`) {
		t.Fatalf("message_start usage missing cache_creation nested object, got: %s", out)
	}
	// 但绝不能包含 server_tool_use: null —— 会触发 JS `||` 短路转 undefined 后访问 .input_tokens 崩溃
	if strings.Contains(out, `"server_tool_use":null`) {
		t.Fatalf("message_start usage must NOT contain server_tool_use:null (triggers SDK undefined.input_tokens), got: %s", out)
	}
}

func TestNormalizeAnthropicMessageID(t *testing.T) {
	cases := map[string]string{
		"":                              "",
		"resp_abc123":                   "msg_abc123",
		"msg_xyz":                       "msg_xyz",
		"  resp_trim  ":                 "msg_trim",
		"resp_0a530ec6a62d78460169df00": "msg_0a530ec6a62d78460169df00",
		"unknown_prefix_99":             "msg_unknown_prefix_99",
	}
	for in, want := range cases {
		if got := normalizeAnthropicMessageID(in); got != want {
			t.Errorf("normalizeAnthropicMessageID(%q) = %q, want %q", in, got, want)
		}
	}
}
