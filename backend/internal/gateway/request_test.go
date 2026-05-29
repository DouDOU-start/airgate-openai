package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

// TestIsAnthropicRequest 只认两个权威信号：X-Forwarded-Path + Anthropic-Version 头。
// body 启发式已废除（见 isAnthropicRequest 注释）。
func TestIsAnthropicRequest(t *testing.T) {
	cases := []struct {
		name    string
		headers http.Header
		body    []byte
		want    bool
	}{
		// path 命中 Anthropic
		{
			name:    "path=/v1/messages",
			headers: http.Header{"X-Forwarded-Path": []string{"/v1/messages"}},
			body:    []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"}],"max_tokens":4}`),
			want:    true,
		},
		{
			name:    "path=/v1/messages/count_tokens（子路径）",
			headers: http.Header{"X-Forwarded-Path": []string{"/v1/messages/count_tokens"}},
			body:    []byte(`{"model":"claude","messages":[]}`),
			want:    true,
		},
		{
			name:    "path=/v1/messages?foo=bar（带 query）",
			headers: http.Header{"X-Forwarded-Path": []string{"/v1/messages?foo=bar"}},
			body:    nil,
			want:    true,
		},
		// 子串匹配防漏点
		{
			name:    "path=/v1/messages-custom 非 Anthropic 派生前缀",
			headers: http.Header{"X-Forwarded-Path": []string{"/v1/messages-custom"}},
			body:    nil,
			want:    false,
		},
		{
			name:    "query 里夹杂 /v1/messages 字样不应触发",
			headers: http.Header{"X-Forwarded-Path": []string{"/v1/chat/completions?referer=/v1/messages"}},
			body:    nil,
			want:    false,
		},
		// path 命中 OpenAI
		{
			name:    "path=/v1/chat/completions",
			headers: http.Header{"X-Forwarded-Path": []string{"/v1/chat/completions"}},
			body:    []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}],"max_tokens":4}`),
			want:    false,
		},
		{
			name:    "path=/v1/responses",
			headers: http.Header{"X-Forwarded-Path": []string{"/v1/responses"}},
			body:    []byte(`{"model":"gpt-5.4","input":"hi"}`),
			want:    false,
		},
		// 头部兜底
		{
			name:    "Anthropic-Version 头",
			headers: http.Header{"Anthropic-Version": []string{"2023-06-01"}},
			body:    []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"}],"max_tokens":4}`),
			want:    true,
		},
		// 不再依靠 body 启发——body 有 Anthropic 风味但没 path/header 信号时，默认 OpenAI
		{
			name:    "body 有 top-level system 但无 path/header → 默认 OpenAI",
			headers: nil,
			body:    []byte(`{"model":"x","system":"You are helpful","messages":[{"role":"user","content":"hi"}],"max_tokens":4}`),
			want:    false,
		},
		{
			name:    "OpenAI chat.completions 无 path/header（之前会被误判，回归用例）",
			headers: nil,
			body:    []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`),
			want:    false,
		},
		{
			name:    "OpenAI vision 带 content block 数组（以前会误判）",
			headers: nil,
			body:    []byte(`{"model":"gpt-4-vision","messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"..."}}]}],"max_tokens":4}`),
			want:    false,
		},
		{
			name:    "空 body + 无 headers",
			headers: nil,
			body:    nil,
			want:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &sdk.ForwardRequest{Headers: tc.headers, Body: tc.body}
			if got := isAnthropicRequest(req); got != tc.want {
				t.Errorf("isAnthropicRequest() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestApplyContinuationStateBackfillsPreviousResponseIDForToolOutput(t *testing.T) {
	reqBody := map[string]any{
		"input": []any{
			map[string]any{
				"type":    "function_call_output",
				"call_id": "call_1",
				"output":  "ok",
			},
		},
	}

	session := openAISessionResolution{PreviousRespID: "resp_prev"}
	reqBody = applyContinuationState(reqBody, session)
	if got, _ := reqBody["previous_response_id"].(string); got != "resp_prev" {
		t.Fatalf("previous_response_id = %q, want resp_prev", got)
	}
}

func TestApplyContinuationStateDoesNotBackfillWithToolCallContext(t *testing.T) {
	reqBody := map[string]any{
		"input": []any{
			map[string]any{
				"type":    "function_call",
				"call_id": "call_1",
				"name":    "lookup",
			},
			map[string]any{
				"type":    "function_call_output",
				"call_id": "call_1",
				"output":  "ok",
			},
		},
	}

	session := openAISessionResolution{PreviousRespID: "resp_prev"}
	reqBody = applyContinuationState(reqBody, session)
	if got, _ := reqBody["previous_response_id"].(string); got != "" {
		t.Fatalf("expected previous_response_id to stay empty when tool call context is present, got %q", got)
	}
}

func TestNormalizeOpenAIServiceTier_FastIsInvalid(t *testing.T) {
	if got := normalizeOpenAIServiceTier("fast"); got != "" {
		t.Fatalf("normalizeOpenAIServiceTier(fast) = %q, want empty", got)
	}
}

func TestNormalizeOpenAIWireServiceTier_FastIsInvalid(t *testing.T) {
	if got := normalizeOpenAIWireServiceTier("fast"); got != "" {
		t.Fatalf("normalizeOpenAIWireServiceTier(fast) = %q, want empty", got)
	}
}

func TestEnsureResponsesDefaultsWithTier_FastIgnored(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","input":"hi"}`)
	result := ensureResponsesDefaultsWithTier(body, "fast")

	var payload map[string]any
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, ok := payload["service_tier"]; ok {
		t.Fatalf("service_tier should be omitted for fast, got %v", payload["service_tier"])
	}
}

func TestApplyOpenAIWireServiceTier_FastRemoved(t *testing.T) {
	result := applyOpenAIWireServiceTier([]byte(`{"model":"gpt-5.5","input":"hi","service_tier":"fast"}`))

	var payload map[string]any
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, ok := payload["service_tier"]; ok {
		t.Fatalf("service_tier should be removed for fast, got %v", payload["service_tier"])
	}
}

func TestPreprocessRequestBody_PreservesConversationImageDataURLs(t *testing.T) {
	imageRef := largeConversationImageDataURL(t)
	cases := []struct {
		name       string
		path       string
		body       []byte
		resultPath string
	}{
		{
			name:       "chat completions",
			path:       "/v1/chat/completions",
			body:       []byte(fmt.Sprintf(`{"model":"gpt-5.4","messages":[{"role":"user","content":[{"type":"text","text":"描述图片"},{"type":"image_url","image_url":{"url":%q}}]}]}`, imageRef)),
			resultPath: "messages.0.content.1.image_url.url",
		},
		{
			name:       "responses input",
			path:       "/v1/responses",
			body:       []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"描述图片"},{"type":"input_image","image_url":%q}]}]}`, imageRef)),
			resultPath: "input.0.content.1.image_url",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := preprocessRequestBody(tc.body, "gpt-5.4", tc.path)
			if gotImage := gjson.GetBytes(got, tc.resultPath).String(); gotImage != imageRef {
				t.Fatalf("conversation image should stay unchanged, got %.32q", gotImage)
			}
		})
	}
}

func largeConversationImageDataURL(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1024, 1024))
	for y := 0; y < 1024; y++ {
		for x := 0; x < 1024; x++ {
			v := uint32(x)*1103515245 + uint32(y)*12345
			img.SetRGBA(x, y, color.RGBA{R: uint8(v), G: uint8(v >> 8), B: uint8(v >> 16), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.NoCompression}).Encode(&buf, img); err != nil {
		t.Fatalf("png encode failed: %v", err)
	}
	ref := "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
	if n := len(decodeDataURLBytes(t, ref)); n <= maxResponsesInputImageBytes {
		t.Fatalf("测试图片过小：%d <= %d", n, maxResponsesInputImageBytes)
	}
	return ref
}

func decodeDataURLBytes(t *testing.T, ref string) []byte {
	t.Helper()
	comma := strings.IndexByte(ref, ',')
	if comma < 0 {
		t.Fatalf("data URL 缺少逗号：%.32q", ref)
	}
	raw := ref[comma+1:]
	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(raw)
		if err != nil {
			t.Fatalf("base64 解码失败：%v", err)
		}
	}
	return data
}

func TestWrapAsResponsesAPIPreservesChatImageURL(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"describe this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]}]}`)
	result, err := wrapAsResponsesAPI(body, "gpt-5.4")
	if err != nil {
		t.Fatalf("wrapAsResponsesAPI: %v", err)
	}

	content := gjson.GetBytes(result, "input.0.content")
	if got := content.Get("0.type").String(); got != "input_text" {
		t.Fatalf("first content type = %q, want input_text", got)
	}
	if got := content.Get("1.type").String(); got != "input_image" {
		t.Fatalf("second content type = %q, want input_image", got)
	}
	if got := content.Get("1.image_url").String(); got != "data:image/png;base64,AAA" {
		t.Fatalf("image_url = %q", got)
	}
}

func TestWrapAsResponsesAPIToolResultOutputIsString(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"北京\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":[{"type":"text","text":"sunny"}]}]}`)
	result, err := wrapAsResponsesAPI(body, "gpt-5.4")
	if err != nil {
		t.Fatalf("wrapAsResponsesAPI: %v", err)
	}

	output := gjson.GetBytes(result, "input.1.output")
	if output.Type != gjson.String {
		t.Fatalf("function_call_output.output type = %v, want string; body=%s", output.Type, result)
	}
	if output.String() != "sunny" {
		t.Fatalf("function_call_output.output = %q, want sunny", output.String())
	}
}

func TestNormalizeResponsesInputPreservesChatImageURL(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":[{"type":"text","text":"describe this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]}]}`)
	result := normalizeResponsesInput(body, "/v1/responses")

	if gjson.GetBytes(result, "messages").Exists() {
		t.Fatalf("messages should be removed after conversion: %s", result)
	}
	content := gjson.GetBytes(result, "input.0.content")
	if got := content.Get("0.type").String(); got != "input_text" {
		t.Fatalf("first content type = %q, want input_text", got)
	}
	if got := content.Get("1.type").String(); got != "input_image" {
		t.Fatalf("second content type = %q, want input_image", got)
	}
	if got := content.Get("1.image_url").String(); got != "data:image/png;base64,AAA" {
		t.Fatalf("image_url = %q", got)
	}
}

func TestPreprocessRequestBody_ForcesResponsesStoreFalse(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{
			name: "responses input",
			body: []byte(`{"model":"gpt-5.4","input":"hi","store":true}`),
		},
		{
			name: "responses messages",
			body: []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}],"store":true}`),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := preprocessRequestBody(tc.body, "gpt-5.4", "/v1/responses")
			if store := gjson.GetBytes(got, "store"); !store.Exists() || store.Bool() {
				t.Fatalf("store = %v, want false; body=%s", store.Value(), got)
			}
		})
	}
}

func TestPreprocessRequestBodyPreservesPreviousResponseID(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_old","input":[]}`)
	got := preprocessRequestBody(body, "gpt-5.4", "/v1/responses")
	if previous := gjson.GetBytes(got, "previous_response_id").String(); previous != "resp_old" {
		t.Fatalf("previous_response_id = %q, want resp_old; body=%s", previous, got)
	}
}

func TestFirstNonEmptyTier_RequestFastFallsBackToUpstreamPriority(t *testing.T) {
	if got := firstNonEmptyTier("fast", "priority"); got != "priority" {
		t.Fatalf("firstNonEmptyTier(fast, priority) = %q, want %q", got, "priority")
	}
}
