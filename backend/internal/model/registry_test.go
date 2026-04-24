package model

import "testing"

// TestLookup_UnknownModelFallsBackToBillablePrice 回归测试：未注册模型必须走兜底定价，
// 绝不能返回 0 价（那会导致免费流量 / 计费全 0）。
func TestLookup_UnknownModelFallsBackToBillablePrice(t *testing.T) {
	spec := Lookup("some-brand-new-unknown-model-xyz")
	if spec.InputPrice <= 0 || spec.OutputPrice <= 0 {
		t.Fatalf("未知模型必须有兜底价格；InputPrice=%v OutputPrice=%v", spec.InputPrice, spec.OutputPrice)
	}
}

// TestLookup_ByKeyword 按关键词推断到对应系列。
func TestLookup_ByKeyword(t *testing.T) {
	cases := []struct {
		name      string
		modelID   string
		wantMatch string // 期望命中的注册键（按 InputPrice 识别）
	}{
		{"未知 codex 系列 → gpt-5.3-codex", "gpt-5.9-codex-preview", "gpt-5.3-codex"},
		{"未知 image 系列 → gpt-image-1.5", "gpt-image-3", "gpt-image-1.5"},
		{"未知 mini 系列 → gpt-5.4-mini", "gpt-5.9-mini", "gpt-5.4-mini"},
		{"未知 nano 系列 → gpt-5.4-mini", "gpt-5.9-nano", "gpt-5.4-mini"},
		{"未知 gpt-5 系列 → gpt-5.4", "gpt-5.9", "gpt-5.4"},
		{"o1 推理模型 → gpt-5.4", "o1-preview", "gpt-5.4"},
		{"o3 推理模型 → gpt-5.4", "o3-mini", "gpt-5.4-mini"}, // "mini" 优先
		{"gpt-4 系列 → gpt-5.4（偏保守）", "gpt-4o", "gpt-5.4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Lookup(tc.modelID)
			want := registry[tc.wantMatch]
			if got.InputPrice != want.InputPrice || got.OutputPrice != want.OutputPrice {
				t.Errorf("Lookup(%q): got InputPrice=%v OutputPrice=%v, want match %q (In=%v Out=%v)",
					tc.modelID, got.InputPrice, got.OutputPrice,
					tc.wantMatch, want.InputPrice, want.OutputPrice)
			}
		})
	}
}

// TestLookup_KnownModelUnchanged 已注册模型不受关键词兜底影响。
func TestLookup_KnownModelUnchanged(t *testing.T) {
	t.Run("gpt-5.5", func(t *testing.T) {
		spec := Lookup("gpt-5.5")
		if spec.InputPrice != 5.0 || spec.OutputPrice != 30.0 || spec.CachedPrice != 0.5 {
			t.Errorf("gpt-5.5 定价变化: In=%v Out=%v Cached=%v", spec.InputPrice, spec.OutputPrice, spec.CachedPrice)
		}
		if spec.ContextWindow != 400000 {
			t.Errorf("gpt-5.5 ContextWindow = %v, want 400000", spec.ContextWindow)
		}
		if spec.InputPricePriority != 12.5 || spec.OutputPricePriority != 75.0 || spec.CachedPricePriority != 1.25 {
			t.Errorf("gpt-5.5 priority 定价变化: In=%v Out=%v Cached=%v", spec.InputPricePriority, spec.OutputPricePriority, spec.CachedPricePriority)
		}
		if spec.InputPriceFast != 0 || spec.OutputPriceFast != 0 || spec.CachedPriceFast != 0 {
			t.Errorf("gpt-5.5 不应配置 fast 定价: In=%v Out=%v Cached=%v", spec.InputPriceFast, spec.OutputPriceFast, spec.CachedPriceFast)
		}
	})

	t.Run("gpt-5.4", func(t *testing.T) {
		spec := Lookup("gpt-5.4")
		if spec.InputPrice != 2.5 || spec.OutputPrice != 15.0 {
			t.Errorf("gpt-5.4 定价变化: In=%v Out=%v", spec.InputPrice, spec.OutputPrice)
		}
	})
}
