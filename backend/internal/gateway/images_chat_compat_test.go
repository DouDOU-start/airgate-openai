package gateway

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// ── extractPromptFromMessages ──

func TestExtractPromptFromMessages(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"simple text", `{"messages":[{"role":"user","content":"a cute cat"}]}`, "a cute cat"},
		{"multimodal content array", `{"messages":[{"role":"user","content":[{"type":"text","text":"draw a dog"}]}]}`, "draw a dog"},
		{"system + user", `{"messages":[{"role":"system","content":"you are helpful"},{"role":"user","content":"hello"}]}`, "hello"},
		{"multiple user takes last", `{"messages":[{"role":"user","content":"first"},{"role":"user","content":"second"}]}`, "second"},
		{"no user message", `{"messages":[{"role":"system","content":"sys"}]}`, ""},
		{"empty messages", `{"messages":[]}`, ""},
		{"no messages field", `{"prompt":"test"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractPromptFromMessages([]byte(tc.body))
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// ── extractChatImageInputs ──

func TestExtractChatImageInputs(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantPrompt string
		wantImages int
	}{
		{
			name:       "text only",
			body:       `{"messages":[{"role":"user","content":"a cat"}]}`,
			wantPrompt: "a cat",
			wantImages: 0,
		},
		{
			name: "text + image",
			body: `{"messages":[{"role":"user","content":[
				{"type":"text","text":"edit this"},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,abc123"}}
			]}]}`,
			wantPrompt: "edit this",
			wantImages: 1,
		},
		{
			name: "multiple images",
			body: `{"messages":[{"role":"user","content":[
				{"type":"text","text":"combine"},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,img1"}},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,img2"}}
			]}]}`,
			wantPrompt: "combine",
			wantImages: 2,
		},
		{
			name:       "no user message",
			body:       `{"messages":[{"role":"system","content":"sys"}]}`,
			wantPrompt: "",
			wantImages: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prompt, images, err := extractChatImageInputs([]byte(tc.body))
			if err != nil {
				t.Fatalf("extractChatImageInputs err: %v", err)
			}
			if prompt != tc.wantPrompt {
				t.Errorf("prompt = %q, want %q", prompt, tc.wantPrompt)
			}
			if len(images) != tc.wantImages {
				t.Errorf("images count = %d, want %d", len(images), tc.wantImages)
			}
		})
	}
}

func TestExtractChatImageInputs_NormalizesDataImageURL(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[
		{"type":"text","text":"edit this"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD\nRA"}}
	]}]}`)
	_, images, err := extractChatImageInputs(body)
	if err != nil {
		t.Fatalf("extractChatImageInputs err: %v", err)
	}
	if len(images) != 1 || images[0] != "data:image/png;base64,QUJDRA==" {
		t.Fatalf("images = %#v", images)
	}
}

func TestExtractChatImageInputsRejectsUnsupportedImageURL(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[
		{"type":"text","text":"edit this"},
		{"type":"image_url","image_url":{"url":"QUJDRA=="}}
	]}]}`)
	_, _, err := extractChatImageInputs(body)
	if err == nil {
		t.Fatal("expected err for unsupported image_url, got nil")
	}
}

// ── buildChatCompatImagePayload ──

func TestBuildChatCompatImagePayload(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		chatBody := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
		p := buildChatCompatImagePayload(chatBody, "gpt-image-2", "hi", nil)
		if _, ok := p["size"]; ok {
			t.Errorf("default size should be absent, got %v", p["size"])
		}
		if p["n"] != 1 {
			t.Errorf("default n = %v, want 1", p["n"])
		}
		if _, ok := p["image"]; ok {
			t.Error("image field should be absent for generations")
		}
	})

	t.Run("with params", func(t *testing.T) {
		chatBody := []byte(`{"messages":[],"size":"1536x1024","quality":"high","n":2,"background":"transparent","output_format":"webp"}`)
		p := buildChatCompatImagePayload(chatBody, "gpt-image-1", "draw", nil)
		if p["size"] != "1536x1024" {
			t.Errorf("size = %v", p["size"])
		}
		if p["quality"] != "high" {
			t.Errorf("quality = %v", p["quality"])
		}
		if p["n"] != 2 {
			t.Errorf("n = %v", p["n"])
		}
		if p["background"] != "transparent" {
			t.Errorf("background = %v", p["background"])
		}
		if p["output_format"] != "webp" {
			t.Errorf("output_format = %v", p["output_format"])
		}
	})

	t.Run("clamps oversized size", func(t *testing.T) {
		chatBody := []byte(`{"messages":[],"size":"4096x2304"}`)
		p := buildChatCompatImagePayload(chatBody, "gpt-image-1", "draw", nil)
		if p["size"] != "3840x2160" {
			t.Errorf("size = %v, want 3840x2160", p["size"])
		}
	})

	t.Run("with single image", func(t *testing.T) {
		chatBody := []byte(`{}`)
		p := buildChatCompatImagePayload(chatBody, "gpt-image-1", "edit", []string{"data:img"})
		if p["image"] != "data:img" {
			t.Errorf("single image = %v", p["image"])
		}
	})

	t.Run("with multiple images", func(t *testing.T) {
		chatBody := []byte(`{}`)
		p := buildChatCompatImagePayload(chatBody, "gpt-image-1", "merge", []string{"img1", "img2"})
		imgs, ok := p["image"].([]string)
		if !ok || len(imgs) != 2 {
			t.Errorf("multiple images = %v", p["image"])
		}
	})
}

// ── imagesToChatCompletion ──

func TestImagesToChatCompletion(t *testing.T) {
	imagesResp := `{
		"created": 1700000000,
		"data": [{"b64_json":"aWxvdmVjYXRz","revised_prompt":"cute cat"}],
		"model": "gpt-image-2",
		"usage": {"input_tokens":10,"output_tokens":1056,"total_tokens":1066}
	}`
	result := imagesToChatCompletion([]byte(imagesResp), "gpt-image-2")

	if gjson.GetBytes(result, "object").String() != "chat.completion" {
		t.Error("object != chat.completion")
	}
	if gjson.GetBytes(result, "model").String() != "gpt-image-2" {
		t.Error("model != gpt-image-2")
	}
	if gjson.GetBytes(result, "choices.0.finish_reason").String() != "stop" {
		t.Error("finish_reason != stop")
	}
	if !strings.Contains(gjson.GetBytes(result, "choices.0.message.content").String(), "data:image/png;base64,") {
		t.Error("content missing base64 data URL")
	}
	if gjson.GetBytes(result, "usage.prompt_tokens").Int() != 10 {
		t.Errorf("prompt_tokens = %d", gjson.GetBytes(result, "usage.prompt_tokens").Int())
	}
	if gjson.GetBytes(result, "usage.completion_tokens").Int() != 1056 {
		t.Errorf("completion_tokens = %d", gjson.GetBytes(result, "usage.completion_tokens").Int())
	}
	if gjson.GetBytes(result, "usage.total_tokens").Int() != 1066 {
		t.Errorf("total_tokens = %d", gjson.GetBytes(result, "usage.total_tokens").Int())
	}
}

func TestImagesToChatCompletion_MultipleImages(t *testing.T) {
	imagesResp := `{"created":1700000000,"data":[{"b64_json":"aW1nMQ=="},{"b64_json":"aW1nMg=="}]}`
	result := imagesToChatCompletion([]byte(imagesResp), "gpt-image-1")
	content := gjson.GetBytes(result, "choices.0.message.content").String()
	if count := strings.Count(content, "![image]"); count != 2 {
		t.Errorf("expected 2 images in content, got %d", count)
	}
}

func TestImagesToChatCompletion_NoUsage(t *testing.T) {
	imagesResp := `{"created":1700000000,"data":[{"b64_json":"dGVzdA=="}]}`
	result := imagesToChatCompletion([]byte(imagesResp), "gpt-image-2")
	if gjson.GetBytes(result, "usage.total_tokens").Int() < 1 {
		t.Error("total_tokens should be at least 1")
	}
}

// ── imagesToChatCompletionChunks (streaming) ──

func TestImagesToChatCompletionChunks(t *testing.T) {
	imagesResp := `{
		"created": 1700000000,
		"data": [{"b64_json":"dGVzdA=="}],
		"usage": {"input_tokens":5,"output_tokens":272,"total_tokens":277}
	}`
	chunks := imagesToChatCompletionChunks([]byte(imagesResp), "gpt-image-2")
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(chunks))
	}

	// chunk 0: role
	if gjson.GetBytes(chunks[0], "object").String() != "chat.completion.chunk" {
		t.Error("chunk 0 object wrong")
	}
	if gjson.GetBytes(chunks[0], "choices.0.delta.role").String() != "assistant" {
		t.Error("chunk 0 should have role delta")
	}

	// chunk 1: content
	if !strings.Contains(gjson.GetBytes(chunks[1], "choices.0.delta.content").String(), "data:image/png;base64,") {
		t.Error("chunk 1 should have image content")
	}

	// chunk 2: finish + usage
	if gjson.GetBytes(chunks[2], "choices.0.finish_reason").String() != "stop" {
		t.Error("chunk 2 should have finish_reason=stop")
	}
	if gjson.GetBytes(chunks[2], "usage.prompt_tokens").Int() != 5 {
		t.Errorf("chunk 2 prompt_tokens = %d", gjson.GetBytes(chunks[2], "usage.prompt_tokens").Int())
	}

	// all chunks share same id
	id0 := gjson.GetBytes(chunks[0], "id").String()
	id1 := gjson.GetBytes(chunks[1], "id").String()
	id2 := gjson.GetBytes(chunks[2], "id").String()
	if id0 != id1 || id1 != id2 {
		t.Errorf("chunk IDs should be identical: %q %q %q", id0, id1, id2)
	}
}

// ── buildImageContent ──

func TestBuildImageContent(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "b64_json",
			body: `{"data":[{"b64_json":"abc"}]}`,
			want: "![image](data:image/png;base64,abc)",
		},
		{
			name: "url",
			body: `{"data":[{"url":"https://example.com/img.png"}]}`,
			want: "![image](https://example.com/img.png)",
		},
		{
			name: "empty data",
			body: `{"data":[]}`,
			want: "Image generation completed but no image data returned.",
		},
		{
			name: "no data field",
			body: `{}`,
			want: "Image generation completed but no image data returned.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildImageContent([]byte(tc.body))
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// ── isChatCompatImageModel ──

func TestIsChatCompatImageModel(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"gpt-image-1", true},
		{"gpt-image-1.5", true},
		{"gpt-image-2", true},
		{"gpt-5.4", false},
		{"gpt-5.3-codex", false},
		{"gpt-5.4-mini", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isChatCompatImageModel(tc.model); got != tc.want {
			t.Errorf("isChatCompatImageModel(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}
}

// ── writeSSEError ──

func TestWriteSSEError(t *testing.T) {
	w := httptest.NewRecorder()
	writeSSEError(w, "upstream request id 349f8894")
	body := w.Body.String()
	if strings.Contains(body, "upstream request id") || strings.Contains(body, "349f8894") {
		t.Fatalf("response leaked upstream error: %q", body)
	}
	payload := strings.TrimPrefix(strings.Split(body, "\n")[0], "data: ")
	if !gjson.Valid(payload) {
		t.Fatalf("error event is not valid JSON: %q", payload)
	}
	if gjson.Get(payload, "error.message").String() != sanitizedImageSSEErrorMessage {
		t.Error("error message mismatch")
	}
	if gjson.Get(payload, "error.type").String() != "server_error" {
		t.Error("error type mismatch")
	}
}
