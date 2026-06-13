package gateway

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestAnthropicSchedulingModelMapDefaults(t *testing.T) {
	clearSchedulingMapEnv(t)

	got := decodeSchedulingMap(t, anthropicSchedulingModelMapJSON())
	want := map[string][]string{
		"claude-haiku-":  {"gpt-5.3-codex-spark", "gpt-5.4-mini"},
		"claude-sonnet-": {"gpt-5.5", "gpt-5.4"},
		"claude-opus-":   {"gpt-5.5", "gpt-5.4"},
		"claude-":        {"gpt-5.5", "gpt-5.4"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("anthropicSchedulingModelMapJSON() = %#v, want %#v", got, want)
	}
}

func TestAnthropicSchedulingModelMapEnvOverride(t *testing.T) {
	clearSchedulingMapEnv(t)
	t.Setenv("AIRGATE_DEFAULT_CLAUDE_MODEL", "my-default")
	t.Setenv("AIRGATE_MODEL_HAIKU", "my-fast")

	got := decodeSchedulingMap(t, anthropicSchedulingModelMapJSON())
	if want := []string{"my-fast", "gpt-5.4-mini"}; !reflect.DeepEqual(got["claude-haiku-"], want) {
		t.Fatalf("haiku = %#v, want %#v", got["claude-haiku-"], want)
	}
	if want := []string{"my-default", "gpt-5.4"}; !reflect.DeepEqual(got["claude-opus-"], want) {
		t.Fatalf("opus = %#v, want %#v", got["claude-opus-"], want)
	}
}

func TestAnthropicRouteMetadataDeclaresBothKeys(t *testing.T) {
	clearSchedulingMapEnv(t)

	md := anthropicRouteMetadata()
	if md["error_format"] != "anthropic" {
		t.Fatalf("error_format = %q, want anthropic", md["error_format"])
	}
	if md["scheduling_model_map"] == "" {
		t.Fatal("scheduling_model_map 未声明")
	}
}

func decodeSchedulingMap(t *testing.T, raw string) map[string][]string {
	t.Helper()
	out := make(map[string][]string)
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("解析 scheduling_model_map 失败: %v", err)
	}
	return out
}

func clearSchedulingMapEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"AIRGATE_DEFAULT_CLAUDE_MODEL",
		"AIRGATE_MODEL_OPUS",
		"ANTHROPIC_DEFAULT_OPUS_MODEL",
		"AIRGATE_MODEL_SONNET",
		"ANTHROPIC_DEFAULT_SONNET_MODEL",
		"AIRGATE_MODEL_HAIKU",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL",
		"AIRGATE_MODEL_HAIKU_FALLBACK",
		"AIRGATE_MODEL_OPUS_FALLBACK",
		"AIRGATE_MODEL_SONNET_FALLBACK",
		"AIRGATE_MODEL_DEFAULT_FALLBACK",
	} {
		t.Setenv(key, "")
	}
}
