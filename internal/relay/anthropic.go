package relay

import (
	"net/http"
	"strings"
	"time"
)

// HandleAnthropicMessages 处理 /v1/messages。
func (s *Server) HandleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(r); !ok {
		writeJSON(w, http.StatusUnauthorized, anthropicError("authentication_error", "API Key 无效"))
		return
	}
	body, err := decodeBody(r, s.cfg.MaxBodyMB)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, anthropicError("invalid_request_error", err.Error()))
		return
	}
	model, _ := body["model"].(string)
	streamReq := boolOf(body["stream"])

	oaBody, err := anthropicToOpenAI(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, anthropicError("invalid_request_error", err.Error()))
		return
	}
	cc := BuildChatContext(model, oaBody, DefaultModel)
	start := time.Now()

	if streamReq {
		sink := newAnthropicSSE(w, cc.Model)
		res, err := s.engine.RunOpenAI(r.Context(), cc, sink)
		s.logUsage(r.Context(), res, err, start, "anthropic", nil)
		return
	}

	sink := newAnthropicAgg(cc.Model)
	res, err := s.engine.RunOpenAI(r.Context(), cc, sink)
	s.logUsage(r.Context(), res, err, start, "anthropic", &sink.usage)
	if err != nil {
		code, msg := publicError(err)
		typ := "api_error"
		switch code {
		case http.StatusUnauthorized:
			typ = "authentication_error"
		case http.StatusPaymentRequired:
			typ = "insufficient_quota"
		case http.StatusTooManyRequests:
			typ = "rate_limit_error"
		case http.StatusBadRequest:
			typ = "invalid_request_error"
		}
		writeJSON(w, code, anthropicError(typ, msg))
		return
	}
	writeJSON(w, http.StatusOK, sink.Message())
}

// HandleAnthropicCountTokens 用本地估算。响应里明确标注是估算值，不伪造精确计数。
func (s *Server) HandleAnthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(r); !ok {
		writeJSON(w, http.StatusUnauthorized, anthropicError("authentication_error", "API Key 无效"))
		return
	}
	body, err := decodeBody(r, s.cfg.MaxBodyMB)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, anthropicError("invalid_request_error", err.Error()))
		return
	}
	n := estimateTokens(body)
	writeJSON(w, http.StatusOK, map[string]any{
		"input_tokens": n,
		"estimated":    true,
		"note":         "本地粗估（CJK 按 0.7 token/字，其余 4 字符 1 token），非上游精确计数",
	})
}

// estimateTokens 遍历请求里的所有文本节点做粗估。
func estimateTokens(body map[string]any) int64 {
	var total int64
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			total += roughTokens(t)
		case []any:
			for _, item := range t {
				walk(item)
			}
		case map[string]any:
			for k, item := range t {
				// schema / 元数据字段不计入对话 token
				if k == "input_schema" || k == "metadata" {
					continue
				}
				walk(item)
			}
		}
	}
	walk(body)
	return total
}

func roughTokens(s string) int64 {
	var cjk, other int64
	for _, r := range s {
		if r >= 0x2E80 {
			cjk++
		} else {
			other++
		}
	}
	n := cjk*7/10 + other/4
	if n < 1 && (cjk > 0 || other > 0) {
		n = 1
	}
	return n
}

func anthropicError(typ, msg string) map[string]any {
	return map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": strings.TrimSpace(msg)}}
}

var _ = time.Now
