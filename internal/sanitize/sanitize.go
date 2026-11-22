// Package sanitize 是出站请求体的指纹脱敏层：按词表做字面替换与正则改写，
// 只修改「发给上游的那份副本」，客户端看到的请求与响应都不变。
//
// 规则在保存时编译（见 Compile），转发热路径只做匹配与替换。同一次请求内
// 词表是同一份快照，不会出现「前半段用旧词表、后半段用新词表」。
package sanitize

import (
	"errors"
	"fmt"
	"strings"
)

func errf(format string, args ...any) error {
	return errors.New(fmt.Sprintf(format, args...))
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

// Hit 是一条规则的命中次数（用于流水 / 日志展示）。
type Hit struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

// TextScope 描述一处文本被改写的上下文：角色与是否正文。
type TextScope struct {
	Role    string // 空 = 未知角色
	Content bool   // true = 消息正文；false = reasoning / tool_calls 等辅助字段
}

// appliesTo 判断规则是否作用于这处文本。
//
// 角色列表为空的旧规则沿用「全字段」行为；指定角色时只改这些角色消息的
// 正文（辅助字段不碰），与 Agent2API 同一口径。
func appliesTo(rule Rule, scope TextScope) bool {
	if len(rule.Roles) == 0 {
		return true
	}
	if !scope.Content || scope.Role == "" {
		return false
	}
	for _, role := range rule.Roles {
		if strings.EqualFold(role, scope.Role) {
			return true
		}
	}
	return false
}

// SanitizeText 对一段文本执行词表，返回改写后的文本与命中。
// 没有任何变化时返回原串（调用方据此保留零拷贝）。
func (rs *RuleSet) SanitizeText(text string, scope TextScope) (string, []Hit) {
	if rs == nil || len(rs.compiled) == 0 || text == "" {
		return text, nil
	}
	out := text
	var hits []Hit
	bump := func(label string, n int) {
		for i := range hits {
			if hits[i].Label == label {
				hits[i].Count += n
				return
			}
		}
		hits = append(hits, Hit{Label: label, Count: n})
	}
	for _, c := range rs.compiled {
		rule := c.Rule
		if !appliesTo(rule, scope) {
			continue
		}
		for {
			var next string
			var count int
			if rule.MatchType == MatchLiteral {
				count = strings.Count(out, rule.Pattern)
				if count == 0 {
					break
				}
				next = strings.ReplaceAll(out, rule.Pattern, rule.Replacement)
			} else {
				if c.regex == nil {
					break
				}
				matches := c.regex.FindAllString(out, -1)
				count = len(matches)
				if count == 0 {
					break
				}
				next = c.regex.ReplaceAllLiteralString(out, rule.Replacement)
			}
			if next == out {
				break
			}
			label := rule.Pattern
			if rule.MatchType == MatchRegex {
				label = rule.Name
			}
			// 能走到这里说明确实改了文本（next == out 的「零宽匹配」已被上面拦下），
			// 命中数来自本轮匹配次数，而不是新旧文本的差。
			bump(label, count)
			out = next
			if !rule.Repeat {
				break
			}
		}
	}
	return out, hits
}

// Path 定位请求体里的一处文本，便于统一报告与测试。
const (
	PathSystem          = "system"
	PathSystemParts     = "system[].text"
	PathContent         = "messages[].content"
	PathReasoningEffort = "messages[].reasoning_content"
	PathToolArgs        = "messages[].tool_calls[].function.arguments"
)

// Result 是一次请求体脱敏的结论。
type Result struct {
	// Body 是改写后的副本；仅在 Changed 为 true 时非 nil（调用方沿用原体即可）。
	Body    map[string]any
	Changed bool
	Hits    []Hit
}

// SanitizeRequest 对一份「OpenAI 形态」的请求体执行脱敏。
//
// 覆盖 system 提示词（字符串或内容块数组）、messages[].content（字符串或
// 内容块数组）、reasoning_content 与 tool_calls 的 function.arguments。
// 返回的 Hits 已按命中次数降序排好。
func (rs *RuleSet) SanitizeRequest(body map[string]any) Result {
	if rs == nil || len(rs.compiled) == 0 || body == nil {
		return Result{}
	}
	clone := cloneMap(body)
	all := []Hit{}

	// system 可能是字符串，也可能是内容块数组（Anthropic / Responses 转出来的形状）。
	if sys, ok := clone["system"]; ok {
		switch v := sys.(type) {
		case string:
			next, hits := rs.SanitizeText(v, TextScope{Role: "system", Content: true})
			if next != v {
				clone["system"] = next
			}
			all = mergeHits(all, hits)
		case []any:
			changed, hits := rs.sanitizeParts(v, TextScope{Role: "system", Content: true})
			if changed {
				clone["system"] = v
			}
			all = mergeHits(all, hits)
		}
	}

	if list, ok := clone["messages"].([]any); ok {
		for _, raw := range list {
			msg, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			role, _ := msg["role"].(string)
			contentScope := TextScope{Role: role, Content: true}
			auxScope := TextScope{Role: role, Content: false}
			if content, ok := msg["content"]; ok {
				switch v := content.(type) {
				case string:
					next, hits := rs.SanitizeText(v, contentScope)
					if next != v {
						msg["content"] = next
					}
					all = mergeHits(all, hits)
				case []any:
					changed, hits := rs.sanitizeParts(v, contentScope)
					if changed {
						msg["content"] = v
					}
					all = mergeHits(all, hits)
				}
			}
			if reasoning, ok := msg["reasoning_content"].(string); ok {
				next, hits := rs.SanitizeText(reasoning, auxScope)
				if next != reasoning {
					msg["reasoning_content"] = next
				}
				all = mergeHits(all, hits)
			}
			if calls, ok := msg["tool_calls"].([]any); ok {
				for _, rawCall := range calls {
					call, ok := rawCall.(map[string]any)
					if !ok {
						continue
					}
					fn, ok := call["function"].(map[string]any)
					if !ok {
						continue
					}
					args, ok := fn["arguments"].(string)
					if !ok {
						continue
					}
					next, hits := rs.SanitizeText(args, auxScope)
					if next != args {
						fn["arguments"] = next
					}
					all = mergeHits(all, hits)
				}
			}
		}
	}

	sortHits(all)
	return Result{Body: clone, Changed: len(all) > 0, Hits: all}
}

// sanitizeParts 处理内容块数组：只改 type 为 text 的块（图片等原样保留）。
func (rs *RuleSet) sanitizeParts(parts []any, scope TextScope) (bool, []Hit) {
	changed := false
	var all []Hit
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		text, ok := part["text"].(string)
		if !ok {
			continue
		}
		next, hits := rs.SanitizeText(text, scope)
		if next != text {
			part["text"] = next
			changed = true
		}
		all = mergeHits(all, hits)
	}
	return changed, all
}

func mergeHits(dst, src []Hit) []Hit {
	for _, s := range src {
		found := false
		for i := range dst {
			if dst[i].Label == s.Label {
				dst[i].Count += s.Count
				found = true
				break
			}
		}
		if !found {
			dst = append(dst, s)
		}
	}
	return dst
}

func sortHits(hits []Hit) {
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0 && hits[j].Count > hits[j-1].Count; j-- {
			hits[j], hits[j-1] = hits[j-1], hits[j]
		}
	}
}

// cloneMap 浅拷贝 map（值里的 map / 切片来自 JSON 解码，脱敏时我们会就地修改
// 这些嵌套容器，所以必须逐层复制到会被写的层级）。
func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = cloneValue(v)
	}
	return out
}

func cloneValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return cloneMap(t)
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = cloneValue(item)
		}
		return out
	default:
		return v
	}
}
