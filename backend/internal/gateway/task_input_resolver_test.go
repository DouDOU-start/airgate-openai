package gateway

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

func TestObjectKeyFromRuntimeAssetURL(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{
			name: "simple",
			url:  "/assets-runtime/gateway-openai/task-inputs/user-42/abcdef.png",
			want: "gateway-openai/task-inputs/user-42/abcdef.png",
		},
		{
			name: "escaped segment",
			url:  "/assets-runtime/gateway-openai%2Fweird/inputs/user-1/a%20b.png",
			want: "gateway-openai/weird/inputs/user-1/a b.png",
		},
		{
			name: "with query string (thumbnail)",
			url:  "/assets-runtime/gateway-openai/task-inputs/user-1/x.png?w=256",
			want: "gateway-openai/task-inputs/user-1/x.png",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := objectKeyFromRuntimeAssetURL(tc.url)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestObjectKeyFromRuntimeAssetURL_Errors(t *testing.T) {
	if _, err := objectKeyFromRuntimeAssetURL("/assets-runtime/"); err == nil {
		t.Fatalf("empty key should error")
	}
	if _, err := objectKeyFromRuntimeAssetURL("/assets-runtime/bad%ZZ.png"); err == nil {
		t.Fatalf("invalid percent escape should error")
	}
}

// fakeHost 截获 assets.get_bytes 调用并返回预设字节。其它方法不实现。
type fakeHost struct {
	bytesByKey map[string][]byte
	mime       string
	calls      int
}

func (f *fakeHost) Invoke(ctx context.Context, req sdk.HostInvokeRequest) (*sdk.HostInvokeResponse, error) {
	f.calls++
	if req.Method != hostMethodAssetsGetBytes {
		return nil, fmt.Errorf("unexpected method: %s", req.Method)
	}
	key, _ := req.Payload["object_key"].(string)
	data, ok := f.bytesByKey[key]
	if !ok {
		return &sdk.HostInvokeResponse{
			Status:  "error",
			Payload: map[string]any{"message": "not found: " + key},
		}, nil
	}
	mime := f.mime
	if mime == "" {
		mime = "image/png"
	}
	// 模拟 core 的 json.Marshal 行为：[]byte 序列化成 base64 string
	return &sdk.HostInvokeResponse{
		Status: "ok",
		Payload: map[string]any{
			"data":         base64.StdEncoding.EncodeToString(data),
			"content_type": mime,
		},
	}, nil
}

func (f *fakeHost) InvokeStream(ctx context.Context, req sdk.HostStreamRequest) (sdk.HostStream, error) {
	return nil, fmt.Errorf("not used")
}

func TestResolveTaskInputAssets_ReplacesRuntimeURLs(t *testing.T) {
	const key = "gateway-openai/task-inputs/user-42/abc.png"
	imgBytes := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	g := &OpenAIGateway{host: &fakeHost{
		bytesByKey: map[string][]byte{key: imgBytes},
		mime:       "image/png",
	}}
	input := map[string]any{
		"prompt": "rotate",
		"images": []any{
			"/assets-runtime/" + key,
			"https://cdn.example.com/keep-as-url.png",
			"data:image/png;base64,AAAA",
		},
		"mask": "/assets-runtime/" + key,
	}
	if err := g.resolveTaskInputAssets(context.Background(), input); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	images := input["images"].([]any)
	if got := images[0].(string); !strings.HasPrefix(got, "data:image/png;base64,") {
		t.Fatalf("images[0] not resolved: %q", got[:minInt(60, len(got))])
	}
	if got := images[1].(string); got != "https://cdn.example.com/keep-as-url.png" {
		t.Fatalf("external URL was mutated: %q", got)
	}
	if got := images[2].(string); got != "data:image/png;base64,AAAA" {
		t.Fatalf("inline data URI was mutated: %q", got)
	}
	if got := input["mask"].(string); !strings.HasPrefix(got, "data:image/png;base64,") {
		t.Fatalf("mask not resolved: %q", got[:minInt(60, len(got))])
	}
}

func TestResolveTaskInputAssets_NoopOnEmpty(t *testing.T) {
	g := &OpenAIGateway{host: &fakeHost{}}
	if err := g.resolveTaskInputAssets(context.Background(), nil); err != nil {
		t.Fatalf("nil input: %v", err)
	}
	if err := g.resolveTaskInputAssets(context.Background(), map[string]any{}); err != nil {
		t.Fatalf("empty input: %v", err)
	}
}

func TestResolveTaskInputAssets_PropagatesHostError(t *testing.T) {
	g := &OpenAIGateway{host: &fakeHost{bytesByKey: map[string][]byte{}}}
	input := map[string]any{
		"images": []any{"/assets-runtime/missing/key.png"},
	}
	err := g.resolveTaskInputAssets(context.Background(), input)
	if err == nil {
		t.Fatalf("expected error for missing key")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error should mention not found: %v", err)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
