package imgen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

// ChatRequirementsResult /backend-api/sentinel/chat-requirements 的产物。
// ChatToken 必须作为 Openai-Sentinel-Chat-Requirements-Token 头塞进后续的
// /f/conversation/prepare 与 /f/conversation 请求。ProofToken 为空时说明本次
// 不要求 PoW 挑战。
type ChatRequirementsResult struct {
	ChatToken  string
	ProofToken string
}

// getChatRequirements 拿 Sentinel 风控通行证。走的是 legacy 单接口
// /backend-api/sentinel/chat-requirements（body 带 requirements token 的 "p"
// 字段），不是网页端当前的 prepare + finalize 两步式。legacy 接口仍然可用，
// 且 PoW 字段结构在本包里已验证；切换到两步式需要重新抓 body 样本。
func (c *Client) getChatRequirements() (*ChatRequirementsResult, error) {
	reqToken := GenerateRequirementsToken(DefaultUA)

	body := map[string]string{"p": reqToken}
	bodyData, _ := json.Marshal(body)

	req, err := c.newReq("POST", "/backend-api/sentinel/chat-requirements", bytes.NewReader(bodyData))
	if err != nil {
		return nil, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, b)
	}

	var result struct {
		Token string `json:"token"`
		POW   struct {
			Required   bool   `json:"required"`
			Seed       string `json:"seed"`
			Difficulty string `json:"difficulty"`
		} `json:"proofofwork"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	cr := &ChatRequirementsResult{ChatToken: result.Token}

	if result.POW.Required {
		start := time.Now()
		cr.ProofToken = SolveProofToken(result.POW.Seed, result.POW.Difficulty, DefaultUA)
		slog.Default().Debug("imgen_pow_solved",
			"difficulty", result.POW.Difficulty,
			sdk.LogFieldDurationMs, time.Since(start).Milliseconds(),
		)
	}

	return cr, nil
}

// sentinelPing POST /backend-api/sentinel/ping
// 前端在 SSE 建立后每隔若干秒 POST 一次，维持 Sentinel 通行证不过期。
// GenerateImage 期间起一个 goroutine 周期性发送。
func (c *Client) sentinelPing() error {
	req, err := c.newReq("POST", "/backend-api/sentinel/ping", bytes.NewReader([]byte(`{}`)))
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
		return fmt.Errorf("sentinel/ping HTTP %d", resp.StatusCode)
	}
	return nil
}

// startHeartbeat 启动后台心跳 goroutine，返回 stop 函数。
func (c *Client) startHeartbeat(interval time.Duration) (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if err := c.sentinelPing(); err != nil {
					slog.Default().Warn("imgen_sentinel_ping_failed", sdk.LogFieldError, err)
				}
			}
		}
	}()
	return func() { close(done) }
}
