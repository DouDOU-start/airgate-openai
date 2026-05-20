package gateway

import (
	"net/http"
	"testing"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

func TestPassHeadersForAccount_Sub2APIStripsClientIdentityHeaders(t *testing.T) {
	src := http.Header{}
	src.Set("User-Agent", "claude-cli/2.1.81 (external, cli)")
	src.Set("originator", "codex_cli_rs")
	src.Set("x-stainless-timeout", "30")
	src.Set("accept-language", "zh-CN")

	dst := http.Header{}
	passHeadersForAccount(src, dst, &sdk.Account{
		Credentials: map[string]string{
			"base_url": "https://sub2api.k8ray.com",
		},
	})

	if got := dst.Get("User-Agent"); got != "" {
		t.Fatalf("expected user-agent to be stripped, got %q", got)
	}
	if got := dst.Get("originator"); got != "" {
		t.Fatalf("expected originator to be stripped, got %q", got)
	}
	if got := dst.Get("x-stainless-timeout"); got != "30" {
		t.Fatalf("expected stainless timeout to remain, got %q", got)
	}
	if got := dst.Get("accept-language"); got != "zh-CN" {
		t.Fatalf("expected accept-language to remain, got %q", got)
	}
}

func TestPassHeadersForAccount_NonSub2APIKeepsAllowedHeaders(t *testing.T) {
	src := http.Header{}
	src.Set("User-Agent", "claude-cli/2.1.81 (external, cli)")
	src.Set("originator", "codex_cli_rs")

	dst := http.Header{}
	passHeadersForAccount(src, dst, &sdk.Account{
		Credentials: map[string]string{
			"base_url": "https://api.openai.com",
		},
	})

	if got := dst.Get("User-Agent"); got == "" {
		t.Fatalf("expected user-agent to be kept")
	}
	if got := dst.Get("originator"); got == "" {
		t.Fatalf("expected originator to be kept")
	}
}

func TestCodexUsageSnapshotNormalize_PrimaryOnlyShortResetIs5h(t *testing.T) {
	snapshot := &CodexUsageSnapshot{
		PrimaryUsedPercent:       42,
		PrimaryResetAfterSeconds: 5 * 60 * 60,
	}

	normalized := snapshot.Normalize()
	if normalized == nil {
		t.Fatal("expected normalized limits")
	}
	if normalized.Used5hPercent == nil || *normalized.Used5hPercent != 42 {
		t.Fatalf("expected primary-only short reset to be 5h, got %#v", normalized.Used5hPercent)
	}
	if normalized.Used7dPercent != nil {
		t.Fatalf("expected 7d to be empty, got %#v", normalized.Used7dPercent)
	}
}

func TestCodexUsageSnapshotNormalize_PrimaryOnlyWithoutResetIs5h(t *testing.T) {
	snapshot := &CodexUsageSnapshot{
		PrimaryUsedPercent: 42,
	}

	normalized := snapshot.Normalize()
	if normalized == nil {
		t.Fatal("expected normalized limits")
	}
	if normalized.Used5hPercent == nil || *normalized.Used5hPercent != 42 {
		t.Fatalf("expected primary-only usage without reset to stay 5h, got %#v", normalized.Used5hPercent)
	}
	if normalized.Used7dPercent != nil {
		t.Fatalf("expected 7d to be empty, got %#v", normalized.Used7dPercent)
	}
}

func TestCodexUsageSnapshotNormalize_SecondaryOnlyShortResetIs5h(t *testing.T) {
	snapshot := &CodexUsageSnapshot{
		SecondaryUsedPercent:       42,
		SecondaryResetAfterSeconds: 5 * 60 * 60,
	}

	normalized := snapshot.Normalize()
	if normalized == nil {
		t.Fatal("expected normalized limits")
	}
	if normalized.Used5hPercent == nil || *normalized.Used5hPercent != 42 {
		t.Fatalf("expected secondary short reset usage to map to 5h, got %#v", normalized.Used5hPercent)
	}
	if normalized.Used7dPercent != nil {
		t.Fatalf("expected 7d to be empty, got %#v", normalized.Used7dPercent)
	}
}

func TestCodexUsageSnapshotNormalize_SecondaryResetOnlyKeepsProviderOrder(t *testing.T) {
	snapshot := &CodexUsageSnapshot{
		PrimaryUsedPercent:         42,
		SecondaryUsedPercent:       55,
		SecondaryResetAfterSeconds: 48 * 60 * 60,
	}

	normalized := snapshot.Normalize()
	if normalized == nil {
		t.Fatal("expected normalized limits")
	}
	if normalized.Used5hPercent == nil || *normalized.Used5hPercent != 42 {
		t.Fatalf("expected primary usage to stay 5h, got %#v", normalized.Used5hPercent)
	}
	if normalized.Used7dPercent == nil || *normalized.Used7dPercent != 55 {
		t.Fatalf("expected secondary usage to stay 7d, got %#v", normalized.Used7dPercent)
	}
}

func TestStoreCodexUsagePreservesBaseWindowResetMetadata(t *testing.T) {
	accountID := int64(91001)
	usageStore.Delete(accountID)
	defer usageStore.Delete(accountID)
	base := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	StoreCodexUsage(accountID, &CodexUsageSnapshot{
		PrimaryUsedPercent:                  30,
		PrimaryResetAfterSeconds:            2 * 60 * 60,
		SecondaryUsedPercent:                50,
		SecondaryResetAfterSeconds:          48 * 60 * 60,
		BengalfoxPrimaryUsedPercent:         12,
		BengalfoxPrimaryResetAfterSeconds:   90 * 60,
		BengalfoxSecondaryUsedPercent:       18,
		BengalfoxSecondaryResetAfterSeconds: 36 * 60 * 60,
		BengalfoxSecondaryWindowMinutes:     7 * 24 * 60,
		CapturedAt:                          base,
	})
	StoreCodexUsage(accountID, &CodexUsageSnapshot{
		PrimaryUsedPercent: 40,
		CapturedAt:         base.Add(30 * time.Minute),
	})

	snapshot := GetCodexUsage(accountID)
	if snapshot == nil {
		t.Fatal("expected stored snapshot")
	}
	if snapshot.PrimaryUsedPercent != 40 {
		t.Fatalf("PrimaryUsedPercent = %v, want 40", snapshot.PrimaryUsedPercent)
	}
	if snapshot.PrimaryResetAfterSeconds <= 0 || snapshot.PrimaryResetAfterSeconds > 2*60*60 {
		t.Fatalf("PrimaryResetAfterSeconds = %d, want remaining reset", snapshot.PrimaryResetAfterSeconds)
	}
	if snapshot.SecondaryUsedPercent != 50 || snapshot.SecondaryResetAfterSeconds <= 0 {
		t.Fatalf("secondary window not preserved: %+v", snapshot)
	}
	if snapshot.BengalfoxPrimaryUsedPercent != 12 || snapshot.BengalfoxPrimaryResetAfterSeconds <= 0 {
		t.Fatalf("bengalfox primary window not preserved: %+v", snapshot)
	}
	if snapshot.BengalfoxSecondaryUsedPercent != 18 || snapshot.BengalfoxSecondaryWindowMinutes != 7*24*60 {
		t.Fatalf("bengalfox secondary window not preserved: %+v", snapshot)
	}
}

func TestStoreCodexUsagePrimaryOnlyAfterShortResetKeeps5hOrder(t *testing.T) {
	accountID := int64(91002)
	usageStore.Delete(accountID)
	defer usageStore.Delete(accountID)
	base := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	StoreCodexUsage(accountID, &CodexUsageSnapshot{
		PrimaryUsedPercent:         30,
		PrimaryResetAfterSeconds:   30 * 60,
		SecondaryUsedPercent:       50,
		SecondaryResetAfterSeconds: 48 * 60 * 60,
		CapturedAt:                 base,
	})
	StoreCodexUsage(accountID, &CodexUsageSnapshot{
		PrimaryUsedPercent: 40,
		CapturedAt:         base.Add(31 * time.Minute),
	})

	snapshot := GetCodexUsage(accountID)
	if snapshot == nil {
		t.Fatal("expected stored snapshot")
	}
	normalized := snapshot.Normalize()
	if normalized == nil {
		t.Fatal("expected normalized limits")
	}
	if normalized.Used5hPercent == nil || *normalized.Used5hPercent != 40 {
		t.Fatalf("expected fresh primary-only usage to stay 5h, got %#v", normalized.Used5hPercent)
	}
	if normalized.Used7dPercent == nil || *normalized.Used7dPercent != 50 {
		t.Fatalf("expected preserved secondary usage to stay 7d, got %#v", normalized.Used7dPercent)
	}
}

func TestCodexUsageSnapshotNormalize_MissingWindowMinutesUsesResetOrder(t *testing.T) {
	snapshot := &CodexUsageSnapshot{
		PrimaryUsedPercent:         12,
		PrimaryResetAfterSeconds:   90 * 60,
		SecondaryUsedPercent:       34,
		SecondaryResetAfterSeconds: 3 * 24 * 60 * 60,
	}

	normalized := snapshot.Normalize()
	if normalized == nil {
		t.Fatal("expected normalized limits")
	}
	if normalized.Used5hPercent == nil || *normalized.Used5hPercent != 12 {
		t.Fatalf("expected shorter reset to be 5h, got %#v", normalized.Used5hPercent)
	}
	if normalized.Used7dPercent == nil || *normalized.Used7dPercent != 34 {
		t.Fatalf("expected longer reset to be 7d, got %#v", normalized.Used7dPercent)
	}
}

func TestCodexUsageSnapshotNormalize_WindowMinutesTakePrecedence(t *testing.T) {
	snapshot := &CodexUsageSnapshot{
		PrimaryUsedPercent:         12,
		PrimaryResetAfterSeconds:   5 * 60 * 60,
		PrimaryWindowMinutes:       7 * 24 * 60,
		SecondaryUsedPercent:       34,
		SecondaryResetAfterSeconds: 90 * 60,
		SecondaryWindowMinutes:     5 * 60,
	}

	normalized := snapshot.Normalize()
	if normalized == nil {
		t.Fatal("expected normalized limits")
	}
	if normalized.Used5hPercent == nil || *normalized.Used5hPercent != 34 {
		t.Fatalf("expected window_minutes to map secondary to 5h, got %#v", normalized.Used5hPercent)
	}
	if normalized.Used7dPercent == nil || *normalized.Used7dPercent != 12 {
		t.Fatalf("expected window_minutes to map primary to 7d, got %#v", normalized.Used7dPercent)
	}
}
