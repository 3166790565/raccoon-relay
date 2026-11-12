package relay

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// Anthropic 请求 → OpenAI 请求。只处理对话所需字段，其余忽略。
func anthropicToOpenAI(body map[string]any) (map[string]any, error) {
	out := map[string]any{}

	model, _ := body["model"].(string)
	out["model"] = model
	if v, ok := body["max_tokens"]; ok {
		out["max_tokens"] = v
	}
	if v, ok := body["temperature"]; ok {
		out["temperature"] = v
	}
	if v, ok := body["top_p"]; ok {
		out["top_p"] = v
	}
	if v, ok := body["stop_sequences"]; ok {
		out["stop"] = v
	}

	var msgs []any
	// system 可能是字符串或内容块数组
	switch sys := body["system"].(type) {
	case string:
		if strings.TrimSpace(sys) != "" {
			msgs = append(msgs, map[string]any{"role": "system", "content": sys})
		}
	case []any:
		var parts []string
		for _, raw := range sys {
			if m, ok := raw.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		if len(parts) > 0 {
			msgs = append(msgs, map[string]any{"role": "system", "content": strings.Join(parts, "\n")})
		}
	}

	rawMsgs, _ := body["messages"].([]any)
	for _, raw := range rawMsgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		converted := anthropicMessageToOpenAI(role, m["content"])
		msgs = append(msgs, converted...)
	}
	out["messages"] = msgs

	if tools, ok := body["tools"].([]any); ok && len(tools) > 0 {
		var oaTools []any
		for _, raw := range tools {
			t, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			name, _ := t["name"].(string)
			desc, _ := t["description"].(string)
			fn := map[string]any{"name": name}
			if desc != "" {
				fn["description"] = desc
			}
			if schema, ok := t["input_schema"]; ok {
				fn["parameters"] = schema
			}
			oaTools = append(oaTools, map[string]any{"type": "function", "function": fn})
		}
		if len(oaTools) > 0 {
			out["tools"] = oaTools
		}
	}

	switch tc := body["tool_choice"].(type) {
	case map[string]any:
		switch tc["type"] {
		case "any":
			out["tool_choice"] = "required"
		case "auto":
			out["tool_choice"] = "auto"
		case "tool":
			if name, ok := tc["name"].(string); ok && name != "" {
				out["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": name}}
			}
		}
	case string:
		if tc == "any" {
			out["tool_choice"] = "required"
		} else if tc != "" {
			out["tool_choice"] = tc
		}
	}

	// thinking 映射到 reasoning_effort，交给家族归一器处理
	if th, ok := body["thinking"].(map[string]any); ok {
		if t, _ := th["type"].(string); t == "disabled" {
			out["reasoning_effort"] = "off"
		} else if budget, ok := th["budget_tokens"].(float64); ok && budget > 0 {
			switch {
			case budget <= 2048:
				out["reasoning_effort"] = "low"
			case budget <= 8192:
				out["reasoning_effort"] = "medium"
			default:
				out["reasoning_effort"] = "high"
			}
		}
	}
	return out, nil
}

// anthropicMessageToOpenAI 把一条 Anthropic 消息拆成一条或多条 OpenAI 消息。
// tool_use 走 assistant.tool_calls；tool_result 走 role:"tool"。
func anthropicMessageToOpenAI(role string, content any) []any {
	switch c := content.(type) {
	case string:
		return []any{map[string]any{"role": role, "content": c}}
	case []any:
		var textParts []string
		var images []any
		var toolCalls []any
		var toolResults []any

		for _, raw := range c {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := block["type"].(string)
			switch typ {
			case "text":
				if t, ok := block["text"].(string); ok {
					textParts = append(textParts, t)
				}
			case "image":
				if src, ok := block["source"].(map[string]any); ok {
					if url := imageToDataURL(src); url != "" {
						images = append(images, map[string]any{
							"type": "image_url", "image_url": map[string]any{"url": url},
						})
					}
				}
			case "tool_use":
				id, _ := block["id"].(string)
				name, _ := block["name"].(string)
				input, _ := json.Marshal(block["input"])
				toolCalls = append(toolCalls, map[string]any{
					"id": id, "type": "function",
					"function": map[string]any{"name": name, "arguments": string(input)},
				})
			case "tool_result":
				id, _ := block["tool_use_id"].(string)
				toolResults = append(toolResults, map[string]any{
					"role": "tool", "tool_call_id": id, "content": flattenToolResult(block["content"]),
				})
			}
		}

		var out []any
		switch role {
		case "assistant":
			msg := map[string]any{"role": "assistant"}
			text := strings.Join(textParts, "")
			if text != "" {
				msg["content"] = text
			} else if len(toolCalls) > 0 {
				msg["content"] = nil
			} else {
				msg["content"] = ""
			}
			if len(toolCalls) > 0 {
				msg["tool_calls"] = toolCalls
			}
			out = append(out, msg)
		default: // user
			if len(textParts) > 0 || len(images) > 0 {
				if len(images) == 0 {
					out = append(out, map[string]any{"role": "user", "content": strings.Join(textParts, "")})
				} else {
					var parts []any
					if t := strings.Join(textParts, ""); t != "" {
						parts = append(parts, map[string]any{"type": "text", "text": t})
					}
					parts = append(parts, images...)
					out = append(out, map[string]any{"role": "user", "content": parts})
				}
			}
			out = append(out, toolResults...)
		}
		return out
	}
	return []any{map[string]any{"role": role, "content": fmt.Sprint(content)}}
}

func imageToDataURL(src map[string]any) string {
	typ, _ := src["type"].(string)
	switch typ {
	case "base64":
		media, _ := src["media_type"].(string)
		data, _ := src["data"].(string)
		if media == "" || data == "" {
			return ""
		}
		return "data:" + media + ";base64," + data
	case "url":
		u, _ := src["url"].(string)
		return u
	}
	return ""
}

func flattenToolResult(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, raw := range c {
			if m, ok := raw.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	if b, err := json.Marshal(content); err == nil {
		return string(b)
	}
	return ""
}

var _ = base64.StdEncoding
