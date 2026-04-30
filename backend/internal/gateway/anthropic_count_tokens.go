package gateway

import (
	"context"
	"encoding/json"
	"net/http"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

// forwardAnthropicCountTokens 返回本地估算，避免客户端把 404 当作不可计数并继续发送过长请求。
func (g *OpenAIGateway) forwardAnthropicCountTokens(_ context.Context, req *sdk.ForwardRequest) (sdk.ForwardOutcome, error) {
	body, err := json.Marshal(map[string]int{"input_tokens": estimateAnthropicInputTokens(req.Body)})
	if err != nil {
		body = []byte(`{"input_tokens":0}`)
	}
	if req.Writer != nil {
		setAnthropicStyleResponseHeaders(req.Writer)
		req.Writer.Header().Set("Content-Type", "application/json")
		req.Writer.WriteHeader(http.StatusOK)
		_, _ = req.Writer.Write(body)
	}
	return sdk.ForwardOutcome{
		Kind:     sdk.OutcomeSuccess,
		Upstream: sdk.UpstreamResponse{StatusCode: http.StatusOK, Body: body},
		Reason:   "count_tokens estimated locally",
	}, nil
}
