package gateway

import "testing"

func TestResolveAnthropicModelMapping_UsesUpdatedDefaultClaudeTarget(t *testing.T) {
	tests := []struct {
		name  string
		model string
	}{
		{name: "unknown model fallback", model: "claude-foo-9"},
		{name: "claude 3 wildcard fallback", model: "claude-3-7-legacy"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mapping := resolveAnthropicModelMapping(tt.model)
			if mapping == nil {
				t.Fatal("mapping is nil")
			}
			if mapping.OpenAIModel != "gpt-5.5" {
				t.Fatalf("OpenAIModel = %q, want %q", mapping.OpenAIModel, "gpt-5.5")
			}
			if mapping.FallbackModel != "gpt-5.4" {
				t.Fatalf("FallbackModel = %q, want %q", mapping.FallbackModel, "gpt-5.4")
			}
		})
	}
}
