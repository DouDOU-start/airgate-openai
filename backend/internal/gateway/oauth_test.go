package gateway

import "testing"

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
