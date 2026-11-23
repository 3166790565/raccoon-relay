package raccoon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
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
	Body  map[string]any
	Token string
	IDN   Identity
	UID   string
	Title string
	// Session 是 X-RACCOON-Session-ID：同一段对话跨轮不变。
	Session string
	// TurnID 是 X-RACCOON-Turn-ID：工具续轮复用同一个值。
	// 为空时（例如其它上游复用本结构）退化为每次请求随机生成。
	TurnID string
}

// summary 是排障用的请求侧摘要：模型与客户端指纹，不含任何凭据。
// 上游的 403 很多是设备指纹/渠道被判异常，光看响应体分不清是哪一次请求。
func (r ChatRequest) summary(turnID string) string {
	model, _ := r.Body["model"].(string)
	var b strings.Builder
	fmt.Fprintf(&b, "模型：%s\n", model)
	fmt.Fprintf(&b, "turn：%s\n", turnID)
	fmt.Fprintf(&b, "客户端：%s / %s / %s\n", ClientName, CurrentFingerprint().ClientVersion, r.IDN.ClientChannel)
	fmt.Fprintf(&b, "平台：%s\n", r.IDN.Platform)
	fmt.Fprintf(&b, "设备：%s\n", r.IDN.DeviceID)
	fmt.Fprintf(&b, "会话：%s\n", r.Session)
	if isTeam(r.IDN.OfficeIdentity) {
		fmt.Fprintf(&b, "组织：%s\n", r.IDN.OfficeIdentity)
	}
	return strings.TrimRight(b.String(), "\n")
}

// OpenChat 发起上游请求，返回可迭代的事件通道。
// 调用方负责在拿到首帧前失败时换号；一旦从这里读到事件就说明已经写出过内容。
func (c *Client) OpenChat(ctx context.Context, req ChatRequest) (*ChatStream, error) {
	raw, err := json.Marshal(req.Body)
	if err != nil {
		return nil, err
	}
	// 流式响应用「无数据空闲超时」管：每秒都可能来帧，但整条流可能跑几十分钟。
	// 这里只负责在空闲超期时取消请求，计时由 ChatStream 在每次读到数据后重置。
	streamCtx, cancel := context.WithCancel(ctx)
	turnID := strings.TrimSpace(req.TurnID)
	if turnID == "" {
		turnID = NewTurnID()
	}
	h := chatHeaders(req.Token, req.IDN, req.Session, turnID, titleTrim(req.Title))
	httpReq, err := http.NewRequestWithContext(streamCtx, http.MethodPost, c.url(PathChat), bytes.NewReader(raw))
	if err != nil {
		cancel()
		return nil, err
	}
	for k, v := range h {
		httpReq.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		cancel()
		// 连响应头都没等到就失败时同样要保留 Cause：客户端在建连阶段取消
		// （例如 App 关掉页面）与上游真的连不上，对调度层是两种完全不同的处置。
		return nil, &UpstreamError{Status: 0, Message: "上游连接失败: " + err.Error(),
			Endpoint: PathChat, TurnID: turnID, Cause: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		body = normalizeBodyEncoding(body)
		resp.Body.Close()
		cancel()
		// 业务错误码必须带出来：200108（账号异常）、6004（模型限流）这些
		// 都挂在响应体的 {code,message} 上，只看 HTTP 状态会全部退化成 429/4xx。
		code, codeName := codeFromBodyEx(body)
		ue := &UpstreamError{Status: resp.StatusCode, Code: code, CodeName: codeName,
			Message:    upstreamErrText(resp.StatusCode, body),
			RetryAfter: RetryAfter(resp.Header, body),
			Endpoint:   PathChat, TurnID: turnID, RequestSummary: req.summary(turnID)}
		ue.attachDiagnostics(resp.Header, body)
		return nil, ue
	}
	sc := bufio.NewScanner(resp.Body)
	// 单帧上限 4MB：图片/长工具参数偶尔会撑到几百 KB
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	s := &ChatStream{resp: resp, scanner: sc, cancel: cancel, parent: ctx, idle: c.IdleTimeout}
	if s.idle > 0 {
		s.timer = time.AfterFunc(s.idle, cancel)
	}
	return s, nil
}

// NewChatStream 用已经建立的上游响应构造流。供其它上游实现（日日新）
// 复用同一套 SSE 解析与空闲超时：它的 token-plan 端点也是 OpenAI 方言，
// 帧格式与小浣熊一致，转发层只认 ChatEvent。
func NewChatStream(resp *http.Response, scanner *bufio.Scanner, cancel context.CancelFunc,
	parent context.Context, idle time.Duration) *ChatStream {
	s := &ChatStream{resp: resp, scanner: scanner, cancel: cancel, parent: parent, idle: idle}
	if idle > 0 {
		s.timer = time.AfterFunc(idle, cancel)
	}
	return s
}

type ChatStream struct {
	resp    *http.Response
	scanner *bufio.Scanner
	cancel  context.CancelFunc
	// parent 是调用方的 ctx；它被取消说明是客户端断开，不是上游空闲。
	parent context.Context
	// idle>0 时，timer 在空闲超期后取消请求；每读到一帧就重置。
	idle  time.Duration
	timer *time.Timer
	seen  bool
	err   error

	// OnEvent 是可选的事件改写钩子：在事件返回给调用方之前调用。
	// 目前只有 CodeArts 用它把截断过的工具名还原成原名。
	// 必须在第一次调用 Next 之前设置。
	OnEvent func(*ChatEvent)
}

// Seen 表示已经产出过内容，此后失败不能换号重试。
func (s *ChatStream) Seen() bool { return s.seen }

func (s *ChatStream) Close() {
	if s.timer != nil {
		s.timer.Stop()
	}
	if s.resp != nil && s.resp.Body != nil {
		s.resp.Body.Close()
	}
	if s.cancel != nil {
		s.cancel()
	}
}

// Next 读到下一个事件；io.EOF 表示上游正常结束。
func (s *ChatStream) Next() (*ChatEvent, error) {
	if s.err != nil {
		return nil, s.err
	}
	for s.scanner.Scan() {
		// 收到任何数据就说明上游还活着，重置空闲计时
		if s.timer != nil {
			s.timer.Reset(s.idle)
		}
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
		var frame streamFrame
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			continue // 坏帧跳过，不中断整条流
		}
		if ue := frame.errorOf(); ue != nil {
			return nil, ue
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
			if s.OnEvent != nil {
				s.OnEvent(ev)
			}
			return ev, nil
		}
	}
	if err := s.scanner.Err(); err != nil {
		// 读流失败有三种来源，必须分开：
		//   - 请求方的 ctx 已取消：客户端断开 / 主动停止，与上游无关；
		//   - 我们自己的空闲计时器取消了请求：上游太久没出数据；
		//   - 其余：上游连接真的断了。
		// 前两种的底层都是 context.Canceled，只看错误串分不出来，只能看父 ctx。
		clientGone := s.parent != nil && s.parent.Err() != nil
		switch {
		case clientGone:
			// Cause 直接用父 ctx 的 Err()，不要用 scanner 的原始错误：
			// net/http 在取消了 ctx 之后读响应体，HTTP/1.1 报的是
			// "net/http: request canceled"（普通字符串，不是 sentinel），
			// HTTP/2 才报 context.Canceled。转发层靠 errors.Is(err, context.Canceled)
			// 判「请求方已经走了」，只有父 ctx 的 Err() 两个协议下都稳定。
			s.err = &UpstreamError{Status: 0, Message: "客户端已断开，上游请求已取消", Cause: s.parent.Err()}
		case s.idle > 0 && errors.Is(err, context.Canceled):
			// 取消只能来自我们自己的空闲计时器。这里不挂 Cause：
			// 上游静默是上游侧的问题，不能让它被误判成客户端断开而跳过退避。
			s.err = &UpstreamError{Status: 0,
				Message: fmt.Sprintf("上游超过 %d 秒无数据，已断开", int(s.idle.Seconds()))}
		default:
			s.err = &UpstreamError{Status: 0, Message: "上游流读取中断: " + err.Error(), Cause: err}
		}
		return nil, s.err
	}
	return nil, io.EOF
}

// streamFrame 是上游流里的一帧。两种错误形态都要认：
//
//   - OpenAI 形态：{"error":{code,message}}，或顶层 {code,message}
//   - Anthropic 形态：{"type":"error","error":{type,message}}
//
// 上游在流中途才判定失败时会发这种帧，HTTP 仍是 200；不识别就只能当坏帧跳过，
// 最后表现为「上游返回空响应」，把一条明确的限流/拒答错报成网络问题。
type streamFrame struct {
	Type  string `json:"type"`
	Error *struct {
		// Code 用 RawMessage：上游会把错误码发成字符串（`"insufficient_points"`），
		// 直接解成 int 会让整帧解析失败，被误判成「坏帧」跳过，最后表现为
		// 「上游返回空响应」——一条明确的限流/拒答就此变成网络问题。
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
	} `json:"error"`
	Code    json.RawMessage `json:"code"`
	Message string          `json:"message"`

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

// errorOf 把一帧判成上游错误，返回 nil 表示这是正常数据帧。
//
// 状态码一律留 0 而不是 502：流内错误帧的 HTTP 本来就是 200，这个 502 是
// 编出来的，而转发层看到 502 会按「上游波动」换号——限流、积分耗尽这类
// 应当冷却账号的错误于是被当成偶发故障。留 0 让分类器按业务码与文案判，
// 判不出来时才落到「上游波动」的兜底分支（与 HTTP 非 2xx 的处置一致）。
func (f *streamFrame) errorOf() *UpstreamError {
	if f.Type == "error" && f.Error != nil {
		code, name := frameCode(f.Error.Code)
		return &UpstreamError{
			Status:   0,
			Message:  firstNonEmpty(f.Error.Message, "上游返回错误帧"),
			Code:     code,
			CodeName: name,
		}
	}
	if f.Error != nil {
		if code, name := frameCode(f.Error.Code); code != 0 || name != "" || f.Error.Message != "" {
			return &UpstreamError{Status: 0, Code: code, CodeName: name, Message: f.Error.Message}
		}
	}
	if code, name := frameCode(f.Code); (code != 0 && code != 200) || name != "" {
		if len(f.Choices) == 0 {
			return &UpstreamError{Status: 0, Code: code, CodeName: name,
				Message: firstNonEmpty(f.Message, "上游返回错误帧")}
		}
	}
	return nil
}

// frameCode 把一帧里的 code 解析成 (数字码, 文本码)。
// 数字与数字字符串都归数字码；其它字符串（`insufficient_points`）归文本码。
func frameCode(raw json.RawMessage) (int, string) {
	if len(raw) == 0 {
		return 0, ""
	}
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			return 0, ""
		}
		if v, err := strconv.Atoi(s); err == nil {
			return v, ""
		}
		return 0, s
	}
	return 0, ""
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

// codeFromBody 从错误体里取业务错误码。
//
// 上游的信封形态不止一种，实测出现过：
//
//	{code, message}
//	{error:{code, message}}
//	{error_code, error_msg}
//	{data:{code, message}}
//	{ResponseMetadata:{Error:{Code}}}
//
// HTTP 状态码只是传输层的结果，200108 这类业务码才是分类依据。
// 数字与数字字符串都认（有些实现把 code 序列化成字符串）。
func codeFromBody(body []byte) int {
	var outer map[string]json.RawMessage
	if err := json.Unmarshal(body, &outer); err != nil {
		return 0
	}
	for _, key := range []string{"code", "error_code", "errorCode"} {
		if v := codeValue(outer[key]); v != 0 {
			return v
		}
	}
	for _, key := range []string{"error", "data"} {
		raw, ok := outer[key]
		if !ok {
			continue
		}
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(raw, &nested); err != nil {
			continue
		}
		if v := codeValue(nested["code"]); v != 0 {
			return v
		}
	}
	if raw, ok := outer["ResponseMetadata"]; ok {
		var meta map[string]json.RawMessage
		if err := json.Unmarshal(raw, &meta); err == nil {
			var e map[string]json.RawMessage
			if err := json.Unmarshal(meta["Error"], &e); err == nil {
				if v := codeValue(e["Code"]); v != 0 {
					return v
				}
			}
		}
	}
	return 0
}

// codeFromBodyEx 与 codeFromBody 相同，但把**文本码**也带出来。
//
// 上游有些错误把 code 发成字符串名字（`insufficient_points` / `rate_limit`）：
// 数字解析会失败，只看数字码等于「没有错误码」。文本码交给分类器按名字判，
// 不认识的也保留下来供文案与日志使用。
func codeFromBodyEx(body []byte) (int, string) {
	if len(body) == 0 {
		return 0, ""
	}
	var outer map[string]json.RawMessage
	if err := json.Unmarshal(body, &outer); err != nil {
		return 0, ""
	}
	first := func(raw json.RawMessage) (int, string) {
		if v := codeValue(raw); v != 0 {
			return v, ""
		}
		return 0, codeNameValue(raw)
	}
	for _, key := range []string{"code", "error_code", "errorCode", "error_type"} {
		if code, name := first(outer[key]); code != 0 || name != "" {
			return code, name
		}
	}
	for _, key := range []string{"error", "data"} {
		raw, ok := outer[key]
		if !ok {
			continue
		}
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(raw, &nested); err != nil {
			continue
		}
		for _, k := range []string{"code", "error_code", "errorCode", "type"} {
			if code, name := first(nested[k]); code != 0 || name != "" {
				return code, name
			}
		}
	}
	if raw, ok := outer["ResponseMetadata"]; ok {
		var meta map[string]json.RawMessage
		if err := json.Unmarshal(raw, &meta); err == nil {
			var e map[string]json.RawMessage
			if err := json.Unmarshal(meta["Error"], &e); err == nil {
				for _, k := range []string{"Code", "code"} {
					if code, name := first(e[k]); code != 0 || name != "" {
						return code, name
					}
				}
			}
		}
	}
	return 0, ""
}

// codeNameValue 把一个非数字的 code 形态读成文本码；数字/布尔/对象一律返回空串。
func codeNameValue(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// 纯数字串已在 codeValue 里处理过；能走到这里的都是名字形态。
	if _, err := strconv.Atoi(s); err == nil {
		return ""
	}
	return s
}

// quotaCodeNames 是上游用**名字**表达「余额/积分不足」的码。
// 这些码有时挂在 HTTP 429 上——不认名字就会被当成限流，把一个再也调不通的号
// 短暂放出来反复撞墙。
var quotaCodeNames = map[string]bool{
	"insufficient_points": true,
	"insufficient_quota":  true,
	"arrearage":           true,
}

// rateLimitCodeNames 是上游用名字表达「限流」的码。
var rateLimitCodeNames = map[string]bool{
	"rate_limit":              true,
	"rate_limit_exceeded":     true,
	"user_model_rate_limited": true,
}

// IsQuotaCode 判断业务码是不是「余额/积分不足」（数字码或名字码）。
func IsQuotaCode(code int, codeName string) bool {
	if code == 1000007 {
		return true
	}
	return quotaCodeNames[strings.ToLower(strings.TrimSpace(codeName))]
}

// IsRateLimitCode 判断业务码是不是「限流」（名字码；数字码由 429 状态兜底）。
func IsRateLimitCode(codeName string) bool {
	return rateLimitCodeNames[strings.ToLower(strings.TrimSpace(codeName))]
}

// codeValue 把 JSON 数字或数字字符串转成 int；其它形态返回 0。
func codeValue(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return n
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if v, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			return v
		}
	}
	return 0
}

// normalizeBodyEncoding 把上游错误体统一成 UTF-8。
//
// 小浣熊的部分错误文案是 GBK 编码发出的（Windows 客户端链路常见），
// 直接 json.Unmarshal 会得到一串「ϵͳ��⵽」：字段能解出来，但人在面板上看不懂，
// 也无法按 error.code 之外的条件检索。这里只在「不是合法 UTF-8」时按 GB18030 转码，
// 合法 UTF-8 原样返回，正常的 JSON 不会被误伤。
func normalizeBodyEncoding(body []byte) []byte {
	if len(body) == 0 || utf8.Valid(body) {
		return body
	}
	if out, err := simplifiedchinese.GB18030.NewDecoder().Bytes(body); err == nil {
		return out
	}
	return body
}

// upstreamErrText 尽量从错误体里抠出可读文案。
//
// 嵌套形态（`{"error":{"message":...}}`）也要认：只读顶层 message/msg 时，
// 这种响应会把整段 JSON 原样塞进面板，人读到的是一坨花括号而不是错误原因。
func upstreamErrText(status int, body []byte) string {
	body = normalizeBodyEncoding(body)
	var env Envelope
	if err := json.Unmarshal(body, &env); err == nil {
		if m := env.ErrMessage(); m != "" {
			return m
		}
	}
	if m := nestedErrMessage(body); m != "" {
		return m
	}
	s := strings.TrimSpace(string(body))
	// 按字符截断，不按字节：上游错误体常常是中文，按字节切会把汉字切成半个，
	// 面板上直接显示成乱码。上限放宽到 2000，足够放下完整错误 JSON。
	if r := []rune(s); len(r) > 2000 {
		s = string(r[:2000]) + "…"
	}
	if s == "" {
		return fmt.Sprintf("HTTP %d", status)
	}
	return s
}

// nestedErrMessage 从 `{error:{message|msg}}` / `{error:"..."}` 这类嵌套形态里取文案。
func nestedErrMessage(body []byte) string {
	var outer map[string]json.RawMessage
	if err := json.Unmarshal(body, &outer); err != nil {
		return ""
	}
	raw, ok := outer["error"]
	if !ok {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text)
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(raw, &nested); err != nil {
		return ""
	}
	for _, key := range []string{"message", "msg", "error_msg", "detail"} {
		var value string
		if err := json.Unmarshal(nested[key], &value); err == nil {
			if value = strings.TrimSpace(value); value != "" {
				return value
			}
		}
	}
	return ""
}
