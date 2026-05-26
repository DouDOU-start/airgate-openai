package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

func testPNGDataURL(width, height int, pixel func(int, int) color.RGBA) string {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetRGBA(x, y, pixel(x, y))
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func testPNGBase64(width, height int, pixel func(int, int) color.RGBA) string {
	dataURL := testPNGDataURL(width, height, pixel)
	return strings.TrimPrefix(dataURL, "data:image/png;base64,")
}

func testPNGBytes(width, height int, pixel func(int, int) color.RGBA) []byte {
	b64 := testPNGBase64(width, height, pixel)
	data, _ := base64.StdEncoding.DecodeString(b64)
	return data
}

func testPNGDataURLFromImage(img image.Image) string {
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestIsImagesRequest(t *testing.T) {
	cases := []struct {
		path     string
		want     bool
		wantEdit bool
	}{
		{"/v1/images/generations", true, false},
		{"/images/generations", true, false},
		{"/v1/responses", false, false},
		{"/v1/chat/completions", false, false},
		{"/v1/images/edits", true, true},
		{"/images/edits", true, true},
		{"", false, false},
	}
	for _, tc := range cases {
		if got := isImagesRequest(tc.path); got != tc.want {
			t.Errorf("isImagesRequest(%q) = %v, want %v", tc.path, got, tc.want)
		}
		if got := isImagesEditRequest(tc.path); got != tc.wantEdit {
			t.Errorf("isImagesEditRequest(%q) = %v, want %v", tc.path, got, tc.wantEdit)
		}
	}
}

func TestPrefersAsyncResponse(t *testing.T) {
	if prefersAsyncResponse(http.Header{"Content-Type": []string{"application/json"}}) {
		t.Fatal("plain Images API request should keep the standard synchronous response")
	}

	for _, headers := range []http.Header{
		{"Prefer": []string{"respond-async"}},
		{"Prefer": []string{"wait=60, respond-async"}},
		{"Prefer": []string{"respond-async; handling=lenient"}},
	} {
		if !prefersAsyncResponse(headers) {
			t.Fatalf("Prefer header should opt into async response: %+v", headers)
		}
	}
}

func TestImageTaskLocation(t *testing.T) {
	const taskID = "018f2f8a-9f8a-7c11-9f2a-8a9f8a7c119f"
	if got := imageTaskLocation("/v1/images/generations", taskID); got != "/v1/images/tasks?task_id="+taskID {
		t.Fatalf("v1 location = %q", got)
	}
	if got := imageTaskLocation("/images/generations", taskID); got != "/images/tasks?task_id="+taskID {
		t.Fatalf("non-v1 location = %q", got)
	}
}

// TestShouldUseImagesWebReverse pins WebReverse off across every previously
// matching shape. WebReverse is deprecated — kept compiled, but never picked.
// If someone re-enables it, this test will flag every legacy-positive case so
// the rollout is deliberate.
func TestShouldUseImagesWebReverse(t *testing.T) {
	cases := []struct {
		name    string
		account *sdk.Account
		model   string
	}{
		{"free oauth with gpt-image-2 (legacy positive)", &sdk.Account{Credentials: map[string]string{
			"access_token": "token",
			"plan_type":    "free",
		}}, "gpt-image-2"},
		{"plus oauth with gpt-image-2", &sdk.Account{Credentials: map[string]string{
			"access_token": "token",
			"plan_type":    "plus",
		}}, "gpt-image-2"},
		{"oauth without plan type", &sdk.Account{Credentials: map[string]string{
			"access_token": "token",
		}}, "gpt-image-2"},
		{"free oauth with other model", &sdk.Account{Credentials: map[string]string{
			"access_token": "token",
			"plan_type":    "free",
		}}, "gpt-image-1"},
		{"apikey account", &sdk.Account{Credentials: map[string]string{
			"api_key": "sk-test",
		}}, "gpt-image-2"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldUseImagesWebReverse(tc.account, tc.model); got {
				t.Fatalf("shouldUseImagesWebReverse() = true; WebReverse is deprecated, must be false")
			}
		})
	}
}

func TestApplyWebReverseSizeHint(t *testing.T) {
	const prompt = "draw a cat"
	cases := []struct {
		name string
		size string
		want string
	}{
		{
			name: "frontend landscape size",
			size: "2048x1360",
			want: "Generate a landscape image at 2048x1360 resolution. draw a cat",
		},
		{
			name: "portrait size",
			size: "1024x1536",
			want: "Generate a portrait image at 1024x1536 resolution. draw a cat",
		},
		{
			name: "square size",
			size: "1024x1024",
			want: "Generate a square image at 1024x1024 resolution. draw a cat",
		},
		{
			name: "spaced uppercase separator",
			size: " 2048 X 1360 ",
			want: "Generate a landscape image at 2048x1360 resolution. draw a cat",
		},
		{
			name: "oversized landscape is clamped",
			size: "4096x2304",
			want: "Generate a landscape image at 3840x2160 resolution. draw a cat",
		},
		{
			name: "oversized portrait is clamped",
			size: "2304x4096",
			want: "Generate a portrait image at 2160x3840 resolution. draw a cat",
		},
		{
			name: "ratio is ignored",
			size: "16:9",
			want: prompt,
		},
		{
			name: "invalid size is ignored",
			size: "0x1024",
			want: prompt,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := applyWebReverseSizeHint(prompt, tc.size); got != tc.want {
				t.Fatalf("applyWebReverseSizeHint() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestHandleImagesResponse_TokenAttribution 覆盖官方响应格式：
//   - usage.input_tokens / output_tokens 落入 Outcome.Usage
//   - cached tokens 从 input 中扣减，避免重复计费
//   - API Key Images 按请求 size 分档计费
func TestHandleImagesResponse_TokenAttribution(t *testing.T) {
	body := `{
		"created": 1713833628,
		"data": [{"b64_json": "iVBORw0..."}],
		"usage": {
			"total_tokens": 4210,
			"input_tokens": 50,
			"output_tokens": 4160,
			"input_tokens_details": {"text_tokens": 50, "cached_tokens": 10}
		}
	}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       ioNopCloserFromString(body),
	}
	w := httptest.NewRecorder()

	outcome, err := handleImagesResponse(resp, w, nil, time.Now(), "gpt-image-1.5", imagesResponseOptions{BillingSize: "2048x2048"})
	if err != nil {
		t.Fatalf("handleImagesResponse returned err: %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("Kind = %v, want Success", outcome.Kind)
	}
	u := outcome.Usage
	if u == nil {
		t.Fatal("Usage = nil, want non-nil")
	}
	if u.Model != "gpt-image-1.5" {
		t.Errorf("Model = %q, want gpt-image-1.5", u.Model)
	}
	if got := usageMetricInt(u, usageMetricInputTokens); got != 40 {
		t.Errorf("input_tokens = %d, want 40 (50 - 10 cached)", got)
	}
	if got := usageMetricInt(u, usageMetricOutputTokens); got != 4160 {
		t.Errorf("output_tokens = %d, want 4160", got)
	}
	if got := usageMetricInt(u, usageMetricCachedInputTokens); got != 10 {
		t.Errorf("cached_input_tokens = %d, want 10", got)
	}

	if got := usageCostByKey(u, usageCostInput); !almostEqual(got, 0, 1e-9) {
		t.Errorf("input cost = %v, want 0 (per-image billing)", got)
	}
	if !almostEqual(u.AccountCost, 0.20, 1e-9) {
		t.Errorf("AccountCost = %v, want 0.20 (1 image × 2K tier $0.20)", u.AccountCost)
	}

	if w.Code != http.StatusOK {
		t.Errorf("writer status = %d, want 200", w.Code)
	}
	gotBody, _ := io.ReadAll(w.Result().Body)
	if got := gjson.GetBytes(gotBody, "data.0.quality").String(); got != "medium" {
		t.Errorf("response quality = %q, want medium", got)
	}
}

func TestWriteSSEPingUsesOpenAIStyleEvent(t *testing.T) {
	w := httptest.NewRecorder()
	writeSSEPing(w)

	if got, want := w.Body.String(), "event: ping\ndata: {}\n\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestStartSSEPingKeepAliveDoesNotCommitImmediately(t *testing.T) {
	w := httptest.NewRecorder()
	sseKA := startSSEPingKeepAlive(w)
	sseKA.Stop()

	if sseKA.Wrote() {
		t.Fatalf("首个 ping 前不应标记流已写入")
	}
	if body := w.Body.String(); body != "" {
		t.Fatalf("body = %q，首个 ping 前应为空", body)
	}
	if got := w.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
}

func TestHandleImagesResponse_StreamWrapsRESTJSONAsSSE(t *testing.T) {
	body := `{"created":1713833628,"data":[{"b64_json":"iVBORw0"}],"usage":{"input_tokens":1,"output_tokens":2}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       ioNopCloserFromString(body),
	}
	w := httptest.NewRecorder()
	sseKA := startSSEPingKeepAlive(w)

	outcome, err := handleImagesResponse(resp, w, sseKA, time.Now(), "gpt-image-1")
	if err != nil {
		t.Fatalf("handleImagesResponse returned err: %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("Kind = %v, want Success", outcome.Kind)
	}
	if outcome.Upstream.Body != nil {
		t.Fatalf("Upstream.Body = %q, want nil for streamed response", outcome.Upstream.Body)
	}
	if got := outcome.Upstream.Headers.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("writer status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("writer Content-Type = %q, want text/event-stream", got)
	}
	gotBody := w.Body.String()
	wantBody := "data: {\"created\":1713833628,\"data\":[{\"b64_json\":\"iVBORw0\",\"quality\":\"medium\"}],\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}\n\ndata: [DONE]\n\n"
	if gotBody != wantBody {
		t.Fatalf("writer body = %q, want %q", gotBody, wantBody)
	}
}

func TestHandleImagesResponse_APIKeyBillingUsesRequestSize(t *testing.T) {
	body := `{"data":[{"url":"https://example/a.png"},{"url":"https://example/b.png"}],"usage":{"input_tokens":10,"output_tokens":100}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       ioNopCloserFromString(body),
	}

	outcome, err := handleImagesResponse(resp, nil, nil, time.Now(), "gpt-image-1.5", imagesResponseOptions{BillingSize: "3840x2160"})
	if err != nil {
		t.Fatalf("handleImagesResponse returned err: %v", err)
	}
	if got, want := outcome.Usage.AccountCost, 0.80; !almostEqual(got, want, 1e-9) {
		t.Fatalf("AccountCost = %v, want %v (2 images × 4K tier $0.40)", got, want)
	}
	if got, want := usageImageUnitPrice(outcome.Usage), "0.4"; got != want {
		t.Fatalf("image unit_price = %q, want %q", got, want)
	}
}

func TestHandleImagesResponse_NonStreamReturnsBodyWithoutWriter(t *testing.T) {
	body := `{"data":[{"url":"https://example/a.png"}],"usage":{"input_tokens":10,"output_tokens":100}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       ioNopCloserFromString(body),
	}

	outcome, err := handleImagesResponse(resp, nil, nil, time.Now(), "gpt-image-1")
	if err != nil {
		t.Fatalf("handleImagesResponse returned err: %v", err)
	}
	if got := gjson.GetBytes(outcome.Upstream.Body, "data.0.quality").String(); got != "medium" {
		t.Fatalf("Upstream.Body data[0].quality = %q, want medium", got)
	}
	if got := outcome.Upstream.Headers.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
}

func TestHandleImagesResponse_RequestQualityOverridesResponseEcho(t *testing.T) {
	body := `{"quality":"low","data":[{"url":"https://example/a.png","quality":"low"},{"url":"https://example/b.png"}],"usage":{"input_tokens":10,"output_tokens":100}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":   []string{"application/json"},
			"Content-Length": []string{fmt.Sprint(len(body))},
		},
		Body: ioNopCloserFromString(body),
	}

	outcome, err := handleImagesResponse(resp, nil, nil, time.Now(), "gpt-image-2", imagesResponseOptions{RequestQuality: "high"})
	if err != nil {
		t.Fatalf("handleImagesResponse returned err: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(outcome.Upstream.Body, &got); err != nil {
		t.Fatalf("unmarshal response body: %v", err)
	}
	if got["quality"] != "high" {
		t.Fatalf("root quality = %v, want high", got["quality"])
	}
	data := got["data"].([]any)
	for i, item := range data {
		obj := item.(map[string]any)
		if obj["quality"] != "high" {
			t.Fatalf("data[%d].quality = %v, want high", i, obj["quality"])
		}
	}
	if got, want := outcome.Upstream.Headers.Get("Content-Length"), fmt.Sprint(len(outcome.Upstream.Body)); got != want {
		t.Fatalf("Content-Length = %q, want rewritten length %q", got, want)
	}
}

func TestHandleImagesResponse_DefaultQualityEchoesMedium(t *testing.T) {
	body := `{"quality":"low","data":[{"url":"https://example/a.png","quality":"low"}],"usage":{"input_tokens":10,"output_tokens":100}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       ioNopCloserFromString(body),
	}

	outcome, err := handleImagesResponse(resp, nil, nil, time.Now(), "gpt-image-2")
	if err != nil {
		t.Fatalf("handleImagesResponse returned err: %v", err)
	}
	if got := gjson.GetBytes(outcome.Upstream.Body, "quality").String(); got != "medium" {
		t.Fatalf("root quality = %q, want medium", got)
	}
	if got := gjson.GetBytes(outcome.Upstream.Body, "data.0.quality").String(); got != "medium" {
		t.Fatalf("data[0].quality = %q, want medium", got)
	}
}

func TestHandleImagesResponse_AutoQualityEchoesMedium(t *testing.T) {
	body := `{"quality":"low","data":[{"url":"https://example/a.png","quality":"low"}],"usage":{"input_tokens":10,"output_tokens":100}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       ioNopCloserFromString(body),
	}

	outcome, err := handleImagesResponse(resp, nil, nil, time.Now(), "gpt-image-2", imagesResponseOptions{RequestQuality: "auto"})
	if err != nil {
		t.Fatalf("handleImagesResponse returned err: %v", err)
	}
	if got := gjson.GetBytes(outcome.Upstream.Body, "quality").String(); got != "medium" {
		t.Fatalf("root quality = %q, want medium", got)
	}
	if got := gjson.GetBytes(outcome.Upstream.Body, "data.0.quality").String(); got != "medium" {
		t.Fatalf("data[0].quality = %q, want medium", got)
	}
}

// TestHandleImagesResponse_FallbackModelWhenBodyLacksModel 验证 Images 响应里
// 没有 model 字段时，会回退到请求侧传入的 fallbackModel，避免 fillUsageCost 查不到定价。
func TestHandleImagesResponse_FallbackModelWhenBodyLacksModel(t *testing.T) {
	body := `{"data":[{"url":"https://example/a.png"}],"usage":{"input_tokens":10,"output_tokens":100}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       ioNopCloserFromString(body),
	}

	outcome, err := handleImagesResponse(resp, nil, nil, time.Now(), "gpt-image-1")
	if err != nil {
		t.Fatalf("handleImagesResponse returned err: %v", err)
	}
	if outcome.Usage == nil || outcome.Usage.Model != "gpt-image-1" {
		t.Fatalf("Usage.Model = %q, want gpt-image-1 (fallback)", outcome.Usage.Model)
	}
	// Writer 为 nil 时 Upstream.Body/Headers 应带回给 core
	if got := gjson.GetBytes(outcome.Upstream.Body, "data.0.quality").String(); got != "medium" {
		t.Errorf("Upstream.Body data[0].quality = %q, want medium", got)
	}
	if outcome.Upstream.Headers.Get("Content-Type") != "application/json" {
		t.Errorf("Upstream.Headers Content-Type not preserved")
	}
	if outcome.Usage.AccountCost <= 0 {
		t.Errorf("AccountCost = %v, want > 0", outcome.Usage.AccountCost)
	}
}

// TestFillUsageCostPerImageBySize_1K 按尺寸分档计费 1K。
func TestFillUsageCostPerImageBySize_1K(t *testing.T) {
	usage := &sdk.Usage{Model: "gpt-image-1"}
	fillUsageCostPerImageBySize(usage, 3, "1024x1024")
	// 3 张 × $0.10 = 0.30
	if !almostEqual(usage.AccountCost, 0.30, 1e-9) {
		t.Errorf("AccountCost = %v, want 0.30", usage.AccountCost)
	}
}

// TestImageActualSizeFromBase64 验证从 base64 PNG header 解码出真实宽高。
// auto 计费场景关键 fallback：上游若不返 size 字段，靠这个保证按真图计费。
func TestImageActualSizeFromBase64(t *testing.T) {
	// 生成 1536×1024 的真 PNG，编 base64
	b64 := testPNGBase64(1536, 1024, func(x, y int) color.RGBA {
		return color.RGBA{R: 100, G: 100, B: 100, A: 255}
	})
	got, ok := imageActualSizeFromBase64(b64)
	if !ok {
		t.Fatal("imageActualSizeFromBase64 = ok=false, want true")
	}
	if got != "1536x1024" {
		t.Errorf("size = %q, want 1536x1024", got)
	}
	// 异常 base64 应返回 false
	if _, ok := imageActualSizeFromBase64("not-valid-base64!!!"); ok {
		t.Error("invalid base64 should return ok=false")
	}
	// 空字符串
	if _, ok := imageActualSizeFromBase64(""); ok {
		t.Error("empty string should return ok=false")
	}
}

// TestImagePriceForSize 覆盖 1K/2K/4K 三档 + auto/空/异常 fallback。
func TestImagePriceForSize(t *testing.T) {
	cases := []struct {
		size string
		want float64
	}{
		// 1K (≤1536)
		{"1024x1024", 0.10},
		{"1536x1024", 0.10},
		{"1024x1536", 0.10},
		// 2K (1537-2048)
		{"2048x2048", 0.20},
		{"2048x1152", 0.20},
		{"1152x2048", 0.20},
		// 4K (>2048)
		{"3840x2160", 0.40},
		{"2160x3840", 0.40},
		// fallback 1K
		{"", 0.10},
		{"auto", 0.10},
		{"garbage", 0.10},
	}
	for _, tc := range cases {
		if got := imagePriceForSize(tc.size); !almostEqual(got, tc.want, 1e-9) {
			t.Errorf("imagePriceForSize(%q) = %v, want %v", tc.size, got, tc.want)
		}
	}
}

// TestFillUsageCostPerImageBySize 验证按张 × 档位单价填到 OutputCost。
func TestFillUsageCostPerImageBySize(t *testing.T) {
	cases := []struct {
		name      string
		size      string
		numImages int
		want      float64
	}{
		{"1K single", "1024x1024", 1, 0.10},
		{"1K triple", "1536x1024", 3, 0.30},
		{"2K single", "2048x2048", 1, 0.20},
		{"4K double", "3840x2160", 2, 0.80},
		{"auto fallback to 1K", "auto", 4, 0.40},
		{"zero images skipped", "1024x1024", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usage := &sdk.Usage{Model: "gpt-image-2"}
			fillUsageCostPerImageBySize(usage, tc.numImages, tc.size)
			if !almostEqual(usage.AccountCost, tc.want, 1e-9) {
				t.Errorf("AccountCost = %v, want %v", usage.AccountCost, tc.want)
			}
			if !almostEqual(usageCostByKey(usage, usageCostInput), 0, 1e-9) {
				t.Errorf("input cost = %v, want 0", usageCostByKey(usage, usageCostInput))
			}
		})
	}
}

func usageCostByKey(usage *sdk.Usage, key string) float64 {
	if usage == nil {
		return 0
	}
	for _, detail := range usage.CostDetails {
		if detail.Key == key {
			return detail.AccountCost
		}
	}
	return 0
}

func usageImageUnitPrice(usage *sdk.Usage) string {
	if usage == nil {
		return ""
	}
	for _, detail := range usage.CostDetails {
		if detail.Key == usageCostImage {
			return detail.Metadata["unit_price"]
		}
	}
	return ""
}

func almostEqual(a, b, eps float64) bool {
	return math.Abs(a-b) <= eps
}

// TestParseUsage_ToolImageGen 验证 parseUsage 从 JSON body 中提取
// tool_usage.image_gen 的 input/output tokens。
func TestParseUsage_ToolImageGen(t *testing.T) {
	body := []byte(`{
		"usage": {"input_tokens": 100, "output_tokens": 50, "input_tokens_details": {"cached_tokens": 20}},
		"tool_usage": {"image_gen": {"input_tokens": 5, "output_tokens": 4160}}
	}`)
	got := parseUsage(body)
	if got.inputTokens != 80 { // 100 - 20 cached
		t.Errorf("inputTokens = %d, want 80", got.inputTokens)
	}
	if got.outputTokens != 50 {
		t.Errorf("outputTokens = %d, want 50", got.outputTokens)
	}
	if got.cachedInputTokens != 20 {
		t.Errorf("cachedInputTokens = %d, want 20", got.cachedInputTokens)
	}
	if got.toolImageInputTokens != 5 {
		t.Errorf("toolImageInputTokens = %d, want 5", got.toolImageInputTokens)
	}
	if got.toolImageOutputTokens != 4160 {
		t.Errorf("toolImageOutputTokens = %d, want 4160", got.toolImageOutputTokens)
	}
}

func TestParseUsage_ImageGenerationCallSummary(t *testing.T) {
	b64 := strings.TrimPrefix(testPNGDataURL(1024, 1536, func(x, y int) color.RGBA {
		return color.RGBA{R: 1, G: 2, B: 3, A: 255}
	}), "data:image/png;base64,")
	body := []byte(fmt.Sprintf(`{
		"usage": {"input_tokens": 100, "output_tokens": 50},
		"output": [{
			"type": "image_generation_call",
			"result": %q,
			"size": "1024x1536",
			"quality": "high"
		}]
	}`, b64))
	got := parseUsage(body)
	if got.imageGenCallCount != 1 {
		t.Fatalf("imageGenCallCount = %d, want 1", got.imageGenCallCount)
	}
	if got.imageGenCallSize != "1024x1536" {
		t.Fatalf("imageGenCallSize = %q, want 1024x1536", got.imageGenCallSize)
	}
}

func TestCollectImageGenCallSummary(t *testing.T) {
	b64 := strings.TrimPrefix(testPNGDataURL(1024, 1024, func(x, y int) color.RGBA {
		return color.RGBA{R: 5, G: 6, B: 7, A: 255}
	}), "data:image/png;base64,")
	event := []byte(fmt.Sprintf(`{
		"type":"response.output_item.done",
		"item":{
			"type":"image_generation_call",
			"result":%q,
			"size":"1024x1024"
		}
	}`, b64))
	ok, size := collectImageGenCallSummary(event)
	if !ok {
		t.Fatal("expected image generation call summary")
	}
	if size != "1024x1024" {
		t.Fatalf("size = %q, want 1024x1024", size)
	}
}

// TestParseSSEUsage_ToolImageGen 验证 SSE response.completed 事件中
// response.tool_usage.image_gen 被正确抽取到累加器指针。
func TestParseSSEUsage_ToolImageGen(t *testing.T) {
	data := []byte(`{
		"type":"response.completed",
		"response":{
			"model":"gpt-5.4",
			"usage":{"input_tokens":100,"output_tokens":50},
			"tool_usage":{"image_gen":{"input_tokens":8,"output_tokens":4160}}
		}
	}`)
	usage := &sdk.Usage{}
	var toolIn, toolOut int
	parseSSEUsage(data, usage, &toolIn, &toolOut)
	if usage.Model != "gpt-5.4" {
		t.Errorf("Model = %q, want gpt-5.4", usage.Model)
	}
	if usageMetricInt(usage, usageMetricInputTokens) != 100 || usageMetricInt(usage, usageMetricOutputTokens) != 50 {
		t.Errorf("Input/Output = %d/%d, want 100/50", usageMetricInt(usage, usageMetricInputTokens), usageMetricInt(usage, usageMetricOutputTokens))
	}
	if toolIn != 8 || toolOut != 4160 {
		t.Errorf("toolIn/Out = %d/%d, want 8/4160", toolIn, toolOut)
	}
}

// TestFillUsageCostWithImageTool 叠加计费：主 model (gpt-5.4) 的 chat token 按
// 其单价、image tool 按尺寸分档计费。
func TestFillUsageCostWithImageTool(t *testing.T) {
	usage := newTokenUsage("gpt-5.4", "", 1000, 500, 0, 0, 0)
	fillUsageCostWithImageTool(usage, 1, "1024x1024")

	// 主 gpt-5.4 standard: input=$2.5/1M → 0.0025, output=$15/1M → 0.0075
	// image tool: 1 张 × $0.10 (1K) = 0.10
	// total account cost = 0.0025 + 0.0075 + 0.10 = 0.1100
	if !almostEqual(usageCostByKey(usage, usageCostInput), 0.0025, 1e-9) {
		t.Errorf("input cost = %v, want 0.0025", usageCostByKey(usage, usageCostInput))
	}
	if !almostEqual(usage.AccountCost, 0.1100, 1e-9) {
		t.Errorf("AccountCost = %v, want 0.1100", usage.AccountCost)
	}
	if got := usage.Metrics[0].Metadata["unit_price"]; got != "2.5" {
		t.Errorf("input unit_price = %q, want 2.5", got)
	}
}

func TestAddUsageCostForModel_CombinesResponsesContextAndImageCost(t *testing.T) {
	usage := newTokenUsage("gpt-image-2", "", 12, 0, 0, 0, 0)
	addUsageCostForModel(usage, "gpt-5.4", "", 1000, 500, 0, 0, "responses_context", "上下文")
	fillUsageCostPerImageBySize(usage, 1, "1024x1024")

	if got := usageCostByKey(usage, "responses_context_"+usageCostInput); !almostEqual(got, 0.0025, 1e-9) {
		t.Errorf("context input cost = %v, want 0.0025", got)
	}
	if got := usageCostByKey(usage, "responses_context_"+usageCostOutput); !almostEqual(got, 0.0075, 1e-9) {
		t.Errorf("context output cost = %v, want 0.0075", got)
	}
	if got := usageCostByKey(usage, usageCostImage); !almostEqual(got, 0.10, 1e-9) {
		t.Errorf("image cost = %v, want 0.10", got)
	}
	if !almostEqual(usage.AccountCost, 0.1100, 1e-9) {
		t.Errorf("AccountCost = %v, want 0.1100", usage.AccountCost)
	}
	if usage.Model != "gpt-image-2" {
		t.Errorf("Usage.Model = %q, want gpt-image-2", usage.Model)
	}
}

// TestFillUsageCostWithImageTool_NoToolUsage 退化为 fillUsageCost 行为不变。
func TestFillUsageCostWithImageTool_NoToolUsage(t *testing.T) {
	usage := newTokenUsage("gpt-5.4", "", 1000, 500, 0, 0, 0)
	fillUsageCostWithImageTool(usage, 0, "")
	if usageMetricInt(usage, usageMetricInputTokens) != 1000 || usageMetricInt(usage, usageMetricOutputTokens) != 500 {
		t.Errorf("token counts mutated when no image tool usage")
	}
	if !almostEqual(usageCostByKey(usage, usageCostInput), 0.0025, 1e-9) {
		t.Errorf("input cost = %v, want 0.0025", usageCostByKey(usage, usageCostInput))
	}
}

// TestCollectImageGenCall 抽取 output_item.done 里的 image_generation_call 条目。
func TestCollectImageGenCall(t *testing.T) {
	item := map[string]any{
		"type":           "image_generation_call",
		"status":         "completed",
		"id":             "ig_123",
		"result":         "iVBORw0KGgoAAA",
		"size":           "1024x1024",
		"quality":        "high",
		"output_format":  "png",
		"background":     "opaque",
		"revised_prompt": "a cute shiba inu",
	}
	var ws WSResult
	collectImageGenCall(&ws, item)
	if len(ws.ImageGenCalls) != 1 {
		t.Fatalf("ImageGenCalls len = %d, want 1", len(ws.ImageGenCalls))
	}
	got := ws.ImageGenCalls[0]
	if got.Result != "iVBORw0KGgoAAA" || got.Size != "1024x1024" || got.RevisedPrompt != "a cute shiba inu" {
		t.Errorf("ImageGenCall fields not populated: %+v", got)
	}
	// 非 image_generation_call 的 item 应被忽略
	collectImageGenCall(&ws, map[string]any{"type": "message"})
	if len(ws.ImageGenCalls) != 1 {
		t.Errorf("non-image item should be ignored")
	}
	// 缺 result 的也应被忽略，但要保留上游诊断，便于 failover 日志定位真实原因。
	collectImageGenCall(&ws, map[string]any{
		"type":   "image_generation_call",
		"id":     "ig_123",
		"status": "failed",
		"error":  map[string]any{"message": "safety system rejected the prompt"},
	})
	if len(ws.ImageGenCalls) != 1 {
		t.Errorf("item without result should be ignored")
	}
	if len(ws.ImageGenCallDiagnostics) != 1 {
		t.Fatalf("ImageGenCallDiagnostics len = %d, want 1", len(ws.ImageGenCallDiagnostics))
	}
	if len(ws.ImageGenCallFailures) != 1 {
		t.Fatalf("ImageGenCallFailures len = %d, want 1", len(ws.ImageGenCallFailures))
	}
	if ws.ImageGenCallFailures[0].Message != "safety system rejected the prompt" {
		t.Errorf("ImageGenCallFailures[0].Message = %q", ws.ImageGenCallFailures[0].Message)
	}
	if !strings.Contains(ws.ImageGenCallDiagnostics[0], "status=failed") || !strings.Contains(ws.ImageGenCallDiagnostics[0], "safety system rejected the prompt") {
		t.Errorf("diagnostic missing upstream cause: %s", ws.ImageGenCallDiagnostics[0])
	}
}

func TestCollectImageGenCallPartialImageMergesMetadata(t *testing.T) {
	var ws WSResult
	collectImageGenCallMetadata(&ws, map[string]any{
		"type":         "image_generation_call",
		"id":           "ig_123",
		"output_index": 0,
		"status":       "in_progress",
		"size":         "1024x1024",
		"model":        "gpt-image-1.5",
	})
	collectImageGenCallPartial(&ws, map[string]any{
		"type":          "response.image_generation_call.partial_image",
		"output_index":  0,
		"partial_image": "iVBORw0KGgoAAA",
	})

	if len(ws.ImageGenCalls) != 1 {
		t.Fatalf("ImageGenCalls len = %d, want 1", len(ws.ImageGenCalls))
	}
	got := ws.ImageGenCalls[0]
	if got.ID != "ig_123" {
		t.Fatalf("ID = %q, want ig_123", got.ID)
	}
	if !got.HasOutputIndex || got.OutputIndex != 0 {
		t.Fatalf("OutputIndex = %d / %v, want 0 / true", got.OutputIndex, got.HasOutputIndex)
	}
	if got.Result != "iVBORw0KGgoAAA" {
		t.Fatalf("Result = %q, want partial image", got.Result)
	}
	if got.Size != "1024x1024" {
		t.Fatalf("Size = %q, want 1024x1024", got.Size)
	}
	if got.Model != "gpt-image-1.5" {
		t.Fatalf("Model = %q, want gpt-image-1.5", got.Model)
	}
}

func TestClassifyImageGenCallFailuresSafetyRejected(t *testing.T) {
	failures := []ImageGenCallFailure{{
		Status:    "failed",
		ErrorCode: "content_policy_violation",
		Message:   "Your request was rejected by the safety system. If you believe this is an error, contact us at help.openai.com and include the request ID 916c6516-5f37-9121-b05a-a604888c0055.",
	}}
	failure := classifyImageGenCallFailures(failures, "")
	if failure == nil {
		t.Fatalf("expected failure")
	}
	if failure.Kind != responsesFailureKindClient {
		t.Fatalf("Kind = %q, want client", failure.Kind)
	}
	if failure.StatusCode != http.StatusBadRequest {
		t.Fatalf("StatusCode = %d, want 400", failure.StatusCode)
	}
	if failure.Code != "safety_rejected" {
		t.Fatalf("Code = %q, want safety_rejected", failure.Code)
	}
	body := buildImagesErrorBodyWithCode(failure.StatusCode, failure.Code, failure.Message)
	if got := gjson.GetBytes(body, "error.code").String(); got != "safety_rejected" {
		t.Fatalf("error.code = %q, want safety_rejected; body=%s", got, body)
	}
}

func TestClassifyUpstreamTaskErrorSafetyRejected(t *testing.T) {
	body := []byte(`{"error":{"message":"Your request was rejected by the safety system.","type":"invalid_request_error","code":"content_policy_violation"}}`)
	taskErr := classifyUpstreamTaskError(http.StatusBadGateway, body)
	if taskErr.Type != "invalid_request" {
		t.Fatalf("Type = %q, want invalid_request", taskErr.Type)
	}
	if taskErr.Code != "safety_rejected" {
		t.Fatalf("Code = %q, want safety_rejected", taskErr.Code)
	}
	if taskErr.Retryable {
		t.Fatalf("Retryable = true, want false")
	}
}

func TestImageTaskQualityEcho(t *testing.T) {
	input, attrs, err := imageGenerateHandler{}.BuildInput(&sdk.ForwardRequest{
		Body:    []byte(`{"model":"gpt-image-2","prompt":"a shiba","size":"1024x1024","quality":"high"}`),
		Headers: http.Header{},
	}, "/v1/images/generations")
	if err != nil {
		t.Fatalf("BuildInput returned err: %v", err)
	}
	if got := input["quality"]; got != "high" {
		t.Fatalf("input quality = %v, want high", got)
	}
	if got := attrs["quality"]; got != "high" {
		t.Fatalf("attribute quality = %q, want high", got)
	}

	resp := buildImageTaskResponse(&sdk.HostTask{
		ID:     123,
		Status: sdk.TaskStatusCompleted,
		Input: map[string]interface{}{
			"prompt":  "a shiba",
			"size":    "1024x1024",
			"quality": "high",
		},
	})
	if got := resp["quality"]; got != "high" {
		t.Fatalf("response quality = %v, want high", got)
	}
}

// TestBuildImagesToolCreateMsg 翻译 Images REST 请求体为 Responses API
// response.create 消息，tool 配置保持 Codex 对齐的极简 schema。
func TestBuildImagesToolCreateMsg(t *testing.T) {
	body := []byte(`{"model":"gpt-image-1.5","prompt":"a shiba","n":1,"size":"1024x1024","quality":"low","background":"transparent","output_format":"png"}`)
	msg, n, promptTokens, err := buildImagesToolCreateMsg(body, "application/json", false, openAISessionResolution{})
	if err != nil {
		t.Fatalf("buildImagesToolCreateMsg returned err: %v", err)
	}
	if n != 1 {
		t.Errorf("n = %d, want 1", n)
	}
	// "a shiba" = 7 runes → (7+2)/3 = 3 tokens
	if promptTokens != 3 {
		t.Errorf("promptTokens = %d, want 3", promptTokens)
	}

	if gjson.GetBytes(msg, "type").String() != "response.create" {
		t.Errorf("type = %q, want response.create", gjson.GetBytes(msg, "type").String())
	}
	if gjson.GetBytes(msg, "model").String() != imagesOAuthChatModel {
		t.Errorf("model = %q, want %q", gjson.GetBytes(msg, "model").String(), imagesOAuthChatModel)
	}
	if gjson.GetBytes(msg, "tool_choice").String() != "auto" {
		t.Errorf("tool_choice = %q, want auto", gjson.GetBytes(msg, "tool_choice").String())
	}
	inputItem := gjson.GetBytes(msg, "input.0")
	if inputItem.Get("type").String() != "message" || inputItem.Get("role").String() != "user" {
		t.Errorf("input[0] type/role wrong: %s", inputItem.Raw)
	}
	prompt := inputItem.Get("content.0.text").String()
	if !strings.HasPrefix(prompt, "a shiba\n\nImage API constraints:\n") {
		t.Errorf("prompt prefix wrong: %q", prompt)
	}
	for _, want := range []string{
		"Generate the image at 1024x1024 pixels.",
		"Use image quality setting low.",
		"Use background setting transparent.",
		"Use requested image model gpt-image-1.5.",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing constraint %q: %q", want, prompt)
		}
		if !strings.Contains(gjson.GetBytes(msg, "instructions").String(), want) {
			t.Errorf("instructions missing constraint %q", want)
		}
	}
	tool := gjson.GetBytes(msg, "tools.0")
	if tool.Get("type").String() != "image_generation" {
		t.Errorf("tools[0].type = %q, want image_generation", tool.Get("type").String())
	}
	if tool.Get("output_format").String() != "png" {
		t.Errorf("tools[0].output_format = %q, want png", tool.Get("output_format").String())
	}
	// size / quality / background 必须直接落到 tool 字段，
	// 否则上游 image_generation 收不到这些参数会按默认（1024×1024）出图。
	if got := tool.Get("size").String(); got != "1024x1024" {
		t.Errorf("tools[0].size = %q, want 1024x1024", got)
	}
	if got := tool.Get("quality").String(); got != "low" {
		t.Errorf("tools[0].quality = %q, want low", got)
	}
	if got := tool.Get("background").String(); got != "transparent" {
		t.Errorf("tools[0].background = %q, want transparent", got)
	}
	// 这些字段不应出现：
	//   - n：image_generation 一次只产 1 张，n>1 需要多轮工具调用编排，不通过 tool 字段
	//   - model：image_generation tool 不接受 model 字段，image 模型由工具自己定（默认 gpt-image-2），
	//     写了上游会 502；顶层 payload.model 给 chat 模型用是另一回事
	for _, forbidden := range []string{"n", "model"} {
		if tool.Get(forbidden).Exists() {
			t.Errorf("tools[0].%s should not be present", forbidden)
		}
	}
}

func TestBuildImagesToolCreateMsg_ClampsOversizedSize(t *testing.T) {
	body := []byte(`{"model":"gpt-image-1.5","prompt":"a shiba","n":1,"size":"4096x2304"}`)
	msg, _, _, err := buildImagesToolCreateMsg(body, "application/json", false, openAISessionResolution{})
	if err != nil {
		t.Fatalf("buildImagesToolCreateMsg returned err: %v", err)
	}
	prompt := gjson.GetBytes(msg, "input.0.content.0.text").String()
	if !strings.Contains(prompt, "Generate the image at 3840x2160 pixels.") {
		t.Errorf("prompt missing clamped size constraint: %q", prompt)
	}
	// clamp 后的 size 也要落到 tool 字段（之前期望"不存在"是按 prompt-only 策略写的，
	// 现在改双轨：tool 字段是权威来源）。
	if got := gjson.GetBytes(msg, "tools.0.size").String(); got != "3840x2160" {
		t.Errorf("tools[0].size = %q, want clamped 3840x2160", got)
	}
	if got := gjson.GetBytes(msg, "tools.0.quality").String(); got != "medium" {
		t.Errorf("tools[0].quality = %q, want medium", got)
	}
}

// TestBuildImagesToolCreateMsg_NGreaterThanOne V1 不支持 n>1，应直接返错。
func TestBuildImagesToolCreateMsg_NGreaterThanOne(t *testing.T) {
	body := []byte(`{"prompt":"x","n":3}`)
	_, _, _, err := buildImagesToolCreateMsg(body, "application/json", false, openAISessionResolution{})
	if err == nil {
		t.Fatal("expected err for n>1, got nil")
	}
}

// TestBuildImagesToolCreateMsg_EmptyPrompt prompt 空串应报错。
func TestBuildImagesToolCreateMsg_EmptyPrompt(t *testing.T) {
	_, _, _, err := buildImagesToolCreateMsg([]byte(`{"n":1}`), "application/json", false, openAISessionResolution{})
	if err == nil {
		t.Fatal("expected err for empty prompt, got nil")
	}
}

// TestBuildImagesToolCreateMsg_Edit_JSON 验证 /images/edits 走 JSON 路径时：
// 参考图以 input_image 注入，mask 转成 Codex built-in image_gen 可理解的区域标注图。
func TestBuildImagesToolCreateMsg_Edit_JSON(t *testing.T) {
	imageRef := testPNGDataURL(2, 2, func(x, y int) color.RGBA {
		return color.RGBA{R: 80, G: 90, B: 100, A: 255}
	})
	maskBytes := testPNGBytes(2, 2, func(x, y int) color.RGBA {
		if x == 1 && y == 1 {
			return color.RGBA{A: 0}
		}
		return color.RGBA{R: 255, G: 255, B: 255, A: 255}
	})
	maskSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(maskBytes)
	}))
	defer maskSrv.Close()
	maskRef := maskSrv.URL + "/mask.png"
	body := []byte(fmt.Sprintf(`{
		"model":"gpt-image-1.5",
		"prompt":"make it cyberpunk",
		"size":"1024x1024",
		"input_fidelity":"high",
		"output_format":"jpeg",
		"image":%q,
		"mask":%q
	}`, imageRef, maskRef))
	msg, n, inputTokens, err := buildImagesToolCreateMsg(body, "application/json", true, openAISessionResolution{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 1 {
		t.Errorf("n = %d, want 1", n)
	}
	// text prompt "make it cyberpunk" = 17 runes → 6；reference image + region annotation = 2 * 272 → 550
	if inputTokens != 6+272*2 {
		t.Errorf("inputTokens = %d, want %d", inputTokens, 6+272*2)
	}
	content := gjson.GetBytes(msg, "input.0.content")
	if !content.IsArray() || len(content.Array()) != 3 {
		t.Fatalf("content len = %d, want 3 (text + image + region annotation)", len(content.Array()))
	}
	if content.Get("0.type").String() != "input_text" || content.Get("1.type").String() != "input_image" || content.Get("2.type").String() != "input_image" {
		t.Errorf("content types wrong: %s", content.Raw)
	}
	if content.Get("1.image_url").String() != imageRef {
		t.Errorf("image_url not propagated: %s", content.Raw)
	}
	annotationRef := content.Get("2.image_url").String()
	if annotationRef == "" || annotationRef == maskRef || !strings.HasPrefix(annotationRef, "data:image/png;base64,") {
		t.Errorf("region annotation not generated: %s", content.Raw)
	}
	prompt := content.Get("0.text").String()
	for _, want := range []string{
		"Image 1 is the edit target; preserve its framing, identity, geometry, lighting, and all unrequested details.",
		"Preserve the input image with high fidelity.",
		"Image 2 is a region annotation derived from the edit mask; change only the red marked area in Image 1 and keep everything outside that region unchanged.",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing edit constraint %q: %q", want, prompt)
		}
		if !strings.Contains(gjson.GetBytes(msg, "instructions").String(), want) {
			t.Errorf("instructions missing edit constraint %q", want)
		}
	}
	tool := gjson.GetBytes(msg, "tools.0")
	if tool.Get("output_format").String() != "png" {
		t.Errorf("tools[0].output_format = %q, want png", tool.Get("output_format").String())
	}
	// /edits 模式下 size 同样要落到 tool 字段（双轨：tool 字段权威 + prompt 兜底）。
	if got := tool.Get("size").String(); got != "1024x1024" {
		t.Errorf("tools[0].size = %q, want 1024x1024", got)
	}
	// 这些字段仍不应出现：
	//   - action / input_image_mask：codex CLI 字段，不是 Responses API image_generation tool 的合法字段
	//   - input_fidelity：当前未作为 tool 字段透传（在 prompt constraints 里走兜底，gpt-image-2 上跳过）
	//   - model：image_generation tool 不接受 model 字段，写了会 502
	for _, forbidden := range []string{"action", "input_image_mask", "input_fidelity", "model"} {
		if tool.Get(forbidden).Exists() {
			t.Errorf("tools[0].%s should not be present", forbidden)
		}
	}
}

func TestBuildAPIKeyImagesEditMultipartBody_RemoteRefs(t *testing.T) {
	imageBytes := testPNGBytes(2, 2, func(x, y int) color.RGBA {
		return color.RGBA{R: 90, G: 100, B: 110, A: 255}
	})
	maskBytes := testPNGBytes(2, 2, func(x, y int) color.RGBA {
		if x == 0 && y == 0 {
			return color.RGBA{A: 0}
		}
		return color.RGBA{A: 255}
	})
	imageSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(imageBytes)
	}))
	defer imageSrv.Close()
	maskSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(maskBytes)
	}))
	defer maskSrv.Close()

	body := []byte(fmt.Sprintf(`{
		"prompt":"relight the scene",
		"model":"gpt-image-2",
		"size":"1024x1024",
		"quality":"auto",
		"n":1,
		"image":%q,
		"mask":%q
	}`, imageSrv.URL+"/image.png", maskSrv.URL+"/mask.png"))

	multipartBody, contentType, err := buildAPIKeyImagesEditMultipartBody(body, "application/json")
	if err != nil {
		t.Fatalf("buildAPIKeyImagesEditMultipartBody err: %v", err)
	}
	req, err := parseImagesRequest(multipartBody, contentType, true)
	if err != nil {
		t.Fatalf("parseImagesRequest err: %v", err)
	}
	if len(req.Images) != 1 || !strings.HasPrefix(req.Images[0], "data:image/png;base64,") {
		t.Fatalf("remote image not normalized into multipart data URL: %+v", req.Images)
	}
	if !strings.HasPrefix(req.Mask, "data:image/png;base64,") {
		t.Fatalf("remote mask not normalized into multipart data URL: %q", req.Mask)
	}
}

// TestBuildImagesToolCreateMsg_Edit_MissingImage /edits 模式下缺 image 字段应报错。
func TestBuildImagesToolCreateMsg_Edit_MissingImage(t *testing.T) {
	_, _, _, err := buildImagesToolCreateMsg(
		[]byte(`{"prompt":"x"}`), "application/json", true, openAISessionResolution{},
	)
	if err == nil {
		t.Fatal("expected err for missing image, got nil")
	}
}

// TestBuildImagesToolCreateMsg_Edit_Img2ImgNoMask 钉死 OAuth Responses-tool 路径
// 上"纯图生图"（无 mask）的载荷形状：必须 1 张 input_image 而且不生成 region
// annotation。这是 studio ComposerBar 图生图入口最常见的请求形态，是 OAuth
// 路径上历史最容易"看上去通了但参考图没生效"的退化点。
func TestBuildImagesToolCreateMsg_Edit_Img2ImgNoMask(t *testing.T) {
	imageRef := testPNGDataURL(2, 2, func(x, y int) color.RGBA {
		return color.RGBA{R: 120, G: 30, B: 30, A: 255}
	})
	body := []byte(fmt.Sprintf(`{
		"model":"gpt-image-2",
		"prompt":"make it cyberpunk",
		"size":"1024x1024",
		"image":%q
	}`, imageRef))

	msg, n, _, err := buildImagesToolCreateMsg(body, "application/json", true, openAISessionResolution{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 1 {
		t.Errorf("n = %d, want 1", n)
	}
	content := gjson.GetBytes(msg, "input.0.content")
	if !content.IsArray() || len(content.Array()) != 2 {
		t.Fatalf("content len = %d, want 2 (text + 1 image, no region annotation); raw=%s",
			len(content.Array()), content.Raw)
	}
	if content.Get("0.type").String() != "input_text" {
		t.Errorf("content[0].type = %q, want input_text", content.Get("0.type").String())
	}
	if content.Get("1.type").String() != "input_image" {
		t.Errorf("content[1].type = %q, want input_image", content.Get("1.type").String())
	}
	if got := content.Get("1.image_url").String(); got != imageRef {
		t.Errorf("content[1].image_url not propagated verbatim; got len=%d", len(got))
	}
	// Region annotation 只在 mask 存在时生成。img2img 不应有第三个 input_image。
	if content.Get("2").Exists() {
		t.Errorf("img2img without mask must not append region annotation; raw=%s", content.Raw)
	}
}

// TestBuildImagesToolCreateMsg_Edit_MultiImage 钉死多参考图场景：studio 在
// ComposerBar 上传多张图时，buildImageRequestBody 会把 image 输出成数组。
// OAuth Responses-tool 路径必须把每张都附为 input_image，且顺序与请求一致。
func TestBuildImagesToolCreateMsg_Edit_MultiImage(t *testing.T) {
	imageA := testPNGDataURL(2, 2, func(x, y int) color.RGBA {
		return color.RGBA{R: 10, G: 20, B: 30, A: 255}
	})
	imageB := testPNGDataURL(2, 2, func(x, y int) color.RGBA {
		return color.RGBA{R: 200, G: 210, B: 220, A: 255}
	})
	body := []byte(fmt.Sprintf(`{
		"model":"gpt-image-2",
		"prompt":"merge the two scenes",
		"size":"1024x1024",
		"image":[%q,%q]
	}`, imageA, imageB))

	msg, _, _, err := buildImagesToolCreateMsg(body, "application/json", true, openAISessionResolution{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	content := gjson.GetBytes(msg, "input.0.content")
	if !content.IsArray() || len(content.Array()) != 3 {
		t.Fatalf("content len = %d, want 3 (text + 2 images); raw=%s", len(content.Array()), content.Raw)
	}
	if got := content.Get("1.image_url").String(); got != imageA {
		t.Errorf("content[1].image_url should be image A (verbatim); len=%d", len(got))
	}
	if got := content.Get("2.image_url").String(); got != imageB {
		t.Errorf("content[2].image_url should be image B (verbatim); len=%d", len(got))
	}
}

// TestBuildImagesToolCreateMsg_Edit_StudioShape 验证 gateway-openai 的 task
// 执行链路在 OAuth 账号上的端到端形状：执行 buildImageRequestBody 后得到的
// JSON 字节，直接喂给 buildImagesToolCreateMsg，必须解析出 input_image。
// 守住"studio 任务路径 + OAuth 账号"这一组合，不再被静默降级成纯文生图。
func TestBuildImagesToolCreateMsg_Edit_StudioShape(t *testing.T) {
	imageRef := testPNGDataURL(2, 2, func(x, y int) color.RGBA {
		return color.RGBA{R: 60, G: 60, B: 200, A: 255}
	})
	// 复刻 executeImageTask → buildImageRequestBody 在 studio img2img 任务上
	// 真实写出的 input map（image 单元素时取字符串、size=auto 是 studio 默认）。
	taskInput := map[string]any{
		"prompt":             "replace the red ball with a blue cube",
		"model":              "gpt-image-2",
		"size":               "auto",
		"images":             []string{imageRef},
		"preserve_reference": true,
	}
	jsonBody, err := buildImageRequestBody(taskInput)
	if err != nil {
		t.Fatalf("buildImageRequestBody: %v", err)
	}
	// 必要前置：emitter 一定要在体内放上 image 字段。
	if !gjson.GetBytes(jsonBody, "image").Exists() {
		t.Fatalf("buildImageRequestBody dropped image field: %s", jsonBody)
	}

	msg, n, _, err := buildImagesToolCreateMsg(jsonBody, "application/json", true, openAISessionResolution{})
	if err != nil {
		t.Fatalf("buildImagesToolCreateMsg: %v", err)
	}
	if n != 1 {
		t.Errorf("n = %d, want 1", n)
	}
	content := gjson.GetBytes(msg, "input.0.content")
	if !content.IsArray() || len(content.Array()) < 2 {
		t.Fatalf("studio shape must produce ≥2 content items (text + image); got %s", content.Raw)
	}
	if content.Get("1.type").String() != "input_image" {
		t.Errorf("studio shape must surface input_image as second content item; raw=%s", content.Raw)
	}
	if got := content.Get("1.image_url").String(); got != imageRef {
		t.Errorf("studio source image not propagated through Responses-tool path")
	}
}

type noopWSEventHandler struct{}

func (noopWSEventHandler) OnTextDelta(string)        {}
func (noopWSEventHandler) OnReasoningDelta(string)   {}
func (noopWSEventHandler) OnRawEvent(string, []byte) {}
func (noopWSEventHandler) OnRateLimits(float64)      {}

func TestReceiveWSResponseClassifiesMessageTooBigAsClientError(t *testing.T) {
	upgrader := websocket.Upgrader{ReadBufferSize: 1024, WriteBufferSize: 1024}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseMessageTooBig, "message too big"), time.Now().Add(time.Second))
		_ = conn.Close()
	}))
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	result := ReceiveWSResponse(context.Background(), conn, noopWSEventHandler{})
	var failure *responsesFailureError
	if !errors.As(result.Err, &failure) {
		t.Fatalf("Err = %T %v, want responsesFailureError", result.Err, result.Err)
	}
	if failure.Kind != responsesFailureKindClient || failure.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("failure = %#v, want client 413", failure)
	}
	if failure.Message != sanitizedImageSSEErrorMessage {
		t.Fatalf("failure message = %q, want %q", failure.Message, sanitizedImageSSEErrorMessage)
	}
}

func TestBuildImagesToolCreateMsg_Edit_ShrinksLargeInputImage(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 1600, 1600))
	seed := uint32(1)
	for y := 0; y < 1600; y++ {
		for x := 0; x < 1600; x++ {
			seed = seed*1664525 + 1013904223
			img.SetRGBA(x, y, color.RGBA{R: uint8(seed >> 16), G: uint8(seed >> 8), B: uint8(seed), A: 255})
		}
	}
	imageRef := testPNGDataURLFromImage(img)
	body := []byte(fmt.Sprintf(`{"prompt":"edit this","image":%q}`, imageRef))
	msg, _, _, err := buildImagesToolCreateMsg(body, "application/json", true, openAISessionResolution{})
	if err != nil {
		t.Fatalf("buildImagesToolCreateMsg returned err: %v", err)
	}
	got := gjson.GetBytes(msg, "input.0.content.1.image_url").String()
	if !strings.HasPrefix(got, "data:image/jpeg;base64,") {
		t.Fatalf("image_url prefix = %.32q, want jpeg data URL", got)
	}
	comma := strings.IndexByte(got, ',')
	data, err := base64.StdEncoding.DecodeString(got[comma+1:])
	if err != nil {
		t.Fatalf("decoded shrunk image: %v", err)
	}
	if len(data) > maxResponsesInputImageBytes {
		t.Fatalf("shrunk image bytes = %d, want <= %d", len(data), maxResponsesInputImageBytes)
	}
}

// TestParseImagesEditMultipart 覆盖 OpenAI SDK 标准的 multipart/form-data 请求：
// image 文件 + prompt 文本 + mask 文件 → 规范化后 images / mask 都应是 data URL。
func TestBuildAPIKeyImagesEditMultipartBody(t *testing.T) {
	pngBytes := []byte{0x89, 0x50, 0x4E, 0x47}
	body := []byte(fmt.Sprintf(`{
		"prompt":"relight the scene",
		"model":"gpt-image-2",
		"size":"1024x1024",
		"quality":"auto",
		"n":1,
		"image":"data:image/png;base64,%s"
	}`, base64.StdEncoding.EncodeToString(pngBytes)))

	multipartBody, contentType, err := buildAPIKeyImagesEditMultipartBody(body, "application/json")
	if err != nil {
		t.Fatalf("buildAPIKeyImagesEditMultipartBody err: %v", err)
	}
	if !strings.HasPrefix(contentType, "multipart/form-data; boundary=") {
		t.Fatalf("contentType = %q", contentType)
	}

	req, err := parseImagesRequest(multipartBody, contentType, true)
	if err != nil {
		t.Fatalf("parseImagesRequest err: %v", err)
	}
	if req.Prompt != "relight the scene" || req.Model != "gpt-image-2" || req.Size != "1024x1024" || req.Quality != "auto" || req.N != 1 {
		t.Fatalf("fields wrong: %+v", req)
	}
	if len(req.Images) != 1 || req.Images[0] != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(pngBytes) {
		t.Fatalf("image wrong: %+v", req.Images)
	}
}

func TestParseImagesEditMultipart(t *testing.T) {
	pngBytes := []byte{0x89, 0x50, 0x4E, 0x47}
	maskBytes := []byte{0x89, 0x50, 0x4E, 0x47, 0xFF}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("prompt", "relight the scene")
	_ = mw.WriteField("model", "gpt-image-1.5")
	_ = mw.WriteField("size", "1024x1024")
	_ = mw.WriteField("quality", "high")
	_ = mw.WriteField("input_fidelity", "high")

	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="image"; filename="in.png"`)
	h.Set("Content-Type", "image/png")
	w, _ := mw.CreatePart(h)
	_, _ = w.Write(pngBytes)

	hm := textproto.MIMEHeader{}
	hm.Set("Content-Disposition", `form-data; name="mask"; filename="mask.png"`)
	hm.Set("Content-Type", "image/png")
	wm, _ := mw.CreatePart(hm)
	_, _ = wm.Write(maskBytes)

	_ = mw.Close()

	req, err := parseImagesRequest(buf.Bytes(), mw.FormDataContentType(), true)
	if err != nil {
		t.Fatalf("parseImagesRequest err: %v", err)
	}
	if !req.IsEdit || req.Prompt != "relight the scene" {
		t.Errorf("prompt / edit flag wrong: %+v", req)
	}
	if req.Model != "gpt-image-1.5" || req.Size != "1024x1024" ||
		req.Quality != "high" || req.InputFidelity != "high" {
		t.Errorf("fields mis-parsed: %+v", req)
	}
	if len(req.Images) != 1 ||
		req.Images[0] != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(pngBytes) {
		t.Errorf("image not encoded as data URL: %+v", req.Images)
	}
	if req.Mask != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(maskBytes) {
		t.Errorf("mask not encoded as data URL: %q", req.Mask)
	}
}

func TestImagesResponseOptionsFromMultipartDoesNotParseImagePart(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="image"; filename="bad.bin"`)
	h.Set("Content-Type", "application/octet-stream")
	w, _ := mw.CreatePart(h)
	_, _ = w.Write(bytes.Repeat([]byte{0x01, 0x02, 0x03, 0x04}, 1024))
	_ = mw.WriteField("quality", "high")
	_ = mw.WriteField("size", "2048x2048")
	_ = mw.Close()

	opts := imagesResponseOptionsFromRequestBody(buf.Bytes(), mw.FormDataContentType(), true)
	if opts.RequestQuality != "high" || opts.BillingSize != "2048x2048" {
		t.Fatalf("options = %+v, want quality high and size 2048x2048", opts)
	}
}

func TestImagesResponseOptionsDefaultAndAutoQualityUseMedium(t *testing.T) {
	opts := imagesResponseOptionsFromRequestBody([]byte(`{"prompt":"x"}`), "application/json", false)
	if opts.RequestQuality != "medium" {
		t.Fatalf("default JSON quality = %q, want medium", opts.RequestQuality)
	}

	opts = imagesResponseOptionsFromRequestBody([]byte(`{"prompt":"x","quality":"auto"}`), "application/json", false)
	if opts.RequestQuality != "medium" {
		t.Fatalf("auto JSON quality = %q, want medium", opts.RequestQuality)
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("prompt", "x")
	_ = mw.WriteField("quality", "auto")
	_ = mw.Close()
	opts = imagesResponseOptionsFromRequestBody(buf.Bytes(), mw.FormDataContentType(), true)
	if opts.RequestQuality != "medium" {
		t.Fatalf("auto multipart quality = %q, want medium", opts.RequestQuality)
	}
}

func TestNormalizeImageRef(t *testing.T) {
	cases := map[string]string{
		"data:image/png;base64,AAA=":     "data:image/png;base64,AAA=",
		"data:image/png;base64,QUJD\nRA": "data:image/png;base64,QUJDRA==",
		"https://example.com/a.png":      "https://example.com/a.png",
	}
	for in, want := range cases {
		got, err := normalizeImageRef(in)
		if err != nil {
			t.Fatalf("normalizeImageRef(%q) err: %v", in, err)
		}
		if got != want {
			t.Errorf("normalizeImageRef(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeImageRefRejectsUnsupportedFormat(t *testing.T) {
	if got, err := normalizeImageRef("QUJD\nRA"); err == nil {
		t.Fatalf("normalizeImageRef bare base64 = %q, want err", got)
	}
}

// TestEstimatePromptTokens 覆盖常见输入。粗略 / 3 上取整，够用即可。
func TestEstimatePromptTokens(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},           // 1 rune → 1
		{"abc", 1},         // 3 runes → 1
		{"abcd", 2},        // 4 runes → 2
		{"a shiba", 3},     // 7 runes → 3
		{"可爱柴犬", 2},        // 4 runes → 2
		{"一只可爱的柴犬在草地上", 4}, // 10 runes → 4
	}
	for _, tc := range cases {
		if got := estimatePromptTokens(tc.in); got != tc.want {
			t.Errorf("estimatePromptTokens(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestBuildImagesRESTResponse 把 WSResult 打包回 OpenAI Images REST 响应格式。
// 计费口径对齐 OpenAI 官方：usage.input_tokens = prompt tokens、output_tokens = 图像 tokens、
// root 级 model 使用实际响应的图像模型。instructions / 工具包装的 chat tokens 不暴露。
func TestBuildImagesRESTResponse(t *testing.T) {
	ws := WSResult{
		InputTokens:           4808, // chat text tokens (内层吸收，不对外)
		OutputTokens:          40,   // chat output tokens (内层吸收，不对外)
		ToolImageInputTokens:  0,
		ToolImageOutputTokens: 4160,
		ToolImageModel:        "gpt-image-2",
		ImageGenCalls: []ImageGenCall{
			{Result: "PNG_BASE64_A", RevisedPrompt: "revised a"},
			{Result: "PNG_BASE64_B"},
		},
	}
	promptTokens := 12
	imageOut := 4160
	body := buildImagesRESTResponse(ws, promptTokens, imageOut, imageGenerationBillingModel(ws.ToolImageModel, "dall-e-3"))

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["model"] != "gpt-image-2" {
		t.Errorf("root model = %v, want gpt-image-2", got["model"])
	}
	data, _ := got["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("data len = %d, want 2", len(data))
	}
	first, _ := data[0].(map[string]any)
	if first["b64_json"] != "PNG_BASE64_A" || first["revised_prompt"] != "revised a" {
		t.Errorf("data[0] fields wrong: %+v", first)
	}
	second, _ := data[1].(map[string]any)
	if second["b64_json"] != "PNG_BASE64_B" {
		t.Errorf("data[1].b64_json = %v, want PNG_BASE64_B", second["b64_json"])
	}
	if _, ok := second["revised_prompt"]; ok {
		t.Errorf("empty revised_prompt should be omitted")
	}
	usage, ok := got["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage missing")
	}
	if int(usage["input_tokens"].(float64)) != promptTokens {
		t.Errorf("usage.input_tokens = %v, want %d", usage["input_tokens"], promptTokens)
	}
	if int(usage["output_tokens"].(float64)) != imageOut {
		t.Errorf("usage.output_tokens = %v, want %d", usage["output_tokens"], imageOut)
	}
	if int(usage["total_tokens"].(float64)) != promptTokens+imageOut {
		t.Errorf("usage.total_tokens wrong")
	}
}

func TestBuildImagesRESTResponse_QualityEchoPrefersRequest(t *testing.T) {
	ws := WSResult{ImageGenCalls: []ImageGenCall{{Result: "PNG_BASE64", Quality: "low"}}}
	body := buildImagesRESTResponse(ws, 1, 2, "gpt-image-2", "high")

	data := gjson.GetBytes(body, "data.0")
	if got := data.Get("quality").String(); got != "high" {
		t.Fatalf("data[0].quality = %q, want high", got)
	}

	body = buildImagesRESTResponse(ws, 1, 2, "gpt-image-2")
	if got := gjson.GetBytes(body, "data.0.quality").String(); got != "low" {
		t.Fatalf("data[0].quality without request = %q, want upstream low", got)
	}

	body = buildImagesRESTResponse(ws, 1, 2, "gpt-image-2", "")
	if got := gjson.GetBytes(body, "data.0.quality").String(); got != "medium" {
		t.Fatalf("data[0].quality with default request = %q, want medium", got)
	}

	body = buildImagesRESTResponse(ws, 1, 2, "gpt-image-2", "auto")
	if got := gjson.GetBytes(body, "data.0.quality").String(); got != "medium" {
		t.Fatalf("data[0].quality with auto request = %q, want medium", got)
	}
}

// TestBuildImagesRESTResponse_ChainedCostParity 验证 AirGate 套 AirGate 时两级
// 金额一致：下一级拿到 body 按 root model 单价重算，应等于本级结果。
func TestBuildImagesRESTResponse_ChainedCostParity(t *testing.T) {
	promptTokens := 12
	imageOut := 1056
	ws := WSResult{ImageGenCalls: []ImageGenCall{{Result: "X"}}}
	body := buildImagesRESTResponse(ws, promptTokens, imageOut, "gpt-image-2")

	inner := newTokenUsage("gpt-image-2", "", promptTokens, imageOut, 0, 0, 0)
	fillUsageCost(inner)
	innerCost := inner.AccountCost

	var got map[string]any
	_ = json.Unmarshal(body, &got)
	u := got["usage"].(map[string]any)
	outer := newTokenUsage(got["model"].(string), "", int(u["input_tokens"].(float64)), int(u["output_tokens"].(float64)), 0, 0, 0)
	fillUsageCost(outer)
	outerCost := outer.AccountCost

	if innerCost != outerCost {
		t.Errorf("cost mismatch: inner=%.6f outer=%.6f", innerCost, outerCost)
	}
}

// TestLookupImageGenOutputTokens 按 OpenAI 官方表验证 size×quality→token 估算。
func TestLookupImageGenOutputTokens(t *testing.T) {
	cases := []struct {
		size    string
		quality string
		want    int
	}{
		{"1024x1024", "low", 272},
		{"1024x1024", "medium", 1056},
		{"1024x1024", "high", 4160},
		{"1024x1536", "low", 408},
		{"1536x1024", "high", 6240},
		// quality="auto" → medium
		{"1024x1024", "auto", 1056},
		{"1024x1024", "", 1056},
		// 未知 size 保底 1024×1024 medium
		{"9999x9999", "high", 1056},
		{"1024x1024", "unknown", 1056}, // unknown quality → medium
		// 大小写归一
		{"1024X1024", "HIGH", 4160},
	}
	for _, tc := range cases {
		if got := lookupImageGenOutputTokens(tc.size, tc.quality); got != tc.want {
			t.Errorf("lookup(%q,%q) = %d, want %d", tc.size, tc.quality, got, tc.want)
		}
	}
}

// TestEstimateImageGenOutputTokens 多张图总 token 数 = 每张相加。
func TestEstimateImageGenOutputTokens(t *testing.T) {
	calls := []ImageGenCall{
		{Size: "1024x1024", Quality: "low"},  // 272
		{Size: "1024x1536", Quality: "high"}, // 6240
		{Size: "1024x1024", Quality: ""},     // auto → medium → 1056
	}
	got := estimateImageGenOutputTokens(calls)
	want := 272 + 6240 + 1056
	if got != want {
		t.Errorf("estimateImageGenOutputTokens = %d, want %d", got, want)
	}
}

// TestForwardImagesViaResponsesTool_EmptyPrompt 客户端传空 prompt 时，
// 翻译层应在未建立 WS 连接的情况下返回 ClientError + 400，不伤账号状态。
func TestForwardImagesViaResponsesTool_EmptyPrompt(t *testing.T) {
	g := &OpenAIGateway{}
	w := httptest.NewRecorder()
	req := &sdk.ForwardRequest{
		Account: &sdk.Account{ID: 1, Credentials: map[string]string{"access_token": "tok"}},
		Body:    []byte(`{"prompt":"","n":1}`),
		Headers: http.Header{},
		Writer:  w,
	}
	outcome, err := g.forwardImagesViaResponsesTool(t.Context(), req)
	if err != nil {
		t.Fatalf("expected nil err for client-side issue, got %v", err)
	}
	if outcome.Kind != sdk.OutcomeClientError {
		t.Errorf("Kind = %v, want OutcomeClientError", outcome.Kind)
	}
	if outcome.Upstream.StatusCode != http.StatusBadRequest {
		t.Errorf("Upstream.StatusCode = %d, want 400", outcome.Upstream.StatusCode)
	}
	if w.Body.Len() != 0 {
		t.Errorf("Core 处理 outcome 前不应提交 writer，实际写入 %q", w.Body.String())
	}
}

// TestValidateImageSize 覆盖 gpt-image-2 size 硬约束的全部分支。
// 包括：空/auto 透传、显式 1.5 跳过、16 倍数对齐、3:1 比例、
// 总像素 [655360, 8294400] 范围、3840 边长上限。
func TestValidateImageSize(t *testing.T) {
	cases := []struct {
		name    string
		size    string
		model   string
		wantErr bool
	}{
		// 透传：空 / auto 不校验
		{"empty size", "", "gpt-image-2", false},
		{"auto size", "auto", "gpt-image-2", false},
		{"AUTO uppercase", "AUTO", "", false},

		// 跳过：显式 gpt-image-1 / 1.5 不应用 SKILL 严格约束
		{"gpt-image-1.5 skips strict check", "1000x1000", "gpt-image-1.5", false},
		{"gpt-image-1 skips strict check", "999x999", "gpt-image-1", false},

		// gpt-image-2 / 空 model：合规
		{"square 1024 ok", "1024x1024", "gpt-image-2", false},
		{"landscape 1536x1024 ok", "1536x1024", "gpt-image-2", false},
		{"4K landscape ok", "3840x2160", "gpt-image-2", false},
		{"empty model treats as gpt-image-2 ok", "1024x1024", "", false},

		// 格式错
		{"malformed size", "1024", "gpt-image-2", true},
		{"non-numeric", "abcxdef", "gpt-image-2", true},

		// 边长超过 3840
		{"oversize edge", "4096x2304", "gpt-image-2", true},

		// 不是 16 的倍数
		{"width not multiple of 16", "1000x1024", "gpt-image-2", true},
		{"height not multiple of 16", "1024x1000", "gpt-image-2", true},

		// 比例 > 3:1
		{"ratio over 3 to 1", "3840x1024", "gpt-image-2", true}, // 3.75:1

		// 总像素低于 655360
		{"too few pixels", "512x512", "gpt-image-2", true}, // 262144

		// 总像素高于 8294400 —— 但同时也会触发"边长超 3840"或"比例超"
		// 单独构造一个仅超总像素而不超其它约束的 case 不可能（3840×3840=14745600，
		// 但比例 1:1 ✓、边长 3840 ✓、16 倍数 ✓，仅超总像素），用这个验证总像素分支
		{"too many pixels", "3840x3840", "gpt-image-2", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateImageSize(tc.size, tc.model)
			if tc.wantErr && err == nil {
				t.Errorf("validateImageSize(%q, %q) = nil, want err", tc.size, tc.model)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateImageSize(%q, %q) = %v, want nil", tc.size, tc.model, err)
			}
		})
	}
}

// TestForwardImagesViaResponsesTool_InvalidSize 客户端在 gpt-image-2 上传非法 size，
// 应在 OAuth/PoW 链路启动前 400 返回，省一次配额。
func TestForwardImagesViaResponsesTool_InvalidSize(t *testing.T) {
	g := &OpenAIGateway{}
	w := httptest.NewRecorder()
	req := &sdk.ForwardRequest{
		Account: &sdk.Account{ID: 1, Credentials: map[string]string{"access_token": "tok"}},
		// size 1000 不是 16 倍数
		Body:    []byte(`{"prompt":"hi","n":1,"model":"gpt-image-2","size":"1000x1000"}`),
		Headers: http.Header{},
		Writer:  w,
	}
	outcome, err := g.forwardImagesViaResponsesTool(t.Context(), req)
	if err != nil {
		t.Fatalf("expected nil err for client-side issue, got %v", err)
	}
	if outcome.Kind != sdk.OutcomeClientError {
		t.Errorf("Kind = %v, want OutcomeClientError", outcome.Kind)
	}
	if outcome.Upstream.StatusCode != http.StatusBadRequest {
		t.Errorf("Upstream.StatusCode = %d, want 400", outcome.Upstream.StatusCode)
	}
	if w.Body.Len() != 0 {
		t.Errorf("Core 处理 outcome 前不应提交 writer，实际写入 %q", w.Body.String())
	}
	if !strings.Contains(string(outcome.Upstream.Body), "16") {
		t.Errorf("error body should mention the 16-multiple constraint: %s", outcome.Upstream.Body)
	}
}

// TestBuildImagesToolCreateMsg_Edit_GPTImage2_SkipsInputFidelity gpt-image-2 始终
// 用 high fidelity 处理输入图，input_fidelity 是 no-op，constraints 不应再追加。
// SKILL.md: "do not set input_fidelity with this model".
func TestBuildImagesToolCreateMsg_Edit_GPTImage2_SkipsInputFidelity(t *testing.T) {
	imageRef := testPNGDataURL(2, 2, func(x, y int) color.RGBA {
		return color.RGBA{R: 80, G: 90, B: 100, A: 255}
	})
	body := []byte(fmt.Sprintf(`{
		"model":"gpt-image-2",
		"prompt":"make it cyberpunk",
		"size":"1024x1024",
		"input_fidelity":"high",
		"image":%q
	}`, imageRef))
	msg, _, _, err := buildImagesToolCreateMsg(body, "application/json", true, openAISessionResolution{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	prompt := gjson.GetBytes(msg, "input.0.content.0.text").String()
	if strings.Contains(prompt, "fidelity") {
		t.Errorf("gpt-image-2 prompt should not mention fidelity, got: %q", prompt)
	}
	instructions := gjson.GetBytes(msg, "instructions").String()
	if strings.Contains(instructions, "fidelity") {
		t.Errorf("gpt-image-2 instructions should not mention fidelity, got: %q", instructions)
	}
	// 其它编辑约束不受影响，校验它们仍然存在
	if !strings.Contains(prompt, "Image 1 is the edit target") {
		t.Errorf("edit-target constraint missing: %q", prompt)
	}
}
