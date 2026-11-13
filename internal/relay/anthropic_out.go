package relay

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/3166790565/raccoon-relay/internal/raccoon"
)

// anthropicSSE 把上游增量翻译成 Anthropic SSE 事件序列。
// 事件顺序：message_start → (content_block_start/delta/stop)* → message_delta → message_stop
type anthropicSSE struct {
	w       http.ResponseWriter
	flusher http.Flusher
	model   string
	id      string

	started  bool
	finished bool
	seq      int64
	usage    raccoon.Usage

	// 当前打开的块
	blockOpen bool
	blockType string // "text" | "thinking" | "tool_use"
	blockIdx  int
	toolIdx   int
	toolID    string
	toolName  string

	toolByIndex map[int]*toolBlock
	toolOrder   []int
	nextToolBlk int
	stopReason  string
}

type toolBlock struct {
	accepting bool
	blockIdx  int
	id        string
	name      string
}

func newAnthropicSSE(w http.ResponseWriter, model string) *anthropicSSE {
	fl, _ := w.(http.Flusher)
	return &anthropicSSE{
		w: w, flusher: fl, model: model,
		id: "msg_" + raccoon.NewTurnID(), toolByIndex: map[int]*toolBlock{},
	}
}

func (a *anthropicSSE) emit(event string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(a.w, "event: %s\ndata: %s\n\n", event, raw); err != nil {
		return err
	}
	if a.flusher != nil {
		a.flusher.Flush()
	}
	return nil
}

func (a *anthropicSSE) Begin() error {
	if a.started {
		return nil
	}
	a.started = true
	h := a.w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	a.w.WriteHeader(http.StatusOK)

	return a.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": a.id, "type": "message", "role": "assistant",
			"model": a.model, "content": []any{},
			"stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

// ensureBlock 切换到指定类型的内容块，必要时先关掉当前块。
func (a *anthropicSSE) ensureBlock(kind string) error {
	if a.blockOpen && a.blockType == kind {
		return nil
	}
	if a.blockOpen {
		if err := a.closeBlock(); err != nil {
			return err
		}
	}
	idx := a.seq
	a.seq++
	a.blockOpen = true
	a.blockType = kind
	a.blockIdx = int(idx)

	var contentBlock map[string]any
	switch kind {
	case "thinking":
		contentBlock = map[string]any{"type": "thinking", "thinking": ""}
	case "text":
		contentBlock = map[string]any{"type": "text", "text": ""}
	case "tool_use":
		contentBlock = map[string]any{"type": "tool_use", "id": a.toolID, "name": a.toolName, "input": map[string]any{}}
	}
	return a.emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": a.blockIdx, "content_block": contentBlock,
	})
}

func (a *anthropicSSE) closeBlock() error {
	if !a.blockOpen {
		return nil
	}
	a.blockOpen = false
	return a.emit("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": a.blockIdx,
	})
}

func (a *anthropicSSE) Delta(d raccoon.Delta) error {
	if d.Reasoning != "" {
		if err := a.ensureBlock("thinking"); err != nil {
			return err
		}
		if err := a.emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": a.blockIdx,
			"delta": map[string]any{"type": "thinking_delta", "thinking": d.Reasoning},
		}); err != nil {
			return err
		}
	}
	if d.Content != "" {
		if err := a.ensureBlock("text"); err != nil {
			return err
		}
		if err := a.emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": a.blockIdx,
			"delta": map[string]any{"type": "text_delta", "text": d.Content},
		}); err != nil {
			return err
		}
	}
	for _, tc := range d.ToolCalls {
		tb, ok := a.toolByIndex[tc.Index]
		if !ok {
			tb = &toolBlock{blockIdx: -1}
			a.toolByIndex[tc.Index] = tb
			a.toolOrder = append(a.toolOrder, tc.Index)
		}
		if tc.ID != "" {
			tb.id = tc.ID
		}
		if tc.Function.Name != "" {
			tb.name = tc.Function.Name
		}
		// 首次拿到工具名时开块；之后分片写 input_json_delta
		if !tb.accepting && tb.name != "" {
			if a.blockOpen {
				if a.blockType != "tool_use" {
					if err := a.closeBlock(); err != nil {
						return err
					}
				}
			}
			a.toolID, a.toolName = tb.id, tb.name
			if err := a.ensureBlock("tool_use"); err != nil {
				return err
			}
			tb.accepting = true
			tb.blockIdx = a.blockIdx
			a.toolIdx++
		}
		if tb.accepting && tc.Function.Arguments != "" && a.blockOpen && a.blockType == "tool_use" {
			if err := a.emit("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": a.blockIdx,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *anthropicSSE) Usage(u raccoon.Usage) { a.usage = u }

func (a *anthropicSSE) Finish(reason string) { a.stopReason = reason }

// stopReason 字段单独留着，Complete 时统一映射。
var _ = time.Now

func (a *anthropicSSE) Complete() {
	if err := a.closeBlock(); err != nil {
		return
	}
	stop := mapAnthropicStop(a.stopReason, len(a.toolOrder) > 0)
	_ = a.emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": a.usage.CompletionTokens},
	})
	_ = a.emit("message_stop", map[string]any{"type": "message_stop"})
}

func mapAnthropicStop(reason string, hasTool bool) string {
	switch reason {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "stop":
		if hasTool {
			return "tool_use"
		}
		return "end_turn"
	case "":
		if hasTool {
			return "tool_use"
		}
		return "end_turn"
	}
	return "end_turn"
}

// ---------- 非流式 ----------

type anthropicAgg struct {
	model   string
	id      string
	content strings.Builder
	reason  strings.Builder
	usage   raccoon.Usage
	stop    string
	toolMap map[int]*toolAcc
	toolSeq []int
}

func newAnthropicAgg(model string) *anthropicAgg {
	return &anthropicAgg{model: model, id: "msg_" + raccoon.NewTurnID(), toolMap: map[int]*toolAcc{}}
}

func (a *anthropicAgg) Begin() error { return nil }

func (a *anthropicAgg) Delta(d raccoon.Delta) error {
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

func (a *anthropicAgg) Usage(u raccoon.Usage) { a.usage = u }
func (a *anthropicAgg) Finish(reason string) {
	if reason != "" {
		a.stop = reason
	}
}
func (a *anthropicAgg) Complete() {}

// Message 生成 Anthropic 非流式响应体。
func (a *anthropicAgg) Message() map[string]any {
	var content []any
	if r := a.reason.String(); r != "" {
		content = append(content, map[string]any{"type": "thinking", "thinking": r})
	}
	if c := a.content.String(); c != "" {
		content = append(content, map[string]any{"type": "text", "text": c})
	}
	for _, idx := range a.toolSeq {
		acc := a.toolMap[idx]
		var input any = map[string]any{}
		if acc.args.Len() > 0 {
			var parsed any
			if err := json.Unmarshal([]byte(acc.args.String()), &parsed); err == nil {
				input = parsed
			} else {
				input = map[string]any{}
			}
		}
		content = append(content, map[string]any{
			"type": "tool_use", "id": acc.id, "name": acc.name, "input": input,
		})
	}
	stop := mapAnthropicStop(a.stop, len(a.toolSeq) > 0)
	return map[string]any{
		"id": a.id, "type": "message", "role": "assistant", "model": a.model,
		"content": content, "stop_reason": stop, "stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens": a.usage.PromptTokens, "output_tokens": a.usage.CompletionTokens,
		},
	}
}
