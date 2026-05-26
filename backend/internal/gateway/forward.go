package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/DouDOU-start/airgate-openai/backend/internal/model"
	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

// redactURL 去掉 query string，仅保留 host+path（避免敏感参数泄漏到日志）
func redactURL(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u != nil {
		u.RawQuery = ""
		u.User = nil
		return u.String()
	}
	if idx := strings.Index(rawURL, "?"); idx >= 0 {
		return rawURL[:idx]
	}
	return rawURL
}

// ──────────────────────────────────────────────────────
// 转发入口（三模式分发）
// ──────────────────────────────────────────────────────

// forwardHTTP 根据账号凭证类型分发到不同转发模式
func (g *OpenAIGateway) forwardHTTP(ctx context.Context, req *sdk.ForwardRequest) (sdk.ForwardOutcome, error) {
	if isAnthropicCountTokensRequest(req) {
		return g.forwardAnthropicCountTokens(ctx, req)
	}

	// 检测 Anthropic Messages API 请求，走协议翻译
	if isAnthropicRequest(req) {
		return g.forwardAnthropicMessage(ctx, req)
	}

	// GET /v1/models：用插件内置模型清单本地返回。
	if isModelsListingRequest(req) {
		return buildLocalModelsResponse(), nil
	}

	// 统一预处理请求体。multipart 请求（images/edits 上传图片）body 是二进制，
	// 不能按 JSON 处理否则会被 sjson 覆盖丢失数据。
	_, reqPath := resolveAPIKeyRoute(req)
	var reqServiceTier string
	if !strings.HasPrefix(req.Headers.Get("Content-Type"), "multipart/") {
		req.Body = preprocessRequestBody(req.Body, req.Model, reqPath)
		req.Body = applyForceInstructions(req.Body, req.Headers)
		reqServiceTier = normalizeOpenAIServiceTier(gjson.GetBytes(req.Body, "service_tier").String())
		if req.Account.Credentials["api_key"] != "" && req.Account.Credentials["access_token"] == "" {
			req.Body = applyOpenAIWireServiceTier(req.Body)
		}
	}

	account := req.Account

	// GET /v1/images/tasks/list：任务历史列表
	// GET /v1/images/tasks：任务状态查询
	// 两者都通过 image.generate handler 的 BuildResponse 投影响应。
	if isImageTaskListRequest(reqPath) || isImageTaskQuery(reqPath) {
		if g.host == nil {
			body := jsonError("任务系统未启用")
			return sdk.ForwardOutcome{
				Kind:     sdk.OutcomeClientError,
				Upstream: sdk.UpstreamResponse{StatusCode: http.StatusServiceUnavailable, Body: body},
			}, nil
		}
		handler := g.tasks.Get(taskTypeImageGenerate)
		if handler == nil {
			body := jsonError("任务类型未注册")
			return sdk.ForwardOutcome{
				Kind:     sdk.OutcomeClientError,
				Upstream: sdk.UpstreamResponse{StatusCode: http.StatusInternalServerError, Body: body},
			}, nil
		}
		if isImageTaskListRequest(reqPath) {
			return g.handleTaskList(ctx, req, handler)
		}
		return g.handleTaskQuery(ctx, req, handler)
	}

	if !isImagesRequest(reqPath) && model.IsImageOnly(req.Model) {
		body := jsonError("图像模型不支持 Chat Completions，请使用 Images API")
		return sdk.ForwardOutcome{
			Kind: sdk.OutcomeClientError,
			Upstream: sdk.UpstreamResponse{
				StatusCode: http.StatusBadRequest,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       body,
			},
			Reason: "图像模型不支持 chat completions",
		}, nil
	}

	// Images API 默认保持 OpenAI 标准同步响应，避免 SDK 把 task_id 响应解析成 data=nil。
	// 客户端显式发送 Prefer: respond-async 时，才创建 Core Task 并立即返回 202。
	if g.host != nil && isImagesRequest(reqPath) && !isTaskExecution(req.Headers) && prefersAsyncResponse(req.Headers) {
		taskType := taskTypeImageGenerate
		if isImagesEditRequest(reqPath) {
			taskType = taskTypeImageEdit
		}
		if handler := g.tasks.Get(taskType); handler != nil {
			return g.forwardTask(ctx, req, reqPath, handler)
		}
	}

	if account.Credentials["api_key"] != "" {
		return g.forwardAPIKey(ctx, req, reqServiceTier)
	}
	if account.Credentials["access_token"] != "" {
		if isImagesRequest(reqPath) {
			if shouldUseImagesWebReverse(account, req.Model) {
				return g.forwardImagesViaWebReverse(ctx, req)
			}
			return g.forwardImagesViaResponsesTool(ctx, req)
		}
		return g.forwardOAuth(ctx, req)
	}
	reason := "账号缺少 api_key 或 access_token"
	sdk.LoggerFromContext(ctx).Error("forward_dispatch_failed",
		sdk.LogFieldAccountID, account.ID,
		sdk.LogFieldReason, reason,
		sdk.LogFieldError, reason,
	)
	return accountDeadOutcome(reason), fmt.Errorf("%s", reason)
}

// isImageTaskListRequest 判断是否为 GET /v1/images/tasks/list 历史列表查询。
func isImageTaskListRequest(reqPath string) bool {
	return strings.HasSuffix(reqPath, "/images/tasks/list")
}

// isImageTaskQuery 判断是否为 GET /v1/images/tasks 任务状态查询。
func isImageTaskQuery(reqPath string) bool {
	return strings.HasSuffix(reqPath, "/images/tasks")
}

func prefersAsyncResponse(headers http.Header) bool {
	for _, value := range headers.Values("Prefer") {
		for _, token := range strings.Split(value, ",") {
			preference := strings.ToLower(strings.TrimSpace(token))
			if i := strings.IndexByte(preference, ';'); i >= 0 {
				preference = strings.TrimSpace(preference[:i])
			}
			if preference == "respond-async" {
				return true
			}
		}
	}
	return false
}

// isModelsListingRequest 判断当前请求是否为 GET /v1/models。
//
// Core 不会透传原始方法和路径，只能用既有线索推断：
//  1. 优先看 X-Forwarded-Path（如果 core 设置了）
//  2. 回退到 resolveAPIKeyRoute 的兜底推断（空 body + 非 stream → /v1/models）
//
// 这保持了与 resolveAPIKeyRoute 一致的推断逻辑，避免两处判据漂移。
func isModelsListingRequest(req *sdk.ForwardRequest) bool {
	if req == nil {
		return false
	}
	method, path := resolveAPIKeyRoute(req)
	return method == http.MethodGet && (path == "/v1/models" || strings.HasPrefix(path, "/v1/models?"))
}

// buildLocalModelsResponse 用插件内置模型注册表合成 OpenAI 兼容的 /v1/models 响应。
func buildLocalModelsResponse() sdk.ForwardOutcome {
	specs := model.AllSpecs(true)
	data := make([]map[string]any, 0, len(specs))
	created := time.Now().Unix()
	for _, spec := range specs {
		isImage := model.IsImageOnly(spec.ID)
		entry := map[string]any{
			"id":           spec.ID,
			"object":       "model",
			"created":      created,
			"owned_by":     "airgate",
			"capabilities": []string{"chat", "reasoning"},
		}
		if isImage {
			entry["capabilities"] = []string{"image_generation"}
			entry["image_only"] = true
		}
		if spec.ContextWindow > 0 {
			entry["context_window"] = spec.ContextWindow
			entry["context_length"] = spec.ContextWindow
			entry["max_input_tokens"] = spec.ContextWindow
		}
		if spec.MaxOutputTokens > 0 {
			entry["max_output_tokens"] = spec.MaxOutputTokens
		}
		data = append(data, entry)
	}
	body, _ := json.Marshal(map[string]any{
		"object": "list",
		"data":   data,
	})
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	return successOutcome(http.StatusOK, body, headers, nil)
}

// ──────────────────────────────────────────────────────
// API Key 模式：HTTP/SSE 直连上游
// ──────────────────────────────────────────────────────

func (g *OpenAIGateway) forwardAPIKey(ctx context.Context, req *sdk.ForwardRequest, reqServiceTier string) (sdk.ForwardOutcome, error) {
	start := time.Now()
	account := req.Account
	logger := sdk.LoggerFromContext(ctx)

	reqMethod, reqPath := resolveAPIKeyRoute(req)
	targetURL := buildAPIKeyURL(account, reqPath)
	isImageReq := isImagesRequest(reqPath)
	isImageEdit := isImagesEditRequest(reqPath)
	reqContentType := req.Headers.Get("Content-Type")
	isMultipart := isMultipartContentType(reqContentType)
	imagesRespOpts := imagesResponseOptions{}
	if isImageReq && len(req.Body) > 0 && !(isImageEdit && !isMultipart) {
		imagesRespOpts = imagesResponseOptionsFromRequestBody(req.Body, reqContentType, isImageEdit)
	}
	if isImageEdit && len(req.Body) > 0 && !isMultipart {
		body, contentType, parsed, err := buildAPIKeyImagesEditMultipartBodyWithRequest(req.Body, reqContentType)
		if err != nil {
			errBody := jsonError(err.Error())
			return sdk.ForwardOutcome{
				Kind: sdk.OutcomeClientError,
				Upstream: sdk.UpstreamResponse{
					StatusCode: http.StatusBadRequest,
					Headers:    http.Header{"Content-Type": []string{"application/json"}},
					Body:       errBody,
				},
				Reason:   err.Error(),
				Duration: time.Since(start),
			}, nil
		}
		req.Body = body
		req.Headers.Set("Content-Type", contentType)
		if parsed != nil {
			imagesRespOpts.BillingSize = parsed.Size
			imagesRespOpts.RequestQuality = normalizeImageQualityDefaultMedium(parsed.Quality)
		}
	} else if isImageReq && len(req.Body) > 0 && !isMultipart {
		if patched, err := sjson.DeleteBytes(req.Body, "stream"); err == nil {
			req.Body = patched
		}
	}

	var bodyReader io.Reader
	if methodAllowsBody(reqMethod) && len(req.Body) > 0 {
		bodyReader = bytes.NewReader(req.Body)
	}

	upstreamReq, err := http.NewRequestWithContext(ctx, reqMethod, targetURL, bodyReader)
	if err != nil {
		reason := fmt.Sprintf("构建上游请求失败: %v", err)
		logger.Warn("upstream_request_build_failed",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldModel, req.Model,
			"url", redactURL(targetURL),
			sdk.LogFieldError, err,
		)
		return transientOutcome(reason), fmt.Errorf("%s", reason)
	}

	setAuthHeaders(upstreamReq, account)
	if methodAllowsBody(reqMethod) {
		// /v1/images/edits 是 multipart/form-data，必须保留 boundary。其它路径
		// Core 侧已把 body 归一化成 JSON 文本，统一 application/json。
		if ct := req.Headers.Get("Content-Type"); isImagesEditRequest(reqPath) &&
			strings.HasPrefix(strings.ToLower(ct), "multipart/") {
			upstreamReq.Header.Set("Content-Type", ct)
		} else {
			upstreamReq.Header.Set("Content-Type", "application/json")
		}
	}
	passHeadersForAccount(req.Headers, upstreamReq.Header, account)

	// 重启恢复：有上游异步 task_id 的 images 请求直接 poll，不再重复发起上游请求
	if isImagesRequest(reqPath) {
		if recoveryID := req.Headers.Get(upstreamTaskIDHeader); recoveryID != "" {
			logger.Info("images_async_task_recovery_before_request",
				sdk.LogFieldAccountID, account.ID,
				"upstream_task_id", recoveryID,
			)
			finalBody, pollErr := g.pollAsyncImageTask(ctx, account, recoveryID, logger)
			if pollErr == nil {
				mockResp := &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(bytes.NewReader(finalBody)),
				}
				return g.handleImagesResponse(mockResp, req.Writer, nil, start, req.Model, imagesRespOpts)
			}
			reason := fmt.Sprintf("上游异步任务恢复失败: %v", pollErr)
			logger.Warn("images_async_task_recovery_failed",
				"upstream_task_id", recoveryID,
				sdk.LogFieldError, pollErr,
			)
			return transientOutcome(reason), fmt.Errorf("%s", reason)
		}
	}

	var sseKA *ssePingKeepAlive
	if isImagesRequest(reqPath) && req.Stream {
		sseKA = startSSEPingKeepAlive(req.Writer)
	}

	logger.Debug("upstream_request_start",
		sdk.LogFieldAccountID, account.ID,
		sdk.LogFieldModel, req.Model,
		"url", redactURL(targetURL),
		sdk.LogFieldMethod, reqMethod,
		"stream", req.Stream,
		"account_type", "apikey",
	)

	client := g.buildHTTPClient(account)
	resp, err := client.Do(upstreamReq)
	if err != nil {
		dur := time.Since(start)
		if sseKA != nil {
			sseKA.Stop()
			logger.Warn("images_apikey_stream_failed_redacted",
				sdk.LogFieldPath, reqPath,
				sdk.LogFieldModel, req.Model,
				sdk.LogFieldError, err,
			)
			writeSSEErrorIfStarted(req.Writer, sseKA, sanitizedImageSSEErrorMessage)
		}
		logger.Warn("upstream_request_failed",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldModel, req.Model,
			sdk.LogFieldDurationMs, dur.Milliseconds(),
			sdk.LogFieldError, err,
		)
		// 网络层错误，无上游 HTTP 响应
		return transientOutcome(err.Error()), fmt.Errorf("请求上游失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 非 2xx 统一走 failureOutcome 归类。包含 4xx（客户端错）/ 429 / 401 / 403 / 5xx。
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		errDetail := gjson.GetBytes(respBody, "error.message").String()
		if errDetail == "" {
			errDetail = truncate(string(respBody), 200)
		}
		dur := time.Since(start)
		if sseKA != nil {
			sseKA.Stop()
			logger.Warn("images_apikey_upstream_error_redacted",
				sdk.LogFieldPath, reqPath,
				sdk.LogFieldModel, req.Model,
				sdk.LogFieldStatus, resp.StatusCode,
				sdk.LogFieldReason, errDetail,
			)
			writeSSEErrorIfStarted(req.Writer, sseKA, sanitizedImageSSEErrorMessage)
		}
		logger.Warn("upstream_request_non_2xx",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldModel, req.Model,
			sdk.LogFieldStatus, resp.StatusCode,
			sdk.LogFieldDurationMs, dur.Milliseconds(),
			sdk.LogFieldReason, errDetail,
		)
		outcome := failureOutcome(resp.StatusCode, respBody, resp.Header.Clone(), errDetail, extractRetryAfterHeader(resp.Header))
		outcome.Duration = time.Since(start)
		return outcome, nil
	}

	logger.Debug("upstream_request_completed",
		sdk.LogFieldAccountID, account.ID,
		sdk.LogFieldModel, req.Model,
		sdk.LogFieldStatus, resp.StatusCode,
		sdk.LogFieldDurationMs, time.Since(start).Milliseconds(),
		"content_length", resp.ContentLength,
		"stream", req.Stream,
	)

	// /v1/models 路径补齐上下文元信息
	if reqMethod == http.MethodGet && strings.HasPrefix(reqPath, "/v1/models") {
		resp = enrichModelsResponse(resp)
	}

	// 捕获上游 Codex 用量头
	if snapshot := parseCodexUsageFromHeaders(resp.Header); snapshot != nil {
		StoreCodexUsage(account.ID, snapshot)
	}

	// Images API 响应体无 model 字段，另走专用处理器回填后再 fillCost
	if isImagesRequest(reqPath) {
		// 检测异步任务模式（如 apimart.ai gpt-image-2 返回 task_id）
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			reason := fmt.Sprintf("读取 Images 响应失败: %v", readErr)
			if sseKA != nil {
				sseKA.Stop()
				writeSSEErrorIfStarted(req.Writer, sseKA, sanitizedImageSSEErrorMessage)
			}
			return transientOutcome(reason), fmt.Errorf("%s", reason)
		}

		if taskID, ok := isAsyncImageTaskResponse(body); ok {
			logger.Debug("images_async_task_detected",
				sdk.LogFieldAccountID, account.ID,
				sdk.LogFieldModel, req.Model,
				"task_id", taskID,
			)
			// 保存上游 task_id 到 core task 的 execution，服务重启后可恢复
			if coreTaskID := req.Headers.Get(taskIDHeader); coreTaskID != "" {
				cid, _ := strconv.ParseInt(coreTaskID, 10, 64)
				if cid > 0 {
					_ = g.updateHostTask(ctx, cid, "", 0, nil, "",
						WithExecution(map[string]any{"upstream_task_id": taskID}))
				}
			}

			finalBody, pollErr := g.pollAsyncImageTask(ctx, account, taskID, logger)
			if pollErr != nil {
				reason := fmt.Sprintf("异步图片任务轮询失败: %v", pollErr)
				logger.Warn("images_async_task_poll_failed",
					sdk.LogFieldAccountID, account.ID,
					sdk.LogFieldModel, req.Model,
					"task_id", taskID,
					sdk.LogFieldError, pollErr,
				)
				if sseKA != nil {
					sseKA.Stop()
					writeSSEErrorIfStarted(req.Writer, sseKA, sanitizedImageSSEErrorMessage)
				}
				return transientOutcome(reason), fmt.Errorf("%s", reason)
			}
			body = finalBody
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return g.handleImagesResponse(resp, req.Writer, sseKA, start, req.Model, imagesRespOpts)
	}

	if req.Stream && req.Writer != nil {
		return handleStreamResponseWithLogger(logger, resp, req.Writer, start, reqServiceTier)
	}
	return handleNonStreamResponse(resp, req.Writer, start, reqServiceTier)
}

func enrichModelsResponse(resp *http.Response) *http.Response {
	if resp == nil || resp.Body == nil {
		return resp
	}

	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || len(raw) == 0 {
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		if len(raw) > 0 {
			resp.ContentLength = int64(len(raw))
		}
		return resp
	}

	dataNode := gjson.GetBytes(raw, "data")
	if !dataNode.Exists() || !dataNode.IsArray() {
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		resp.ContentLength = int64(len(raw))
		return resp
	}

	updated := raw
	changed := false
	for idx, item := range dataNode.Array() {
		modelID := strings.TrimSpace(item.Get("id").String())
		if modelID == "" {
			continue
		}

		meta := getModelMetadataByID(modelID)
		if len(meta) == 0 {
			continue
		}
		for key, value := range meta {
			path := fmt.Sprintf("data.%d.%s", idx, key)
			if gjson.GetBytes(updated, path).Exists() {
				continue
			}
			patched, setErr := sjson.SetBytes(updated, path, value)
			if setErr != nil {
				continue
			}
			updated = patched
			changed = true
		}
	}

	if !changed {
		updated = raw
	}

	resp.Body = io.NopCloser(bytes.NewReader(updated))
	resp.ContentLength = int64(len(updated))
	if resp.Header != nil {
		resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(updated)))
		resp.Header.Set("Content-Type", "application/json")
	}
	return resp
}

// ──────────────────────────────────────────────────────
// OAuth 模式：WebSocket 连上游，SSE 写回客户端
// ──────────────────────────────────────────────────────

// forwardOAuth 使用 WebSocket 连接上游，将响应以 SSE 格式写回客户端
func (g *OpenAIGateway) forwardOAuth(ctx context.Context, req *sdk.ForwardRequest) (sdk.ForwardOutcome, error) {
	start := time.Now()
	account := req.Account
	logger := sdk.LoggerFromContext(ctx)
	session := resolveOpenAISession(req.Headers, req.Body)
	updateSessionStateFromRequest(session)

	cfg := WSConfig{
		Token:          account.Credentials["access_token"],
		AccountID:      account.Credentials["chatgpt_account_id"],
		ProxyURL:       account.ProxyURL,
		SessionID:      session.SessionID,
		ConversationID: session.ConversationID,
		TurnState:      session.LastTurnState,
		Originator:     req.Headers.Get("originator"),
	}

	logger.Debug("upstream_request_start",
		sdk.LogFieldAccountID, account.ID,
		sdk.LogFieldModel, req.Model,
		"url", "wss://chatgpt.com/backend-api/codex",
		sdk.LogFieldMethod, "WS",
		"stream", req.Stream,
		"account_type", "oauth",
	)

	conn, wsResp, err := DialWebSocket(cfg)
	if err != nil {
		dur := time.Since(start)
		// WS 握手失败：按 HTTP 响应码归类。无响应则视为网络层 transient。
		if wsResp != nil {
			logger.Warn("upstream_request_non_2xx",
				sdk.LogFieldAccountID, account.ID,
				sdk.LogFieldStatus, wsResp.StatusCode,
				sdk.LogFieldDurationMs, dur.Milliseconds(),
				sdk.LogFieldReason, err.Error(),
				"phase", "ws_handshake",
			)
			wsBody := openAIErrorJSON(openAIErrorTypeForStatus(wsResp.StatusCode), "", err.Error())
			outcome := failureOutcome(wsResp.StatusCode, wsBody, wsResp.Header.Clone(), err.Error(), extractRetryAfterHeader(wsResp.Header))
			outcome.Duration = time.Since(start)
			return outcome, forwardErrForOutcome(outcome, err)
		}
		logger.Warn("upstream_request_failed",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldDurationMs, dur.Milliseconds(),
			sdk.LogFieldError, err,
			"phase", "ws_handshake",
		)
		return transientOutcome(err.Error()), err
	}
	defer func() { _ = conn.Close() }()
	if wsResp != nil {
		if turnState := decodeTurnStateHeader(wsResp.Header); turnState != "" {
			updateSessionStateTurnState(session.SessionKey, turnState)
		}
	}

	// 构建 response.create 消息
	createMsg, err := g.buildWSRequest(req, session)
	if err != nil {
		reason := fmt.Sprintf("构建 WebSocket 请求失败: %v", err)
		logger.Warn("ws_build_request_failed",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldModel, req.Model,
			sdk.LogFieldError, err,
		)
		outcome := sdk.ForwardOutcome{
			Kind: sdk.OutcomeClientError,
			Upstream: sdk.UpstreamResponse{
				StatusCode: http.StatusBadRequest,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       openAIErrorJSON("invalid_request_error", "invalid_request", reason),
			},
			Reason:   reason,
			Duration: time.Since(start),
		}
		return outcome, forwardErrForOutcome(outcome, fmt.Errorf("%s", reason))
	}

	// 协议分叉：客户端如果走的是 /v1/chat/completions（而不是原生 /v1/responses），
	// OAuth 上游依然只吐 Responses API 的 SSE 事件——需要把这些事件翻译回 Chat
	// Completions 协议，否则标准 OpenAI SDK 的 chat.completions.create 无法解析。
	isChatCompletions := isChatCompletionsRequest(req)
	chatStreamInclude := isChatCompletions && req.Stream &&
		gjson.GetBytes(req.Body, "stream_options.include_usage").Bool()

	// 是否走静默缓冲路径（等整条响应就绪再吐 JSON）。两种场景触发：
	//   - /v1/chat/completions 非流式
	//   - /v1/responses 非流式
	silentBuffered := !req.Stream

	var (
		lastSSEHandler      *sseEventWriter
		lastChatWriter      *chatCompletionsStreamWriter
		lastChatSilent      *chatCompletionsSilentHandler
		lastResponsesSilent *responsesSilentHandler
	)

	runAttempt := func(msg []byte, w http.ResponseWriter) (WSResult, error) {
		if err := conn.WriteJSON(json.RawMessage(msg)); err != nil {
			return WSResult{}, fmt.Errorf("发送 WebSocket 消息失败: %w", err)
		}
		var handler WSEventHandler
		switch {
		case isChatCompletions && req.Stream:
			writer := newChatCompletionsStreamWriter(
				w, req.Model, account.ID, session.SessionKey, chatStreamInclude, start,
			)
			lastChatWriter = writer
			handler = writer
		case isChatCompletions && !req.Stream:
			silent := &chatCompletionsSilentHandler{
				accountID:  account.ID,
				sessionKey: session.SessionKey,
				start:      start,
			}
			lastChatSilent = silent
			handler = silent
		case !isChatCompletions && !req.Stream:
			// 原生 /v1/responses 非流式：缓冲事件，末尾一次性吐 JSON
			silent := &responsesSilentHandler{
				accountID:  account.ID,
				sessionKey: session.SessionKey,
				start:      start,
			}
			lastResponsesSilent = silent
			handler = silent
		default:
			// 原生 /v1/responses 流式：SSE 透传
			sseHandler := &sseEventWriter{
				w:          w,
				accountID:  account.ID,
				sessionKey: session.SessionKey,
				start:      start,
			}
			if f, ok := w.(http.Flusher); ok {
				sseHandler.flusher = f
			}
			lastSSEHandler = sseHandler
			handler = sseHandler
		}
		return ReceiveWSResponse(ctx, conn, handler), nil
	}

	w := req.Writer

	// 流式响应先设置头，但不提前 WriteHeader。
	// 如果上游在首个可转发事件前返回校验错误（例如模型不支持），调用方仍能写出 4xx JSON。
	if w != nil && !silentBuffered {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
	}

	result, err := runAttempt(createMsg, w)
	if err != nil {
		logger.Warn("upstream_request_failed",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldModel, req.Model,
			sdk.LogFieldDurationMs, time.Since(start).Milliseconds(),
			sdk.LogFieldError, err,
			"phase", "ws_send",
		)
		return transientOutcome(err.Error()), err
	}
	if session.SessionKey != "" {
		if result.ResponseID != "" {
			updateSessionStateResponseID(session.SessionKey, result.ResponseID)
		}
	}

	elapsed := time.Since(start)
	firstTokenMs := elapsed.Milliseconds()
	switch {
	case lastChatWriter != nil && lastChatWriter.firstTokenMs > 0:
		firstTokenMs = lastChatWriter.firstTokenMs
	case lastSSEHandler != nil && lastSSEHandler.firstTokenMs > 0:
		firstTokenMs = lastSSEHandler.firstTokenMs
	case lastChatSilent != nil && lastChatSilent.firstTokenMs > 0:
		firstTokenMs = lastChatSilent.firstTokenMs
	case lastResponsesSilent != nil && lastResponsesSilent.firstTokenMs > 0:
		firstTokenMs = lastResponsesSilent.firstTokenMs
	}

	usage := newTokenUsage(
		result.Model,
		normalizeOpenAIServiceTier(gjson.GetBytes(req.Body, "service_tier").String()),
		result.InputTokens,
		result.OutputTokens,
		result.CachedInputTokens,
		result.ReasoningOutputTokens,
		firstTokenMs,
	)
	numImages := len(result.ImageGenCalls)
	imageToolSize := ""
	if numImages > 0 {
		imageToolSize = result.ImageGenCalls[0].Size
	}

	if result.Err != nil {
		var failure *responsesFailureError
		kind := sdk.OutcomeUpstreamTransient
		statusCode := http.StatusBadGateway
		message := result.Err.Error()
		var retryAfter time.Duration
		if errors.As(result.Err, &failure) {
			kind = failure.outcomeKind()
			statusCode = failure.StatusCode
			message = failure.Message
			retryAfter = failure.RetryAfter
		}
		streamOutputStarted := (lastChatWriter != nil && lastChatWriter.wrote) ||
			(lastSSEHandler != nil && lastSSEHandler.wrote)
		// 只有已经向客户端写过可见输出时才视为流中断；首包前错误仍交给 Core failover。
		if req.Stream && streamOutputStarted && kind != sdk.OutcomeClientError {
			kind = sdk.OutcomeStreamAborted
		}
		errBody := openAIErrorJSON(openAIErrorTypeForStatus(statusCode), kind.String(), message)
		logger.Warn("upstream_request_non_2xx",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldModel, req.Model,
			sdk.LogFieldStatus, statusCode,
			sdk.LogFieldDurationMs, elapsed.Milliseconds(),
			sdk.LogFieldReason, message,
			"phase", "ws_response",
		)
		outcome := sdk.ForwardOutcome{
			Kind:       kind,
			Upstream:   sdk.UpstreamResponse{StatusCode: statusCode, Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: errBody},
			Reason:     message,
			RetryAfter: retryAfter,
			Duration:   elapsed,
		}
		// 即使请求失败，上游可能已消耗 token（如 response.failed / response.incomplete），
		// 仍需计费避免漏洞。
		if result.InputTokens > 0 || result.OutputTokens > 0 || result.CachedInputTokens > 0 {
			fillUsageCostWithImageTool(usage, numImages, imageToolSize)
			outcome.Usage = usage
		}
		return outcome, result.Err
	}

	// 结束标记 / 响应体写回。必须在 result.Err 判定之后执行，避免把上游错误补成
	// finish_reason=stop + [DONE] 的空成功流。
	switch {
	case isChatCompletions && req.Stream:
		if lastChatWriter != nil {
			lastChatWriter.finalize()
		}
	case isChatCompletions && !req.Stream:
		if w != nil {
			body := buildNonStreamChatCompletion(result, req.Model)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		}
	case !isChatCompletions && !req.Stream:
		// /v1/responses 非流式：从 WSResult 抽 response 字段回写 JSON
		if w != nil {
			body := buildNonStreamResponses(result)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		}
	default:
		// /v1/responses 流式：补 [DONE] 标记
		if w != nil {
			if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err == nil {
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			}
		}
	}

	logger.Debug("upstream_request_completed",
		sdk.LogFieldAccountID, account.ID,
		sdk.LogFieldModel, req.Model,
		sdk.LogFieldStatus, http.StatusOK,
		sdk.LogFieldDurationMs, elapsed.Milliseconds(),
		"first_token_ms", firstTokenMs,
		"input_tokens", result.InputTokens,
		"output_tokens", result.OutputTokens,
		"stream", req.Stream,
	)
	fillUsageCostWithImageTool(usage, numImages, imageToolSize)
	return sdk.ForwardOutcome{
		Kind:     sdk.OutcomeSuccess,
		Upstream: sdk.UpstreamResponse{StatusCode: http.StatusOK},
		Usage:    usage,
		Duration: elapsed,
	}, nil
}

// ──────────────────────────────────────────────────────
// SSE 事件写入器（WSEventHandler 实现）
// ──────────────────────────────────────────────────────

// sseEventWriter 将 WS 事件转为 SSE 格式写入 http.ResponseWriter
type sseEventWriter struct {
	w              http.ResponseWriter
	flusher        http.Flusher
	accountID      int64 // 用于存储 Codex 用量快照
	sessionKey     string
	start          time.Time // 请求开始时间，用于计算首 token 延迟
	firstTokenMs   int64     // 首 token 到达时间（毫秒）
	firstTokenOnce sync.Once // 确保只记录一次
	wrote          bool
}

func (s *sseEventWriter) OnTextDelta(string)      {}
func (s *sseEventWriter) OnReasoningDelta(string) {}
func (s *sseEventWriter) OnRateLimits(used float64) {
	if s.accountID > 0 {
		StoreCodexUsage(s.accountID, &CodexUsageSnapshot{
			PrimaryUsedPercent: used,
			CapturedAt:         time.Now(),
		})
	}
}

func (s *sseEventWriter) OnRawEvent(eventType string, data []byte) {
	if s.w == nil || eventType == "" {
		return
	}
	// 记录首 token 延迟（第一个有效事件到达客户端的时间）
	s.firstTokenOnce.Do(func() {
		s.firstTokenMs = time.Since(s.start).Milliseconds()
	})
	// 过滤不需要转发给客户端的内部事件，并捕获用量
	switch eventType {
	case "codex.rate_limits":
		if s.accountID > 0 {
			if snapshot := parseCodexUsageFromSSEEvent(data); snapshot != nil {
				StoreCodexUsage(s.accountID, snapshot)
			}
		}
		return
	case "response.created", "response.completed", "response.done":
		if s.sessionKey != "" {
			if responseID := gjson.GetBytes(data, "response.id").String(); strings.TrimSpace(responseID) != "" {
				updateSessionStateResponseID(s.sessionKey, responseID)
			}
		}
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", eventType, strings.ReplaceAll(string(data), "\n", "")); err != nil {
		return
	}
	s.wrote = true
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// ──────────────────────────────────────────────────────
// HTTP 客户端
// ──────────────────────────────────────────────────────

func methodAllowsBody(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		return true
	default:
		return false
	}
}

// requestTimeout 获取插件默认请求超时配置
func (g *OpenAIGateway) requestTimeout() time.Duration {
	const fallback = 300 * time.Second
	if g == nil || g.ctx == nil || g.ctx.Config() == nil {
		return fallback
	}
	timeout := g.ctx.Config().GetDuration("default_timeout")
	if timeout <= 0 {
		return fallback
	}
	return timeout
}

// buildHTTPClient 构建带代理支持的 HTTP 客户端
// 使用 TransportPool 按账户+代理隔离连接，同一账户复用连接
func (g *OpenAIGateway) buildHTTPClient(account *sdk.Account) *http.Client {
	if g == nil || g.transportPool == nil {
		return &http.Client{
			Timeout: g.requestTimeout(),
		}
	}
	transport := g.transportPool.GetTransport(account.ID, account.ProxyURL)

	return &http.Client{
		Transport: transport,
		Timeout:   g.requestTimeout(),
	}
}
