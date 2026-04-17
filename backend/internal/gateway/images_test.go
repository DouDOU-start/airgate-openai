package gateway

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

func TestIsImagesRequest(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/v1/images/generations", true},
		{"/images/generations", true},
		{"/v1/responses", false},
		{"/v1/chat/completions", false},
		{"/v1/images/edits", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isImagesRequest(tc.path); got != tc.want {
			t.Errorf("isImagesRequest(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestHandleImagesResponse_TokenAttribution 覆盖官方响应格式：
//   - usage.input_tokens / output_tokens 落入 ForwardResult
//   - cached tokens 从 input 中扣减，避免重复计费
//   - fillCost 根据 gpt-image-1.5 定价填充费用
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

	result, err := handleImagesResponse(resp, w, time.Now(), "gpt-image-1.5")
	if err != nil {
		t.Fatalf("handleImagesResponse returned err: %v", err)
	}

	if result.Model != "gpt-image-1.5" {
		t.Errorf("Model = %q, want gpt-image-1.5", result.Model)
	}
	if result.InputTokens != 40 {
		t.Errorf("InputTokens = %d, want 40 (50 - 10 cached)", result.InputTokens)
	}
	if result.OutputTokens != 4160 {
		t.Errorf("OutputTokens = %d, want 4160", result.OutputTokens)
	}
	if result.CachedInputTokens != 10 {
		t.Errorf("CachedInputTokens = %d, want 10", result.CachedInputTokens)
	}

	// gpt-image-1.5 standard: input=$5/1M, cached=$1.25/1M, output=$40/1M
	// InputCost = 40/1e6 * 5 = 0.0002
	// CachedInputCost = 10/1e6 * 1.25 = 0.0000125
	// OutputCost = 4160/1e6 * 40 = 0.1664
	if !almostEqual(result.InputCost, 0.0002, 1e-9) {
		t.Errorf("InputCost = %v, want 0.0002", result.InputCost)
	}
	if !almostEqual(result.CachedInputCost, 0.0000125, 1e-9) {
		t.Errorf("CachedInputCost = %v, want 0.0000125", result.CachedInputCost)
	}
	if !almostEqual(result.OutputCost, 0.1664, 1e-9) {
		t.Errorf("OutputCost = %v, want 0.1664", result.OutputCost)
	}

	// 客户端侧响应体应原样透传
	if w.Code != http.StatusOK {
		t.Errorf("writer status = %d, want 200", w.Code)
	}
	gotBody, _ := io.ReadAll(w.Result().Body)
	if len(gotBody) != len(body) {
		t.Errorf("response body len = %d, want %d", len(gotBody), len(body))
	}
}

// TestHandleImagesResponse_FallbackModelWhenBodyLacksModel 验证 Images 响应里
// 没有 model 字段时，会回退到请求侧传入的 fallbackModel。否则 fillCost 查不到
// 定价，账单会失真。
func TestHandleImagesResponse_FallbackModelWhenBodyLacksModel(t *testing.T) {
	body := `{"data":[{"url":"https://example/a.png"}],"usage":{"input_tokens":10,"output_tokens":100}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       ioNopCloserFromString(body),
	}

	result, err := handleImagesResponse(resp, nil, time.Now(), "gpt-image-1")
	if err != nil {
		t.Fatalf("handleImagesResponse returned err: %v", err)
	}
	if result.Model != "gpt-image-1" {
		t.Fatalf("Model = %q, want gpt-image-1 (fallback)", result.Model)
	}
	// Writer 为 nil 时应把 body/header 直接带回给 core
	if len(result.Body) != len(body) {
		t.Errorf("ForwardResult.Body len = %d, want %d", len(result.Body), len(body))
	}
	if result.Headers.Get("Content-Type") != "application/json" {
		t.Errorf("ForwardResult.Headers Content-Type not preserved")
	}
	// output 非零则 OutputCost 应大于零
	if result.OutputCost <= 0 {
		t.Errorf("OutputCost = %v, want > 0", result.OutputCost)
	}
}

// TestFillCost_GPTImage1_Priority 覆盖 gpt-image-1 的 priority 档：
// priority 按 std() 约定 = standard × 2。
func TestFillCost_GPTImage1_Priority(t *testing.T) {
	result := &sdk.ForwardResult{
		Model:        "gpt-image-1",
		InputTokens:  100,
		OutputTokens: 1000,
		ServiceTier:  "priority",
	}
	fillCost(result)
	// priority: input=$10/1M, output=$80/1M
	// InputCost = 100/1e6 * 10 = 0.001
	// OutputCost = 1000/1e6 * 80 = 0.08
	if !almostEqual(result.InputCost, 0.001, 1e-9) {
		t.Errorf("InputCost = %v, want 0.001", result.InputCost)
	}
	if !almostEqual(result.OutputCost, 0.08, 1e-9) {
		t.Errorf("OutputCost = %v, want 0.08", result.OutputCost)
	}
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
	var result sdk.ForwardResult
	var toolIn, toolOut int
	parseSSEUsage(data, &result, &toolIn, &toolOut)
	if result.Model != "gpt-5.4" {
		t.Errorf("Model = %q, want gpt-5.4", result.Model)
	}
	if result.InputTokens != 100 || result.OutputTokens != 50 {
		t.Errorf("Input/Output = %d/%d, want 100/50", result.InputTokens, result.OutputTokens)
	}
	if toolIn != 8 || toolOut != 4160 {
		t.Errorf("toolIn/Out = %d/%d, want 8/4160", toolIn, toolOut)
	}
}

// TestFillCostWithImageTool 叠加计费：主 model (gpt-5.4) 的 chat token 按
// 其单价、tool_usage 的图像 token 按 gpt-image-1.5 单价分别结算，最后合到
// InputCost/OutputCost 里。
func TestFillCostWithImageTool(t *testing.T) {
	result := &sdk.ForwardResult{
		Model:        "gpt-5.4",
		InputTokens:  1000,
		OutputTokens: 500,
	}
	fillCostWithImageTool(result, 10, 4160)

	// 主 model (gpt-5.4 standard): input=$2.5/1M, output=$15/1M
	// chatInputCost  = 1000/1e6 * 2.5 = 0.0025
	// chatOutputCost = 500/1e6  * 15  = 0.0075
	// image tool (gpt-image-1.5 standard): input=$5/1M, output=$40/1M
	// imgInputCost   = 10/1e6  * 5   = 0.00005
	// imgOutputCost  = 4160/1e6 * 40 = 0.1664
	// total InputCost  = 0.00255
	// total OutputCost = 0.1739
	if !almostEqual(result.InputCost, 0.00255, 1e-9) {
		t.Errorf("InputCost = %v, want 0.00255", result.InputCost)
	}
	if !almostEqual(result.OutputCost, 0.1739, 1e-9) {
		t.Errorf("OutputCost = %v, want 0.1739", result.OutputCost)
	}
	// Tokens 累加：主 + 图像 tool
	if result.InputTokens != 1010 {
		t.Errorf("InputTokens = %d, want 1010 (1000 + 10)", result.InputTokens)
	}
	if result.OutputTokens != 4660 {
		t.Errorf("OutputTokens = %d, want 4660 (500 + 4160)", result.OutputTokens)
	}
	// 单价展示字段保留主 model（混合调用下 total_cost 才是权威）
	if !almostEqual(result.InputPrice, 2.5, 1e-9) {
		t.Errorf("InputPrice = %v, want 2.5 (gpt-5.4 standard)", result.InputPrice)
	}
}

// TestFillCostWithImageTool_NoToolUsage 退化为普通 fillCost 时行为不变。
func TestFillCostWithImageTool_NoToolUsage(t *testing.T) {
	result := &sdk.ForwardResult{
		Model:        "gpt-5.4",
		InputTokens:  1000,
		OutputTokens: 500,
	}
	fillCostWithImageTool(result, 0, 0)
	if result.InputTokens != 1000 || result.OutputTokens != 500 {
		t.Errorf("token counts mutated when no image tool usage")
	}
	// 仅 gpt-5.4 standard pricing
	if !almostEqual(result.InputCost, 0.0025, 1e-9) {
		t.Errorf("InputCost = %v, want 0.0025", result.InputCost)
	}
}

// TestCollectImageGenCall 抽取 output_item.done 里的 image_generation_call 条目。
func TestCollectImageGenCall(t *testing.T) {
	item := map[string]any{
		"type":           "image_generation_call",
		"status":         "completed",
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
	// 缺 result 的也应被忽略
	collectImageGenCall(&ws, map[string]any{"type": "image_generation_call"})
	if len(ws.ImageGenCalls) != 1 {
		t.Errorf("item without result should be ignored")
	}
}

// TestBuildImagesToolCreateMsg 翻译 Images REST 请求体为 Responses API
// response.create 消息，tool 配置应正确透传 size/quality/background 等字段。
func TestBuildImagesToolCreateMsg(t *testing.T) {
	body := []byte(`{"model":"gpt-image-1.5","prompt":"a shiba","n":1,"size":"1024x1024","quality":"low","background":"transparent","output_format":"png"}`)
	msg, n, promptTokens, err := buildImagesToolCreateMsg(body, openAISessionResolution{})
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

	// 结构检查
	if gjson.GetBytes(msg, "type").String() != "response.create" {
		t.Errorf("type = %q, want response.create", gjson.GetBytes(msg, "type").String())
	}
	if gjson.GetBytes(msg, "model").String() != imagesOAuthChatModel {
		t.Errorf("model = %q, want %q", gjson.GetBytes(msg, "model").String(), imagesOAuthChatModel)
	}
	// input 必须是 list 形式
	inputItem := gjson.GetBytes(msg, "input.0")
	if inputItem.Get("type").String() != "message" || inputItem.Get("role").String() != "user" {
		t.Errorf("input[0] type/role wrong: %s", inputItem.Raw)
	}
	if inputItem.Get("content.0.text").String() != "a shiba" {
		t.Errorf("input[0].content[0].text = %q, want a shiba", inputItem.Get("content.0.text").String())
	}
	tool := gjson.GetBytes(msg, "tools.0")
	if tool.Get("type").String() != "image_generation" {
		t.Errorf("tools[0].type = %q, want image_generation", tool.Get("type").String())
	}
	if tool.Get("size").String() != "1024x1024" {
		t.Errorf("tools[0].size = %q, want 1024x1024", tool.Get("size").String())
	}
	if tool.Get("quality").String() != "low" {
		t.Errorf("tools[0].quality = %q, want low", tool.Get("quality").String())
	}
	if tool.Get("background").String() != "transparent" {
		t.Errorf("tools[0].background = %q, want transparent", tool.Get("background").String())
	}
	// Responses API image_generation tool schema 不包含 n 字段，此处不应出现
	if tool.Get("n").Exists() {
		t.Errorf("tools[0].n should not be present (image_generation tool schema forbids it)")
	}
}

// TestBuildImagesToolCreateMsg_NGreaterThanOne V1 不支持 n>1，应直接返错。
func TestBuildImagesToolCreateMsg_NGreaterThanOne(t *testing.T) {
	body := []byte(`{"prompt":"x","n":3}`)
	_, _, _, err := buildImagesToolCreateMsg(body, openAISessionResolution{})
	if err == nil {
		t.Fatal("expected err for n>1, got nil")
	}
}

// TestBuildImagesToolCreateMsg_EmptyPrompt prompt 空串应报错。
func TestBuildImagesToolCreateMsg_EmptyPrompt(t *testing.T) {
	_, _, _, err := buildImagesToolCreateMsg([]byte(`{"n":1}`), openAISessionResolution{})
	if err == nil {
		t.Fatal("expected err for empty prompt, got nil")
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
// root 级 model = gpt-image-1.5。instructions / 工具包装的 chat tokens 不暴露。
func TestBuildImagesRESTResponse(t *testing.T) {
	ws := WSResult{
		InputTokens:           4808, // chat text tokens (内层吸收，不对外)
		OutputTokens:          40,   // chat output tokens (内层吸收，不对外)
		ToolImageInputTokens:  0,
		ToolImageOutputTokens: 4160,
		ImageGenCalls: []ImageGenCall{
			{Result: "PNG_BASE64_A", RevisedPrompt: "revised a"},
			{Result: "PNG_BASE64_B"},
		},
	}
	promptTokens := 12
	imageOut := 4160
	body := buildImagesRESTResponse(ws, promptTokens, imageOut)

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["model"] != "gpt-image-1.5" {
		t.Errorf("root model = %v, want gpt-image-1.5", got["model"])
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

// TestBuildImagesRESTResponse_ChainedCostParity 验证 AirGate 套 AirGate 时两级
// 金额一致：下一级拿到 body 按 gpt-image-1.5 单价重算，应等于本级 fillCost 结果。
func TestBuildImagesRESTResponse_ChainedCostParity(t *testing.T) {
	promptTokens := 12
	imageOut := 1056
	ws := WSResult{ImageGenCalls: []ImageGenCall{{Result: "X"}}}
	body := buildImagesRESTResponse(ws, promptTokens, imageOut)

	// 本级成本：用 fillCost 按 gpt-image-1.5 算
	inner := &sdk.ForwardResult{
		Model:        "gpt-image-1.5",
		InputTokens:  promptTokens,
		OutputTokens: imageOut,
	}
	fillCost(inner)
	innerCost := inner.InputCost + inner.OutputCost

	// 下一级：从 body 读 usage + model，再按 fillCost 算
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	u := got["usage"].(map[string]any)
	outer := &sdk.ForwardResult{
		Model:        got["model"].(string),
		InputTokens:  int(u["input_tokens"].(float64)),
		OutputTokens: int(u["output_tokens"].(float64)),
	}
	fillCost(outer)
	outerCost := outer.InputCost + outer.OutputCost

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
// 翻译层应在未建立 WS 连接的情况下返回 400 + AccountStatus=OK，不伤账号状态。
func TestForwardImagesViaResponsesTool_EmptyPrompt(t *testing.T) {
	g := &OpenAIGateway{}
	w := httptest.NewRecorder()
	req := &sdk.ForwardRequest{
		Account: &sdk.Account{ID: 1, Credentials: map[string]string{"access_token": "tok"}},
		Body:    []byte(`{"prompt":"","n":1}`),
		Headers: http.Header{},
		Writer:  w,
	}
	result, err := g.forwardImagesViaResponsesTool(t.Context(), req)
	if err != nil {
		t.Fatalf("expected nil err for client-side issue, got %v", err)
	}
	if result.StatusCode != http.StatusBadRequest {
		t.Errorf("StatusCode = %d, want 400", result.StatusCode)
	}
	if result.AccountStatus != sdk.AccountStatusOK {
		t.Errorf("AccountStatus = %q, want OK (not a failure)", result.AccountStatus)
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("writer status = %d, want 400", w.Code)
	}
}
