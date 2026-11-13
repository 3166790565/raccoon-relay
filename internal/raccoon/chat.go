package raccoon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Delta 是流式增量。Reasoning 单独拆出来，便于 Anthropic 侧映射成 thinking 块。
type Delta struct {
	Content   string
	Reasoning string
	ToolCalls []ToolCallDelta
}

type ToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type Usage struct {
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
}

// ChatEvent 是上游对话流的抽象：一个增量、或终帧的 usage / finish_reason。
type ChatEvent struct {
	Delta        *Delta
	Usage        *Usage
	FinishReason string
}

// PrepareBody 按上游契约改写请求体：强制流式、补默认 max_tokens、清理不认的字段。
func PrepareBody(model string, in map[string]any, defaultModel string) map[string]any {
	out := make(map[string]any, len(in)+4)
	for k, v := range in {
		out[k] = v
	}
	out["model"] = resolveModel(model, defaultModel)
	out["stream"] = true
	so, _ := out["stream_options"].(map[string]any)
	if so == nil {
		so = map[string]any{}
	}
	so["include_usage"] = true
	out["stream_options"] = so

	if mt, ok := out["max_tokens"]; !ok || isZero(mt) {
		if mct, has := out["max_completion_tokens"]; has && !isZero(mct) {
			out["max_tokens"] = toNumber(mct)
		} else {
			out["max_tokens"] = 80000
		}
	}
	delete(out, "max_completion_tokens")
	delete(out, "conversation_id")
	delete(out, "conversationId")
	delete(out, "prompt_cache_key")
	return NormalizeReasoning(out, out["model"].(string))
}

func resolveModel(model, defaultModel string) string {
	m := strings.TrimSpace(model)
	if defaultModel == "" {
		defaultModel = "raccoon-chat-ml-5-5"
	}
	if m == "" || strings.EqualFold(m, "raccoon-chat") || strings.EqualFold(m, "raccoon-chat-ml") {
		return defaultModel
	}
	return m
}

func isZero(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case float64:
		return t == 0
	case int:
		return t == 0
	case int64:
		return t == 0
	case json.Number:
		f, err := t.Float64()
		return err == nil && f == 0
	}
	return false
}

func toNumber(v any) any {
	if f, ok := v.(float64); ok {
		return int64(f)
	}
	return v
}

// titleOf 取首条用户文本前 20 字做会话标题。
func titleOf(messages []any) string {
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok || msg["role"] != "user" {
			continue
		}
		if s, ok := msg["content"].(string); ok {
			return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
		}
	}
	return ""
}

func titleTrim(s string) string {
	r := []rune(s)
	if len(r) > 20 {
		return string(r[:20])
	}
	return s
}

// ChatRequest 是转发所需的全部上下文。
type ChatRequest struct {
	Body    map[string]any
	Token   string
	IDN     Identity
	UID     string
	Title   string
	Session string
}

// OpenChat 发起上游请求，返回可迭代的事件通道。
// 调用方负责在拿到首帧前失败时换号；一旦从这里读到事件就说明已经写出过内容。
func (c *Client) OpenChat(ctx context.Context, req ChatRequest) (*ChatStream, error) {
	raw, err := json.Marshal(req.Body)
	if err != nil {
		return nil, err
	}
	h := chatHeaders(req.Token, req.IDN, req.Session, NewTurnID(), titleTrim(req.Title))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(PathChat), bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	for k, v := range h {
		httpReq.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, &UpstreamError{Status: 0, Message: "上游连接失败: " + err.Error()}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		return nil, &UpstreamError{Status: resp.StatusCode, Message: upstreamErrText(resp.StatusCode, body)}
	}
	sc := bufio.NewScanner(resp.Body)
	// 单帧上限 4MB：图片/长工具参数偶尔会撑到几百 KB
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	return &ChatStream{resp: resp, scanner: sc}, nil
}

type ChatStream struct {
	resp    *http.Response
	scanner *bufio.Scanner
	seen    bool
	err     error
}

// Seen 表示已经产出过内容，此后失败不能换号重试。
func (s *ChatStream) Seen() bool { return s.seen }

func (s *ChatStream) Close() {
	if s.resp != nil && s.resp.Body != nil {
		s.resp.Body.Close()
	}
}

// Next 读到下一个事件；io.EOF 表示上游正常结束。
func (s *ChatStream) Next() (*ChatEvent, error) {
	if s.err != nil {
		return nil, s.err
	}
	for s.scanner.Scan() {
		line := strings.TrimRight(s.scanner.Text(), "\r")
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var frame struct {
			Error *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
			Code    int    `json:"code"`
			Message string `json:"message"`
			Choices []struct {
				Delta        map[string]any `json:"delta"`
				Message      map[string]any `json:"message"`
				FinishReason *string        `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
				TotalTokens      int64 `json:"total_tokens"`
				InputTokens      int64 `json:"input_tokens"`
				OutputTokens     int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			continue // 坏帧跳过，不中断整条流
		}
		if frame.Error != nil && (frame.Error.Code != 0 || frame.Error.Message != "") {
			return nil, &UpstreamError{Status: 502, Code: frame.Error.Code, Message: frame.Error.Message}
		}
		if frame.Code != 0 && frame.Code != 200 && len(frame.Choices) == 0 {
			return nil, &UpstreamError{Status: 502, Code: frame.Code, Message: firstNonEmpty(frame.Message, "上游返回错误帧")}
		}

		ev := &ChatEvent{}
		got := false
		if len(frame.Choices) > 0 {
			ch := frame.Choices[0]
			d := ch.Delta
			if d == nil {
				d = ch.Message
			}
			if delta := extractDelta(d); delta != nil {
				ev.Delta = delta
				got = true
				s.seen = true
			}
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				ev.FinishReason = *ch.FinishReason
				got = true
			}
		}
		if frame.Usage != nil {
			ev.Usage = &Usage{
				PromptTokens:     firstNonZero(frame.Usage.PromptTokens, frame.Usage.InputTokens),
				CompletionTokens: firstNonZero(frame.Usage.CompletionTokens, frame.Usage.OutputTokens),
				TotalTokens:      frame.Usage.TotalTokens,
			}
			got = true
		}
		if got {
			return ev, nil
		}
	}
	if err := s.scanner.Err(); err != nil {
		s.err = &UpstreamError{Status: 0, Message: "上游流读取中断: " + err.Error()}
		return nil, s.err
	}
	return nil, io.EOF
}

func extractDelta(d map[string]any) *Delta {
	if d == nil {
		return nil
	}
	out := &Delta{}
	if c, ok := d["content"].(string); ok && c != "" {
		out.Content = c
	}
	for _, key := range []string{"reasoning_content", "reasoning"} {
		if c, ok := d[key].(string); ok && c != "" {
			out.Reasoning = c
			break
		}
	}
	if raw, ok := d["tool_calls"].([]any); ok && len(raw) > 0 {
		for _, item := range raw {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			var tc ToolCallDelta
			tc.Index = int(numOf(m["index"]))
			tc.ID, _ = m["id"].(string)
			tc.Type, _ = m["type"].(string)
			if fn, ok := m["function"].(map[string]any); ok {
				tc.Function.Name, _ = fn["name"].(string)
				tc.Function.Arguments, _ = fn["arguments"].(string)
			}
			out.ToolCalls = append(out.ToolCalls, tc)
		}
	}
	if out.Content == "" && out.Reasoning == "" && len(out.ToolCalls) == 0 {
		return nil
	}
	return out
}

func numOf(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case json.Number:
		f, _ := t.Float64()
		return f
	}
	return 0
}

func firstNonZero(vals ...int64) int64 {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}

// upstreamErrText 尽量从错误体里抠出可读文案。
func upstreamErrText(status int, body []byte) string {
	var env Envelope
	if err := json.Unmarshal(body, &env); err == nil {
		if m := env.ErrMessage(); m != "" {
			return m
		}
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200]
	}
	if s == "" {
		return fmt.Sprintf("HTTP %d", status)
	}
	return s
}
