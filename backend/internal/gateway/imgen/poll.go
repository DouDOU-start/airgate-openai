package imgen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

// ---------- stream_status ----------
//
// 返回 conversation 当前 SSE 流是否仍在活动（= 异步任务未跑完）。仅做观察。

func (c *Client) streamStatus(conversationID string) (isActive bool, err error) {
	req, rerr := c.newReq("GET", "/backend-api/conversation/"+conversationID+"/stream_status", nil)
	if rerr != nil {
		return false, rerr
	}
	resp, derr := c.http.Do(req)
	if derr != nil {
		return false, derr
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return false, fmt.Errorf("stream_status HTTP %d", resp.StatusCode)
	}
	var out struct {
		IsActive bool   `json:"is_active"`
		Status   string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, err
	}
	if out.IsActive {
		return true, nil
	}
	if out.Status == "active" || out.Status == "in_progress" {
		return true, nil
	}
	return false, nil
}

// ---------- async-status ----------

// AsyncStatusResult async-status 的规范化结果。
type AsyncStatusResult struct {
	Completed     bool
	AssetPointers []string // "file-service://..." 或 "sediment://..."
	RawStatus     string   // 原始 status 字段（调试用）
}

// asyncStatus POST /backend-api/conversation/{id}/async-status
//
// 对齐调研文档：查询会话异步任务（图像生成等）完成状态，
// 返回 status + result.asset_pointer(s)。completed 且有 asset_pointer 即可直接下载。
//
// body 结构未完全验证，先尝试空 {}。响应结构兼容几种常见 shape：
//  1. 单任务：{ "status": "completed", "result": { "asset_pointer": "..." } }
//  2. 单任务多图：{ "status": "completed", "result": { "asset_pointers": [...] } }
//  3. 多任务数组：{ "tasks": [ { "status": "...", "result": { "asset_pointer": "..." } } ] }
func (c *Client) asyncStatus(conversationID string) (*AsyncStatusResult, error) {
	req, err := c.newReq("POST", "/backend-api/conversation/"+conversationID+"/async-status",
		bytes.NewReader([]byte(`{}`)))
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("async-status HTTP %d: %s", resp.StatusCode, body)
	}

	var raw struct {
		Status string `json:"status"`
		Result struct {
			AssetPointer  string   `json:"asset_pointer"`
			AssetPointers []string `json:"asset_pointers"`
		} `json:"result"`
		Tasks []struct {
			Status string `json:"status"`
			Result struct {
				AssetPointer  string   `json:"asset_pointer"`
				AssetPointers []string `json:"asset_pointers"`
			} `json:"result"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("async-status 响应解析失败: %w，body=%s", err, body)
	}

	r := &AsyncStatusResult{RawStatus: raw.Status}
	seen := map[string]bool{}
	addPointer := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		r.AssetPointers = append(r.AssetPointers, p)
	}

	if raw.Status != "" {
		r.Completed = (raw.Status == "completed")
		addPointer(raw.Result.AssetPointer)
		for _, p := range raw.Result.AssetPointers {
			addPointer(p)
		}
	}

	if len(raw.Tasks) > 0 {
		all := true
		for _, t := range raw.Tasks {
			if t.Status != "completed" {
				all = false
			}
			addPointer(t.Result.AssetPointer)
			for _, p := range t.Result.AssetPointers {
				addPointer(p)
			}
		}
		r.Completed = r.Completed || (all && len(r.AssetPointers) > 0)
	}

	return r, nil
}

// ---------- Step 4: 轮询策略 ----------
//
//	主路径：async-status → completed + asset_pointer → 立即返回
//	辅路径：stream_status 仅做观察
//	Fallback：mapping 拉 conversation 读 tool 消息的 asset_pointer；连续 N 轮
//	          不变或含 file-service 即认。
//
// 不区分 IMG1/IMG2 —— sediment:// 在当前现网就是终稿。

const (
	defaultImagePollAttempts = 30
	gptImage2PollAttempts    = 100
)

func imagePollAttempts(model string) int {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "gpt-image-2") {
		return gptImage2PollAttempts
	}
	return defaultImagePollAttempts
}

func (c *Client) pollForImages(conversationID string, maxAttempts int) ([]string, error) {
	const (
		interval     = 3 * time.Second
		stableRounds = 2
	)

	var (
		lastFallbackSig string
		stableCount     int
		asyncFailCount  int
	)

	logger := slog.Default()
	for i := 0; i < maxAttempts; i++ {
		time.Sleep(interval)
		logger.Debug("imgen_poll_attempt", "attempt", i+1, "max_attempts", maxAttempts)

		// 主路径：async-status
		if asyncFailCount < 3 {
			if as, err := c.asyncStatus(conversationID); err != nil {
				asyncFailCount++
				logger.Warn("imgen_async_status_failed",
					"fail_count", asyncFailCount,
					"max_fail", 3,
					sdk.LogFieldError, err,
				)
			} else {
				asyncFailCount = 0
				if as.Completed && len(as.AssetPointers) > 0 {
					logger.Debug("imgen_poll_completed",
						"source", "async_status",
						"asset_count", len(as.AssetPointers),
					)
					return as.AssetPointers, nil
				}
			}
		}

		// 辅路径：stream_status（仅日志）
		_, _ = c.streamStatus(conversationID)

		// Fallback：mapping
		refs, modelSlug := c.readMappingRefsAndModel(conversationID)
		if len(refs) == 0 {
			continue
		}

		for _, r := range refs {
			if strings.HasPrefix(r, "file-service://") {
				attrs := []any{
					"source", "mapping_file_service",
					"ref_count", len(refs),
				}
				if modelSlug != "" {
					attrs = append(attrs, sdk.LogFieldModel, modelSlug)
				}
				logger.Debug("imgen_poll_completed", attrs...)
				return refs, nil
			}
		}

		sortedRefs := append([]string(nil), refs...)
		sort.Strings(sortedRefs)
		sig := strings.Join(sortedRefs, ",")
		if sig == lastFallbackSig && sig != "" {
			stableCount++
			if stableCount >= stableRounds {
				attrs := []any{
					"source", "mapping_sediment",
					"stable_rounds", stableRounds,
					"ref_count", len(refs),
				}
				if modelSlug != "" {
					attrs = append(attrs, sdk.LogFieldModel, modelSlug)
				}
				logger.Debug("imgen_poll_completed", attrs...)
				return refs, nil
			}
		} else {
			stableCount = 0
			lastFallbackSig = sig
		}
	}

	logger.Warn("imgen_poll_timeout", "max_attempts", maxAttempts)
	return nil, fmt.Errorf("轮询 %d 次未拿到图片 asset_pointer", maxAttempts)
}

func (c *Client) readMappingRefsAndModel(conversationID string) ([]string, string) {
	req, err := c.newReq("GET", "/backend-api/conversation/"+conversationID, nil)
	if err != nil {
		return nil, ""
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return nil, ""
	}
	body, _ := io.ReadAll(resp.Body)
	var conv map[string]any
	if json.Unmarshal(body, &conv) != nil {
		return nil, ""
	}
	mapping, _ := conv["mapping"].(map[string]any)
	tools := extractImageToolMsgs(mapping)

	var refs []string
	var modelSlug string
	seenF := map[string]bool{}
	seenS := map[string]bool{}
	for _, t := range tools {
		if t.ModelSlug != "" {
			modelSlug = t.ModelSlug
		}
		for _, f := range t.FileIDs {
			if !seenF[f] {
				seenF[f] = true
				refs = append(refs, "file-service://"+f)
			}
		}
		for _, s := range t.SedIDs {
			if !seenS[s] {
				seenS[s] = true
				refs = append(refs, "sediment://"+s)
			}
		}
	}
	return refs, modelSlug
}

func hasFileService(refs []string) bool {
	for _, r := range refs {
		if strings.HasPrefix(r, "file-service://") {
			return true
		}
	}
	return false
}

func filterFileService(refs []string) []string {
	var out []string
	for _, r := range refs {
		if strings.HasPrefix(r, "file-service://") {
			out = append(out, r)
		}
	}
	return out
}
