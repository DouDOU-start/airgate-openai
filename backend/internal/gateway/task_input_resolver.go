package gateway

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
)

// runtimeAssetURLPrefix 是 core 落盘后返回给 plugin 的本地 URL 前缀。
// S3 模式下 core 返回的是完整的 https://... presigned URL 或 publicBaseURL，
// 不走这条路径，直接由 OpenAI 自己拉。
const runtimeAssetURLPrefix = "/assets-runtime/"

// resolveTaskInputAssets 把 task.input 里所有 /assets-runtime/... 形式的 URL 反查
// 成 data:image/*;base64,... 形式，便于后续 shrinkTaskInputImages 与
// buildImageRequestBody 复用现有的 data URI 处理路径。
//
// 设计意图：core 在 tasks.create 时把 base64 大图落盘换成 URL（保证 DB 与 dispatch
// RPC 不爆 64MB 上限），plugin 这一端在真正执行任务前需要把 bytes 拉回来才能
// 喂给 OpenAI。这两步配合，让"前端继续提交 data URI、上游继续吃 data URI"，
// 中间链路（DB + gRPC）则恒小。
//
// 只处理本地 /assets-runtime/ 前缀。外部 http(s) URL 与 data: URI 保留原行为。
func (g *OpenAIGateway) resolveTaskInputAssets(ctx context.Context, input map[string]any) error {
	if len(input) == 0 {
		return nil
	}
	return g.walkResolveAssets(ctx, input)
}

func (g *OpenAIGateway) walkResolveAssets(ctx context.Context, node any) error {
	switch v := node.(type) {
	case map[string]any:
		for k, child := range v {
			if s, ok := child.(string); ok {
				replaced, err := g.maybeResolveRuntimeAssetURL(ctx, s)
				if err != nil {
					return fmt.Errorf("input[%s]: %w", k, err)
				}
				if replaced != s {
					v[k] = replaced
				}
				continue
			}
			if err := g.walkResolveAssets(ctx, child); err != nil {
				return err
			}
		}
	case []any:
		for i, child := range v {
			if s, ok := child.(string); ok {
				replaced, err := g.maybeResolveRuntimeAssetURL(ctx, s)
				if err != nil {
					return fmt.Errorf("input[%d]: %w", i, err)
				}
				if replaced != s {
					v[i] = replaced
				}
				continue
			}
			if err := g.walkResolveAssets(ctx, child); err != nil {
				return err
			}
		}
	}
	return nil
}

func (g *OpenAIGateway) maybeResolveRuntimeAssetURL(ctx context.Context, ref string) (string, error) {
	if !strings.HasPrefix(ref, runtimeAssetURLPrefix) {
		return ref, nil
	}
	objectKey, err := objectKeyFromRuntimeAssetURL(ref)
	if err != nil {
		return "", err
	}
	data, contentType, err := g.fetchAssetBytes(ctx, objectKey)
	if err != nil {
		return "", err
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

// objectKeyFromRuntimeAssetURL 把 /assets-runtime/<escapedKey> 反推成原始 objectKey。
// 与 asset_storage.go 的 escapeAssetKey 互逆 —— 每段单独 url.PathUnescape 后再拼回去。
func objectKeyFromRuntimeAssetURL(ref string) (string, error) {
	rest := strings.TrimPrefix(ref, runtimeAssetURLPrefix)
	if rest == "" {
		return "", fmt.Errorf("runtime asset URL 缺少 object key: %s", ref)
	}
	// 把可能存在的 query string 砍掉（缩略图 ?w=256 不影响原始 key 解析）
	if q := strings.IndexByte(rest, '?'); q >= 0 {
		rest = rest[:q]
	}
	parts := strings.Split(rest, "/")
	for i, part := range parts {
		decoded, err := url.PathUnescape(part)
		if err != nil {
			return "", fmt.Errorf("unescape object key segment: %w", err)
		}
		parts[i] = decoded
	}
	return strings.Join(parts, "/"), nil
}
