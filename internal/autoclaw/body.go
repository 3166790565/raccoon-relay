package autoclaw

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/3166790565/raccoon-relay/internal/store"
)

// RewriteBody 把下游请求整形为 AutoClaw 对话网关能接受的样子。
//
// 网关对请求形状的要求是硬性的，任何一条不满足都回 406 且 body 为空：
//
//  1. 必须有一条 system 消息，且内容以官方前缀开头；
//  2. 不能带 max_tokens / max_completion_tokens；
//  3. system 里不能出现其它 agent 的身份语句（Claude Code / Codex / …）。
//
// 第 3 条是网关的内容风控：它只扫描 system/developer 消息，user/assistant
// 内容不碰。所以这里也只改这两类消息，避免误伤用户正文。
func RewriteBody(body map[string]any, station string) map[string]any {
	if body == nil {
		return body
	}
	out := make(map[string]any, len(body))
	for k, v := range body {
		out[k] = v
	}
	// 对外名带 autoclaw/ 前缀，发给上游必须去掉。
	if m, ok := out["model"].(string); ok {
		out["model"] = stripOwnPrefix(m)
	}
	msgs, _ := out["messages"].([]any)
	if msgs == nil {
		return out
	}
	msgs = tidyMessages(msgs)
	unflagMessages(msgs)
	if station == store.StationCN {
		msgs = prefixSystemFirst(msgs)
		if m, ok := out["model"].(string); !ok || strings.TrimSpace(m) == "" {
			out["model"] = "zai_auto"
		}
	} else {
		msgs = prefixSystemAny(msgs)
	}
	out["messages"] = msgs
	// 网关拒收这两个字段。
	delete(out, "max_tokens")
	delete(out, "max_completion_tokens")
	return out
}

func stripOwnPrefix(model string) string {
	const p = store.ProviderAutoClaw + "/"
	if len(model) > len(p) && strings.EqualFold(model[:len(p)], p) {
		return model[len(p):]
	}
	return model
}

// tidyMessages 修掉会让网关 500「parse response failed」的畸形历史。
// 这些畸形来自各家 agent 客户端往历史里塞的私有字段。
func tidyMessages(msgs []any) []any {
	out := make([]any, 0, len(msgs))
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		nm := make(map[string]any, len(m))
		for k, v := range m {
			nm[k] = v
		}
		if c, has := nm["content"]; !has || c == nil {
			nm["content"] = ""
		}
		// 回放的思考内容网关不认，且会让它把请求判成畸形。
		delete(nm, "reasoning_content")
		if calls, ok := nm["tool_calls"].([]any); ok {
			for _, item := range calls {
				tc, ok := item.(map[string]any)
				if !ok {
					continue
				}
				fn, ok := tc["function"].(map[string]any)
				if !ok {
					continue
				}
				switch args := fn["arguments"].(type) {
				case nil:
					fn["arguments"] = "{}"
				case string:
					// 已经是字符串，保持原样。
				default:
					raw, err := json.Marshal(args)
					if err != nil {
						fn["arguments"] = "{}"
					} else {
						fn["arguments"] = string(raw)
					}
				}
			}
		}
		out = append(out, nm)
	}
	return out
}

// harnessRewrite 是「其它 agent 身份语句 → 中性措辞」的替换表。
//
// 顺序有意义（前者可能产生后者要处理的文本），且每条都幂等：
// 已经改过的文本再跑一遍不会继续变化。
type harnessRewrite struct {
	re   *regexp.Regexp
	repl string
}

var harnessRules = []harnessRewrite{
	{regexp.MustCompile(`(?i)You are a Claude agent, built on Anthropic's Claude Agent SDK\.?`), "You are a coding assistant."},
	{regexp.MustCompile(`(?i)(?:Anthropic's |the )?Claude Agent SDK`), "the agent framework"},
	{regexp.MustCompile(`(?i)You are an AI agent,?\s+powered by DeepSeek\s+Harness\.?`), "You are an AI agent powered by a local coding harness."},
	{regexp.MustCompile(`(?i)\bDeepSeek\s+Harness\b`), "DeepSeek"},
	{regexp.MustCompile(`(?i)You are Claude Code`), "You are the Claude Code CLI"},
	// Go 的 regexp 不支持负向前瞻，用「先替换带 CLI 的形态，再替换裸形态」
	// 等价实现：带 CLI 的那条先改成含 CLI 的句子，裸形态就不会再匹配到它。
	{regexp.MustCompile(`(?i)an agent for Claude Code CLI`), "an agent for the Claude Code CLI"},
	{regexp.MustCompile(`(?i)an agent for Claude Code`), "an agent for the Claude Code CLI"},
	{regexp.MustCompile(`(?i)You are Codex`), "You are the Codex agent"},
	{regexp.MustCompile(`(?i)Codex CLI`), "Codex command-line tool"},
	{regexp.MustCompile(`(?i)You are opencode`), "You are the opencode agent"},
	{regexp.MustCompile(`(?i)You are Cursor`), "You are the Cursor agent"},
	{regexp.MustCompile(`(?i)You are Cline`), "You are the Cline agent"},
	{regexp.MustCompile(`(?i)You are Qwen Code`), "You are the Qwen Code agent"},
	{regexp.MustCompile(`(?i)You are ZCode`), "You are the ZCode agent"},
	{regexp.MustCompile(`(?i)an agent for ZCode`), "an agent for the ZCode"},
	{regexp.MustCompile(`(?i)operating inside pi, a coding agent harness`), "operating inside the pi coding agent harness"},
	{regexp.MustCompile(`(?i)This conversation is powered by GLM-([\w-]+(?:\.[\w-]+)*)`), "This conversation is powered by the GLM-$1 model"},
	{regexp.MustCompile(`(?i)(powered by )the model GLM-([\w-]+(?:\.[\w-]+)*)(\s*\[[\w-]+\])?`), "$1the GLM-$2 model"},
	{regexp.MustCompile(`(?i)(powered by )the GLM-([\w-]+(?:\.[\w-]+)*)(\s*\[[\w-]+\])?(\s+model\b)`), "$1the GLM-$2$4"},
	{regexp.MustCompile(`(?i)You are CodeBuddy Code`), "You are the CodeBuddy Code CLI"},
	{regexp.MustCompile(`(?i)You are Hermes(?: Agent)?\b`), "You are the agent Hermes"},
	{regexp.MustCompile(`(?i)Hermes Agent\b`), "Hermes-Agent"},
	{regexp.MustCompile(`(?i)You are Gemini CLI`), "You are the Gemini CLI agent"},
	{regexp.MustCompile(`(?i)You are Kimi Code CLI`), "You are the Kimi Code CLI agent"},
	{regexp.MustCompile(`(?i)You are goose\b`), "You are the goose agent"},
	{regexp.MustCompile(`(?i)You are Grok((?: \d[\w.-]*)?) released by xAI`), "You are the Grok$1 assistant released by xAI"},
}

func unflagText(s string) string {
	for _, r := range harnessRules {
		if r.re.MatchString(s) {
			s = r.re.ReplaceAllString(s, r.repl)
		}
	}
	return s
}

// unflagMessages 只改 system/developer 消息的文本。
func unflagMessages(msgs []any) {
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role != "system" && role != "developer" {
			continue
		}
		switch c := m["content"].(type) {
		case string:
			m["content"] = unflagText(c)
		case []any:
			for _, part := range c {
				p, ok := part.(map[string]any)
				if !ok {
					continue
				}
				if t, ok := p["text"].(string); ok {
					p["text"] = unflagText(t)
				}
			}
		}
	}
}

const systemHead = OfficialSystemPrompt

// prefixSystemAny 给第一条 system 消息加前缀（国际站语义）：
// 字符串内容拼在开头，数组内容在最前面插一个 text 块；没有 system 就补一条。
func prefixSystemAny(msgs []any) []any {
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); role != "system" {
			continue
		}
		switch c := m["content"].(type) {
		case string:
			if !strings.HasPrefix(c, systemHead) {
				m["content"] = systemHead + "\n\n" + c
			}
		case []any:
			first := ""
			if len(c) > 0 {
				if p, ok := c[0].(map[string]any); ok {
					first, _ = p["text"].(string)
				}
			}
			if !strings.HasPrefix(first, systemHead) {
				m["content"] = append([]any{map[string]any{"type": "text", "text": systemHead}}, c...)
			}
		default:
			m["content"] = systemHead
		}
		return msgs
	}
	return append([]any{map[string]any{"role": "system", "content": systemHead}}, msgs...)
}

// prefixSystemFirst 是国内站的语义：只看 messages[0]，不是 system 就在最前面插一条。
func prefixSystemFirst(msgs []any) []any {
	if len(msgs) > 0 {
		if m, ok := msgs[0].(map[string]any); ok {
			if role, _ := m["role"].(string); role == "system" {
				prefixOnly(m)
				return msgs
			}
		}
	}
	return append([]any{map[string]any{"role": "system", "content": systemHead}}, msgs...)
}

func prefixOnly(m map[string]any) {
	switch c := m["content"].(type) {
	case string:
		if !strings.HasPrefix(c, systemHead) {
			m["content"] = systemHead + "\n\n" + c
		}
	case []any:
		first := ""
		if len(c) > 0 {
			if p, ok := c[0].(map[string]any); ok {
				first, _ = p["text"].(string)
			}
		}
		if !strings.HasPrefix(first, systemHead) {
			m["content"] = append([]any{map[string]any{"type": "text", "text": systemHead}}, c...)
		}
	default:
		m["content"] = systemHead
	}
}

// SessionID 由「第一条非 system 消息」稳定派生，形如 UUID。
//
// 上游把同一个 X-Session-Id 视为一段对话：同一段对话的多轮必须一致，
// 不同对话必须不同，否则要么串话、要么被当成全新会话。
func SessionID(body map[string]any) string {
	msgs, _ := body["messages"].([]any)
	var role string
	var content any
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		r, _ := m["role"].(string)
		if r == "system" || r == "developer" {
			continue
		}
		role, content = r, m["content"]
		break
	}
	if role == "" {
		return randomUUID()
	}
	seed, _ := json.Marshal([]any{role, content})
	sum := sha256.Sum256(seed)
	h := hex.EncodeToString(sum[:])
	// 与官方客户端一样，把摘要拼成 v4 形态的 UUID。
	return h[0:8] + "-" + h[8:12] + "-4" + h[13:16] + "-a" + h[17:20] + "-" + h[20:32]
}

func randomUUID() string {
	b, _ := json.Marshal(randomHex(16))
	_ = b
	h := randomHex(16)
	return h[0:8] + "-" + h[8:12] + "-4" + h[13:16] + "-a" + h[17:20] + "-" + h[20:32]
}
