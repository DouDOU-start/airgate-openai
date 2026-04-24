package imgen

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

// Image 单张图像产物。
type Image struct {
	Data    []byte // PNG 二进制
	Ref     string // 原始引用："file-service://file_xxx" 或 "sediment://file_xxx"
	RefKind string // "file-service" 或 "sediment"
}

// Result GenerateImage 返回结果。
type Result struct {
	ConversationID string  // chatgpt.com 会话 id（这次新创建的）
	ModelSlug      string  // conversation.mapping 中 image_gen tool 的最终 model_slug
	Images         []Image // 下载成功的图像产物
}

// GenerateImage 用 Client 绑定的 OAuth access_token 生成一张或多张图。
//
// 完整链路：
//
//	Bootstrap → conversation/init → 启心跳
//	→ chat-requirements → f/conversation/prepare → f/conversation SSE
//	→ 轮询 async-status（fallback: mapping）→ 下载
//
// ctx 当前仅用于取消已启动的轮询循环；底层 HTTP 请求没有 wire up ctx，
// 后续若要支持精确超时需要重构 newReq 为 newReqWithCtx。
//
// 失败时返回 partial result：已经成功下载的图片会在 Result.Images 里，
// error 指明哪一步失败。
func (c *Client) GenerateImage(ctx context.Context, prompt string, images []ImageInput) (*Result, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil, fmt.Errorf("prompt 为空")
	}

	// Step 0: Bootstrap（仅首次）
	if !c.bootstrapped {
		if err := c.Bootstrap(); err != nil {
			log.Printf("[imgen] bootstrap 失败（继续尝试）: %v", err)
		} else {
			c.bootstrapped = true
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Step 0.5: conversation/init 槽位
	if err := c.conversationInit(); err != nil {
		log.Printf("[imgen] conversation/init 失败（继续尝试）: %v", err)
	}

	// 心跳覆盖整个生成期
	stopHB := c.startHeartbeat(30 * time.Second)
	defer stopHB()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Step 0.8: 上传参考图片（/edits 场景）
	var uploaded []*UploadedFile
	for i, img := range images {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		uf, err := c.uploadFile(img)
		if err != nil {
			return nil, fmt.Errorf("上传第 %d 张图片失败: %w", i+1, err)
		}
		uploaded = append(uploaded, uf)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Step 1: chat requirements
	cr, err := c.getChatRequirements()
	if err != nil {
		return nil, fmt.Errorf("获取 chat token 失败: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Step 2: prepare（失败不致命，缺 conduit_token 也能跑）
	conduitToken, err := c.prepareConversation(prompt, cr.ChatToken, cr.ProofToken, "", "", uploaded)
	if err != nil {
		log.Printf("[imgen] prepare 失败（继续尝试）: %v", err)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Step 3: SSE 流式对话
	sr, err := c.streamConversation(prompt, cr.ChatToken, conduitToken, cr.ProofToken, "", "", uploaded)
	if err != nil {
		return nil, fmt.Errorf("流式会话失败: %w", err)
	}

	// Step 4: 定位 asset_pointer
	//   优先级：SSE 直出 file-service > async-status > mapping fallback
	var imageRefs []string
	switch {
	case hasFileService(sr.ImageRefs):
		imageRefs = filterFileService(sr.ImageRefs)
	case sr.ConversationID != "":
		refs, perr := c.pollForImages(sr.ConversationID, 30)
		if perr != nil {
			return nil, fmt.Errorf("轮询失败: %w", perr)
		}
		if fs := filterFileService(refs); len(fs) > 0 {
			imageRefs = fs
		} else {
			imageRefs = refs
		}
	default:
		imageRefs = sr.ImageRefs
	}

	if len(imageRefs) == 0 {
		return nil, fmt.Errorf("未获取到任何图片（可能原因: PoW 未通过 / AT 过期 / 触发风控）")
	}

	modelSlug := ""
	if sr.ConversationID != "" {
		_, modelSlug = c.readMappingRefsAndModel(sr.ConversationID)
		if modelSlug != "" {
			log.Printf("[imgen] 最终 model_slug=%s", modelSlug)
		}
	}

	// Step 5: 下载
	result := &Result{ConversationID: sr.ConversationID, ModelSlug: modelSlug}
	for _, ref := range imageRefs {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		data, err := c.downloadImage(sr.ConversationID, ref)
		if err != nil {
			log.Printf("[imgen] 下载 %s 失败: %v", ref, err)
			continue
		}
		kind := "sediment"
		if strings.HasPrefix(ref, "file-service://") {
			kind = "file-service"
		}
		result.Images = append(result.Images, Image{
			Data:    data,
			Ref:     ref,
			RefKind: kind,
		})
	}

	if len(result.Images) == 0 {
		return result, fmt.Errorf("所有图片下载均失败")
	}
	return result, nil
}
