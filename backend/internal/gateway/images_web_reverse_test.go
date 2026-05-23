package gateway

import (
	"net/http"
	"strings"
	"testing"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

func TestWebReverseImagesErrorClientStatusReturnsNilErr(t *testing.T) {
	outcome, err := webReverseImagesError(time.Now(), http.StatusBadRequest, nil, "图片尺寸不合法")
	if err != nil {
		t.Fatalf("expected nil err for client status, got %v", err)
	}
	if outcome.Kind != sdk.OutcomeClientError {
		t.Fatalf("Kind = %v, want OutcomeClientError", outcome.Kind)
	}
	if outcome.Upstream.StatusCode != http.StatusBadRequest {
		t.Fatalf("StatusCode = %d, want %d", outcome.Upstream.StatusCode, http.StatusBadRequest)
	}
	if !strings.Contains(string(outcome.Upstream.Body), "图片尺寸不合法") {
		t.Fatalf("body = %s, want message to be preserved", outcome.Upstream.Body)
	}
}

func TestWebReverseImagesErrorAccountStatusKeepsErr(t *testing.T) {
	outcome, err := webReverseImagesError(time.Now(), http.StatusUnauthorized, nil, "OAuth 账号缺少 access_token")
	if err == nil {
		t.Fatalf("expected err for account status")
	}
	if outcome.Kind != sdk.OutcomeAccountDead {
		t.Fatalf("Kind = %v, want OutcomeAccountDead", outcome.Kind)
	}
}
