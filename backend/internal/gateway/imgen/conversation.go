package imgen

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ---------- Step 0.5: conversation/init 槽位占位 ----------
//
// 真实 chatgpt.com 在 bootstrap 与每次"新会话的第一条消息"前都会 POST 这个
// 接口。它只是槽位占位，不返回 conversation_id；我们的目的是让请求序列更
// 贴近浏览器，降低被 Sentinel / Cloudflare 识别为脚本的风险。

func (c *Client) conversationInit() error {
	payload := map[string]any{
		"conversation_mode_kind": "primary_assistant",
	}
	data, _ := json.Marshal(payload)
	req, err := c.newReq("POST", "/backend-api/conversation/init", bytes.NewReader(data))
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("conversation/init HTTP %d", resp.StatusCode)
	}
	return nil
}

// ---------- Step 2: /f/conversation/prepare 预校验 ----------

func (c *Client) prepareConversation(prompt, chatToken, proofToken, convID, parentID string, imgs []*UploadedFile) (conduitToken string, err error) {
	if parentID == "" {
		parentID = uuid.New().String()
	}

	var contentPayload map[string]any
	var systemHints []string
	if len(imgs) > 0 {
		parts := make([]any, 0, len(imgs)+1)
		for _, img := range imgs {
			parts = append(parts, map[string]any{
				"content_type":  "image_asset_pointer",
				"asset_pointer": "sediment://" + img.FileID,
				"size_bytes":    img.Size,
				"width":         img.Width,
				"height":        img.Height,
			})
		}
		parts = append(parts, prompt)
		contentPayload = map[string]any{
			"content_type": "multimodal_text",
			"parts":        parts,
		}
		systemHints = []string{}
	} else {
		contentPayload = map[string]any{
			"content_type": "text",
			"parts":        []string{prompt},
		}
		systemHints = []string{"picture_v2"}
	}

	payload := map[string]any{
		"action":                "next",
		"fork_from_shared_post": false,
		"parent_message_id":     parentID,
		"model":                 "auto",
		"client_prepare_state":  "success",
		"timezone_offset_min":   -480,
		"timezone":              "Asia/Shanghai",
		"conversation_mode":     map[string]string{"kind": "primary_assistant"},
		"system_hints":          systemHints,
		"supports_buffering":    true,
		"supported_encodings":   []string{"v1"},
		"client_contextual_info": map[string]any{
			"app_name": "chatgpt.com",
		},
		"partial_query": map[string]any{
			"id":      uuid.New().String(),
			"author":  map[string]string{"role": "user"},
			"content": contentPayload,
		},
	}
	if convID != "" {
		payload["conversation_id"] = convID
	}

	data, _ := json.Marshal(payload)
	req, err := c.newReq("POST", "/backend-api/f/conversation/prepare", bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Openai-Sentinel-Chat-Requirements-Token", chatToken)
	if proofToken != "" {
		req.Header.Set("Openai-Sentinel-Proof-Token", proofToken)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, b)
	}

	var result struct {
		ConduitToken string `json:"conduit_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.ConduitToken, nil
}

// ---------- Step 3: /f/conversation SSE 流 ----------

// StreamResult SSE 流消费完后的沉淀：conversation_id + 扫描出的所有 asset_pointer。
type StreamResult struct {
	ConversationID string
	ImageRefs      []string
}

func (c *Client) streamConversation(prompt, chatToken, conduitToken, proofToken, convID, parentID string, imgs []*UploadedFile) (*StreamResult, error) {
	msgID := uuid.New().String()
	parentMsgID := parentID
	if parentMsgID == "" {
		parentMsgID = uuid.New().String()
	}
	createTime := float64(time.Now().UnixMilli()) / 1000.0

	var contentPayload map[string]any
	var msgMetadata map[string]any
	var systemHints []string

	if len(imgs) > 0 {
		parts := make([]any, 0, len(imgs)+1)
		for _, img := range imgs {
			parts = append(parts, map[string]any{
				"content_type":  "image_asset_pointer",
				"asset_pointer": "sediment://" + img.FileID,
				"size_bytes":    img.Size,
				"width":         img.Width,
				"height":        img.Height,
			})
		}
		parts = append(parts, prompt)
		contentPayload = map[string]any{
			"content_type": "multimodal_text",
			"parts":        parts,
		}
		attachments := make([]map[string]any, 0, len(imgs))
		for _, img := range imgs {
			attachments = append(attachments, map[string]any{
				"id":           img.FileID,
				"size":         img.Size,
				"name":         img.FileName,
				"mime_type":    img.MimeType,
				"width":        img.Width,
				"height":       img.Height,
				"source":       "local",
				"is_big_paste": false,
			})
		}
		msgMetadata = map[string]any{
			"attachments":               attachments,
			"selected_github_repos":     []any{},
			"selected_all_github_repos": false,
			"serialization_metadata":    map[string]any{"custom_symbol_offsets": []any{}},
		}
		systemHints = []string{}
	} else {
		contentPayload = map[string]any{
			"content_type": "text",
			"parts":        []string{prompt},
		}
		msgMetadata = map[string]any{
			"developer_mode_connector_ids": []any{},
			"selected_github_repos":        []any{},
			"selected_all_github_repos":    false,
			"system_hints":                 []string{"picture_v2"},
			"serialization_metadata":       map[string]any{"custom_symbol_offsets": []any{}},
		}
		systemHints = []string{"picture_v2"}
	}

	payload := map[string]any{
		"action": "next",
		"messages": []map[string]any{
			{
				"id":          msgID,
				"author":      map[string]string{"role": "user"},
				"create_time": createTime,
				"content":     contentPayload,
				"metadata":    msgMetadata,
			},
		},
		"parent_message_id":                    parentMsgID,
		"model":                                "auto",
		"system_hints":                         systemHints,
		"force_parallel_switch":                "auto",
		"client_prepare_state":                 "sent",
		"timezone_offset_min":                  -480,
		"timezone":                             "Asia/Shanghai",
		"conversation_mode":                    map[string]string{"kind": "primary_assistant"},
		"enable_message_followups":             true,
		"supports_buffering":                   true,
		"supported_encodings":                  []string{"v1"},
		"paragen_cot_summary_display_override": "allow",
		"client_contextual_info": map[string]any{
			"is_dark_mode":      false,
			"time_since_loaded": 1200,
			"page_height":       1072,
			"page_width":        1724,
			"pixel_ratio":       1.2,
			"screen_height":     1440,
			"screen_width":      2560,
			"app_name":          "chatgpt.com",
		},
	}

	if convID != "" {
		payload["conversation_id"] = convID
	}

	data, _ := json.Marshal(payload)
	req, err := c.newReq("POST", "/backend-api/f/conversation", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Openai-Sentinel-Chat-Requirements-Token", chatToken)
	if conduitToken != "" {
		req.Header.Set("X-Conduit-Token", conduitToken)
	}
	if proofToken != "" {
		req.Header.Set("Openai-Sentinel-Proof-Token", proofToken)
	}
	req.Header.Set("X-Oai-Turn-Trace-Id", uuid.New().String())

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, b)
	}

	result := &StreamResult{}
	seen := map[string]bool{}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		eventData := strings.TrimPrefix(line, "data: ")
		if eventData == "[DONE]" {
			break
		}

		var event map[string]any
		if json.Unmarshal([]byte(eventData), &event) != nil {
			continue
		}

		if cid, ok := event["conversation_id"].(string); ok && result.ConversationID == "" {
			result.ConversationID = cid
			slog.Default().Debug("imgen_conversation_id_received", "conversation_id", cid)
		}

		c.extractImageRefs(event, seen, &result.ImageRefs)
	}

	return result, nil
}

// extractImageRefs 从 SSE 事件消息中提取 file-service:// 和 sediment:// 引用。
func (c *Client) extractImageRefs(msg map[string]any, seen map[string]bool, refs *[]string) {
	message, _ := msg["message"].(map[string]any)
	if message == nil {
		return
	}
	content, _ := message["content"].(map[string]any)
	if content == nil {
		return
	}
	parts, _ := content["parts"].([]any)
	for _, part := range parts {
		partMap, ok := part.(map[string]any)
		if !ok {
			if s, ok := part.(string); ok {
				for _, prefix := range []string{"file-service://", "sediment://"} {
					for _, seg := range strings.Split(s, prefix) {
						if seg == s {
							continue
						}
						end := strings.IndexAny(seg, " \n\t\"')")
						if end < 0 {
							end = len(seg)
						}
						ref := prefix + seg[:end]
						if !seen[ref] {
							seen[ref] = true
							*refs = append(*refs, ref)
						}
					}
				}
			}
			continue
		}
		pointer, _ := partMap["asset_pointer"].(string)
		if pointer != "" && !seen[pointer] {
			seen[pointer] = true
			*refs = append(*refs, pointer)
		}
	}
}

// ---------- IMG2 tool 消息解析 ----------

var (
	reFileRef = regexp.MustCompile(`file-service://([A-Za-z0-9_-]+)`)
	reSedRef  = regexp.MustCompile(`sediment://([A-Za-z0-9_-]+)`)
)

// imgToolMsg 对应 conversation mapping 里一条 image_gen tool 消息。
type imgToolMsg struct {
	MessageID  string
	CreateTime float64
	ModelSlug  string
	Recipient  string
	AuthorName string
	FileIDs    []string
	SedIDs     []string
}

// extractImageToolMsgs 从 conversation.mapping 里过滤出 image_gen tool 消息：
//
//	author.role == "tool"
//	metadata.async_task_type == "image_gen"
//	content.content_type == "multimodal_text"
func extractImageToolMsgs(mapping map[string]any) []imgToolMsg {
	out := make([]imgToolMsg, 0, 4)
	for mid, raw := range mapping {
		node, _ := raw.(map[string]any)
		if node == nil {
			continue
		}
		msg, _ := node["message"].(map[string]any)
		if msg == nil {
			continue
		}
		author, _ := msg["author"].(map[string]any)
		meta, _ := msg["metadata"].(map[string]any)
		content, _ := msg["content"].(map[string]any)
		if author == nil || meta == nil || content == nil {
			continue
		}
		if r, _ := author["role"].(string); r != "tool" {
			continue
		}
		if t, _ := meta["async_task_type"].(string); t != "image_gen" {
			continue
		}
		if ct, _ := content["content_type"].(string); ct != "multimodal_text" {
			continue
		}

		tm := imgToolMsg{MessageID: mid}
		if v, ok := msg["create_time"].(float64); ok {
			tm.CreateTime = v
		}
		if v, ok := meta["model_slug"].(string); ok {
			tm.ModelSlug = v
		}
		if v, ok := msg["recipient"].(string); ok {
			tm.Recipient = v
		}
		if v, ok := author["name"].(string); ok {
			tm.AuthorName = v
		}

		parts, _ := content["parts"].([]any)
		seenF := map[string]bool{}
		seenS := map[string]bool{}
		pickRefs := func(text string) {
			for _, m := range reFileRef.FindAllStringSubmatch(text, -1) {
				if !seenF[m[1]] {
					seenF[m[1]] = true
					tm.FileIDs = append(tm.FileIDs, m[1])
				}
			}
			for _, m := range reSedRef.FindAllStringSubmatch(text, -1) {
				if !seenS[m[1]] {
					seenS[m[1]] = true
					tm.SedIDs = append(tm.SedIDs, m[1])
				}
			}
		}
		for _, p := range parts {
			switch v := p.(type) {
			case map[string]any:
				if s, _ := v["asset_pointer"].(string); s != "" {
					pickRefs(s)
				}
			case string:
				pickRefs(v)
			}
		}
		out = append(out, tm)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreateTime < out[j].CreateTime })
	return out
}
