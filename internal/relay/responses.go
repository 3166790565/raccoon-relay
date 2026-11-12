package relay

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/3166790565/raccoon-relay/internal/raccoon"
)

// responsesToOpenAI 把 /v1/responses 请求转成 OpenAI Chat Completions 请求。
// input 支持字符串，或 item 数组（message / input_text / input_image / function_call / function_call_output）。
func responsesToOpenAI(body map[string]any) map[string]any {
	out := map[string]any{}
	if m, ok := body["model"].(string); ok {
		out["model"] = m
	}
	if v, ok := body["max_output_tokens"]; ok {
		out["max_tokens"] = v
	}
	if v, ok := body["temperature"]; ok {
		out["temperature"] = v
	}
	if v, ok := body["top_p"]; ok {
		out["top_p"] = v
	}

	var msgs []any
	// instructions 等价于 system
	if ins, ok := body["instructions"].(string); ok && strings.TrimSpace(ins) != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": ins})
	}

	switch input := body["input"].(type) {
	case string:
		msgs = append(msgs, map[string]any{"role": "user", "content": input})
	case []any:
		var pendingToolCalls []any
		flushToolCalls := func() {
			if len(pendingToolCalls) > 0 {
				msgs = append(msgs, map[string]any{
					"role": "assistant", "content": nil, "tool_calls": pendingToolCalls,
				})
				pendingToolCalls = nil
			}
		}
		for _, raw := range input {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := item["type"].(string)
			switch typ {
			case "message", "":
				role, _ := item["role"].(string)
				if role == "" {
					role = "user"
				}
				content := responsesContentToOpenAI(item["content"])
				flushToolCalls()
				msgs = append(msgs, map[string]any{"role": role, "content": content})
			case "function_call":
				callID, _ := item["call_id"].(string)
				name, _ := item["name"].(string)
				args, _ := item["arguments"].(string)
				if args == "" {
					args = "{}"
				}
				pendingToolCalls = append(pendingToolCalls, map[string]any{
					"id": callID, "type": "function",
					"function": map[string]any{"name": name, "arguments": args},
				})
			case "function_call_output":
				flushToolCalls()
				callID, _ := item["call_id"].(string)
				var content string
				switch c := item["output"].(type) {
				case string:
					content = c
				default:
					if b, err := json.Marshal(c); err == nil {
						content = string(b)
					}
				}
				msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": callID, "content": content})
			}
		}
		flushToolCalls()
	}
	out["messages"] = msgs

	// responses 的 tools 是扁平结构：{type:"function",name,description,parameters}
	if tools, ok := body["tools"].([]any); ok && len(tools) > 0 {
		var oaTools []any
		for _, raw := range tools {
			t, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			kind, _ := t["type"].(string)
			if kind != "" && kind != "function" {
				continue
			}
			name, _ := t["name"].(string)
			desc, _ := t["description"].(string)
			fn := map[string]any{"name": name}
			if desc != "" {
				fn["description"] = desc
			}
			if p, ok := t["parameters"]; ok {
				fn["parameters"] = p
			}
			oaTools = append(oaTools, map[string]any{"type": "function", "function": fn})
		}
		if len(oaTools) > 0 {
			out["tools"] = oaTools
		}
	}
	if v, ok := body["tool_choice"]; ok {
		out["tool_choice"] = v
	}
	return out
}

func responsesContentToOpenAI(content any) any {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var texts []string
		var images []any
		for _, raw := range c {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch part["type"] {
			case "input_text", "output_text", "text":
				if t, ok := part["text"].(string); ok {
					texts = append(texts, t)
				}
			case "input_image":
				url, _ := part["image_url"].(string)
				if url != "" {
					images = append(images, map[string]any{
						"type": "image_url", "image_url": map[string]any{"url": url},
					})
				}
			}
		}
		if len(images) == 0 {
			return strings.Join(texts, "")
		}
		var parts []any
		if t := strings.Join(texts, ""); t != "" {
			parts = append(parts, map[string]any{"type": "text", "text": t})
		}
		parts = append(parts, images...)
		return parts
	}
	return fmt.Sprint(content)
}

// ---------- 流式 ----------

type responsesSSE struct {
	w        http.ResponseWriter
	flusher  http.Flusher
	model    string
	id       string
	itemID   string
	seq      int64
	started  bool
	output   strings.Builder
	reason   strings.Builder
	usage    raccoon.Usage
	tools    []*toolAcc
	toolByIx map[int]*toolAcc
	toolSeq  []int
}

func newResponsesSSE(w http.ResponseWriter, model string) *responsesSSE {
	fl, _ := w.(http.Flusher)
	suffix := raccoon.NewTurnID()
	return &responsesSSE{
		w: w, flusher: fl, model: model,
		id: "resp_" + suffix, itemID: "msg_" + suffix,
		toolByIx: map[int]*toolAcc{},
	}
}

func (r *responsesSSE) emit(event string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	// 每个事件带单调递增的 sequence_number
	r.seq++
	payload["sequence_number"] = r.seq - 1
	raw, _ = json.Marshal(payload)
	if _, err := fmt.Fprintf(r.w, "event: %s\ndata: %s\n\n", event, raw); err != nil {
		return err
	}
	if r.flusher != nil {
		r.flusher.Flush()
	}
	return nil
}

func (r *responsesSSE) Begin() error {
	if r.started {
		return nil
	}
	r.started = true
	h := r.w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	r.w.WriteHeader(http.StatusOK)

	if err := r.emit("response.created", map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id": r.id, "object": "response", "created_at": time.Now().Unix(),
			"status": "in_progress", "model": r.model, "output": []any{},
		},
	}); err != nil {
		return err
	}
	if err := r.emit("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": 0,
		"item": map[string]any{"id": r.itemID, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}},
	}); err != nil {
		return err
	}
	return r.emit("response.content_part.added", map[string]any{
		"type": "response.content_part.added", "item_id": r.itemID, "output_index": 0,
		"content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
}

func (r *responsesSSE) Delta(d raccoon.Delta) error {
	r.output.WriteString(d.Content)
	r.reason.WriteString(d.Reasoning)
	if d.Content != "" {
		if err := r.emit("response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "item_id": r.itemID,
			"output_index": 0, "content_index": 0, "delta": d.Content,
		}); err != nil {
			return err
		}
	}
	for _, tc := range d.ToolCalls {
		acc, ok := r.toolByIx[tc.Index]
		if !ok {
			acc = &toolAcc{}
			r.toolByIx[tc.Index] = acc
			r.toolSeq = append(r.toolSeq, tc.Index)
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

func (r *responsesSSE) Usage(u raccoon.Usage) { r.usage = u }
func (r *responsesSSE) Finish(reason string) {}
func (r *responsesSSE) Complete() {
	text := r.output.String()
	_ = r.emit("response.output_text.done", map[string]any{
		"type": "response.output_text.done", "item_id": r.itemID,
		"output_index": 0, "content_index": 0, "text": text,
	})
	_ = r.emit("response.content_part.done", map[string]any{
		"type": "response.content_part.done", "item_id": r.itemID,
		"output_index": 0, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
	})
	_ = r.emit("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": 0,
		"item": map[string]any{
			"id": r.itemID, "type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
		},
	})
	_ = r.emit("response.completed", map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id": r.id, "object": "response", "created_at": time.Now().Unix(),
			"status": "completed", "model": r.model,
			"output": []any{map[string]any{
				"id": r.itemID, "type": "message", "role": "assistant", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
			}},
			"usage": map[string]any{
				"input_tokens": r.usage.PromptTokens, "output_tokens": r.usage.CompletionTokens,
				"total_tokens": r.usage.TotalTokens,
			},
		},
	})
}

// ---------- 非流式 ----------

type responsesAgg struct {
	model   string
	id      string
	itemID  string
	content strings.Builder
	reason  strings.Builder
	usage   raccoon.Usage
	toolMap map[int]*toolAcc
	toolSeq []int
}

func newResponsesAgg(model string) *responsesAgg {
	suffix := raccoon.NewTurnID()
	return &responsesAgg{
		model: model, id: "resp_" + suffix, itemID: "msg_" + suffix,
		toolMap: map[int]*toolAcc{},
	}
}

func (a *responsesAgg) Begin() error { return nil }
func (a *responsesAgg) Delta(d raccoon.Delta) error {
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
func (a *responsesAgg) Usage(u raccoon.Usage) { a.usage = u }
func (a *responsesAgg) Finish(string)        {}
func (a *responsesAgg) Complete()            {}

func (a *responsesAgg) Message() map[string]any {
	var output []any
	text := a.content.String()
	if text != "" || len(a.toolSeq) == 0 {
		output = append(output, map[string]any{
			"id": a.itemID, "type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
		})
	}
	for _, idx := range a.toolSeq {
		acc := a.toolMap[idx]
		args := acc.args.String()
		if args == "" {
			args = "{}"
		}
		output = append(output, map[string]any{
			"type": "function_call", "id": "fc_" + raccoon.NewTurnID(),
			"call_id": acc.id, "name": acc.name, "arguments": args, "status": "completed",
		})
	}
	return map[string]any{
		"id": a.id, "object": "response", "created_at": time.Now().Unix(),
		"status": "completed", "model": a.model, "output": output,
		"usage": map[string]any{
			"input_tokens": a.usage.PromptTokens, "output_tokens": a.usage.CompletionTokens,
			"total_tokens": a.usage.TotalTokens,
		},
	}
}

// HandleResponses 处理 /v1/responses。
func (s *Server) HandleResponses(w http.ResponseWriter, r *http.Request) {
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

	oaBody := responsesToOpenAI(body)
	cc := BuildChatContext(model, oaBody, DefaultModel)
	start := time.Now()

	if streamReq {
		sink := newResponsesSSE(w, cc.Model)
		res, err := s.engine.RunOpenAI(r.Context(), cc, sink)
		s.logUsage(r.Context(), res, err, start, "responses", nil)
		return
	}

	sink := newResponsesAgg(cc.Model)
	res, err := s.engine.RunOpenAI(r.Context(), cc, sink)
	s.logUsage(r.Context(), res, err, start, "responses", &sink.usage)
	if err != nil {
		code, msg := publicError(err)
		writeJSON(w, code, errorBody(code, msg))
		return
	}
	writeJSON(w, http.StatusOK, sink.Message())
}
