package gateway

import (
	sdk "github.com/DouDOU-start/airgate-sdk"
)

type anthropicUpstreamStrategy string

const (
	anthropicStrategyOAuth         anthropicUpstreamStrategy = "oauth"
	anthropicStrategySub2APIAPIKey anthropicUpstreamStrategy = "sub2api_apikey"
	anthropicStrategyGenericAPIKey anthropicUpstreamStrategy = "generic_apikey_http"
)

func resolveAnthropicUpstreamStrategy(account *sdk.Account) anthropicUpstreamStrategy {
	if account == nil {
		return anthropicStrategyGenericAPIKey
	}
	if account.Credentials["access_token"] != "" {
		return anthropicStrategyOAuth
	}
	if isSub2APIAccount(account) {
		return anthropicStrategySub2APIAPIKey
	}
	return anthropicStrategyGenericAPIKey
}

func (s anthropicUpstreamStrategy) allowsContinuation() bool {
	switch s {
	case anthropicStrategyGenericAPIKey:
		return enableAnthropicContinuation
	default:
		return false
	}
}

func (s anthropicUpstreamStrategy) isSub2APIAPIKey() bool {
	return s == anthropicStrategySub2APIAPIKey
}
