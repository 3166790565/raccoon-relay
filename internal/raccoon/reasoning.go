package raccoon

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// 不同模型家族认完全不同的思考参数。把 xhigh 原样发给只认两档的上游会 400，
// 把 off 发给认开关的上游同样不认。家族表来自实测，表外模型一律不动。

const (
	famThinking = "thinking" // sn-deepseek-* / deepseek-*
	famLowHigh  = "low_high" // sn-glm-5-3* / sn-kimi-k3*
	famEffort   = "effort"   // sn-sensenova-* / raccoon-*
)

var effortLevels = []string{"minimal", "low", "medium", "high", "xhigh", "max"}
var offLevels = map[string]bool{"off": true, "none": true}

func modelBaseName(model string) string {
	m := strings.TrimSpace(model)
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	return strings.ToLower(m)
}

func familyOf(model string) string {
	raw := modelBaseName(model)
	// 客户端常写别名 glm-5.3 / kimi-k3，点上横杠差异不应让它漏判
	name := strings.ReplaceAll(raw, ".", "-")
	switch {
	case strings.HasPrefix(name, "sn-deepseek-"), strings.HasPrefix(name, "deepseek-"):
		return famThinking
	case strings.HasPrefix(name, "sn-glm-5-3"), strings.HasPrefix(name, "glm-5-3"),
		strings.HasPrefix(name, "sn-kimi-k3"), strings.HasPrefix(name, "kimi-k3"):
		return famLowHigh
	case strings.HasPrefix(name, "sn-sensenova-"), strings.HasPrefix(name, "raccoon-"):
		return famEffort
	}
	return ""
}

func effortRank(level string) int {
	key := strings.ToLower(strings.TrimSpace(level))
	for i, l := range effortLevels {
		if l == key {
			return i
		}
	}
	return -1
}

func isOff(level string) bool {
	return offLevels[strings.ToLower(strings.TrimSpace(level))]
}

// NormalizeReasoning 就地改写请求体，返回同一个 map。
// 只做「客户端写了档位 → 翻成该家族认的形态」；没写就不动，不替用户开思考。
func NormalizeReasoning(body map[string]any, model string) map[string]any {
	if body == nil {
		return body
	}
	// 驼峰别名先归到蛇形
	if _, ok := body["reasoning_effort"]; !ok {
		if v, has := body["reasoningEffort"]; has {
			body["reasoning_effort"] = v
			delete(body, "reasoningEffort")
		}
	}
	kind := familyOf(model)
	if kind == "" {
		return body
	}
	effort, _ := body["reasoning_effort"].(string)
	hasThinking := body["thinking"] != nil

	switch kind {
	case famThinking:
		if !hasThinking && isOff(effort) {
			delete(body, "reasoning_effort")
			body["thinking"] = map[string]any{"type": "disabled"}
			return body
		}
		if !hasThinking && effortRank(effort) >= 0 {
			delete(body, "reasoning_effort")
			body["thinking"] = map[string]any{"type": "enabled"}
		}
		return body

	case famLowHigh:
		if th, ok := body["thinking"].(map[string]any); ok && th["type"] == "disabled" {
			delete(body, "thinking")
			body["reasoning_effort"] = "low"
			return body
		}
		if rank := effortRank(effort); rank >= 0 {
			if rank <= 1 {
				body["reasoning_effort"] = "low"
			} else {
				body["reasoning_effort"] = "high"
			}
		} else if isOff(effort) {
			body["reasoning_effort"] = "low"
		}
		return body

	case famEffort:
		if rank := effortRank(effort); rank >= 0 {
			switch {
			case rank <= 1:
				body["reasoning_effort"] = "low"
			case rank == 2:
				body["reasoning_effort"] = "medium"
			default:
				body["reasoning_effort"] = "high"
			}
		} else if isOff(effort) {
			delete(body, "reasoning_effort")
		}
		return body
	}
	return body
}

func hashHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
