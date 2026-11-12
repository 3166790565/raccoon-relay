package relay

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/3166790565/raccoon-relay/internal/raccoon"
)

// ---------- 流式写出 ----------

type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	id      string
	created int64
	model   string
	started bool
	usage   *raccoon.Usage
	finish  string
	toolMap map[int]*toolAcc
	toolSeq []int
}

type toolAcc struct {
	id   string
	name string
	args strings.Builder
}

func newSSEWriter(w http.ResponseWriter, model string) *sseWriter {
	fl, _ := w.(http.Flusher)
	return &sseWriter{
		w: w, flusher: fl, id: "chatcmpl-" + raccoon.NewTurnID(),
		created: time.Now().Unix(), model: model, toolMap: map[int]*toolAcc{},
	}
}

func (s *sseWriter) writeFrame(obj any) error {
	raw, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", raw); err != nil {
		return err
	}
	if s.flusher != nil {
		s.flusher.Flush()
	}
	return nil
}

func (s *sseWriter) Begin() error {
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	s.w.WriteHeader(http.StatusOK)
	s.started = true
	// 首帧只带 role，与 OpenAI 语义一致
	return s.writeFrame(map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
		"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}, "finish_reason": nil,
		}},
	})
}

func (s *sseWriter) Delta(d raccoon.Delta) error {
	delta := map[string]any{}
	if d.Reasoning != "" {
		delta["reasoning_content"] = d.Reasoning
	}
	if d.Content != "" {
		delta["content"] = d.Content
	}
	var calls []any
	for _, tc := range d.ToolCalls {
		acc, ok := s.toolMap[tc.Index]
		if !ok {
			acc = &toolAcc{}
			s.toolMap[tc.Index] = acc
			s.toolSeq = append(s.toolSeq, tc.Index)
		}
		if tc.ID != "" {
			acc.id = tc.ID
		}
		if tc.Function.Name != "" {
			acc.name = tc.Function.Name
		}
		acc.args.WriteString(tc.Function.Arguments)
		fn := map[string]any{}
		if tc.Function.Name != "" {
			fn["name"] = tc.Function.Name
		}
		if tc.Function.Arguments != "" {
			fn["arguments"] = tc.Function.Arguments
		}
		item := map[string]any{"index": tc.Index, "function": fn}
		if tc.ID != "" {
			item["id"] = tc.ID
		}
		if tc.Type != "" {
			if tc.Type != "function" {
				item["type"] = tc.Type
			}
		} else if tc.ID != "" {
			item["type"] = "function"
		}
		calls = append(calls, item)
	}
	if len(calls) > 0 {
		delta["tool_calls"] = calls
	}
	if len(delta) == 0 {
		return nil
	}
	return s.writeFrame(map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}},
	})
}

func (s *sseWriter) Usage(u raccoon.Usage) { s.usage = &u }

func (s *sseWriter) Finish(reason string) {
	if reason != "" {
		s.finish = reason
	}
}

func (s *sseWriter) Complete() {
	finish := s.finish
	if finish == "" {
		if len(s.toolSeq) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	_ = s.writeFrame(map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}},
	})
	if s.usage != nil {
		_ = s.writeFrame(map[string]any{
			"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
			"choices": []any{},
			"usage": map[string]any{
				"prompt_tokens": s.usage.PromptTokens, "completion_tokens": s.usage.CompletionTokens,
				"total_tokens": s.usage.TotalTokens,
			},
		})
	}
	_, _ = fmt.Fprint(s.w, "data: [DONE]\n\n")
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// ---------- 非流式聚合 ----------

type aggWriter struct {
	model   string
	id      string
	created int64
	content strings.Builder
	reason  strings.Builder
	usage   *raccoon.Usage
	finish  string
	toolMap map[int]*toolAcc
	toolSeq []int
}

func newAggWriter(model string) *aggWriter {
	return &aggWriter{
		model: model, id: "chatcmpl-" + raccoon.NewTurnID(), created: time.Now().Unix(),
		toolMap: map[int]*toolAcc{},
	}
}

func (a *aggWriter) Begin() error { return nil }

func (a *aggWriter) Delta(d raccoon.Delta) error {
	a.content.WriteString(d.Content)
	a.reason.WriteString(d.Reasoning)
	for _, tc := range d.ToolCalls {
		acc, ok := a.toolMap[tc.Index]
		if !ok {
			acc = &toolAcc{}
			a.toolMap[tc.Index] = acc
			a.toolSeq = append(a.toolSeq, tc.Index)
		}
		if tc.ID != "" {
			acc.id = tc.ID
		}
		if tc.Function.Name != "" {
			acc.name = tc.Function.Name
		}
		acc.args.WriteString(tc.Function.Arguments)
	}
	return nil
}

func (a *aggWriter) Usage(u raccoon.Usage) { a.usage = &u }

func (a *aggWriter) Finish(reason string) {
	if reason != "" {
		a.finish = reason
	}
}

func (a *aggWriter) Complete() {}

// Message 生成非流式响应体。
func (a *aggWriter) Message() map[string]any {
	msg := map[string]any{"role": "assistant"}
	content := a.content.String()
	if content == "" && len(a.toolSeq) > 0 {
		msg["content"] = nil
	} else {
		msg["content"] = content
	}
	if r := a.reason.String(); r != "" {
		msg["reasoning_content"] = r
	}
	var calls []any
	for _, idx := range a.toolSeq {
		acc := a.toolMap[idx]
		args := acc.args.String()
		if args == "" {
			args = "{}"
		}
		calls = append(calls, map[string]any{
			"id": acc.id, "type": "function", "index": idx,
			"function": map[string]any{"name": acc.name, "arguments": args},
		})
	}
	if len(calls) > 0 {
		msg["tool_calls"] = calls
	}
	finish := a.finish
	if finish == "" {
		if len(calls) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	out := map[string]any{
		"id": a.id, "object": "chat.completion", "created": a.created, "model": a.model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
	}
	usage := map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	if a.usage != nil {
		usage["prompt_tokens"] = a.usage.PromptTokens
		usage["completion_tokens"] = a.usage.CompletionTokens
		usage["total_tokens"] = a.usage.TotalTokens
	}
	out["usage"] = usage
	return out
}

// errorBody 生成 OpenAI 形态的错误响应。
func errorBody(code int, msg string) map[string]any {
	typ := "upstream_error"
	switch code {
	case http.StatusUnauthorized:
		typ = "authentication_error"
	case http.StatusPaymentRequired:
		typ = "insufficient_quota"
	case http.StatusTooManyRequests:
		typ = "rate_limit_exceeded"
	case http.StatusBadRequest:
		typ = "invalid_request_error"
	}
	return map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": code}}
}

func writeJSON(w http.ResponseWriter, code int, obj any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(obj)
}

// ---------- 入口 ----------

// HandleOpenAIChat 处理 /v1/chat/completions。
func (s *Server) HandleOpenAIChat(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(r); !ok {
		writeJSON(w, http.StatusUnauthorized, errorBody(http.StatusUnauthorized, "API Key 无效"))
		return
	}
	body, err := decodeBody(r, s.cfg.MaxBodyMB)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody(http.StatusBadRequest, err.Error()))
		return
	}
	model, _ := body["model"].(string)
	streamReq := boolOf(body["stream"])

	cc := BuildChatContext(model, body, DefaultModel)
	start := time.Now()

	if streamReq {
		sink := newSSEWriter(w, cc.Model)
		res, err := s.engine.RunOpenAI(r.Context(), cc, sink)
		s.logUsage(r.Context(), res, err, start, "openai", sink.usage)
		if err != nil {
			// 已写出内容时只能如实断流，不再回错误体
			_ = err
		}
		return
	}

	sink := newAggWriter(cc.Model)
	res, err := s.engine.RunOpenAI(r.Context(), cc, sink)
	s.logUsage(r.Context(), res, err, start, "openai", sink.usage)
	if err != nil {
		code, msg := publicError(err)
		writeJSON(w, code, errorBody(code, msg))
		return
	}
	writeJSON(w, http.StatusOK, sink.Message())
}

// publicError 把内部错误翻成对外可读的 code + message。
func publicError(err error) (int, string) {
	kind, _, code, msg := classify(err)
	_ = kind
	if code == 0 {
		code = http.StatusBadGateway
	}
	return code, cleanMessage(msg)
}

// HandleModels 处理 /v1/models。
func (s *Server) HandleModels(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(r); !ok {
		writeJSON(w, http.StatusUnauthorized, errorBody(http.StatusUnauthorized, "API Key 无效"))
		return
	}
	models := s.catalog.List(r.Context())
	data := make([]any, 0, len(models))
	for _, m := range models {
		data = append(data, map[string]any{
			"id": m.ID, "object": "model", "created": time.Now().Unix(), "owned_by": "raccoon",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func boolOf(v any) bool {
	b, ok := v.(bool)
	return ok && b
}
