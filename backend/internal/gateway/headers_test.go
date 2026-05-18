package gateway

import (
	"net/http"
	"testing"

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
