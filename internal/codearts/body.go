package codearts

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/3166790565/raccoon-relay/internal/store"
)

// ChatOptions 是 PrepareChatBody 额外算出来的信息。
type ChatOptions struct {
	// Fold 表示客户端要的是非流式：上游仍以流式请求（码道只对流式计量），
	// 由调用方把 SSE 折叠回一个 chat.completion。
	Fold bool
	// ToolNames 是「截断后工具名 → 原始工具名」的映射，响应里要还原。
	ToolNames map[string]string
	// Benefit 表示这次要打的是免费模型，需要带 maas_type 头。
	Benefit bool
}

const maxOutputTokens = 65536

// PrepareChatBody 把下游请求整形为码道网关能接受的样子。
//
// 与其它上游最大的不同：
//   - 模型名去掉 codearts/ 前缀；
//   - 默认注入 thinking:{type:"enabled"}（码道的思考走 reasoning_content）；
//   - 输出预算硬顶 65536；
//   - 国内站一律删掉 stop（网关拒收任何非空 stop）；
//   - 工具名超过 64 字符要截断并在响应里还原；
//   - 永远带 stream:true。
func PrepareChatBody(body map[string]any, site string) (map[string]any, ChatOptions) {
	out := make(map[string]any, len(body)+4)
	for k, v := range body {
		out[k] = v
	}
	opts := ChatOptions{}
	model, _ := out["model"].(string)
	model = stripPrefix(model)
	out["model"] = model

	if names := shortenToolNames(out); len(names) > 0 {
		opts.ToolNames = names
	}

	// 思考等级：客户端显式指定就照它的来；说了「不思考」就显式关掉；
	// 没说过才注入 enabled。这与插件一致，也与转发层「不替用户开思考」的约定兼容。
	spoken := ""
	if s, ok := out["reasoning_effort"].(string); ok {
		spoken = strings.ToLower(strings.TrimSpace(s))
	}
	switch {
	case spoken == "":
		if _, ok := out["thinking"]; !ok {
			out["thinking"] = map[string]any{"type": "enabled"}
		}
	case spoken == "none" || spoken == "off" || spoken == "disabled":
		delete(out, "reasoning_effort")
		if _, ok := out["thinking"]; !ok {
			out["thinking"] = map[string]any{"type": "disabled"}
		}
	default:
		// 等级合法性由上游校验；不认识时它回 400，比我们猜错更准确。
		out["reasoning_effort"] = spoken
	}

	for _, k := range []string{"max_tokens", "max_completion_tokens"} {
		if n, ok := out[k].(float64); ok && n > maxOutputTokens {
			out[k] = maxOutputTokens
		}
	}

	// stop：国内站一律删；国际站只接受字符串数组。
	if site != store.StationGlobal {
		delete(out, "stop")
	} else {
		normalizeStop(out)
	}

	opts.Fold = out["stream"] != true
	out["stream"] = true
	// 上游不认 stream_options，多带只会被拒。
	delete(out, "stream_options")
	return out, opts
}

func stripPrefix(model string) string {
	const p = store.ProviderCodeArts + "/"
	if len(model) > len(p) && strings.EqualFold(model[:len(p)], p) {
		return model[len(p):]
	}
	return model
}

func normalizeStop(body map[string]any) {
	switch v := body["stop"].(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			delete(body, "stop")
		} else {
			body["stop"] = []string{v}
		}
	case []any:
		var out []string
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		if len(out) == 0 {
			delete(body, "stop")
		} else {
			body["stop"] = out
		}
	default:
		delete(body, "stop")
	}
}

// ---------- 工具名截断 ----------

const (
	toolNameLimit = 64
	toolNameKeep  = 55
)

// shortenToolNames 截断超长工具名，返回「截断名 → 原名」映射。
//
// 码道只接受 64 字符以内的工具名；MCP 工具名动辄上百字符，不截断会被
// 网关以 InferHub.001001017.413 拒掉。截断规则与插件一致：前 55 字符 +
// "_" + 8 位摘要，保证同一原名总是映射到同一截断名（流式与重试都需要稳定）。
func shortenToolNames(body map[string]any) map[string]string {
	names := map[string]string{}
	short := func(name string) string {
		if len(name) <= toolNameLimit {
			return name
		}
		head := name[:toolNameKeep]
		for strings.HasSuffix(head, "�") {
			head = head[:len(head)-1]
		}
		sum := sha256.Sum256([]byte(name))
		return head + "_" + hex.EncodeToString(sum[:])[:8]
	}
	visit := func(holder map[string]any, key string) {
		if n, ok := holder[key].(string); ok && len(n) > toolNameLimit {
			holder[key] = short(n)
			names[holder[key].(string)] = n
		}
	}
	if tools, ok := body["tools"].([]any); ok {
		for _, t := range tools {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			if fn, ok := tm["function"].(map[string]any); ok {
				visit(fn, "name")
			} else {
				visit(tm, "name")
			}
		}
	}
	if msgs, ok := body["messages"].([]any); ok {
		for _, m := range msgs {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			if calls, ok := mm["tool_calls"].([]any); ok {
				for _, c := range calls {
					cm, ok := c.(map[string]any)
					if !ok {
						continue
					}
					if fn, ok := cm["function"].(map[string]any); ok {
						visit(fn, "name")
					}
				}
			}
			if role, _ := mm["role"].(string); role == "tool" {
				visit(mm, "name")
			}
		}
	}
	if len(names) == 0 {
		return nil
	}
	return names
}

// restoreName 把响应里的截断工具名换回原名。
func restoreName(name string, m map[string]string) string {
	if len(m) == 0 {
		return name
	}
	if orig, ok := m[name]; ok {
		return orig
	}
	return name
}

// ---------- 错误码 ----------

var (
	rateLimitRe = regexp.MustCompile(`4291|8111\d|(^|\.)429(\.|$)`)
	clientErrRe = regexp.MustCompile(`(^|\.)40[0-4](\.|$)|InferHub\.400[0-9]\.`)
	notMultiRe  = regexp.MustCompile(`InferHub\.001001020\.406|not multimodal`)
	staleCredRe = regexp.MustCompile(`APIG\.0(301|307|602)|security token has expired|verify ak sk signature fail`)
	codeRe      = regexp.MustCompile(`"(?:error_code|code)"\s*:\s*"((?:[A-Za-z]+\.)*[A-Za-z]*[0-9][0-9A-Za-z.]*)"`)
	messageRe   = regexp.MustCompile(`"(?:error_msg|message)"\s*:\s*"((?:[^"\\]|\\.)*)"`)
)

// errorCodeOf 从响应体里抠出码道的错误码（形如 InferHub.4004.200）。
func errorCodeOf(body []byte) string {
	if m := codeRe.FindSubmatch(body); m != nil {
		return string(m[1])
	}
	// 也认裸 JSON 的 error_code / code 字符串字段。
	var env struct {
		ErrorCode string          `json:"error_code"`
		Code      json.RawMessage `json:"code"`
	}
	if json.Unmarshal(body, &env) == nil {
		if env.ErrorCode != "" {
			return env.ErrorCode
		}
		if c := strOf(jsonValue(env.Code)); c != "" {
			return c
		}
	}
	return ""
}

func errorMessageOf(body []byte) string {
	if m := messageRe.FindSubmatch(body); m != nil {
		s := string(m[1])
		if u, err := unescapeJSONString(s); err == nil {
			return u
		}
		return s
	}
	return ""
}

func unescapeJSONString(s string) (string, error) {
	var out string
	if err := json.Unmarshal([]byte(`"`+s+`"`), &out); err != nil {
		return "", err
	}
	return out, nil
}

// embeddedErrorStatus 把「塞在 200 流里」的错误码换算成 HTTP 语义。
func embeddedErrorStatus(code string) int {
	switch {
	case rateLimitRe.MatchString(code):
		return 429
	case clientErrRe.MatchString(code):
		return 400
	case notMultiRe.MatchString(code):
		return 400
	case code == "":
		return 0
	}
	return 502
}

// IsStaleCredential 判断错误是否在说「临时凭据过期 / 签名校验失败」。
// 这种错误续期一次再重发即可，不应该把账号打成需重登。
func IsStaleCredential(status int, text string) bool {
	if status == http.StatusUnauthorized {
		return true
	}
	return staleCredRe.MatchString(text)
}

func isNotMultimodal(code, msg string) bool {
	return notMultiRe.MatchString(code) || notMultiRe.MatchString(msg)
}
