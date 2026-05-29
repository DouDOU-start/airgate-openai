package gateway

import (
	"net/http"
	"testing"
)

func TestResolveOpenAISessionUsesPromptCacheKeyAsFallback(t *testing.T) {
	headers := http.Header{}
	resolution := resolveOpenAISession(headers, []byte(`{"prompt_cache_key":"pcache_123"}`), 101)
	if resolution.SessionKey != "pcache:pcache_123" {
		t.Fatalf("expected session key from prompt_cache_key, got %q", resolution.SessionKey)
	}
	if resolution.SessionID != "pcache_123" {
		t.Fatalf("expected session_id fallback from prompt_cache_key, got %q", resolution.SessionID)
	}
}

func TestResolveOpenAISessionReadsStoredState(t *testing.T) {
	sessionStateStore.Delete("pcache:pcache_456")
	upsertSessionState(&openAISessionState{
		SessionKey:     "pcache:pcache_456",
		PromptCacheKey: "pcache_456",
		SessionID:      "pcache_456",
		AccountID:      202,
		LastResponseID: "resp_abc",
		LastTurnState:  "turn_state_xyz",
	})

	resolution := resolveOpenAISession(http.Header{}, []byte(`{"prompt_cache_key":"pcache_456"}`), 202)
	if resolution.PreviousRespID != "resp_abc" {
		t.Fatalf("expected previous response id from stored state, got %q", resolution.PreviousRespID)
	}
	if resolution.LastTurnState != "turn_state_xyz" {
		t.Fatalf("expected turn state from stored state, got %q", resolution.LastTurnState)
	}
}

func TestResolveOpenAISessionIgnoresStoredResponseFromDifferentAccount(t *testing.T) {
	sessionStateStore.Delete("pcache:pcache_account_mismatch")
	upsertSessionState(&openAISessionState{
		SessionKey:     "pcache:pcache_account_mismatch",
		PromptCacheKey: "pcache_account_mismatch",
		SessionID:      "pcache_account_mismatch",
		AccountID:      301,
		LastResponseID: "resp_wrong_account",
	})

	resolution := resolveOpenAISession(http.Header{}, []byte(`{"prompt_cache_key":"pcache_account_mismatch"}`), 302)
	if resolution.PreviousRespID != "" {
		t.Fatalf("expected previous response id to be ignored across accounts, got %q", resolution.PreviousRespID)
	}
	if !resolution.FromStoredState {
		t.Fatalf("expected stored state to still be detected")
	}
}

func TestDeriveAnthropicPromptCacheKey_IgnoresLaterUserEphemeralChanges(t *testing.T) {
	body1 := []byte(`{
		"system":[{"type":"text","text":"stable system","cache_control":{"type":"ephemeral"}}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"anchor user","cache_control":{"type":"ephemeral"}}]},
			{"role":"assistant","content":[{"type":"text","text":"assistant step","cache_control":{"type":"ephemeral"}}]},
			{"role":"user","content":[{"type":"text","text":"later user one","cache_control":{"type":"ephemeral"}}]}
		]
	}`)
	body2 := []byte(`{
		"system":[{"type":"text","text":"stable system","cache_control":{"type":"ephemeral"}}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"anchor user","cache_control":{"type":"ephemeral"}}]},
			{"role":"assistant","content":[{"type":"text","text":"assistant step","cache_control":{"type":"ephemeral"}}]},
			{"role":"user","content":[{"type":"text","text":"later user two","cache_control":{"type":"ephemeral"}}]}
		]
	}`)

	k1 := deriveAnthropicPromptCacheKey(body1)
	k2 := deriveAnthropicPromptCacheKey(body2)
	if k1 == "" || k2 == "" {
		t.Fatalf("expected non-empty keys")
	}
	if k1 != k2 {
		t.Fatalf("expected stable key when only later user ephemeral content changes\nk1=%s\nk2=%s", k1, k2)
	}
}

func TestDeriveAnthropicPromptCacheKey_ChangesWhenSystemChanges(t *testing.T) {
	body1 := []byte(`{
		"system":[{"type":"text","text":"stable system one","cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":[{"type":"text","text":"anchor user","cache_control":{"type":"ephemeral"}}]}]
	}`)
	body2 := []byte(`{
		"system":[{"type":"text","text":"stable system two","cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":[{"type":"text","text":"anchor user","cache_control":{"type":"ephemeral"}}]}]
	}`)

	k1 := deriveAnthropicPromptCacheKey(body1)
	k2 := deriveAnthropicPromptCacheKey(body2)
	if k1 == "" || k2 == "" {
		t.Fatalf("expected non-empty keys")
	}
	if k1 == k2 {
		t.Fatalf("expected different keys when system ephemeral content changes")
	}
}
