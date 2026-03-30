package gateway

import (
	"strings"
	"testing"
)

func TestBuildAnthropicDigestChainConversationPrefixRelationship(t *testing.T) {
	round1 := []byte(`{
		"system":[{"type":"text","text":"You are a helpful assistant."}],
		"messages":[{"role":"user","content":"hello"}]
	}`)
	round2 := []byte(`{
		"system":[{"type":"text","text":"You are a helpful assistant."}],
		"messages":[
			{"role":"user","content":"hello"},
			{"role":"assistant","content":"hi there"},
			{"role":"user","content":"how are you?"}
		]
	}`)

	chain1 := buildAnthropicDigestChain(round1)
	chain2 := buildAnthropicDigestChain(round2)
	if chain1 == "" || chain2 == "" {
		t.Fatalf("expected non-empty digest chains")
	}
	if !strings.HasPrefix(chain2, chain1) {
		t.Fatalf("expected chain1 to be prefix of chain2\nchain1=%s\nchain2=%s", chain1, chain2)
	}
}

func TestFindAnthropicDigestSessionUsesLongestPrefixMatch(t *testing.T) {
	accountID := int64(1)
	saveAnthropicDigestSession(accountID, "s:aaaa-u:bbbb", "session-1", "")
	saveAnthropicDigestSession(accountID, "s:aaaa-u:bbbb-a:cccc", "session-2", "")

	sessionID, matched, found := findAnthropicDigestSession(accountID, "s:aaaa-u:bbbb-a:cccc-u:dddd")
	if !found {
		t.Fatalf("expected a match")
	}
	if sessionID != "session-2" {
		t.Fatalf("expected longest prefix match session-2, got %q", sessionID)
	}
	if matched != "s:aaaa-u:bbbb-a:cccc" {
		t.Fatalf("unexpected matched chain %q", matched)
	}
}
