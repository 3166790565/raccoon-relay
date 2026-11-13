package raccoon

import "testing"

// 移植自 AgentHub 的 T7 系列：三家族档位归一，表外模型不动。
func TestNormalizeReasoning(t *testing.T) {
	cases := []struct {
		name   string
		model  string
		in     map[string]any
		field  string
		want   any
		absent string // 期望被删掉的字段
	}{
		{
			name:  "glm off + thinking.disabled 归一成 low 并摘掉 thinking",
			model: "sn-glm-5-3-flash",
			in:    map[string]any{"reasoning_effort": "off", "thinking": map[string]any{"type": "disabled"}},
			field: "reasoning_effort", want: "low", absent: "thinking",
		},
		{
			name: "glm high 原样", model: "sn-glm-5-3-flash",
			in: map[string]any{"reasoning_effort": "high"}, field: "reasoning_effort", want: "high",
		},
		{
			name: "glm max 归到 high（两档家族）", model: "sn-glm-5-3-flash",
			in: map[string]any{"reasoning_effort": "max"}, field: "reasoning_effort", want: "high",
		},
		{
			name: "glm 别名 glm-5.3 也命中", model: "glm-5.3",
			in: map[string]any{"reasoning_effort": "off"}, field: "reasoning_effort", want: "low",
		},
		{
			name: "kimi-k3 也是两档", model: "sn-kimi-k3",
			in: map[string]any{"reasoning_effort": "minimal"}, field: "reasoning_effort", want: "low",
		},
		{
			name:  "deepseek off → thinking.disabled",
			model: "sn-deepseek-v4",
			in:    map[string]any{"reasoning_effort": "off"},
			field: "thinking", want: map[string]any{"type": "disabled"}, absent: "reasoning_effort",
		},
		{
			name:  "deepseek 正向档 → thinking.enabled",
			model: "deepseek-v4",
			in:    map[string]any{"reasoning_effort": "high"},
			field: "thinking", want: map[string]any{"type": "enabled"}, absent: "reasoning_effort",
		},
		{
			name: "sensenova minimal → low", model: "sn-sensenova-6-8-flash",
			in: map[string]any{"reasoning_effort": "minimal"}, field: "reasoning_effort", want: "low",
		},
		{
			name: "sensenova medium → medium", model: "sn-sensenova-6-8-flash",
			in: map[string]any{"reasoning_effort": "medium"}, field: "reasoning_effort", want: "medium",
		},
		{
			name: "sensenova xhigh → high", model: "sn-sensenova-6-8-flash",
			in: map[string]any{"reasoning_effort": "xhigh"}, field: "reasoning_effort", want: "high",
		},
		{
			name: "sensenova off → 删除档位", model: "sn-sensenova-6-8-flash",
			in: map[string]any{"reasoning_effort": "off"}, absent: "reasoning_effort",
		},
		{
			name: "raccoon-chat 是三档家族", model: "raccoon-chat-ml-5-5",
			in: map[string]any{"reasoning_effort": "max"}, field: "reasoning_effort", want: "high",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := NormalizeReasoning(cloneMap(c.in), c.model)
			if c.field != "" {
				if got := out[c.field]; !deepEqual(got, c.want) {
					t.Fatalf("%s: 期望 %v，实际 %v", c.field, c.want, got)
				}
			}
			if c.absent != "" {
				if _, ok := out[c.absent]; ok {
					t.Fatalf("字段 %s 应被删除，实际仍在", c.absent)
				}
			}
		})
	}
}

func TestNormalizeReasoningLeavesUnknownFamilyAlone(t *testing.T) {
	in := map[string]any{
		"reasoning_effort": "off",
		"thinking":         map[string]any{"type": "disabled"},
		"enable_thinking":  false,
		"temperature":      0.3,
	}
	out := NormalizeReasoning(cloneMap(in), "gpt-4o")
	if out["reasoning_effort"] != "off" {
		t.Fatalf("表外模型不得改档位，实际 %v", out["reasoning_effort"])
	}
	if _, ok := out["thinking"]; !ok {
		t.Fatal("表外模型不得摘 thinking")
	}
	if out["temperature"] != 0.3 {
		t.Fatal("其它标准字段应原样透传")
	}
}

func TestNormalizeReasoningCamelAlias(t *testing.T) {
	out := NormalizeReasoning(map[string]any{"reasoningEffort": "off"}, "sn-glm-5-3")
	if out["reasoning_effort"] != "low" {
		t.Fatalf("驼峰别名应先归到蛇形再归一，实际 %v", out["reasoning_effort"])
	}
	if _, ok := out["reasoningEffort"]; ok {
		t.Fatal("驼峰键应被删除")
	}
}

func TestNormalizeReasoningDoesNotInjectWhenAbsent(t *testing.T) {
	out := NormalizeReasoning(map[string]any{"temperature": 0.5}, "sn-glm-5-3")
	if _, ok := out["reasoning_effort"]; ok {
		t.Fatal("客户端没写思考参数时不应注入")
	}
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func deepEqual(a, b any) bool {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		if len(am) != len(bm) {
			return false
		}
		for k, v := range am {
			if !deepEqual(v, bm[k]) {
				return false
			}
		}
		return true
	}
	return a == b
}
