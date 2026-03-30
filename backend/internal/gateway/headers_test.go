package gateway

import (
	"net/http"
	"testing"

	sdk "github.com/DouDOU-start/airgate-sdk"
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
