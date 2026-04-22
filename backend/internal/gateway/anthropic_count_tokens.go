package gateway

import (
	"context"
	"net/http"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

// forwardAnthropicCountTokens 当前返回 404，让 Claude Code 等客户端回退到本地估算。
// 这样可以避免 count_tokens 辅助请求干扰主链路行为判断。
func (g *OpenAIGateway) forwardAnthropicCountTokens(_ context.Context, req *sdk.ForwardRequest) (sdk.ForwardOutcome, error) {
	const message = "count_tokens endpoint is not supported"
	if req.Writer != nil {
		writeAnthropicErrorJSON(req.Writer, http.StatusNotFound, "not_found_error", message)
	}
	return sdk.ForwardOutcome{
		Kind:     sdk.OutcomeClientError,
		Upstream: sdk.UpstreamResponse{StatusCode: http.StatusNotFound},
		Reason:   message,
	}, nil
}
