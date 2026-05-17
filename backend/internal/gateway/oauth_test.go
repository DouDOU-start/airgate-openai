package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestSelectChatGPTAccountInfoPrefersDefaultAccount(t *testing.T) {
	result := map[string]interface{}{
		"accounts": map[string]interface{}{
			"org_free": map[string]interface{}{
				"account": map[string]interface{}{
					"plan_type":  "free",
					"is_default": false,
				},
			},
			"org_plus": map[string]interface{}{
				"account": map[string]interface{}{
					"plan_type":  "plus",
					"is_default": true,
					"email":      "plus@example.com",
				},
				"entitlement": map[string]interface{}{
					"expires_at": "2026-05-02T20:32:12+00:00",
				},
			},
		},
	}

	info := selectChatGPTAccountInfo(result, "")
	if info == nil {
		t.Fatal("expected account info")
	}
	if info.PlanType != "plus" {
		t.Fatalf("PlanType = %q, want plus", info.PlanType)
	}
	if info.SubscriptionActiveUntil != "2026-05-02T20:32:12+00:00" {
		t.Fatalf("SubscriptionActiveUntil = %q", info.SubscriptionActiveUntil)
	}
	if info.Email != "plus@example.com" {
		t.Fatalf("Email = %q", info.Email)
	}
}

func TestSelectChatGPTAccountInfoPrefersOrgID(t *testing.T) {
	result := map[string]interface{}{
		"accounts": map[string]interface{}{
			"org_free": map[string]interface{}{
				"account": map[string]interface{}{
					"plan_type":  "free",
					"is_default": true,
				},
			},
			"org_pro": map[string]interface{}{
				"account": map[string]interface{}{
					"plan_type": "pro",
				},
			},
		},
	}

	info := selectChatGPTAccountInfo(result, "org_pro")
	if info == nil {
		t.Fatal("expected account info")
	}
	if info.PlanType != "pro" {
		t.Fatalf("PlanType = %q, want pro", info.PlanType)
	}
}

func TestAccountInfoFromAccountFallsBackToEntitlementPlan(t *testing.T) {
	info := accountInfoFromAccount(map[string]interface{}{
		"entitlement": map[string]interface{}{
			"subscription_plan": "team",
			"expires_at":        "2026-05-02T20:32:12+00:00",
		},
	})

	if info.PlanType != "team" {
		t.Fatalf("PlanType = %q, want team", info.PlanType)
	}
	if info.SubscriptionActiveUntil != "2026-05-02T20:32:12+00:00" {
		t.Fatalf("SubscriptionActiveUntil = %q", info.SubscriptionActiveUntil)
	}
}

func TestQueryQuotaFallsBackToAccessTokenWithoutRefreshToken(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	accessToken := testJWT(t, map[string]any{
		"email":                             "user@example.com",
		"chatgpt_account_id":                "acct_123",
		"chatgpt_plan_type":                 "plus",
		"chatgpt_subscription_active_until": "2026-06-01T00:00:00Z",
		"name":                              "Example User",
	})
	quota, err := (&OpenAIGateway{}).QueryQuota(ctx, map[string]string{"access_token": accessToken})
	if err != nil {
		t.Fatalf("QueryQuota returned err: %v", err)
	}
	if quota == nil {
		t.Fatal("QueryQuota returned nil quota")
	}
	if quota.Extra["plan_type"] != "plus" {
		t.Fatalf("plan_type = %q, want plus", quota.Extra["plan_type"])
	}
	if quota.Extra["email"] != "user@example.com" {
		t.Fatalf("email = %q, want user@example.com", quota.Extra["email"])
	}
	if quota.Extra["chatgpt_account_id"] != "acct_123" {
		t.Fatalf("chatgpt_account_id = %q, want acct_123", quota.Extra["chatgpt_account_id"])
	}
	if quota.ExpiresAt != "2026-06-01T00:00:00Z" {
		t.Fatalf("ExpiresAt = %q", quota.ExpiresAt)
	}
}

func testJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]any{"alg": "none", "typ": "JWT"})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}
