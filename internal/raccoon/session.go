package raccoon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// 会话与轮次标识的派生。
//
// 上游把 `X-RACCOON-Session-ID` 当作「这段对话」，把 `X-RACCOON-Turn-ID`
// 当作「这一轮」。两者的稳定性要求完全不同：
//
//   - Session 必须**跨轮不变**：同一段对话的每一轮都是新请求，如果每轮都换
//     session，上游看到的是一串互不相干的孤立请求（会话归并、上下文缓存都失效）。
//   - Turn 每轮一个，但**工具续轮必须复用**：客户端把工具结果接在最后一条
//     user 消息之后再次请求，那是同一轮，不是新一轮。
//
// 因此 Session 的种子优先取调用方给的会话键（X-Session-Id / conversation_id
// / prompt_cache_key …），没有才退到「首条 user 消息」；Turn 的种子取
// 「到**最后一条** user 消息为止」的内容——其后的工具结果不参与，续轮因此稳定。

// seedUUID 把种子哈希成 8-4-4-4-12 形态的稳定标识。
//
// 带长度前缀地喂每一段，避免 "ab"+"c" 与 "a"+"bc" 拼出同一个输入。
func seedUUID(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		var length [8]byte
		n := uint64(len(part))
		for i := 7; i >= 0; i-- {
			length[i] = byte(n)
			n >>= 8
		}
		h.Write(length[:])
		h.Write([]byte(part))
	}
	sum := h.Sum(nil)
	hexed := hex.EncodeToString(sum[:16])
	// 8-4-4-4-12，与上游客户端生成的 id 同形
	return hexed[0:8] + "-" + hexed[8:12] + "-4" + hexed[13:16] + "-a" + hexed[17:20] + "-" + hexed[20:32]
}

// SessionID 由 uid + 会话种子稳定派生，让上游把同一会话归到一起。
func SessionID(uid string, seedText string) string {
	return seedUUID("raccoon:session", defaultStr(uid, "anon"), seedText)
}

// TurnIDFor 由会话 id + 轮次种子稳定派生。
//
// 同一会话里「最后一条 user 消息相同」的请求得到同一个 Turn：工具续轮
// 复用上一轮的标识，而用户真正发了新消息才会变成新的一轮。
func TurnIDFor(sessionID, turnSeed string) string {
	return seedUUID("raccoon:turn", sessionID, turnSeed)
}

// SessionSeedOf 取「这段对话」的种子：显式会话键优先，否则退到首条 user 消息。
//
// 为什么不是「整个 messages 的哈希」：那样每一轮都是新种子，上游会把同一条
// 对话看成互不相干的请求。工具续轮与多轮对话都要落在同一个会话上。
func SessionSeedOf(sessionKey string, messages []any) string {
	if key := NormalizeSessionSeed(sessionKey); key != "" {
		return key
	}
	if first := FirstUserMessageText(messages); first != "" {
		return "first-user:" + first
	}
	seed, _ := json.Marshal(messages)
	return "messages:" + string(seed)
}

// TurnSeedOf 取「这一轮」的种子：只哈希到最后一条 user 消息为止。
//
// 工具结果位于最后一条 user 消息之后，它们不构成新的一轮；把它们算进去
// 会让每一次工具续轮都变成新的 turn。
func TurnSeedOf(messages []any) string {
	end := -1
	for i, m := range messages {
		if msg, ok := m.(map[string]any); ok && msg["role"] == "user" {
			end = i
		}
	}
	if end < 0 {
		seed, _ := json.Marshal(messages)
		return "messages:" + string(seed)
	}
	seed, _ := json.Marshal(messages[:end+1])
	return string(seed)
}

// FirstUserMessageText 取首条 user 消息的文本（字符串或文本块数组）。
func FirstUserMessageText(messages []any) string {
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok || msg["role"] != "user" {
			continue
		}
		if s, ok := msg["content"].(string); ok {
			if s = strings.TrimSpace(s); s != "" {
				return s
			}
		}
		if arr, ok := msg["content"].([]any); ok {
			for _, part := range arr {
				p, ok := part.(map[string]any)
				if !ok {
					continue
				}
				if t, ok := p["text"].(string); ok && strings.TrimSpace(t) != "" {
					return strings.TrimSpace(t)
				}
			}
		}
	}
	return ""
}

// NormalizeSessionSeed 把会话种子裁到合理长度并拒绝控制字符：
// 它会进哈希，长度无关紧要，但脏值（换行、超长串）没有保留价值。
func NormalizeSessionSeed(seed string) string {
	seed = strings.TrimSpace(seed)
	if seed == "" {
		return ""
	}
	const max = 512
	if len([]rune(seed)) > max {
		seed = string([]rune(seed)[:max])
	}
	var b strings.Builder
	for _, r := range seed {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
